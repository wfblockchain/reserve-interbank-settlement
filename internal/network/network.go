// Package network deploys the clearing contracts on a chain and exposes the
// operations a member bank and the operator perform on them: issuing and
// redeeming settlement money against the reserve pool, converting deposits
// between banks, queueing and netting obligations, intraday draws against
// collateral, and passing reserve income through the accrual index.
//
// Roles are wired exactly as the Foundry tests wire them. The operator key is
// the clearing house (issuance, compliance, netting operator, member admin);
// each bank key is a member's settlement account and the issuer and customer
// admin of its own deposit token.
package network

import (
	"context"
	"crypto/sha256"
	"fmt"
	"math/big"
	"sort"

	"github.com/ethereum/go-ethereum/common"

	"reserve-interbank-settlement/internal/chain"
)

// MemberPolicy is the registry policy for settlement-token holders.
const MemberPolicy uint64 = 1

// UnitsPerCent is the scale of every token: 6 decimals per dollar.
var UnitsPerCent = big.NewInt(10_000)

// Units converts cents to token base units.
func Units(cents int64) *big.Int { return new(big.Int).Mul(big.NewInt(cents), UnitsPerCent) }

// Cents converts token base units to cents, truncating sub-cent dust.
func Cents(u *big.Int) int64 {
	if u == nil {
		return 0
	}
	return new(big.Int).Quo(u, UnitsPerCent).Int64()
}

// BankSpec describes a member bank.
type BankSpec struct {
	MemberID string // BIC
	Name     string
	Symbol   string // deposit token symbol
	Routing  string // ABA
}

// Bank is a deployed member: its settlement key and its deposit token.
type Bank struct {
	Spec   BankSpec
	Key    chain.Account
	Token  *chain.Contract
	Policy uint64 // the registry policy of its customers
}

// Options tune the deployment.
type Options struct {
	CollateralAdvanceBps uint16 // intraday pool advance rate on the demo T-bill
}

// Network is the deployed system.
type Network struct {
	C          *chain.Client
	Operator   chain.Account
	Registry   *chain.Contract
	Settlement *chain.Contract
	Netting    *chain.Contract
	PlanBook   *chain.Contract
	Bridge     *chain.Contract
	Pool       *chain.Contract
	Collateral *chain.Contract
	Banks      map[string]*Bank
	ids        []string
}

