package data

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"

	"reserve-interbank-settlement/services/payments-svc/ent"
	"reserve-interbank-settlement/services/payments-svc/ent/approval"
	"reserve-interbank-settlement/services/payments-svc/ent/orderevent"
	"reserve-interbank-settlement/services/payments-svc/ent/paymentorder"
	"reserve-interbank-settlement/services/payments-svc/ent/predicate"
	"reserve-interbank-settlement/services/payments-svc/internal/biz"
)

type orderRepo struct{ d *Data }

// NewOrderRepo creates the order repository.
func NewOrderRepo(d *Data) biz.OrderRepo { return &orderRepo{d: d} }

func (r *orderRepo) Create(ctx context.Context, o *biz.Order) error {
	c := r.d.client(ctx)
	q := c.PaymentOrder.Create().
		SetID(o.ID).SetOrganizationID(o.Org).SetBankID(o.Bank).
		SetRequestHash(o.RequestHash).SetEndToEndID(o.EndToEndID).SetUetr(o.UETR).
		SetDebtorAccount(o.DebtorAccount).SetCreditorName(o.Creditor.Name).SetCreditorRouting(o.Creditor.Routing).
		SetCreditorAccount(o.Creditor.Account).SetAmount(o.Amount).SetPriority(string(o.Priority)).
		SetRemittance(o.Remittance).SetPayeeAccountStatus(o.PayeeCheck.Account).SetPayeeNameMatch(o.PayeeCheck.Name).
		SetPayeeRegisteredName(o.PayeeCheck.Registered).SetApprovalsRequired(o.ApprovalsRequired).
		SetCreatedBy(o.CreatedBy).SetCreatedAt(o.CreatedAt).SetVersion(0)
	if o.IdempotencyKey != "" {
		q.SetIdempotencyKey(o.IdempotencyKey)
	}
	if o.FileID != "" {
		q.SetFileID(o.FileID)
	}
	setMutable(q.Mutation(), o)
	if _, err := q.Save(ctx); err != nil {
		if ent.IsConstraintError(err) {
			return biz.ErrConflict("payment conflicts with an existing one: %v", err)
		}
		return fmt.Errorf("create payment: %w", err)
	}
	o.Version = 0
	return r.appendChildren(ctx, c, o)
}

// setMutable sets the fields that change over a payment's life.
func setMutable(m *ent.PaymentOrderMutation, o *biz.Order) {
	m.SetStatus(string(o.Status))
	m.SetIsoStatus(o.ISO)
	m.SetReasonCode(o.Reason)
	m.SetRoute(o.Route)
	m.SetRouteNote(o.RouteNote)
	m.SetUpdatedAt(o.UpdatedAt)
	m.SetSanctionsMatch(o.SanctionsMatch)
	m.SetPaymentRef(o.PaymentRef)
	m.SetObligationRef(o.ObligationRef)
	m.SetTokenized(o.Tokenized)
	m.SetReleased(o.Released)
	if o.SettledAt != nil {
		m.SetSettledAt(*o.SettledAt)
	}
	if o.OFACReportDue != nil {
		m.SetOfacReportDue(*o.OFACReportDue)
	}
}

func (r *orderRepo) Update(ctx context.Context, o *biz.Order) error {
	c := r.d.client(ctx)
	q := c.PaymentOrder.Update().
		Where(paymentorder.ID(o.ID), paymentorder.Version(o.Version)).
		AddVersion(1)
	setMutable(q.Mutation(), o)
	n, err := q.Save(ctx)
	if err != nil {
		return fmt.Errorf("update payment: %w", err)
	}
	if n == 0 {
		return biz.ErrConflict("payment %s was changed concurrently; retry", o.ID)
	}
	o.Version++
	return r.appendChildren(ctx, c, o)
}

