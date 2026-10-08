// Package rails is the payment hub's view of everything behind it: the
// clearing contracts on chain (settlement token, deposit tokens, conversion
// bridge, netting engine and plan book, intraday pool), the Fed simulator
// with the reserve account, and each member bank's core deposit ledger. It
// adapts the model to biz.Rails, in cents.
//
// The chain keys and the simulators are shared state; the biz engine
// serializes every call.
package rails

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/go-kratos/kratos/v2/log"
	"github.com/google/uuid"

	"reserve-interbank-settlement/internal/anvil"
	"reserve-interbank-settlement/internal/chain"
	"reserve-interbank-settlement/internal/clearing"
	"reserve-interbank-settlement/internal/fedwire"
	"reserve-interbank-settlement/internal/iso20022"
	"reserve-interbank-settlement/internal/network"

	"reserve-interbank-settlement/services/payments-svc/internal/biz"
	"reserve-interbank-settlement/services/payments-svc/internal/conf"
)

// Fed accounts.
const (
	ReserveAccount  = "FRB-JOINT-RESERVE"   // the operator's joint account backing settlement money
	OperatorAccount = "FRB-MASTER-OPERATOR" // the operator's own master account
	ProviderAccount = "FRB-MASTER-PROVIDER" // the intraday pool provider's master account
	PoolMemberID    = "INTRADAY-POOL"       // how the pool appears in interest distributions
)

var operatorAgent = iso20022.Agent{BICFI: "OPERUS30", ABA: "098765438"}

func units(cents int64) *big.Int { return network.Units(cents) }
func cents(u *big.Int) int64     { return network.Cents(u) }

// ─── A bank's core deposit ledger ───

type account struct {
	name     string
	closed   bool
	balance  int64
	postings []biz.Posting
	wallet   common.Address // the customer's deposit-token wallet, once onboarded
}

type node struct {
	info     biz.BankInfo
	master   string
	accounts map[string]*account
	credited map[string]bool // conversions and obligations credited to payees
	draws    []string
	repaid   map[string]int64 // interest paid on repaid draws
	paid     int64            // reserve interest paid to date
}

func (n *node) post(at time.Time, acct string, delta int64, ref string) error {
	a, ok := n.accounts[acct]
	if !ok {
		return fmt.Errorf("no account %s", acct)
	}
	if a.balance+delta < 0 {
		return biz.ErrInsufficientFunds
	}
	a.balance += delta
	a.postings = append(a.postings, biz.Posting{At: at, Ref: ref, Delta: delta, Balance: a.balance})
	return nil
}

// ─── Off-chain records ───

type obligation struct {
	ref         string
	id          [32]byte
	bank        string
	account     string
	toBank      string
	toAccount   string
	amount      int64
	submittedAt time.Time
	expired     bool
}

type defund struct {
	bank   string
	amount int64
	state  string // REQUESTED | APPROVED | COMPLETED | FAILED
}

type inbound struct {
	ref      string
	bank     string
	account  string
	wallet   common.Address
	amount   int64
	redeemed bool // the payee's deposit token is burned; the posting remains
	unknown  bool // the redemption's outcome is unknown: operations resolve it
}

// outcome maps the chain's "sent, result unknown" onto the business layer's.
// Nothing built on such an error may compensate as if the transaction failed.
func outcome(err error) error {
	if errors.Is(err, chain.ErrOutcomeUnknown) {
		return fmt.Errorf("%w: %v", biz.ErrOutcomeUnknown, err)
	}
	return err
}

func isUnknown(err error) bool { return errors.Is(err, chain.ErrOutcomeUnknown) }

// Reserve implements biz.Rails over the clearing model.
type Reserve struct {
	net         *network.Network
	fed         *fedwire.Service
	clock       *fedwire.ManualClock
	nodes       map[string]*node
	ids         []string
	conversions map[string]biz.Conversion // by uetr: exactly once
	tokenized   map[string]bool           // by uetr: the payer's deposit is issued as a token
	refunded    map[string]bool           // by uetr: the token is back in the payer's account
	pending     []inbound                 // conversions not yet redeemed into payees' accounts
	obligations map[string]*obligation
	defunds     map[string]*defund
	netter      *clearing.Netter
	provider    chain.Account
	poolPaid    int64
	cycleSeq    int
	ttl         time.Duration
	window      uint32
	log         *log.Helper
}

var _ biz.Rails = (*Reserve)(nil)

// New connects to the chain (starting anvil when no RPC URL is given),
// deploys the system, opens the Fed accounts, funds the members' settlement
// money and the intraday pool, and opens the configured deposit accounts.
func New(ctx context.Context, c *conf.Network, logger log.Logger) (*Reserve, func(), error) {
	h := log.NewHelper(logger)
	start := time.Now()
	if c.StartTime != "" {
		var err error
		if start, err = time.Parse(time.RFC3339, c.StartTime); err != nil {
			return nil, nil, fmt.Errorf("network.start_time: %w", err)
		}
	}
	cleanup := func() {}
	rpc := c.RPCURL
	if rpc == "" {
		bin := c.AnvilBin
		if bin == "" {
			bin = "anvil"
		}
		url, stop, err := anvil.Start(context.Background(), bin, start.Add(-24*time.Hour))
		if err != nil {
			return nil, nil, fmt.Errorf("network.rpc_url is empty and anvil %q did not start: %w", bin, err)
		}
		rpc, cleanup = url, stop
		h.Infof("started anvil at %s", url)
	}
	r, err := build(ctx, c, rpc, start, h)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return r, cleanup, nil
}

