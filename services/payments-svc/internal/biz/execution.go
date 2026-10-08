package biz

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"
)

// RTPLimit is the RTP network's per-payment limit since 9 Feb 2025 (FedNow's
// since 12 Nov 2025).
var RTPLimit = Dollars(10_000_000)

// advance screens an approved order against the OFAC SDN list and executes
// it, or holds it for compliance.
func (e *Engine) advance(ctx context.Context, o *Order) {
	if !o.Released {
		if m, hit := e.screen.Screen(ctx, o.Creditor.Name); hit {
			o.Reason = "SANCTIONS"
			o.SanctionsMatch = describeMatch(m)
			e.event(o, "screening", StatusOnHold, ISOPending, "held for compliance review", "possible match: "+o.SanctionsMatch)
			return
		}
		e.event(o, "screening", o.Status, o.ISO, "sanctions screening clear", "")
	}
	e.execute(ctx, o)
}

func describeMatch(m SanctionsMatch) string {
	progs := ""
	if len(m.Programs) > 0 {
		progs = " (" + strings.Join(m.Programs, ", ") + ")"
	}
	return fmt.Sprintf("OFAC SDN #%s %s%s, score %.2f", m.SourceID, m.Name, progs, m.Score)
}

// execute routes the order: a payee at the same bank is a book transfer; a
// NORMAL order to another bank waits for a netting cycle; an URGENT one
// settles now as tokenized deposits.
func (e *Engine) execute(ctx context.Context, o *Order) {
	to, _ := e.rails.BankByRouting(o.Creditor.Routing)
	from, _ := e.rails.Bank(o.Bank)
	switch {
	case to.MemberID == o.Bank:
		o.Route = RouteBook
		rec, ok := e.rails.Account(o.Bank, o.Creditor.Account)
		if !ok || rec.Closed {
			e.reject(o, "AC04", "no such open account at "+from.Name)
			return
		}
		if err := e.rails.Post(o.Bank, o.DebtorAccount, -o.Amount, o.ID+"-DR"); err != nil {
			e.rejectOrFail(o, err)
			return
		}
		_ = e.rails.Post(o.Bank, o.Creditor.Account, o.Amount, o.ID+"-CR")
		e.settle(o, "booked between two accounts at "+from.Name, "")
		e.credited(o, from.Name, "")

	case o.Priority == Normal:
		o.Route = RouteNetting
		ref, err := e.rails.SubmitObligation(ctx, o.Bank, o.DebtorAccount, o.Creditor, o.Amount, o.UETR)
		if err != nil {
			e.rejectOrFail(o, err)
			return
		}
		o.ObligationRef = ref
		o.RouteNote = "funded from the deposit account; settles with every other obligation in the operator's next netting cycle, using only net liquidity"
		e.event(o, "system", StatusQueuedForNetting, ISOInProcess, "account debited; queued for the operator's next netting cycle",
			"obligation "+short(ref)+" queued in NettingEngine; nothing moves until a verified cycle discharges it")

	default:
		o.Route = RouteInstant
		o.RouteNote = e.whyInstant(o)
		e.executeUrgent(ctx, o)
	}
}

// executeUrgent converts the payment between the banks now: the deposit is
// tokenized as the paying bank's deposit token and converted, over
// settlement money, into the receiving bank's. Without enough available
// settlement money the order waits and the bank's treasury is alerted.
func (e *Engine) executeUrgent(ctx context.Context, o *Order) {
	from, _ := e.rails.Bank(o.Bank)
	to, _ := e.rails.BankByRouting(o.Creditor.Routing)
	// A payment replayed after a lost database write may already be
	// converted: record what happened instead of doing it again.
	if res, ok := e.rails.Converted(o.UETR); ok && res.State == ConversionSettled {
		o.PaymentRef, o.Tokenized = res.Ref, false
		e.settle(o, "settled instantly between "+from.Name+" and "+to.Name, e.ledgerDetail(o))
		return
	}
	// A conversion is final in one transaction, so the payee's account is
	// checked before anything waits or moves.
	if rec, ok := e.rails.Account(to.MemberID, o.Creditor.Account); !ok || rec.Closed {
		e.reject(o, "AC04", "no such open account at "+to.Name)
		return
	}
	m, err := e.rails.Member(ctx, o.Bank)
	if err != nil {
		e.failed(o, err)
		return
	}
	if m.Available < o.Amount {
		o.Reason = "LIQUIDITY"
		if o.Status != StatusAwaitingLiquidity {
			e.event(o, "system", StatusAwaitingLiquidity, ISOPending,
				"waiting for "+from.Name+" to make liquidity available; the payment goes as soon as it is",
				fmt.Sprintf("available settlement money $%s is short of $%s; alert raised to treasury", Readable(m.Available), Readable(o.Amount)))
			e.raiseLiquidityAlert(ctx, o, m.Available)
		}
		return
	}
	if !o.Tokenized {
		if err := e.rails.Tokenize(ctx, o.Bank, o.DebtorAccount, o.Amount, o.UETR); err != nil {
			e.rejectOrFail(o, err)
			return
		}
		o.Tokenized = true
		o.Reason = ""
		e.event(o, "system", StatusInProcess, ISOInProcess,
			fmt.Sprintf("account debited $%s; payment released", Readable(o.Amount)),
			fmt.Sprintf("$%s issued as %s to the payer's wallet", Readable(o.Amount), from.DepositToken))
	}
	res, err := e.rails.Convert(ctx, o.Bank, o.DebtorAccount, o.Creditor, o.Amount, o.UETR)
	if stderrors.Is(err, ErrOutcomeUnknown) {
		e.unknown(o, err)
		return
	}
	if err != nil {
		e.refund(ctx, o)
		e.failed(o, err)
		return
	}
	o.PaymentRef = res.Ref
	if res.State == ConversionRejected {
		e.refund(ctx, o)
		e.reject(o, res.Reason, "rejected by "+to.Name)
		return
	}
	o.Tokenized = false
	e.settle(o, "settled instantly between "+from.Name+" and "+to.Name, e.ledgerDetail(o))
}