// Deploy deploys and wires the system for the given banks.
func Deploy(ctx context.Context, c *chain.Client, specs []BankSpec, o Options) (*Network, error) {
	op, err := chain.NewAccount("operator")
	if err != nil {
		return nil, err
	}
	if err := c.FundGas(ctx, op.Addr); err != nil {
		return nil, err
	}
	n := &Network{C: c, Operator: op, Banks: map[string]*Bank{}}
	if o.CollateralAdvanceBps == 0 {
		o.CollateralAdvanceBps = 9800
	}

	steps := []struct {
		dst  **chain.Contract
		name string
		args func() []any
	}{
		{&n.Registry, "ParticipantRegistry", func() []any { return []any{op.Addr} }},
		{&n.Settlement, "SettlementToken", func() []any {
			return []any{"Settlement Dollar", "SUSD", uint8(6), n.Registry.Addr, MemberPolicy, op.Addr}
		}},
		{&n.Netting, "NettingEngine", func() []any { return []any{n.Settlement.Addr, op.Addr} }},
		{&n.PlanBook, "NettingPlanBook", func() []any { return []any{n.Netting.Addr, op.Addr} }},
		{&n.Bridge, "ConversionBridge", func() []any { return []any{n.Settlement.Addr, op.Addr} }},
		{&n.Pool, "IntradayLiquidityPool", func() []any {
			return []any{n.Settlement.Addr, n.Registry.Addr, MemberPolicy, op.Addr}
		}},
		{&n.Collateral, "DemoTreasuryBill", func() []any { return []any{op.Addr} }},
	}
	for _, s := range steps {
		k, err := c.Deploy(ctx, op, s.name, s.args()...)
		if err != nil {
			return nil, err
		}
		*s.dst = k
	}

	grants := []struct {
		k    *chain.Contract
		role string
		to   common.Address
	}{
		{n.Settlement, "CLEARING_HOUSE_ROLE", op.Addr},
		{n.Settlement, "COMPLIANCE_ROLE", op.Addr},
		{n.Settlement, "SETTLEMENT_ROLE", n.Netting.Addr},
		{n.Settlement, "SETTLEMENT_ROLE", n.Bridge.Addr},
		// The plan book drives the engine; the operator key drives the book.
		{n.Netting, "OPERATOR_ROLE", n.PlanBook.Addr},
		{n.Netting, "OPERATOR_ROLE", op.Addr},
		{n.PlanBook, "OPERATOR_ROLE", op.Addr},
		{n.Bridge, "OPERATOR_ROLE", op.Addr},
		{n.Pool, "GOVERNOR_ROLE", op.Addr},
	}
	for _, g := range grants {
		if err := n.grant(ctx, g.k, g.role, g.to); err != nil {
			return nil, err
		}
	}
	if err := n.grantPolicyAdmin(ctx, MemberPolicy, op.Addr); err != nil {
		return nil, err
	}
	// The engine holds the float mid-cycle and the pool holds lent cash, so
	// both must be able to send and receive settlement money.
	for _, a := range []common.Address{n.Netting.Addr, n.Pool.Addr} {
		if err := n.AdmitMember(ctx, a); err != nil {
			return nil, err
		}
	}
	if _, err := n.Pool.Send(ctx, op, "registerCollateral", n.Collateral.Addr, o.CollateralAdvanceBps); err != nil {
		return nil, err
	}

	for i, s := range specs {
		if err := n.addBank(ctx, s, uint64(100+i)); err != nil {
			return nil, fmt.Errorf("bank %s: %w", s.MemberID, err)
		}
	}
	return n, nil
}

func (n *Network) grant(ctx context.Context, k *chain.Contract, role string, to common.Address) error {
	r, err := k.Call(ctx, role)
	if err != nil {
		return err
	}
	_, err = k.Send(ctx, n.Operator, "grantRole", r[0].([32]byte), to)
	return err
}

func (n *Network) grantPolicyAdmin(ctx context.Context, policy uint64, to common.Address) error {
	r, err := n.Registry.Call(ctx, "policyAdminRole", policy)
	if err != nil {
		return err
	}
	_, err = n.Registry.Send(ctx, n.Operator, "grantRole", r[0].([32]byte), to)
	return err
}

func (n *Network) addBank(ctx context.Context, s BankSpec, policy uint64) error {
	key, err := chain.NewAccount(s.MemberID)
	if err != nil {
		return err
	}
	if err := n.C.FundGas(ctx, key.Addr); err != nil {
		return err
	}
	tok, err := n.C.Deploy(ctx, n.Operator, "DepositToken", s.Name+" Deposit Dollar", s.Symbol, uint8(6),
		n.Registry.Addr, policy, n.Operator.Addr)
	if err != nil {
		return err
	}
	b := &Bank{Spec: s, Key: key, Token: tok, Policy: policy}
	for _, g := range []struct {
		k    *chain.Contract
		role string
		to   common.Address
	}{
		{tok, "BANK_ROLE", key.Addr},
		{tok, "COMPLIANCE_ROLE", key.Addr},
		{tok, "CONVERSION_ROLE", n.Bridge.Addr},
		{n.Netting, "PARTICIPANT_ROLE", key.Addr},
	} {
		if err := n.grant(ctx, g.k, g.role, g.to); err != nil {
			return err
		}
	}
	// The bank governs its own customers; the operator governs membership.
	if err := n.grantPolicyAdmin(ctx, policy, key.Addr); err != nil {
		return err
	}
	if err := n.AdmitMember(ctx, key.Addr); err != nil {
		return err
	}
	if _, err := n.Bridge.Send(ctx, n.Operator, "registerBank", key.Addr, tok.Addr); err != nil {
		return err
	}
	n.Banks[s.MemberID] = b
	n.ids = append(n.ids, s.MemberID)
	sort.Strings(n.ids)
	return nil
}