func build(ctx context.Context, c *conf.Network, rpc string, start time.Time, h *log.Helper) (*Reserve, error) {
	cl, err := chain.Dial(ctx, rpc, c.Artifacts)
	if err != nil {
		return nil, fmt.Errorf("chain: %w", err)
	}
	cal, err := fedwire.NewCalendar()
	if err != nil {
		return nil, err
	}
	clock := fedwire.NewManualClock(start)
	if err := cl.SetTime(ctx, start); err != nil {
		return nil, err
	}
	fed := fedwire.New(clock, cal)

	advance := uint16(9800)
	if c.IntradayPool != nil && c.IntradayPool.AdvanceRateBps > 0 {
		advance = uint16(c.IntradayPool.AdvanceRateBps)
	}
	var specs []network.BankSpec
	for _, b := range c.Banks {
		specs = append(specs, network.BankSpec{MemberID: b.MemberID, Name: b.Name, Symbol: b.DepositToken, Routing: b.RoutingNumber})
	}
	net, err := network.Deploy(ctx, cl, specs, network.Options{CollateralAdvanceBps: advance})
	if err != nil {
		return nil, fmt.Errorf("deploy: %w", err)
	}
	r := &Reserve{net: net, fed: fed, clock: clock, nodes: map[string]*node{}, conversions: map[string]biz.Conversion{},
		tokenized: map[string]bool{}, refunded: map[string]bool{},
		obligations: map[string]*obligation{}, defunds: map[string]*defund{}, netter: clearing.NewNetter(),
		ttl: c.ObligationTTL.Std(), window: uint32(c.CycleWindow), log: h}

	fed.OpenAccount(ReserveAccount, operatorAgent, big.NewInt(0))
	fed.EnableLMT(ReserveAccount)
	fed.OpenAccount(OperatorAccount, operatorAgent, big.NewInt(0))
	for _, b := range c.Banks {
		n := &node{info: biz.BankInfo{MemberID: b.MemberID, Name: b.Name, DepositToken: b.DepositToken, Routing: b.RoutingNumber},
			master: "FRB-MASTER-" + b.RoutingNumber, accounts: map[string]*account{}, credited: map[string]bool{}, repaid: map[string]int64{}}
		fed.OpenAccount(n.master, iso20022.Agent{BICFI: b.MemberID, ABA: b.RoutingNumber}, iso20022.Dollars(5_000_000_000))
		r.nodes[b.MemberID] = n
		r.ids = append(r.ids, b.MemberID)
	}
	sort.Strings(r.ids)

	for _, b := range c.Banks {
		if b.InitialFunding != "" {
			amt, err := biz.ParseAmount(b.InitialFunding)
			if err != nil {
				return nil, fmt.Errorf("bank %s initial_funding: %w", b.MemberID, err)
			}
			if res := r.fundAt(ctx, b.MemberID, amt, "BTRC"); res.Status != iso20022.StatusAccepted {
				return nil, fmt.Errorf("fund %s: %s", b.MemberID, res.Reason)
			}
		}
		if b.Collateral != "" {
			amt, err := biz.ParseAmount(b.Collateral)
			if err != nil {
				return nil, fmt.Errorf("bank %s collateral: %w", b.MemberID, err)
			}
			if err := net.MintCollateral(ctx, net.Banks[b.MemberID].Key.Addr, units(amt)); err != nil {
				return nil, err
			}
		}
	}
	if p := c.IntradayPool; p != nil && p.ProviderFunding != "" {
		amt, err := biz.ParseAmount(p.ProviderFunding)
		if err != nil {
			return nil, fmt.Errorf("intraday_pool provider_funding: %w", err)
		}
		if err := r.supplyPool(ctx, amt); err != nil {
			return nil, fmt.Errorf("intraday pool: %w", err)
		}
	}
	for _, a := range c.Accounts {
		n, ok := r.nodes[a.Bank]
		if !ok {
			return nil, fmt.Errorf("account %s: unknown bank %s", a.Account, a.Bank)
		}
		var opening int64
		if a.Opening != "" {
			if opening, err = biz.ParseAmount(a.Opening); err != nil {
				return nil, fmt.Errorf("account %s opening: %w", a.Account, err)
			}
		}
		n.accounts[a.Account] = &account{name: a.Name, closed: a.Closed}
		if opening > 0 {
			_ = n.post(start, a.Account, opening, "OPENING")
		}
	}
	h.Infof("network deployed: %d banks, %d accounts, clock %s", len(specs), len(c.Accounts), start.Format(time.RFC3339))
	return r, nil
}

