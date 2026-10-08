package test

// Chaos experiments: the service runs against a chain behind a fault-injecting
// JSON-RPC proxy (or a database held locked, or a process killed), drives a
// randomized payment workload with clients that retry the way real ones do,
// then lets everything settle and checks the invariants that must survive any
// fault:
//
//   I1  the Fed reserve account equals the token's reserve pool (no break);
//   I2  client money is conserved: the three organizations' balances plus all
//       deposit tokens outstanding equal what they opened with;
//   I3  nothing is stranded: no deposit token outstanding once quiet;
//   I4  every organization's balance equals its opening, minus what its
//       settled payments sent, plus what payments credited to it brought;
//   I5  no order is left in a non-terminal state once quiet.
//
// Run with CHAOS=1 (optionally CHAOS_EXPERIMENTS=E1,E4 and CHAOS_PAYMENTS=60).

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	nethttp "net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"reserve-interbank-settlement/internal/anvil"
	pb "reserve-interbank-settlement/services/payments-svc/api/payments/v1"
)

// ─── Fault-injecting JSON-RPC proxy ───

type faults struct {
	LostReply  float64       // eth_sendRawTransaction: forward, then drop the reply
	PreFail    float64       // eth_sendRawTransaction: drop before forwarding
	ReceiptErr float64       // eth_getTransactionReceipt: answer with an error
	ReadErr    float64       // eth_call: answer with an error
	Latency    time.Duration // up to this much delay on every call
}

type proxy struct {
	target *url.URL
	mu     sync.Mutex
	f      faults
	rnd    *rand.Rand
	counts map[string]int
}

func newProxy(t *testing.T, target string) (*proxy, string) {
	u, _ := url.Parse(target)
	p := &proxy{target: u, rnd: rand.New(rand.NewSource(42)), counts: map[string]int{}}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return p, srv.URL
}

func (p *proxy) set(f faults) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.f = f
}

func (p *proxy) roll(prob float64, what string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prob > 0 && p.rnd.Float64() < prob {
		p.counts[what]++
		return true
	}
	return false
}

func (p *proxy) latency() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.f.Latency <= 0 {
		return 0
	}
	return time.Duration(p.rnd.Int63n(int64(p.f.Latency)))
}

func (p *proxy) faults() faults {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.f
}

func (p *proxy) report() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]string, 0, len(p.counts))
	for k := range p.counts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, p.counts[k]))
	}
	return strings.Join(parts, " ")
}

func hangUp(w nethttp.ResponseWriter) {
	if hj, ok := w.(nethttp.Hijacker); ok {
		if c, _, err := hj.Hijack(); err == nil {
			c.Close()
			return
		}
	}
	w.WriteHeader(nethttp.StatusBadGateway)
}