// IDs lists member ids in order.
func (n *Network) IDs() []string { return append([]string(nil), n.ids...) }

// AdmitMember admits an address to hold settlement money.
func (n *Network) AdmitMember(ctx context.Context, a common.Address) error {
	_, err := n.Registry.Send(ctx, n.Operator, "admit", MemberPolicy, a)
	return err
}

// NewMemberAccount creates and admits a settlement-token holder that is not a
// bank, such as an intraday liquidity provider.
func (n *Network) NewMemberAccount(ctx context.Context, name string) (chain.Account, error) {
	a, err := chain.NewAccount(name)
	if err != nil {
		return a, err
	}
	if err := n.C.FundGas(ctx, a.Addr); err != nil {
		return a, err
	}
	return a, n.AdmitMember(ctx, a.Addr)
}

// ─── Customers and deposit tokens ───

// OnboardCustomer creates a customer wallet and the bank admits it to its
// deposit token. Customers never send transactions: the bank's back office
// acts for them.
func (n *Network) OnboardCustomer(ctx context.Context, bankID, name string) (common.Address, error) {
	b, ok := n.Banks[bankID]
	if !ok {
		return common.Address{}, fmt.Errorf("unknown bank %s", bankID)
	}
	a, err := chain.NewAccount(name)
	if err != nil {
		return common.Address{}, err
	}
	_, err = n.Registry.Send(ctx, b.Key, "admit", b.Policy, a.Addr)
	return a.Addr, err
}

// IssueDeposit tokenizes a customer deposit.
func (n *Network) IssueDeposit(ctx context.Context, bankID string, customer common.Address, u *big.Int) error {
	b := n.Banks[bankID]
	_, err := b.Token.Send(ctx, b.Key, "issueDeposit", customer, u)
	return err
}

// RedeemDeposit takes a tokenized deposit back onto the bank's core ledger.
func (n *Network) RedeemDeposit(ctx context.Context, bankID string, customer common.Address, u *big.Int) error {
	b := n.Banks[bankID]
	_, err := b.Token.Send(ctx, b.Key, "redeemDeposit", customer, u)
	return err
}

// DepositBalance is a customer's tokenized deposit.
func (n *Network) DepositBalance(ctx context.Context, bankID string, customer common.Address) (*big.Int, error) {
	return n.Banks[bankID].Token.BigInt(ctx, "balanceOf", customer)
}

// DepositSupply is a bank's tokenized deposits outstanding.
func (n *Network) DepositSupply(ctx context.Context, bankID string) (*big.Int, error) {
	return n.Banks[bankID].Token.BigInt(ctx, "totalSupply")
}

// Convert burns at the sending bank, moves settlement money between the
// banks and mints at the receiving bank, in one transaction, and returns
// that transaction's hash.
func (n *Network) Convert(ctx context.Context, fromBank, toBank string, fromCustomer, toCustomer common.Address, u *big.Int) (common.Hash, error) {
	f, t := n.Banks[fromBank], n.Banks[toBank]
	rcpt, err := n.Bridge.Send(ctx, f.Key, "convert", f.Token.Addr, t.Token.Addr, fromCustomer, toCustomer, u)
	if err != nil {
		return common.Hash{}, err
	}
	return rcpt.TxHash, nil
}

// Admitted reports whether an address may hold settlement money.
func (n *Network) Admitted(ctx context.Context, a common.Address) (bool, error) {
	return n.Registry.Bool(ctx, "isAuthorized", MemberPolicy, a)
}

// ─── Settlement money ───

// Issue mints settlement money to a member against reserves that arrived
// in the pool.
func (n *Network) Issue(ctx context.Context, member common.Address, u *big.Int) error {
	_, err := n.Settlement.Send(ctx, n.Operator, "fund", member, u)
	return err
}