// supplyPool funds the intraday pool: the provider sends reserves into the
// reserve account, receives settlement money against them, and deposits it.
func (r *Reserve) supplyPool(ctx context.Context, amount int64) error {
	p, err := r.net.NewMemberAccount(ctx, "intraday-provider")
	if err != nil {
		return err
	}
	r.provider = p
	r.fed.OpenAccount(ProviderAccount, iso20022.Agent{BICFI: "PROVUS30"}, iso20022.Dollars(5_000_000_000))
	st := r.fed.Send(r.pacs009(ProviderAccount, ReserveAccount, "BTRC", "FUND", amount), units(amount))
	if st.TxSts != iso20022.StatusAccepted {
		return fmt.Errorf("provider funding: %s", st.Reason)
	}
	if err := r.net.Issue(ctx, p.Addr, units(amount)); err != nil {
		return err
	}
	return r.net.SupplyPool(ctx, p, units(amount))
}

func (r *Reserve) pacs009(from, to, instrument, purpose string, amount int64) iso20022.Pacs009 {
	amt, _ := iso20022.USD(units(amount))
	uetr := fedwire.NewUETR()
	return iso20022.Pacs009{GrpHdr: iso20022.GroupHeader{MsgID: "SETL-" + uetr[:8], CreDtTm: r.Now()}, EndToEndID: purpose + "-" + uetr[:8],
		UETR: uetr, LclInstrm: instrument, CtgyPurp: purpose, Amount: amt, Debtor: r.agentOf(from), Creditor: r.agentOf(to),
		DebtorAcct: from, CreditorAcct: to}
}

func (r *Reserve) agentOf(acct string) iso20022.Agent {
	for _, id := range r.ids {
		if n := r.nodes[id]; n.master == acct {
			return iso20022.Agent{BICFI: id, ABA: n.info.Routing}
		}
	}
	return operatorAgent
}

func (r *Reserve) node(bank string) (*node, error) {
	n, ok := r.nodes[bank]
	if !ok {
		return nil, fmt.Errorf("unknown bank %s", bank)
	}
	return n, nil
}

func (r *Reserve) key(bank string) common.Address { return r.net.Banks[bank].Key.Addr }

// ─── Time ───

func (r *Reserve) Now() time.Time                        { return r.clock.Now() }
func (r *Reserve) FedwireOpen(t time.Time) bool          { return r.fed.Calendar().IsOpen(t) }
func (r *Reserve) NextFedwireOpen(t time.Time) time.Time { return r.fed.Calendar().NextOpen(t) }

func (r *Reserve) SetTime(ctx context.Context, t time.Time) error {
	r.clock.Set(t)
	return r.net.C.SetTime(ctx, t)
}

// ─── Directory ───

func (r *Reserve) Banks() []biz.BankInfo {
	out := make([]biz.BankInfo, 0, len(r.ids))
	for _, id := range r.ids {
		out = append(out, r.nodes[id].info)
	}
	return out
}

func (r *Reserve) BankByRouting(routing string) (biz.BankInfo, bool) {
	for _, id := range r.ids {
		if r.nodes[id].info.Routing == routing {
			return r.nodes[id].info, true
		}
	}
	return biz.BankInfo{}, false
}

func (r *Reserve) Bank(memberID string) (biz.BankInfo, bool) {
	n, ok := r.nodes[memberID]
	if !ok {
		return biz.BankInfo{}, false
	}
	return n.info, true
}

func (r *Reserve) Member(ctx context.Context, id string) (biz.Member, error) {
	n, err := r.node(id)
	if err != nil {
		return biz.Member{}, err
	}
	k := r.key(id)
	m := biz.Member{ID: id, Name: n.info.Name, DepositToken: n.info.DepositToken, Routing: n.info.Routing, AccrualPaid: n.paid}
	reads := []struct {
		dst  *int64
		read func() (*big.Int, error)
	}{
		{&m.Settlement, func() (*big.Int, error) { return r.net.Balance(ctx, k) }},
		{&m.Earmarked, func() (*big.Int, error) { return r.net.Frozen(ctx, k) }},
		{&m.Available, func() (*big.Int, error) { return r.net.Available(ctx, k) }},
		{&m.Deposits, func() (*big.Int, error) { return r.net.DepositSupply(ctx, id) }},
		{&m.Intraday, func() (*big.Int, error) { return r.net.Outstanding(ctx, k) }},
		{&m.Collateral, func() (*big.Int, error) { return r.net.CollateralBalance(ctx, k) }},
		{&m.AccrualOwed, func() (*big.Int, error) { return r.net.AccrualOf(ctx, k) }},
	}
	for _, x := range reads {
		v, err := x.read()
		if err != nil {
			return biz.Member{}, err
		}
		*x.dst = cents(v)
	}
	if m.Admitted, err = r.net.Admitted(ctx, k); err != nil {
		return biz.Member{}, err
	}
	return m, nil
}

// ─── Core banking ───

func (r *Reserve) Account(bank, acct string) (biz.AccountRecord, bool) {
	n, ok := r.nodes[bank]
	if !ok {
		return biz.AccountRecord{}, false
	}
	a, ok := n.accounts[acct]
	if !ok {
		return biz.AccountRecord{}, false
	}
	name := a.name
	if name == "" {
		name = acct
	}
	return biz.AccountRecord{Name: name, Closed: a.closed}, true
}

func (r *Reserve) Balance(bank, acct string) int64 {
	if n, ok := r.nodes[bank]; ok {
		if a, ok := n.accounts[acct]; ok {
			return a.balance
		}
	}
	return 0
}