func (p *proxy) ServeHTTP(w nethttp.ResponseWriter, r *nethttp.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &req) // batches pass through untouched
	f := p.faults()
	if d := p.latency(); d > 0 {
		time.Sleep(d)
	}
	rpcErr := func(what string) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%s,"error":{"code":-32603,"message":"chaos: %s"}}`, req.ID, what)
	}
	switch req.Method {
	case "eth_sendRawTransaction":
		if p.roll(f.PreFail, "pre-fail") {
			hangUp(w)
			return
		}
	case "eth_getTransactionReceipt":
		if p.roll(f.ReceiptErr, "receipt-err") {
			rpcErr("receipt backend unavailable")
			return
		}
	case "eth_call":
		if p.roll(f.ReadErr, "read-err") {
			rpcErr("state backend unavailable")
			return
		}
	}
	resp, err := nethttp.Post(p.target.String(), "application/json", bytes.NewReader(body))
	if err != nil {
		w.WriteHeader(nethttp.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if req.Method == "eth_sendRawTransaction" && p.roll(f.LostReply, "lost-reply") {
		hangUp(w) // the transaction is in; the caller never hears so
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}

// ─── The world under test ───

type chaosOrg struct {
	id, maker, approver, account, routing, bank, staff string
	opening                                          int64
}

var chaosOrgs = []chaosOrg{
	{"northwind", "tok-nw-maker", "tok-nw-treasurer", "A-3000001", aRouting, "BNKAUS30", "tok-bank-a-treasury", 150_000_000_00},
	{"contoso", "tok-co-maker", "tok-co-approver", "B-4000001", bRouting, "BNKBUS30", "tok-bank-b-treasury", 80_000_000_00},
	{"fabrikam", "tok-fa-maker", "tok-fa-approver", "C-5000001", cRouting, "BNKCUS30", "tok-bank-c-treasury", 70_000_000_00},
}

var chaosNames = map[string]string{"northwind": "Northwind Corp", "contoso": "Contoso Ltd", "fabrikam": "Fabrikam Inc"}

type world struct {
	t     *testing.T
	e     env
	s     *server
	c     *clients
	proxy *proxy
	dsn   string
	rnd   *rand.Rand
	log   []string
	seq   int // payments issued so far, so keys stay unique across batches
	mu    sync.Mutex
}

// newWorld starts anvil and, with proxied set, a fault proxy in front of it,
// then the service on that chain.
func newWorld(t *testing.T, e env, bin string, proxied bool) *world {
	w := &world{t: t, e: e, rnd: rand.New(rand.NewSource(7))}
	var extra []string
	if proxied {
		genesis, _ := time.Parse(time.RFC3339, "2026-10-01T16:00:00-04:00")
		url, stop, err := anvil.Start(context.Background(), e.anvil, genesis)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stop)
		var purl string
		w.proxy, purl = newProxy(t, url)
		extra = append(extra, "RPC_URL="+purl)
	}
	w.s = launch(t, e, bin, extra)
	w.c = newClients(t, w.s.base)
	for _, kv := range w.s.env {
		if strings.HasPrefix(kv, "DATABASE_URL=") {
			w.dsn = strings.TrimPrefix(kv, "DATABASE_URL=")
		}
	}
	return w
}

func retryable(err error) bool {
	if err == nil {
		return false
	}
	c := code(err)
	return c == 0 || c >= 500
}

// pay creates and approves one payment the way a client integration does:
// it retries creates and approvals that fail with a server error, reusing
// the Idempotency-Key.
func (w *world) pay(i int, urgent bool) {
	w.mu.Lock()
	from := chaosOrgs[w.rnd.Intn(3)]
	to := chaosOrgs[(indexOf(from.id)+1+w.rnd.Intn(2))%3]
	amount := fmt.Sprintf("%d.%02d", 10_000+w.rnd.Intn(1_990_000), w.rnd.Intn(100))
	w.mu.Unlock()
	prio := "NORMAL"
	if urgent {
		prio = "URGENT"
	}
	key := fmt.Sprintf("chaos-%d", i)
	req := &pb.CreatePaymentRequest{Creditor: &pb.Party{Name: chaosNames[to.id], RoutingNumber: to.routing, Account: to.account},
		Amount: usd(amount), Priority: prio}
	var id string
	for try := 0; try < 4; try++ {
		r, err := w.c.pay.CreatePayment(withKey(as(from.maker), key), req)
		if err == nil {
			id = r.Payment.Id
			break
		}
		if !retryable(err) {
			w.logf("create %s: %v", key, err)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	if id == "" {
		w.logf("create %s: gave up", key)
		return
	}
	for try := 0; try < 4; try++ {
		_, err := w.c.pay.ApprovePayment(as(from.approver), &pb.ApprovePaymentRequest{Id: id})
		if err == nil || code(err) == 409 {
			return
		}
		if !retryable(err) {
			w.logf("approve %s: %v", key, err)
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	w.logf("approve %s: gave up", key)
}

func indexOf(id string) int {
	for i, o := range chaosOrgs {
		if o.id == id {
			return i
		}
	}
	return -1
}

func (w *world) logf(format string, a ...any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.log = append(w.log, fmt.Sprintf(format, a...))
}

func (w *world) cycle() {
	if _, err := w.c.op.RunNettingCycle(as("tok-operator-ops"), &pb.RunNettingCycleRequest{}); err != nil {
		w.logf("cycle: %v", err)
	}
}

// workload runs n payments over workers goroutines, with a netting cycle
// every eight payments.
func (w *world) workload(n, workers int) {
	var wg sync.WaitGroup
	next := make(chan int)
	var mu sync.Mutex
	rnds := make([]*rand.Rand, workers)
	for k := range rnds {
		rnds[k] = rand.New(rand.NewSource(int64(100 + k)))
	}
	for k := 0; k < workers; k++ {
		wg.Add(1)
		go func(r *rand.Rand) {
			defer wg.Done()
			for i := range next {
				mu.Lock()
				urgent := r.Intn(2) == 0
				mu.Unlock()
				w.pay(i, urgent)
				if i%8 == 7 {
					w.cycle()
				}
			}
		}(rnds[k])
	}
	w.mu.Lock()
	base := w.seq
	w.seq += n
	w.mu.Unlock()
	for i := 0; i < n; i++ {
		next <- base + i
	}
	close(next)
	wg.Wait()
}

// orders lists every payment of the three banks' clients.
func (w *world) orders() []*pb.Payment {
	var all []*pb.Payment
	for _, o := range chaosOrgs {
		r, err := w.c.ops.ListBankPayments(as(o.staff), &pb.ListBankPaymentsRequest{PageSize: 1000})
		if err != nil {
			w.t.Fatalf("list %s: %v", o.bank, err)
		}
		for _, p := range r.Payments {
			if p.OrganizationId == o.id {
				all = append(all, p)
			}
		}
	}
	return all
}

func terminal(p *pb.Payment) bool {
	switch p.Status {
	case "REJECTED", "CANCELLED", "EXPIRED", "BLOCKED":
		return true
	case "SETTLED":
		return p.IsoStatus == "ACCC"
	}
	return false
}

// quiesce turns faults off, funds every bank generously, runs cycles until
// the queue is empty, and waits for every order to finish.
func (w *world) quiesce() {
	if w.proxy != nil {
		w.proxy.set(faults{})
	}
	for _, o := range chaosOrgs {
		if _, err := w.c.ops.FundSettlement(as(o.staff), &pb.FundSettlementRequest{Amount: usd("100000000.00")}); err != nil {
			w.logf("final funding %s: %v", o.bank, err)
		}
	}
	// After an outage a client re-approves what is still waiting for it.
	approver := map[string]string{}
	for _, o := range chaosOrgs {
		approver[o.id] = o.approver
	}
	for _, p := range w.orders() {
		if p.Status == "AWAITING_APPROVAL" {
			if _, err := w.c.pay.ApprovePayment(as(approver[p.OrganizationId]), &pb.ApprovePaymentRequest{Id: p.Id}); err != nil && code(err) != 409 {
				w.logf("re-approve %s: %v", p.EndToEndId, err)
			}
		}
	}
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		w.cycle()
		done := true
		for _, p := range w.orders() {
			if !terminal(p) {
				done = false
				break
			}
		}
		if done {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// check evaluates the invariants and returns the violations.
func (w *world) check() []string {
	var bad []string
	rec, err := w.c.op.Reconcile(as("tok-operator-ops"), &pb.ReconcileRequest{})
	if err != nil {
		bad = append(bad, "I1 reconcile failed: "+err.Error())
	} else if rec.ReconciliationBreak || !rec.InvariantsHold {
		bad = append(bad, fmt.Sprintf("I1 reconciliation break: Fed reserve %s, token pool %s, supply==pool %v",
			rec.FedBalance.Amount, rec.ReservePool.Amount, rec.InvariantsHold))
	}
	net, err := w.c.op.GetNetwork(as("tok-operator-ops"), &pb.GetNetworkRequest{})
	if err != nil {
		return append(bad, "network view: "+err.Error())
	}
	var deposits, balances, opening int64
	for _, m := range net.Members {
		deposits += cents(m.Deposits.Amount)
	}
	actual := map[string]int64{}
	for _, o := range chaosOrgs {
		a, err := w.c.acct.GetAccount(as(o.maker), &pb.GetAccountRequest{})
		if err != nil {
			return append(bad, "account: "+err.Error())
		}
		actual[o.id] = cents(a.Balance.Amount)
		balances += actual[o.id]
		opening += o.opening
		if f := cents(a.InFlight.Amount); f != 0 {
			bad = append(bad, fmt.Sprintf("I3 %s has $%s stranded as deposit tokens", o.id, money(f)))
		}
	}
	if balances+deposits != opening {
		bad = append(bad, fmt.Sprintf("I2 client money not conserved: balances $%s + deposit tokens $%s = $%s, opened with $%s",
			money(balances), money(deposits), money(balances+deposits), money(opening)))
	}
	if deposits != 0 {
		bad = append(bad, fmt.Sprintf("I3 $%s of deposit tokens outstanding when quiet", money(deposits)))
	}
	expected := map[string]int64{}
	for _, o := range chaosOrgs {
		expected[o.id] = o.opening
	}
	byAccount := map[string]string{}
	for _, o := range chaosOrgs {
		byAccount[o.account] = o.id
	}
	for _, p := range w.orders() {
		amt := cents(p.Amount.Amount)
		if !terminal(p) {
			bad = append(bad, fmt.Sprintf("I5 %s stuck in %s/%s (%s)", p.EndToEndId, p.Status, p.IsoStatus, lastNote(p)))
			continue
		}
		if p.Status == "SETTLED" {
			expected[p.OrganizationId] -= amt
			expected[byAccount[p.Creditor.Account]] += amt
		}
	}
	for _, o := range chaosOrgs {
		if expected[o.id] != actual[o.id] {
			bad = append(bad, fmt.Sprintf("I4 %s balance $%s, but its payment records imply $%s (off by $%s)",
				o.id, money(actual[o.id]), money(expected[o.id]), money(actual[o.id]-expected[o.id])))
		}
	}
	return bad
}

func lastNote(p *pb.Payment) string {
	if len(p.History) == 0 {
		return ""
	}
	return p.History[len(p.History)-1].Note
}

func money(c int64) string {
	sign := ""
	if c < 0 {
		sign, c = "-", -c
	}
	return fmt.Sprintf("%s%d.%02d", sign, c/100, c%100)
}

// statuses summarizes order outcomes.
func (w *world) statuses() string {
	n := map[string]int{}
	for _, p := range w.orders() {
		k := p.Status
		if p.ReasonCode != "" {
			k += "/" + p.ReasonCode
		}
		n[k]++
	}
	keys := make([]string, 0, len(n))
	for k := range n {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, n[k]))
	}
	return strings.Join(parts, " ")
}

// lockDB holds an exclusive lock on the SQLite database for d: every write
// and read the service makes waits out its busy timeout and fails.
func (w *world) lockDB(d time.Duration) {
	path := strings.TrimPrefix(strings.SplitN(w.dsn, "?", 2)[0], "file:")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		w.t.Fatal(err)
	}
	conn, err := db.Conn(context.Background())
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		w.t.Fatalf("lock: %v", err)
	}
	go func() {
		time.Sleep(d)
		_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		conn.Close()
		db.Close()
	}()
}

// ─── The experiments ───

func TestChaos(t *testing.T) {
	if os.Getenv("CHAOS") == "" {
		t.Skip("set CHAOS=1 to run the chaos experiments")
	}
	e := prerequisites(t)
	race := os.Getenv("CHAOS_RACE") != ""
	bin := buildService(t, e, race)
	n := 48
	if v, err := strconv.Atoi(os.Getenv("CHAOS_PAYMENTS")); err == nil && v > 0 {
		n = v
	}
	want := map[string]bool{}
	for _, x := range strings.Split(os.Getenv("CHAOS_EXPERIMENTS"), ",") {
		if x != "" {
			want[strings.TrimSpace(x)] = true
		}
	}
	run := func(name, what string, fn func(t *testing.T)) {
		if len(want) > 0 && !want[name] {
			return
		}
		t.Run(name, func(t *testing.T) {
			t.Logf("%s: %s", name, what)
			fn(t)
		})
	}
	finish := func(t *testing.T, w *world) {
		w.quiesce()
		bad := w.check()
		t.Logf("outcomes: %s", w.statuses())
		if w.proxy != nil {
			t.Logf("faults injected: %s", w.proxy.report())
		}
		for _, l := range w.log {
			t.Logf("client: %s", l)
		}
		for _, b := range bad {
			t.Errorf("VIOLATION %s", b)
		}
		if race {
			if b, _ := os.ReadFile(w.s.log.Name()); bytes.Contains(b, []byte("WARNING: DATA RACE")) {
				t.Errorf("VIOLATION data race in the service log:\n%s", raceExcerpt(b))
			}
		}
	}

	run("E0", "control: no faults, sequential", func(t *testing.T) {
		w := newWorld(t, e, bin, true)
		w.workload(n, 1)
		finish(t, w)
	})
	run("E1", "lost replies: 15% of sent transactions are mined but the reply never arrives", func(t *testing.T) {
		w := newWorld(t, e, bin, true)
		w.proxy.set(faults{LostReply: 0.15})
		w.workload(n, 1)
		finish(t, w)
	})
	run("E2", "clean transient faults: 20% of sends fail before reaching the chain, 10% of reads error, up to 150ms latency", func(t *testing.T) {
		w := newWorld(t, e, bin, true)
		w.proxy.set(faults{PreFail: 0.2, ReadErr: 0.1, Latency: 150 * time.Millisecond})
		w.workload(n, 1)
		finish(t, w)
	})
	run("E3", "receipt lookups fail: 15% of receipt polls error although the transaction is mined", func(t *testing.T) {
		w := newWorld(t, e, bin, true)
		w.proxy.set(faults{ReceiptErr: 0.15})
		w.workload(n, 1)
		finish(t, w)
	})
	run("E4", "database outage: an exclusive lock holds the database for 30s while clients keep paying", func(t *testing.T) {
		w := newWorld(t, e, bin, true)
		w.workload(n/2, 1)
		w.lockDB(30 * time.Second)
		w.workload(6, 1) // runs into the outage: saves fail after the busy timeout, clients retry
		time.Sleep(31 * time.Second)
		w.workload(n/2-6, 1)
		finish(t, w)
	})
	run("E5", "concurrency: eight clients at once", func(t *testing.T) {
		w := newWorld(t, e, bin, true)
		w.workload(n, 8)
		finish(t, w)
	})
	run("E6", "crash: SIGKILL mid-workload, then restart on the same database", func(t *testing.T) {
		w := newWorld(t, e, bin, true)
		w.workload(n/2, 1)
		w.s.crash()
		// The rails' state died with the process. A restart must refuse the
		// database rather than pair its orders with fresh ledgers.
		out := w.s.runExpectingRefusal(t)
		if !strings.Contains(out, "refusing to start") {
			t.Errorf("VIOLATION restart did not fail closed:\n%s", tail(out, 20))
		} else {
			t.Logf("restart refused: %s", firstLine(out, "refusing to start"))
		}
	})
}

func tail(s string, n int) string {
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func firstLine(s, sub string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			if i := strings.Index(l, sub); i >= 0 {
				return l[i:]
			}
		}
	}
	return ""
}

func raceExcerpt(b []byte) string {
	i := bytes.Index(b, []byte("WARNING: DATA RACE"))
	j := i + 3000
	if j > len(b) {
		j = len(b)
	}
	return string(b[i:j])
}