// whyInstant says, in the client's words, why the order goes on-network.
func (e *Engine) whyInstant(o *Order) string {
	now := e.now()
	var why []string
	if !e.rails.FedwireOpen(now) {
		why = append(why, "Fedwire is closed until "+e.rails.NextFedwireOpen(now).In(e.et).Format("Mon 2 Jan 15:04 MST"))
	}
	if o.Amount > RTPLimit {
		why = append(why, "the amount is above RTP's $10,000,000.00 per-payment limit")
	}
	if len(why) == 0 {
		return "instant, final settlement between the banks"
	}
	return strings.Join(why, " and ") + "; settled instantly between the banks instead"
}

func (e *Engine) instrument() string {
	if e.rails.FedwireOpen(e.now()) {
		return "Fedwire BTRC is open"
	}
	return "Fedwire is closed; FedNow LMT can fund it now, or draw intraday liquidity against collateral"
}

// refund redeems an order's deposit token back into the payer's account.
func (e *Engine) refund(ctx context.Context, o *Order) {
	if !o.Tokenized {
		return
	}
	from, _ := e.rails.Bank(o.Bank)
	if err := e.rails.Refund(ctx, o.Bank, o.DebtorAccount, o.Amount, o.UETR); err != nil {
		e.event(o, "system", o.Status, o.ISO, "refund pending; operations notified", "redeem failed: "+err.Error())
		return
	}
	o.Tokenized = false
	e.event(o, "system", o.Status, o.ISO, "account re-credited $"+Readable(o.Amount), from.DepositToken+" redeemed back into the deposit")
}

func (e *Engine) settle(o *Order, note, detail string) {
	t := e.now()
	o.SettledAt = &t
	o.Reason = ""
	e.event(o, "system", StatusSettled, ISOSettled, note, detail)
}

// credited records the receiving bank's credit to the payee (ACCC).
func (e *Engine) credited(o *Order, bankName, detail string) {
	e.event(o, bankName, StatusSettled, ISOCredited, "credited to "+o.Creditor.Name+"'s account "+o.Creditor.Account, detail)
}

// ledgerDetail is what moved when a conversion settled.
func (e *Engine) ledgerDetail(o *Order) string {
	from, _ := e.rails.Bank(o.Bank)
	to, _ := e.rails.BankByRouting(o.Creditor.Routing)
	return fmt.Sprintf("conversion %s: %s burned; $%s of settlement money moved %s → %s; %s minted to the payee's wallet",
		short(o.PaymentRef), from.DepositToken, Readable(o.Amount), from.Name, to.Name, to.DepositToken)
}

func (e *Engine) reject(o *Order, reason, note string) {
	o.Reason = reason
	e.event(o, "system", StatusRejected, ISORejected, fmt.Sprintf("%s (%s)", note, reason), "")
}

// unknown records a side effect whose outcome could not be established. It
// neither compensates nor retries: either could pay twice. The order stays
// in process and operations resolve it against the chain.
func (e *Engine) unknown(o *Order, err error) {
	e.log.Errorf("payment %s: %v", o.ID, err)
	o.Reason = "NARR"
	e.event(o, "system", o.Status, o.ISO, "processing delayed; operations notified", "outcome unknown, not compensated: "+err.Error())
}

func (e *Engine) rejectOrFail(o *Order, err error) {
	if stderrors.Is(err, ErrOutcomeUnknown) {
		e.unknown(o, err)
		return
	}
	if stderrors.Is(err, ErrInsufficientFunds) {
		e.reject(o, "AM04", "insufficient funds")
		return
	}
	if stderrors.Is(err, ErrAccountClosed) {
		to, _ := e.rails.BankByRouting(o.Creditor.Routing)
		e.reject(o, "AC04", "no such open account at "+to.Name)
		return
	}
	e.failed(o, err)
}

func (e *Engine) failed(o *Order, err error) {
	e.log.Errorf("payment %s failed: %v", o.ID, err)
	o.Reason = "NARR"
	e.event(o, "system", StatusRejected, ISORejected, "processing error; operations notified (NARR)", err.Error())
}

// syncNettedOne applies an obligation's state on chain to its order.
func (e *Engine) syncNettedOne(ctx context.Context, o *Order, actor string) {
	st, err := e.rails.ObligationState(ctx, o.ObligationRef)
	if err != nil {
		return
	}
	from, _ := e.rails.Bank(o.Bank)
	to, _ := e.rails.BankByRouting(o.Creditor.Routing)
	switch st {
	case ObligationSettled:
		e.settle(o, "settled in an operator netting cycle",
			fmt.Sprintf("obligation discharged in a cycle the engine re-verified; settlement money moved %s → %s net of every other obligation", from.Name, to.Name))
	case ObligationCancelled:
		e.event(o, actor, StatusCancelled, ISOCancelled, "withdrawn from the netting queue; account re-credited", "")
	case ObligationExpired:
		o.Reason = "AB05"
		e.event(o, "system", StatusExpired, ISORejected, "not settled before the obligation expired; account re-credited (AB05)",
			"cancelled on chain after the hub's time-to-live")
	}
}

func short(ref string) string {
	if len(ref) > 12 {
		return "0x" + ref[:10] + "…"
	}
	return ref
}
