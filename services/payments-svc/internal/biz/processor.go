package biz

import (
	"context"
	"fmt"
	"time"
)

// Processor advances everything asynchronous: the rails' own processing,
// netting outcomes, scheduled cycles, expired obligations, payee credits,
// defunds, orders waiting for liquidity, and watermark alerts.
type Processor struct{ e *Engine }

// NewProcessor creates the processor.
func NewProcessor(e *Engine) *Processor { return &Processor{e: e} }

// Tick runs one pass.
func (pr *Processor) Tick(ctx context.Context) error {
	e := pr.e
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.tick(ctx)
}

func (e *Engine) tick(ctx context.Context) error {
	if err := e.rails.Process(ctx); err != nil {
		e.log.Warnf("rails: %v", err)
	}
	if err := e.resume(ctx); err != nil {
		return err
	}
	if ran, err := e.scheduledNetting(ctx); err != nil {
		e.log.Errorf("scheduled netting: %v", err)
	} else if ran {
		_ = e.rails.Process(ctx)
	}
	if err := e.syncNetted(ctx); err != nil {
		return err
	}
	if err := e.rails.ExpireStaleObligations(ctx); err == nil {
		_ = e.rails.Process(ctx)
		if err := e.syncNetted(ctx); err != nil {
			return err
		}
	}
	if err := e.syncCredited(ctx); err != nil {
		return err
	}
	if err := e.updateDefunds(ctx); err != nil {
		return err
	}
	for _, b := range e.rails.Banks() {
		if _, err := e.retryLiquidity(ctx, b.MemberID); err != nil {
			return err
		}
		if err := e.watermarks(ctx, b.MemberID); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) ordersIn(ctx context.Context, bank string, oldestFirst bool, st ...Status) ([]*Order, error) {
	os, _, err := e.r.Orders.List(ctx, OrderFilter{Bank: bank, Statuses: st, PageSize: -1})
	if err != nil || !oldestFirst {
		return os, err
	}
	for i, j := 0, len(os)-1; i < j; i, j = i+1, j-1 {
		os[i], os[j] = os[j], os[i]
	}
	return os, nil
}

// resume continues orders that were approved (or released by compliance) but
// whose routing never reached the database: the save after the rails acted
// was lost. Rails side effects are keyed by UETR, so resuming repeats
// nothing that already happened. An order whose last side effect has an
// unknown outcome (NARR) is left to operations.
func (e *Engine) resume(ctx context.Context) error {
	os, err := e.ordersIn(ctx, "", true, StatusInProcess)
	if err != nil {
		return err
	}
	for _, o := range os {
		if o.Route != "" || o.Reason == "NARR" {
			continue
		}
		e.event(o, "system", o.Status, o.ISO, "processing resumed", "the previous attempt's outcome was not recorded; resuming by UETR")
		e.advance(ctx, o)
		if err := e.save(ctx, o, false); err != nil {
			return err
		}
	}
	return nil
}

// syncNetted applies netting outcomes to queued orders.
func (e *Engine) syncNetted(ctx context.Context) error {
	os, err := e.ordersIn(ctx, "", true, StatusQueuedForNetting)
	if err != nil {
		return err
	}
	for _, o := range os {
		before := len(o.History)
		e.syncNettedOne(ctx, o, "system")
		if len(o.History) == before {
			continue
		}
		if err := e.save(ctx, o, false); err != nil {
			return err
		}
	}
	return nil
}

// syncCredited confirms, for orders settled between the banks, that the
// receiving bank has credited the payee's deposit account (ACCC).
func (e *Engine) syncCredited(ctx context.Context) error {
	os, err := e.ordersIn(ctx, "", true, StatusSettled)
	if err != nil {
		return err
	}
	for _, o := range os {
		if o.ISO != ISOSettled {
			continue
		}
		to, ok := e.rails.BankByRouting(o.Creditor.Routing)
		if !ok {
			continue
		}
		switch {
		case o.PaymentRef != "" && e.rails.ConversionCredited(to.MemberID, o.PaymentRef):
			e.credited(o, to.Name, to.DepositToken+" redeemed into the payee's deposit account; the payee never held a token")
		case o.ObligationRef != "" && e.rails.ObligationCredited(to.MemberID, o.ObligationRef):
			e.credited(o, to.Name, "")
		default:
			continue
		}
		if err := e.save(ctx, o, false); err != nil {
			return err
		}
	}
	return nil
}

// scheduledNetting runs a cycle when the clock passes a boundary of the
// schedule and obligations are waiting.
func (e *Engine) scheduledNetting(ctx context.Context) (bool, error) {
	every := e.opts.NettingEvery
	if every <= 0 {
		return false, nil
	}
	now := e.now()
	due := !e.nextCycle.IsZero() && !now.Before(e.nextCycle)
	if e.nextCycle.IsZero() || due {
		e.nextCycle = now.Truncate(every).Add(every)
	}
	if !due || e.rails.QueuedObligations(ctx) == 0 {
		return false, nil
	}
	_, err := e.runCycle(ctx, "scheduled")
	return err == nil, err
}

func (e *Engine) runCycle(ctx context.Context, trigger string) (*NettingCycle, error) {
	r, err := e.rails.RunCycle(ctx)
	if err != nil {
		return nil, err
	}
	c := &NettingCycle{ID: newID(), CycleRef: r.CycleRef, At: e.now(), Trigger: trigger,
		Discharged: r.Discharged, Deferred: r.Deferred, Gross: r.Gross, Net: r.Net}
	return c, e.r.Network.CreateCycle(ctx, c)
}

// retryLiquidity executes a bank's orders waiting for liquidity, oldest
// first, and closes its liquidity alerts when none wait any more.
func (e *Engine) retryLiquidity(ctx context.Context, bank string) (int, error) {
	os, err := e.ordersIn(ctx, bank, true, StatusAwaitingLiquidity)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, o := range os {
		e.executeUrgent(ctx, o)
		if o.Status != StatusAwaitingLiquidity {
			n++
			if err := e.save(ctx, o, false); err != nil {
				return n, err
			}
		}
	}
	if n == len(os) {
		open, err := e.r.Alerts.OpenOfKind(ctx, bank, "LIQUIDITY")
		if err != nil {
			return n, err
		}
		for _, a := range open {
			if err := e.r.Alerts.Close(ctx, a.ID, e.now()); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (e *Engine) raiseLiquidityAlert(ctx context.Context, o *Order, available int64) {
	from, _ := e.rails.Bank(o.Bank)
	a := &Alert{ID: newID(), Bank: o.Bank, Kind: "LIQUIDITY", Open: true, At: e.now(),
		Message: fmt.Sprintf("%s needs $%s of settlement money for payment %s; it has $%s available. Fund the settlement account (%s).",
			from.Name, Readable(o.Amount), o.ID, Readable(available), e.instrument())}
	if err := e.r.Alerts.Create(ctx, a); err != nil {
		e.log.Errorf("alert: %v", err)
	}
}

// watermarks opens a treasury alert when a bank's available settlement money
// falls below its low watermark and closes it once it is back at normal.
func (e *Engine) watermarks(ctx context.Context, bank string) error {
	w, ok := e.opts.Watermarks[bank]
	if !ok || w.Low <= 0 {
		return nil
	}
	m, err := e.rails.Member(ctx, bank)
	if err != nil {
		return nil
	}
	open, err := e.r.Alerts.OpenOfKind(ctx, bank, "LOW_WATERMARK")
	if err != nil {
		return err
	}
	switch {
	case len(open) == 0 && m.Available < w.Low:
		return e.r.Alerts.Create(ctx, &Alert{ID: newID(), Bank: bank, Kind: "LOW_WATERMARK", Open: true, At: e.now(),
			Message: fmt.Sprintf("%s has $%s of settlement money available, below its low watermark of $%s. Fund the settlement account (%s).",
				m.Name, Readable(m.Available), Readable(w.Low), e.instrument())})
	case len(open) > 0 && m.Available >= w.Normal:
		for _, a := range open {
			if err := e.r.Alerts.Close(ctx, a.ID, e.now()); err != nil {
				return err
			}
		}
	}
	return nil
}

// updateDefunds records defunds the operator has completed or failed on Fedwire.
func (e *Engine) updateDefunds(ctx context.Context) error {
	ds, err := e.r.Defunds.ListByStatus(ctx, "APPROVED")
	if err != nil {
		return err
	}
	for _, d := range ds {
		st, err := e.rails.DefundState(ctx, d.LedgerRef)
		if err != nil || st == "APPROVED" {
			continue
		}
		d.Status, d.UpdatedAt = st, e.now()
		if err := e.r.Defunds.Update(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

// NextCycle is when the next scheduled netting cycle runs, if scheduled.
func (e *Engine) NextCycle() time.Time { return e.nextCycle }