func (r *Reserve) Postings(bank, acct string) []biz.Posting {
	if n, ok := r.nodes[bank]; ok {
		if a, ok := n.accounts[acct]; ok {
			return append([]biz.Posting(nil), a.postings...)
		}
	}
	return nil
}

// Post is idempotent per (account, ref): the business layer's references name
// one order's leg, so replaying an order after a lost save posts nothing twice.
func (r *Reserve) Post(bank, acct string, delta int64, ref string) error {
	n, err := r.node(bank)
	if err != nil {
		return err
	}
	if a, ok := n.accounts[acct]; ok && ref != "" {
		for _, p := range a.postings {
			if p.Ref == ref {
				return nil
			}
		}
	}
	return n.post(r.Now(), acct, delta, ref)
}

func (r *Reserve) EnsureAccount(bank, acct, name string) {
	if n, ok := r.nodes[bank]; ok {
		if _, ok := n.accounts[acct]; !ok {
			n.accounts[acct] = &account{name: name}
		}
	}
}

// InFlight is the customer's deposit token not yet converted or redeemed.
func (r *Reserve) InFlight(ctx context.Context, bank, acct string) int64 {
	n, ok := r.nodes[bank]
	if !ok {
		return 0
	}
	a, ok := n.accounts[acct]
	if !ok || a.wallet == (common.Address{}) {
		return 0
	}
	b, err := r.net.DepositBalance(ctx, bank, a.wallet)
	if err != nil {
		return 0
	}
	return cents(b)
}

// wallet onboards a customer's deposit-token wallet the first time it is needed.
func (r *Reserve) wallet(ctx context.Context, bank, acct string) (common.Address, error) {
	n, err := r.node(bank)
	if err != nil {
		return common.Address{}, err
	}
	a, ok := n.accounts[acct]
	if !ok {
		return common.Address{}, fmt.Errorf("no account %s at %s", acct, n.info.Name)
	}
	if a.wallet == (common.Address{}) {
		w, err := r.net.OnboardCustomer(ctx, bank, acct)
		if err != nil {
			return common.Address{}, err
		}
		a.wallet = w
	}
	return a.wallet, nil
}

// ─── Gross, 24x7: conversion ───

func (r *Reserve) Tokenize(ctx context.Context, bank, acct string, amount int64, uetr string) error {
	if r.tokenized[uetr] {
		return nil
	}
	n, err := r.node(bank)
	if err != nil {
		return err
	}
	w, err := r.wallet(ctx, bank, acct)
	if err != nil {
		return outcome(err)
	}
	if err := n.post(r.Now(), acct, -amount, "TOKENIZE"); err != nil {
		return err
	}
	if err := r.net.IssueDeposit(ctx, bank, w, units(amount)); err != nil {
		if isUnknown(err) {
			r.tokenized[uetr] = true // it may have issued: never issue again
			return outcome(err)
		}
		_ = n.post(r.Now(), acct, amount, "TOKENIZE-REVERSED")
		return err
	}
	r.tokenized[uetr] = true
	return nil
}

func (r *Reserve) Converted(uetr string) (biz.Conversion, bool) {
	c, ok := r.conversions[uetr]
	return c, ok
}

func (r *Reserve) Convert(ctx context.Context, bank, acct string, to biz.Party, amount int64, uetr string) (biz.Conversion, error) {
	if c, ok := r.conversions[uetr]; ok {
		return c, nil
	}
	toInfo, ok := r.BankByRouting(to.Routing)
	if !ok {
		return biz.Conversion{State: biz.ConversionRejected, Reason: "RC01"}, nil
	}
	if rec, ok := r.Account(toInfo.MemberID, to.Account); !ok || rec.Closed {
		c := biz.Conversion{State: biz.ConversionRejected, Reason: "AC04"}
		r.conversions[uetr] = c
		return c, nil
	}
	from, err := r.wallet(ctx, bank, acct)
	if err != nil {
		return biz.Conversion{}, err
	}
	toWallet, err := r.wallet(ctx, toInfo.MemberID, to.Account)
	if err != nil {
		return biz.Conversion{}, err
	}
	h, err := r.net.Convert(ctx, bank, toInfo.MemberID, from, toWallet, units(amount))
	if err != nil {
		return biz.Conversion{}, outcome(err)
	}
	c := biz.Conversion{Ref: hex.EncodeToString(h[:]), State: biz.ConversionSettled}
	r.conversions[uetr] = c
	r.pending = append(r.pending, inbound{ref: c.Ref, bank: toInfo.MemberID, account: to.Account, wallet: toWallet, amount: amount})
	return c, nil
}

func (r *Reserve) Refund(ctx context.Context, bank, acct string, amount int64, uetr string) error {
	if r.refunded[uetr] {
		return nil
	}
	n, err := r.node(bank)
	if err != nil {
		return err
	}
	w, err := r.wallet(ctx, bank, acct)
	if err != nil {
		return outcome(err)
	}
	if err := r.net.RedeemDeposit(ctx, bank, w, units(amount)); err != nil {
		if isUnknown(err) {
			r.refunded[uetr] = true // it may have burned: never burn again
		}
		return outcome(err)
	}
	r.refunded[uetr] = true
	return n.post(r.Now(), acct, amount, "REFUND")
}

