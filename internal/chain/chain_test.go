package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"reserve-interbank-settlement/internal/anvil"
)

// flaky forwards JSON-RPC to anvil but, for the next n transactions, drops
// the reply after the node has accepted them, and fails the next m receipt
// lookups: the two ways a send can look failed while it executed.
type flaky struct {
	target       string
	mu           sync.Mutex
	dropReplies  int
	failReceipts int
	sends        int
}

func (f *flaky) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	_ = json.Unmarshal(body, &req)
	f.mu.Lock()
	if req.Method == "eth_getTransactionReceipt" && f.failReceipts > 0 {
		f.failReceipts--
		f.mu.Unlock()
		w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(req.ID) + `,"error":{"code":-32603,"message":"flaky"}}`))
		return
	}
	drop := false
	if req.Method == "eth_sendRawTransaction" {
		f.sends++
		if f.dropReplies > 0 {
			f.dropReplies--
			drop = true
		}
	}
	f.mu.Unlock()
	resp, err := http.Post(f.target, "application/json", bytes.NewReader(body))
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	if drop {
		if c, _, err := w.(http.Hijacker).Hijack(); err == nil {
			c.Close()
		}
		return
	}
	w.Write(out)
}

func TestLostReplyIsNotAFailure(t *testing.T) {
	bin, err := anvil.Bin()
	if err != nil {
		t.Skip("anvil not found (set ANVIL_BIN)")
	}
	art, _ := filepath.Abs("../../contracts/out")
	if _, err := os.Stat(filepath.Join(art, "DemoTreasuryBill.sol")); err != nil {
		t.Skip("contracts not built (forge build)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	url, stop, err := anvil.Start(ctx, bin, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	f := &flaky{target: url}
	srv := httptest.NewServer(f)
	defer srv.Close()
	c, err := Dial(ctx, srv.URL, art)
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := NewAccount("admin")
	if err := c.FundGas(ctx, admin.Addr); err != nil {
		t.Fatal(err)
	}
	tbill, err := c.Deploy(ctx, admin, "DemoTreasuryBill", admin.Addr)
	if err != nil {
		t.Fatal(err)
	}
	holder, _ := NewAccount("holder")

	// The node accepts the mint, the reply is lost, and the next receipt
	// lookups fail too. Send must still report the mint, exactly once.
	f.mu.Lock()
	f.dropReplies, f.failReceipts = 1, 3
	f.mu.Unlock()
	if _, err := tbill.Send(ctx, admin, "mint", holder.Addr, big.NewInt(1_000_000)); err != nil {
		t.Fatalf("a mined transaction was reported as failed: %v", err)
	}
	bal, err := tbill.BigInt(ctx, "balanceOf", holder.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if bal.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("balance %s: the mint ran %s times", bal, new(big.Int).Quo(bal, big.NewInt(1_000_000)))
	}
	f.mu.Lock()
	sends := f.sends
	f.mu.Unlock()
	if sends < 2 {
		t.Fatalf("the signed transaction was not resent (%d sends)", sends)
	}
	t.Logf("%d sends of the same signed bytes, one execution", sends)

	// A revert is still a failure, reported at once.
	other, _ := NewAccount("other")
	if err := c.FundGas(ctx, other.Addr); err != nil {
		t.Fatal(err)
	}
	if _, err := tbill.Send(ctx, other, "mint", holder.Addr, big.NewInt(1)); err == nil {
		t.Fatal("an unauthorized mint succeeded")
	}
}