// Retire burns a member's settlement money as its reserves leave the pool.
func (n *Network) Retire(ctx context.Context, member common.Address, u *big.Int) error {
	_, err := n.Settlement.Send(ctx, n.Operator, "defund", member, u)
	return err
}

// Earmark freezes part of a member's settlement money (a pending defund).
func (n *Network) Earmark(ctx context.Context, member common.Address, u *big.Int) error {
	_, err := n.Settlement.Send(ctx, n.Operator, "setFrozenTokens", member, u)
	return err
}

// Balance is a member's settlement money.
func (n *Network) Balance(ctx context.Context, a common.Address) (*big.Int, error) {
	return n.Settlement.BigInt(ctx, "balanceOf", a)
}

// Frozen is a member's earmarked settlement money.
func (n *Network) Frozen(ctx context.Context, a common.Address) (*big.Int, error) {
	return n.Settlement.BigInt(ctx, "getFrozenTokens", a)
}

// Available is a member's unearmarked settlement money.
func (n *Network) Available(ctx context.Context, a common.Address) (*big.Int, error) {
	return n.Settlement.BigInt(ctx, "unfrozenBalanceOf", a)
}

// ReservePool is what the token records as held in the reserve account.
func (n *Network) ReservePool(ctx context.Context) (*big.Int, error) {
	return n.Settlement.BigInt(ctx, "reservePool")
}

// Supply is the settlement money outstanding.
func (n *Network) Supply(ctx context.Context) (*big.Int, error) {
	return n.Settlement.BigInt(ctx, "totalSupply")
}

// BackingIntact is the token's own check: supply equals the reserve pool.
func (n *Network) BackingIntact(ctx context.Context) (bool, error) {
	return n.Settlement.Bool(ctx, "backingIntact")
}

// ─── Reserve income ───

// AccrueYield credits income to every holder through the accrual index.
func (n *Network) AccrueYield(ctx context.Context, u *big.Int) error {
	_, err := n.Settlement.Send(ctx, n.Operator, "accrueYield", u)
	return err
}

// AccrualOf is a member's unpaid entitlement.
func (n *Network) AccrualOf(ctx context.Context, a common.Address) (*big.Int, error) {
	return n.Settlement.BigInt(ctx, "accrualOf", a)
}

// ClaimAccrual zeroes a member's entitlement and returns what it was.
func (n *Network) ClaimAccrual(ctx context.Context, a common.Address) (*big.Int, error) {
	owed, err := n.AccrualOf(ctx, a)
	if err != nil {
		return nil, err
	}
	if owed.Sign() == 0 {
		return owed, nil
	}
	rcpt, err := n.Settlement.Send(ctx, n.Operator, "claimAccrual", a)
	if err != nil {
		return nil, err
	}
	id := n.Settlement.EventID("AccrualClaimed")
	for _, l := range rcpt.Logs {
		if len(l.Topics) > 0 && l.Topics[0] == id {
			var ev struct {
				Participant common.Address
				Amount      *big.Int
			}
			if err := n.Settlement.Unpack(&ev, "AccrualClaimed", *l); err == nil {
				return ev.Amount, nil
			}
		}
	}
	return owed, nil
}

// ─── Netting ───

// Obligation states in NettingEngine.
const (
	ObligationNone uint8 = iota
	ObligationQueued
	ObligationSettled
	ObligationCancelled
)

// ObligationView is an obligation on chain.
type ObligationView struct {
	Payer       common.Address
	Payee       common.Address
	Amount      *big.Int
	SubmittedAt uint64
	Status      uint8
}

// SubmitObligation queues an obligation from one bank to another. Nothing
// moves until a cycle discharges it.
func (n *Network) SubmitObligation(ctx context.Context, fromBank string, id [32]byte, toBank string, u *big.Int) error {
	f, t := n.Banks[fromBank], n.Banks[toBank]
	_, err := n.Netting.Send(ctx, f.Key, "submitObligation", id, t.Key.Addr, u)
	return err
}

// CancelObligation withdraws a queued obligation (the payer only).
func (n *Network) CancelObligation(ctx context.Context, fromBank string, id [32]byte) error {
	_, err := n.Netting.Send(ctx, n.Banks[fromBank].Key, "cancelObligation", id)
	return err
}

