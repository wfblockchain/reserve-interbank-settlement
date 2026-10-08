package network

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"reserve-interbank-settlement/internal/anvil"
	"reserve-interbank-settlement/internal/chain"
)

// deployed starts anvil and deploys the system for three banks. It skips when
// anvil or the Foundry artifacts are missing.
func deployed(t *testing.T) (*Network, context.Context) {
	t.Helper()
	bin, err := anvil.Bin()
	if err != nil {
		t.Skip("anvil not found (set ANVIL_BIN)")
	}
	art, _ := filepath.Abs("../../contracts/out")
	if _, err := os.Stat(filepath.Join(art, "SettlementToken.sol")); err != nil {
		t.Skip("contracts not built (forge build)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	url, stop, err := anvil.Start(ctx, bin, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(stop)
	c, err := chain.Dial(ctx, url, art)
	if err != nil {
		t.Fatal(err)
	}
	n, err := Deploy(ctx, c, []BankSpec{
		{MemberID: "BNKAUS30", Name: "Bank A", Symbol: "ADUSD", Routing: "123456780"},
		{MemberID: "BNKBUS30", Name: "Bank B", Symbol: "BDUSD", Routing: "234567898"},
		{MemberID: "BNKCUS30", Name: "Bank C", Symbol: "CDUSD", Routing: "012345672"},
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return n, ctx
}

func dollars(d int64) *big.Int { return Units(d * 100) }

func eq(t *testing.T, what string, got, want *big.Int) {
	t.Helper()
	if got.Cmp(want) != 0 {
		t.Fatalf("%s: got %s, want %s", what, got, want)
	}
}

func TestConversionBurnsSettlesAndMints(t *testing.T) {
	n, ctx := deployed(t)
	a, b := n.Banks["BNKAUS30"], n.Banks["BNKBUS30"]
	if err := n.Issue(ctx, a.Key.Addr, dollars(1_000)); err != nil {
		t.Fatal(err)
	}
	alice, err := n.OnboardCustomer(ctx, "BNKAUS30", "alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := n.OnboardCustomer(ctx, "BNKBUS30", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.IssueDeposit(ctx, "BNKAUS30", alice, dollars(300)); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Convert(ctx, "BNKAUS30", "BNKBUS30", alice, bob, dollars(200)); err != nil {
		t.Fatal(err)
	}
	got, _ := n.DepositBalance(ctx, "BNKBUS30", bob)
	eq(t, "bob's deposit at B", got, dollars(200))
	got, _ = n.Balance(ctx, a.Key.Addr)
	eq(t, "A's settlement money", got, dollars(800))
	got, _ = n.Balance(ctx, b.Key.Addr)
	eq(t, "B's settlement money", got, dollars(200))
	if ok, _ := n.BackingIntact(ctx); !ok {
		t.Fatal("backing broke")
	}

	// A conversion the sending bank cannot fund reverts whole.
	if err := n.IssueDeposit(ctx, "BNKAUS30", alice, dollars(5_000)); err != nil {
		t.Fatal(err)
	}
	if _, err := n.Convert(ctx, "BNKAUS30", "BNKBUS30", alice, bob, dollars(5_000)); err == nil {
		t.Fatal("an unfunded conversion went through")
	}
	got, _ = n.DepositBalance(ctx, "BNKAUS30", alice)
	eq(t, "alice keeps her deposit", got, dollars(5_100))
}

func TestCycleSettlesThroughThePlanBook(t *testing.T) {
	n, ctx := deployed(t)
	ids := []string{"BNKAUS30", "BNKBUS30", "BNKCUS30"}
	for _, id := range ids {
		if err := n.Issue(ctx, n.Banks[id].Key.Addr, dollars(10)); err != nil {
			t.Fatal(err)
		}
	}
	// A circle: A→B 100, B→C 100, C→A 90. Net: A -10, B 0, C +10.
	o1, o2, o3 := ObligationID("u1"), ObligationID("u2"), ObligationID("u3")
	for _, s := range []struct {
		from, to string
		id       [32]byte
		amt      int64
	}{{"BNKAUS30", "BNKBUS30", o1, 100}, {"BNKBUS30", "BNKCUS30", o2, 100}, {"BNKCUS30", "BNKAUS30", o3, 90}} {
		if err := n.SubmitObligation(ctx, s.from, s.id, s.to, dollars(s.amt)); err != nil {
			t.Fatal(err)
		}
	}
	net := []NetPosition{
		{Participant: n.Banks["BNKAUS30"].Key.Addr, Amount: new(big.Int).Neg(dollars(10))},
		{Participant: n.Banks["BNKBUS30"].Key.Addr, Amount: big.NewInt(0)},
		{Participant: n.Banks["BNKCUS30"].Key.Addr, Amount: dollars(10)},
	}
	if err := n.SettleCycle(ctx, CycleID("t1"), net, [][32]byte{o1, o2, o3}, 3); err != nil {
		t.Fatal(err)
	}
	for _, id := range [][32]byte{o1, o2, o3} {
		v, _ := n.Obligation(ctx, id)
		if v.Status != ObligationSettled {
			t.Fatalf("obligation not settled: %d", v.Status)
		}
	}
	got, _ := n.Balance(ctx, n.Banks["BNKAUS30"].Key.Addr)
	eq(t, "A after paying its net debit", got, big.NewInt(0))
	got, _ = n.Balance(ctx, n.Banks["BNKCUS30"].Key.Addr)
	eq(t, "C after receiving its net credit", got, dollars(20))
}

func TestIntradayDrawAndRepay(t *testing.T) {
	n, ctx := deployed(t)
	lp, err := n.NewMemberAccount(ctx, "liquidity-provider")
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, lp.Addr, dollars(1_000_000)); err != nil {
		t.Fatal(err)
	}
	if err := n.SupplyPool(ctx, lp, dollars(1_000_000)); err != nil {
		t.Fatal(err)
	}
	a := n.Banks["BNKAUS30"]
	if err := n.MintCollateral(ctx, a.Key.Addr, dollars(500_000)); err != nil {
		t.Fatal(err)
	}
	if err := n.Issue(ctx, a.Key.Addr, dollars(1_000)); err != nil { // for interest
		t.Fatal(err)
	}
	id := DrawID("d1")
	posted, err := n.Draw(ctx, "BNKAUS30", id, dollars(98_000))
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "collateral posted at 98%", posted, dollars(100_000))
	out, _ := n.Outstanding(ctx, a.Key.Addr)
	eq(t, "outstanding", out, dollars(98_000))
	if err := n.C.SetTime(ctx, time.Date(2026, 10, 1, 18, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	p, i, err := n.Repay(ctx, "BNKAUS30", id)
	if err != nil {
		t.Fatal(err)
	}
	eq(t, "principal repaid", p, dollars(98_000))
	if i.Sign() <= 0 {
		t.Fatal("no interest charged for six hours")
	}
	got, _ := n.CollateralBalance(ctx, a.Key.Addr)
	eq(t, "collateral released", got, dollars(500_000))
	if ok, _ := n.BackingIntact(ctx); !ok {
		t.Fatal("backing broke")
	}
}

func TestReserveIncomeAccruesToHolders(t *testing.T) {
	n, ctx := deployed(t)
	a, b := n.Banks["BNKAUS30"].Key.Addr, n.Banks["BNKBUS30"].Key.Addr
	_ = n.Issue(ctx, a, dollars(3_000))
	_ = n.Issue(ctx, b, dollars(1_000))
	if err := n.AccrueYield(ctx, dollars(400)); err != nil {
		t.Fatal(err)
	}
	pa, err := n.ClaimAccrual(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	pb, _ := n.ClaimAccrual(ctx, b)
	eq(t, "A's share", pa, dollars(300))
	eq(t, "B's share", pb, dollars(100))
	left, _ := n.AccrualOf(ctx, a)
	eq(t, "claimed entitlement zeroed", left, big.NewInt(0))
	if ok, _ := n.BackingIntact(ctx); !ok {
		t.Fatal("accrual minted units")
	}
}