// appendChildren inserts the audit events and approvals not yet stored.
func (r *orderRepo) appendChildren(ctx context.Context, c *ent.Client, o *biz.Order) error {
	if o.SavedEvents < len(o.History) {
		var bulk []*ent.OrderEventCreate
		for _, e := range o.History[o.SavedEvents:] {
			bulk = append(bulk, c.OrderEvent.Create().SetOrderID(o.ID).SetSeq(e.Seq).SetAt(e.At).SetActor(e.Actor).
				SetStatus(string(e.Status)).SetIsoStatus(e.ISO).SetNote(e.Note).SetDetail(e.Detail))
		}
		if _, err := c.OrderEvent.CreateBulk(bulk...).Save(ctx); err != nil {
			return fmt.Errorf("append audit events: %w", err)
		}
		o.SavedEvents = len(o.History)
	}
	if o.SavedApprovals < len(o.Approvals) {
		var bulk []*ent.ApprovalCreate
		for _, a := range o.Approvals[o.SavedApprovals:] {
			bulk = append(bulk, c.Approval.Create().SetOrderID(o.ID).SetApproverID(a.ApproverID).SetApproverName(a.ApproverName).SetAt(a.At))
		}
		if _, err := c.Approval.CreateBulk(bulk...).Save(ctx); err != nil {
			if ent.IsConstraintError(err) {
				return biz.ErrConflict("this approver already approved the payment")
			}
			return fmt.Errorf("append approvals: %w", err)
		}
		o.SavedApprovals = len(o.Approvals)
	}
	return nil
}

func (r *orderRepo) query(ctx context.Context) *ent.PaymentOrderQuery {
	return r.d.client(ctx).PaymentOrder.Query().
		WithEvents(func(q *ent.OrderEventQuery) { q.Order(ent.Asc(orderevent.FieldSeq)) }).
		WithApprovals(func(q *ent.ApprovalQuery) { q.Order(ent.Asc(approval.FieldAt)) })
}

func (r *orderRepo) Get(ctx context.Context, id string) (*biz.Order, error) {
	row, err := r.query(ctx).Where(paymentorder.ID(id)).Only(ctx)
	if err != nil {
		return nil, notFound(err)
	}
	return toOrder(row), nil
}

func (r *orderRepo) GetByIdempotencyKey(ctx context.Context, org, key string) (*biz.Order, error) {
	row, err := r.query(ctx).Where(paymentorder.OrganizationID(org), paymentorder.IdempotencyKey(key)).Only(ctx)
	if err != nil {
		return nil, notFound(err)
	}
	return toOrder(row), nil
}

func (r *orderRepo) GetByEndToEndID(ctx context.Context, org, e2e string) (*biz.Order, error) {
	row, err := r.query(ctx).Where(paymentorder.OrganizationID(org), paymentorder.EndToEndID(e2e)).Only(ctx)
	if err != nil {
		return nil, notFound(err)
	}
	return toOrder(row), nil
}

// List pages newest first with a keyset cursor on (created_at, id).
func (r *orderRepo) List(ctx context.Context, f biz.OrderFilter) ([]*biz.Order, string, error) {
	q := r.query(ctx)
	var ps []predicate.PaymentOrder
	if f.Org != "" {
		ps = append(ps, paymentorder.OrganizationID(f.Org))
	}
	if f.Bank != "" {
		ps = append(ps, paymentorder.BankID(f.Bank))
	}
	if len(f.Statuses) > 0 {
		st := make([]string, len(f.Statuses))
		for i, s := range f.Statuses {
			st[i] = string(s)
		}
		ps = append(ps, paymentorder.StatusIn(st...))
	}
	if f.PageToken != "" {
		at, id, err := decodeCursor(f.PageToken)
		if err != nil {
			return nil, "", biz.ErrInvalid("page_token is not valid")
		}
		ps = append(ps, paymentorder.Or(paymentorder.CreatedAtLT(at),
			paymentorder.And(paymentorder.CreatedAt(at), paymentorder.IDLT(id))))
	}
	q = q.Where(ps...).Order(paymentorder.ByCreatedAt(sql.OrderDesc()), paymentorder.ByID(sql.OrderDesc()))
	size := f.PageSize
	switch {
	case size < 0:
	case size == 0:
		size = 50
	case size > 100:
		size = 100
	}
	if size > 0 {
		q = q.Limit(size + 1)
	}
	rows, err := q.All(ctx)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if size > 0 && len(rows) > size {
		rows = rows[:size]
		last := rows[len(rows)-1]
		next = encodeCursor(last.CreatedAt, last.ID)
	}
	out := make([]*biz.Order, 0, len(rows))
	for _, row := range rows {
		out = append(out, toOrder(row))
	}
	return out, next, nil
}