// Obligation reads an obligation.
func (n *Network) Obligation(ctx context.Context, id [32]byte) (ObligationView, error) {
	out, err := n.Netting.Call(ctx, "obligations", id)
	if err != nil {
		return ObligationView{}, err
	}
	return ObligationView{Payer: out[0].(common.Address), Payee: out[1].(common.Address), Amount: out[2].(*big.Int),
		SubmittedAt: out[3].(uint64), Status: out[4].(uint8)}, nil
}

// NetPosition is one participant's position in a cycle.
type NetPosition struct {
	Participant common.Address `abi:"participant"`
	Amount      *big.Int       `abi:"amount"`
}

// SettleCycle runs a selection on the plan book: it opens a window, submits
// the plan as a solver, closes the window by mining past it and executes the
// winning plan through the engine, which re-verifies it before moving value.
func (n *Network) SettleCycle(ctx context.Context, cycleID [32]byte, net []NetPosition, discharged [][32]byte, window uint32) error {
	if window == 0 {
		window = 3
	}
	if _, err := n.PlanBook.Send(ctx, n.Operator, "openSelection", cycleID, window); err != nil {
		return err
	}
	if _, err := n.PlanBook.Send(ctx, n.Operator, "submitPlan", cycleID, net, discharged); err != nil {
		return err
	}
	if err := n.C.Mine(ctx, uint64(window)); err != nil {
		return err
	}
	_, err := n.PlanBook.Send(ctx, n.Operator, "executeWinning", cycleID)
	return err
}

// Efficiency is the engine's liquidity efficiency in bps: gross discharged
// over net moved.
func (n *Network) Efficiency(ctx context.Context) (*big.Int, error) {
	return n.Netting.BigInt(ctx, "liquidityEfficiencyBps")
}

// CycleID derives a cycle id from a label.
func CycleID(label string) [32]byte { return sha256.Sum256([]byte("cycle:" + label)) }

// ObligationID derives an obligation id from the payment's UETR, so a
// resubmission of the same payment can never queue twice.
func ObligationID(uetr string) [32]byte { return sha256.Sum256([]byte("obligation:" + uetr)) }

// DrawID derives an intraday draw id.
func DrawID(label string) [32]byte { return sha256.Sum256([]byte("draw:" + label)) }

// ─── Intraday liquidity ───

// Supply deposits a provider's settlement money into the pool.
func (n *Network) SupplyPool(ctx context.Context, provider chain.Account, u *big.Int) error {
	if _, err := n.Settlement.Send(ctx, provider, "approve", n.Pool.Addr, u); err != nil {
		return err
	}
	_, err := n.Pool.Send(ctx, provider, "deposit", u, provider.Addr)
	return err
}

// MintCollateral gives a bank demo T-bills to post.
func (n *Network) MintCollateral(ctx context.Context, to common.Address, u *big.Int) error {
	_, err := n.Collateral.Send(ctx, n.Operator, "mint", to, u)
	return err
}

// CollateralBalance is a bank's unposted demo T-bills.
func (n *Network) CollateralBalance(ctx context.Context, a common.Address) (*big.Int, error) {
	return n.Collateral.BigInt(ctx, "balanceOf", a)
}

// DrawView is an intraday draw.
type DrawView struct {
	Borrower         common.Address
	Collateral       common.Address
	CollateralAmount *big.Int
	Principal        *big.Int
	OpenedAt         uint64
	RateBps          uint16
	Open             bool
	Interest         *big.Int
	Overdue          bool
}