func (r *Reserve) ConversionCredited(receivingBank, ref string) bool {
	n, ok := r.nodes[receivingBank]
	return ok && n.credited[ref]
}

// ─── Netted ───

func (r *Reserve) SubmitObligation(ctx context.Context, bank, acct string, to biz.Party, amount int64, uetr string) (string, error) {
	n, err := r.node(bank)
	if err != nil {
		return "", err
	}
	toInfo, ok := r.BankByRouting(to.Routing)
	if !ok {
		return "", fmt.Errorf("unknown routing number %s", to.Routing)
	}
	if rec, ok := r.Account(toInfo.MemberID, to.Account); !ok || rec.Closed {
		return "", biz.ErrAccountClosed
	}
	id := network.ObligationID(uetr)
	ref := hex.EncodeToString(id[:])
	if _, ok := r.obligations[ref]; ok {
		return ref, nil
	}
	if err := n.post(r.Now(), acct, -amount, "OBLIGATION "+ref[:10]); err != nil {
		return "", err
	}
	if err := r.net.SubmitObligation(ctx, bank, id, toInfo.MemberID, units(amount)); err != nil {
		if isUnknown(err) {
			// It may be queued: keep the debit and track it, so a settled
			// cycle still credits the payee and a cancel still refunds.
			r.obligations[ref] = &obligation{ref: ref, id: id, bank: bank, account: acct, toBank: toInfo.MemberID,
				toAccount: to.Account, amount: amount, submittedAt: r.Now()}
			return "", outcome(err)
		}
		_ = n.post(r.Now(), acct, amount, "OBLIGATION-REVERSED")
		return "", err
	}
	r.obligations[ref] = &obligation{ref: ref, id: id, bank: bank, account: acct, toBank: toInfo.MemberID, toAccount: to.Account,
		amount: amount, submittedAt: r.Now()}
	return ref, nil
}

func (r *Reserve) ObligationState(ctx context.Context, ref string) (string, error) {
	o, ok := r.obligations[ref]
	if !ok {
		return "", fmt.Errorf("unknown obligation %s", ref)
	}
	if o.expired {
		return biz.ObligationExpired, nil
	}
	v, err := r.net.Obligation(ctx, o.id)
	if err != nil {
		return "", err
	}
	switch v.Status {
	case network.ObligationQueued:
		return biz.ObligationQueued, nil
	case network.ObligationSettled:
		return biz.ObligationSettled, nil
	case network.ObligationCancelled:
		return biz.ObligationCancelled, nil
	}
	return "", fmt.Errorf("obligation %s has unknown status %d", ref, v.Status)
}

func (r *Reserve) CancelObligation(ctx context.Context, bank, ref string) error {
	o, ok := r.obligations[ref]
	if !ok || o.bank != bank {
		return fmt.Errorf("unknown obligation %s", ref)
	}
	if err := r.net.CancelObligation(ctx, bank, o.id); err != nil {
		return err
	}
	return r.nodes[bank].post(r.Now(), o.account, o.amount, "OBLIGATION-CANCELLED")
}

func (r *Reserve) ObligationCredited(receivingBank, ref string) bool {
	o, ok := r.obligations[ref]
	return ok && o.toBank == receivingBank && r.nodes[receivingBank].credited[ref]
}

func (r *Reserve) queued(ctx context.Context) []*obligation {
	var out []*obligation
	for _, o := range r.obligations {
		if o.expired {
			continue
		}
		if v, err := r.net.Obligation(ctx, o.id); err == nil && v.Status == network.ObligationQueued {
			out = append(out, o)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].submittedAt.Equal(out[j].submittedAt) {
			return out[i].submittedAt.Before(out[j].submittedAt)
		}
		return out[i].ref < out[j].ref
	})
	return out
}

func (r *Reserve) QueuedObligations(ctx context.Context) int { return len(r.queued(ctx)) }