func encodeCursor(t time.Time, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(t.UnixNano(), 10) + "|" + id))
}

func decodeCursor(s string) (time.Time, string, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, "", err
	}
	ns, id, ok := strings.Cut(string(b), "|")
	if !ok {
		return time.Time{}, "", fmt.Errorf("cursor")
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	return time.Unix(0, n), id, err
}

func (r *orderRepo) ListByFile(ctx context.Context, fileID string) ([]*biz.Order, error) {
	rows, err := r.query(ctx).Where(paymentorder.FileID(fileID)).Order(paymentorder.ByCreatedAt(), paymentorder.ByID()).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*biz.Order, 0, len(rows))
	for _, row := range rows {
		out = append(out, toOrder(row))
	}
	return out, nil
}

func (r *orderRepo) ListCases(ctx context.Context, bank string) ([]*biz.Order, error) {
	rows, err := r.query(ctx).Where(paymentorder.BankID(bank), paymentorder.OfacReportDueNotNil()).
		Order(paymentorder.ByCreatedAt()).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*biz.Order, 0, len(rows))
	for _, row := range rows {
		out = append(out, toOrder(row))
	}
	return out, nil
}

func (r *orderRepo) SumSince(ctx context.Context, org string, t time.Time) (int64, error) {
	amounts, err := r.d.client(ctx).PaymentOrder.Query().
		Where(paymentorder.OrganizationID(org), paymentorder.CreatedAtGTE(t),
			paymentorder.StatusNotIn(string(biz.StatusRejected), string(biz.StatusCancelled), string(biz.StatusExpired))).
		Select(paymentorder.FieldAmount).Ints(ctx)
	if err != nil {
		return 0, err
	}
	var sum int64
	for _, a := range amounts {
		sum += int64(a)
	}
	return sum, nil
}

func toOrder(row *ent.PaymentOrder) *biz.Order {
	o := &biz.Order{
		ID: row.ID, Org: row.OrganizationID, Bank: row.BankID, RequestHash: row.RequestHash, EndToEndID: row.EndToEndID,
		UETR: row.Uetr, DebtorAccount: row.DebtorAccount,
		Creditor: biz.Party{Name: row.CreditorName, Routing: row.CreditorRouting, Account: row.CreditorAccount},
		Amount:   row.Amount, Priority: biz.Priority(row.Priority), Remittance: row.Remittance,
		Status: biz.Status(row.Status), ISO: row.IsoStatus, Reason: row.ReasonCode, Route: row.Route, RouteNote: row.RouteNote,
		PayeeCheck:        biz.PayeeCheck{Account: row.PayeeAccountStatus, Name: row.PayeeNameMatch, Registered: row.PayeeRegisteredName},
		ApprovalsRequired: row.ApprovalsRequired, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		SettledAt: row.SettledAt, OFACReportDue: row.OfacReportDue, SanctionsMatch: row.SanctionsMatch,
		PaymentRef: row.PaymentRef, ObligationRef: row.ObligationRef, Tokenized: row.Tokenized, Released: row.Released,
		Version: row.Version,
	}
	if row.IdempotencyKey != nil {
		o.IdempotencyKey = *row.IdempotencyKey
	}
	if row.FileID != nil {
		o.FileID = *row.FileID
	}
	for _, e := range row.Edges.Events {
		o.History = append(o.History, biz.Event{Seq: e.Seq, At: e.At, Actor: e.Actor, Status: biz.Status(e.Status),
			ISO: e.IsoStatus, Note: e.Note, Detail: e.Detail})
	}
	for _, a := range row.Edges.Approvals {
		o.Approvals = append(o.Approvals, biz.Approval{ApproverID: a.ApproverID, ApproverName: a.ApproverName, At: a.At})
	}
	o.SavedEvents, o.SavedApprovals = len(o.History), len(o.Approvals)
	return o
}