// Draw borrows settlement money from the pool against demo T-bills, posting
// exactly the collateral the advance rate requires.
func (n *Network) Draw(ctx context.Context, bankID string, id [32]byte, u *big.Int) (*big.Int, error) {
	b := n.Banks[bankID]
	adv, err := n.Pool.Call(ctx, "collateralAdvance", n.Collateral.Addr)
	if err != nil {
		return nil, err
	}
	a := big.NewInt(int64(adv[0].(uint16)))
	if a.Sign() == 0 {
		return nil, fmt.Errorf("collateral not registered")
	}
	// ceil(u * 10000 / advance), as the pool computes it.
	need := new(big.Int).Mul(u, big.NewInt(10_000))
	need.Add(need, new(big.Int).Sub(a, big.NewInt(1)))
	need.Quo(need, a)
	if _, err := n.Collateral.Send(ctx, b.Key, "approve", n.Pool.Addr, need); err != nil {
		return nil, err
	}
	if _, err := n.Pool.Send(ctx, b.Key, "draw", id, n.Collateral.Addr, need, u); err != nil {
		return nil, err
	}
	return need, nil
}

// DrawState reads a draw and the interest it owes now.
func (n *Network) DrawState(ctx context.Context, id [32]byte) (DrawView, error) {
	out, err := n.Pool.Call(ctx, "draws", id)
	if err != nil {
		return DrawView{}, err
	}
	d := DrawView{Borrower: out[0].(common.Address), Collateral: out[1].(common.Address), CollateralAmount: out[2].(*big.Int),
		Principal: out[3].(*big.Int), OpenedAt: out[4].(uint64), RateBps: out[5].(uint16), Open: out[6].(bool), Interest: big.NewInt(0)}
	if d.Open {
		io, err := n.Pool.Call(ctx, "interestOwed", id)
		if err != nil {
			return d, err
		}
		d.Interest, d.Overdue = io[0].(*big.Int), io[1].(bool)
	}
	return d, nil
}

// Repay returns a draw's principal and interest; the pool releases the
// collateral. The interest is quoted at the current block; one more block
// passes before the repayment lands, so the approval carries a margin and
// the pool takes only what it computes.
func (n *Network) Repay(ctx context.Context, bankID string, id [32]byte) (principal, interest *big.Int, err error) {
	b := n.Banks[bankID]
	d, err := n.DrawState(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	if !d.Open {
		return nil, nil, fmt.Errorf("draw is not open")
	}
	allow := new(big.Int).Add(d.Principal, d.Interest)
	allow.Add(allow, new(big.Int).Quo(d.Principal, big.NewInt(100_000))) // +1bp margin
	if _, err := n.Settlement.Send(ctx, b.Key, "approve", n.Pool.Addr, allow); err != nil {
		return nil, nil, err
	}
	rcpt, err := n.Pool.Send(ctx, b.Key, "repay", id)
	if err != nil {
		return nil, nil, err
	}
	if _, err := n.Settlement.Send(ctx, b.Key, "approve", n.Pool.Addr, big.NewInt(0)); err != nil {
		return nil, nil, err
	}
	ev := n.Pool.EventID("Repaid")
	for _, l := range rcpt.Logs {
		if len(l.Topics) > 0 && l.Topics[0] == ev {
			var r struct {
				Id        [32]byte
				Principal *big.Int
				Interest  *big.Int
				Overdue   bool
			}
			if err := n.Pool.Unpack(&r, "Repaid", *l); err == nil {
				return r.Principal, r.Interest, nil
			}
		}
	}
	return d.Principal, d.Interest, nil
}

// Outstanding is what a member owes the pool in principal.
func (n *Network) Outstanding(ctx context.Context, a common.Address) (*big.Int, error) {
	return n.Pool.BigInt(ctx, "outstandingOf", a)
}

// PoolView is the pool's standing.
type PoolView struct {
	Assets         *big.Int
	Drawn          *big.Int
	UtilizationBps *big.Int
}

// PoolState reads the pool.
func (n *Network) PoolState(ctx context.Context) (PoolView, error) {
	var p PoolView
	var err error
	if p.Assets, err = n.Pool.BigInt(ctx, "totalAssets"); err != nil {
		return p, err
	}
	if p.Drawn, err = n.Pool.BigInt(ctx, "totalDrawn"); err != nil {
		return p, err
	}
	p.UtilizationBps, err = n.Pool.BigInt(ctx, "utilizationBps")
	return p, err
}