// RunCycle plans the largest feasible set of queued obligations off-chain
// with each bank's available settlement money, and settles it through the
// plan book, where the engine re-verifies every net position before moving
// value.
func (r *Reserve) RunCycle(ctx context.Context) (biz.CycleReport, error) {
	q := r.queued(ctx)
	if len(q) == 0 {
		return biz.CycleReport{}, nil
	}
	balances := map[string]*big.Int{}
	var obs []clearing.Obligation
	for _, o := range q {
		obs = append(obs, clearing.Obligation{ID: o.ref, Payer: o.bank, Payee: o.toBank, Amount: units(o.amount)})
		for _, b := range []string{o.bank, o.toBank} {
			if _, ok := balances[b]; !ok {
				v, err := r.net.Available(ctx, r.key(b))
				if err != nil {
					return biz.CycleReport{}, err
				}
				balances[b] = v
			}
		}
	}
	plan, err := r.netter.Plan(obs, balances)
	if err != nil {
		// No feasible subset: nothing settles, everything waits.
		r.log.Warnf("netting: %v", err)
		return biz.CycleReport{Deferred: len(q)}, nil
	}
	if len(plan.Discharged) == 0 {
		return biz.CycleReport{Deferred: len(plan.Excluded)}, nil
	}
	ids := make([]string, 0, len(plan.Net))
	for id := range plan.Net {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	var net []network.NetPosition
	for _, id := range ids {
		net = append(net, network.NetPosition{Participant: r.key(id), Amount: plan.Net[id]})
	}
	var discharged [][32]byte
	for _, o := range plan.Discharged {
		discharged = append(discharged, r.obligations[o.ID].id)
	}
	r.cycleSeq++
	cid := network.CycleID(fmt.Sprintf("%d-%d", r.cycleSeq, r.Now().Unix()))
	if err := r.net.SettleCycle(ctx, cid, net, discharged, r.window); err != nil {
		return biz.CycleReport{}, err
	}
	return biz.CycleReport{CycleRef: hex.EncodeToString(cid[:]), Discharged: len(plan.Discharged), Deferred: len(plan.Excluded),
		Gross: cents(plan.Stats.GrossValue), Net: cents(plan.Stats.NetFunding)}, nil
}

// ExpireStaleObligations cancels obligations queued longer than the hub's
// time-to-live and refunds the payers.
func (r *Reserve) ExpireStaleObligations(ctx context.Context) error {
	if r.ttl <= 0 {
		return nil
	}
	now := r.Now()
	for _, o := range r.queued(ctx) {
		if now.Sub(o.submittedAt) <= r.ttl {
			continue
		}
		if err := r.net.CancelObligation(ctx, o.bank, o.id); err != nil {
			return err
		}
		o.expired = true
		if err := r.nodes[o.bank].post(now, o.account, o.amount, "OBLIGATION-EXPIRED"); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reserve) EfficiencyBps(ctx context.Context) int {
	v, err := r.net.Efficiency(ctx)
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// ─── Treasury ───

// Fund moves reserves from the bank's master account into the reserve
// account, and the operator issues settlement money against them: Fedwire
// BTRC while it is open, FedNow LMT1 while it is closed.
func (r *Reserve) Fund(ctx context.Context, bank string, amount int64) biz.FundResult {
	instrument := "BTRC"
	if !r.FedwireOpen(r.Now()) {
		instrument = "LMT1"
	}
	return r.fundAt(ctx, bank, amount, instrument)
}

func (r *Reserve) fundAt(ctx context.Context, bank string, amount int64, instrument string) biz.FundResult {
	n, err := r.node(bank)
	if err != nil {
		return biz.FundResult{Instrument: instrument, Status: iso20022.StatusRejected, Reason: err.Error()}
	}
	st := r.fed.Send(r.pacs009(n.master, ReserveAccount, instrument, "FUND", amount), units(amount))
	if st.TxSts != iso20022.StatusAccepted {
		return biz.FundResult{Instrument: instrument, Status: st.TxSts, Reason: st.Reason}
	}
	if err := r.net.Issue(ctx, r.key(bank), units(amount)); err != nil {
		if isUnknown(err) {
			// It may have issued: returning the reserves could leave tokens
			// with nothing behind them. Reconciliation shows the gap until
			// operations resolve it.
			r.log.Errorf("fund %s: %v", bank, err)
			return biz.FundResult{Instrument: instrument, Status: iso20022.StatusPending, Reason: "issuance outcome unknown"}
		}
		// The reserves arrived but the token did not issue: send them back
		// at once so the books still agree.
		r.fed.Send(r.pacs009(ReserveAccount, n.master, "BTRC", "RETURN", amount), units(amount))
		return biz.FundResult{Instrument: instrument, Status: iso20022.StatusRejected, Reason: err.Error()}
	}
	return biz.FundResult{Instrument: instrument, Status: st.TxSts}
}

// RequestDefund earmarks settlement money for return to the master account.
func (r *Reserve) RequestDefund(ctx context.Context, bank string, amount int64) (string, error) {
	k := r.key(bank)
	avail, err := r.net.Available(ctx, k)
	if err != nil {
		return "", err
	}
	if cents(avail) < amount {
		return "", fmt.Errorf("available settlement money $%s is short of $%s", biz.Readable(cents(avail)), biz.Readable(amount))
	}
	if err := r.adjustEarmark(ctx, k, amount); err != nil {
		return "", err
	}
	ref := uuid.NewString()
	r.defunds[ref] = &defund{bank: bank, amount: amount, state: "REQUESTED"}
	return ref, nil
}

func (r *Reserve) adjustEarmark(ctx context.Context, k common.Address, delta int64) error {
	frozen, err := r.net.Frozen(ctx, k)
	if err != nil {
		return err
	}
	return r.net.Earmark(ctx, k, new(big.Int).Add(frozen, units(delta)))
}

// ApproveDefund completes the defund now if Fedwire is open; otherwise it
// waits for the next opening (Process).
func (r *Reserve) ApproveDefund(ctx context.Context, bank, ref string) error {
	d, ok := r.defunds[ref]
	if !ok || d.bank != bank {
		return fmt.Errorf("unknown defund %s", ref)
	}
	if d.state != "REQUESTED" {
		return fmt.Errorf("defund is %s", d.state)
	}
	d.state = "APPROVED"
	if r.FedwireOpen(r.Now()) {
		r.completeDefund(ctx, d)
	}
	return nil
}

// completeDefund releases the earmark, retires the settlement money and
// returns the reserves. Retire first: reserves never leave the account while
// the token still counts them.
func (r *Reserve) completeDefund(ctx context.Context, d *defund) {
	k := r.key(d.bank)
	if err := r.adjustEarmark(ctx, k, -d.amount); err != nil {
		r.log.Errorf("defund: release earmark: %v", err)
		return
	}
	if err := r.net.Retire(ctx, k, units(d.amount)); err != nil {
		r.log.Errorf("defund: retire: %v", err)
		if isUnknown(err) {
			d.state = "UNKNOWN" // it may have burned: neither retry nor re-issue
			return
		}
		d.state = "FAILED"
		return
	}
	st := r.fed.Send(r.pacs009(ReserveAccount, r.nodes[d.bank].master, "BTRC", "DEFUND", d.amount), units(d.amount))
	if st.TxSts != iso20022.StatusAccepted {
		r.log.Errorf("defund: fed: %s", st.Reason)
		if err := r.net.Issue(ctx, k, units(d.amount)); err != nil {
			r.log.Errorf("defund: reissue after failed transfer: %v", err)
		}
		d.state = "FAILED"
		return
	}
	d.state = "COMPLETED"
}

func (r *Reserve) DefundState(ctx context.Context, ref string) (string, error) {
	d, ok := r.defunds[ref]
	if !ok {
		return "", fmt.Errorf("unknown defund %s", ref)
	}
	if d.state == "REQUESTED" {
		return "AWAITING_APPROVAL", nil
	}
	return d.state, nil
}

// DrawIntraday borrows settlement money from the pool against demo T-bills.
func (r *Reserve) DrawIntraday(ctx context.Context, bank string, amount int64) (biz.Draw, error) {
	n, err := r.node(bank)
	if err != nil {
		return biz.Draw{}, err
	}
	ref := uuid.NewString()
	id := network.DrawID(ref)
	if _, err := r.net.Draw(ctx, bank, id, units(amount)); err != nil {
		return biz.Draw{}, err
	}
	n.draws = append(n.draws, ref)
	return r.draw(ctx, n, ref)
}

// RepayIntraday repays a draw with interest; the pool releases the collateral.
func (r *Reserve) RepayIntraday(ctx context.Context, bank, ref string) (biz.Draw, error) {
	n, err := r.node(bank)
	if err != nil {
		return biz.Draw{}, err
	}
	if !n.owns(ref) {
		return biz.Draw{}, fmt.Errorf("unknown draw %s", ref)
	}
	_, interest, err := r.net.Repay(ctx, bank, network.DrawID(ref))
	if err != nil {
		return biz.Draw{}, err
	}
	n.repaid[ref] = cents(interest)
	return r.draw(ctx, n, ref)
}

func (n *node) owns(ref string) bool {
	for _, d := range n.draws {
		if d == ref {
			return true
		}
	}
	return false
}

func (r *Reserve) draw(ctx context.Context, n *node, ref string) (biz.Draw, error) {
	v, err := r.net.DrawState(ctx, network.DrawID(ref))
	if err != nil {
		return biz.Draw{}, err
	}
	d := biz.Draw{Ref: ref, Principal: cents(v.Principal), Collateral: cents(v.CollateralAmount), RateBps: int(v.RateBps),
		OpenedAt: time.Unix(int64(v.OpenedAt), 0), Open: v.Open, Overdue: v.Overdue, Interest: cents(v.Interest)}
	if !v.Open {
		d.Interest = n.repaid[ref]
	}
	return d, nil
}

func (r *Reserve) Draws(ctx context.Context, bank string) ([]biz.Draw, error) {
	n, err := r.node(bank)
	if err != nil {
		return nil, err
	}
	out := make([]biz.Draw, 0, len(n.draws))
	for i := len(n.draws) - 1; i >= 0; i-- {
		d, err := r.draw(ctx, n, n.draws[i])
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}

func (r *Reserve) Pool(ctx context.Context) (biz.PoolState, error) {
	p, err := r.net.PoolState(ctx)
	if err != nil {
		return biz.PoolState{}, err
	}
	return biz.PoolState{Assets: cents(p.Assets), Drawn: cents(p.Drawn), UtilizationBps: int(p.UtilizationBps.Int64())}, nil
}

// ─── The operator ───

func (r *Reserve) FedReserveBalance() int64 { return cents(r.fed.Balance(ReserveAccount)) }

func (r *Reserve) ReservePool(ctx context.Context) (int64, error) {
	p, err := r.net.ReservePool(ctx)
	return cents(p), err
}

// Health: a break is the Fed's reserve account disagreeing with the token's
// reserve pool; the invariant is the token's own supply == pool.
func (r *Reserve) Health(ctx context.Context) (bool, bool) {
	pool, err := r.net.ReservePool(ctx)
	brk := err != nil || r.fed.Balance(ReserveAccount).Cmp(pool) != 0
	inv, err := r.net.BackingIntact(ctx)
	return brk, err == nil && inv
}

func (r *Reserve) Reconcile(ctx context.Context) (biz.Reconciliation, error) {
	pool, err := r.net.ReservePool(ctx)
	if err != nil {
		return biz.Reconciliation{}, err
	}
	brk, inv := r.Health(ctx)
	return biz.Reconciliation{At: r.Now(), FedBalance: cents(r.fed.Balance(ReserveAccount)), ReservePool: cents(pool),
		Break: brk, Invariants: inv}, nil
}

// DistributeInterest books interest the Fed paid on the reserve account,
// divides it among holders through the token's accrual index (no units are
// minted), and pays each member its whole-cent share over Fedwire. The pool's
// share goes to its provider. Sub-cent remainders are swept to the
// operator's own account, so the reserve account again equals the pool.
func (r *Reserve) DistributeInterest(ctx context.Context, amount int64) (biz.InterestDistribution, error) {
	if amount <= 0 {
		return biz.InterestDistribution{}, errors.New("amount must be positive")
	}
	// Accrue on chain first: if it fails, nothing has moved at the Fed.
	if err := r.net.AccrueYield(ctx, units(amount)); err != nil {
		return biz.InterestDistribution{}, outcome(err)
	}
	r.fed.PayInterest(ReserveAccount, units(amount))
	d := biz.InterestDistribution{At: r.Now(), Amount: amount, Shares: map[string]int64{}}
	// Whatever happens below, the reserve account ends equal to the pool:
	// anything the Fed credited that was not paid out is swept to the
	// operator's account and reported as retained, so a failure part-way
	// leaves owed members visible, not a reconciliation break.
	swept := false
	defer func() {
		if swept {
			return
		}
		if pool, err := r.net.ReservePool(ctx); err == nil {
			if rest := new(big.Int).Sub(r.fed.Balance(ReserveAccount), pool); rest.Sign() > 0 {
				r.fed.Send(r.pacs009(ReserveAccount, OperatorAccount, "BTRC", "INTR", cents(rest)), rest)
			}
		}
	}()
	type holder struct {
		id, master string
		addr       common.Address
		node       *node
	}
	var holders []holder
	for _, id := range r.ids {
		holders = append(holders, holder{id: id, master: r.nodes[id].master, addr: r.key(id), node: r.nodes[id]})
	}
	if r.provider.Addr != (common.Address{}) {
		holders = append(holders, holder{id: PoolMemberID, master: ProviderAccount, addr: r.net.Pool.Addr},
			holder{id: PoolMemberID, master: ProviderAccount, addr: r.provider.Addr})
	}
	holders = append(holders, holder{id: "", addr: r.net.Netting.Addr}) // float, normally zero between cycles
	for _, h := range holders {
		owed, err := r.net.ClaimAccrual(ctx, h.addr)
		if err != nil {
			return d, outcome(err)
		}
		c := cents(owed)
		if c == 0 || h.id == "" {
			continue
		}
		st := r.fed.Send(r.pacs009(ReserveAccount, h.master, "BTRC", "INTR", c), units(c))
		if st.TxSts != iso20022.StatusAccepted {
			return d, fmt.Errorf("pay %s: %s", h.id, st.Reason)
		}
		d.Shares[h.id] += c
		d.Paid += c
		if h.node != nil {
			h.node.paid += c
		} else {
			r.poolPaid += c
		}
	}
	pool, err := r.net.ReservePool(ctx)
	if err != nil {
		return d, err
	}
	if rest := new(big.Int).Sub(r.fed.Balance(ReserveAccount), pool); rest.Sign() > 0 {
		st := r.fed.Send(r.pacs009(ReserveAccount, OperatorAccount, "BTRC", "INTR", cents(rest)), rest)
		if st.TxSts != iso20022.StatusAccepted {
			return d, fmt.Errorf("sweep: %s", st.Reason)
		}
		d.Retained = cents(rest)
	}
	swept = true
	return d, nil
}

// ─── Background ───

// Process credits payees: deposit tokens that arrived by conversion are
// redeemed into the payees' accounts, and settled obligations are credited
// by the receiving bank. Approved defunds go out once Fedwire is open.
func (r *Reserve) Process(ctx context.Context) error {
	var errs []error
	rest := r.pending[:0]
	for _, p := range r.pending {
		n := r.nodes[p.bank]
		if p.unknown {
			rest = append(rest, p)
			continue
		}
		if !p.redeemed {
			if err := r.net.RedeemDeposit(ctx, p.bank, p.wallet, units(p.amount)); err != nil {
				errs = append(errs, fmt.Errorf("credit %s: %w", p.ref, err))
				p.unknown = isUnknown(err) // a second burn could take another payment's token
				rest = append(rest, p)
				continue
			}
			p.redeemed = true
		}
		if err := n.post(r.Now(), p.account, p.amount, "CONVERSION "+p.ref[:10]); err != nil {
			errs = append(errs, err)
			rest = append(rest, p)
			continue
		}
		n.credited[p.ref] = true
	}
	r.pending = rest
	for _, o := range r.obligations {
		n := r.nodes[o.toBank]
		if o.expired || n.credited[o.ref] {
			continue
		}
		v, err := r.net.Obligation(ctx, o.id)
		if err != nil || v.Status != network.ObligationSettled {
			continue
		}
		if err := n.post(r.Now(), o.toAccount, o.amount, "OBLIGATION "+o.ref[:10]); err != nil {
			errs = append(errs, err)
			continue
		}
		n.credited[o.ref] = true
	}
	if r.FedwireOpen(r.Now()) {
		for _, d := range r.defunds {
			if d.state == "APPROVED" {
				r.completeDefund(ctx, d)
			}
		}
	}
	return errors.Join(errs...)
}
