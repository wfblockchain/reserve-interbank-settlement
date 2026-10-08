package data

import (
	"context"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect/sql"
	"github.com/google/uuid"

	"reserve-interbank-settlement/services/payments-svc/ent"
	"reserve-interbank-settlement/services/payments-svc/ent/alert"
	"reserve-interbank-settlement/services/payments-svc/ent/defund"
	"reserve-interbank-settlement/services/payments-svc/ent/interestdistribution"
	"reserve-interbank-settlement/services/payments-svc/ent/nettingcycle"
	"reserve-interbank-settlement/services/payments-svc/ent/paymentfile"
	"reserve-interbank-settlement/services/payments-svc/ent/reconciliation"
	"reserve-interbank-settlement/services/payments-svc/ent/schema"
	"reserve-interbank-settlement/services/payments-svc/ent/webhookdelivery"
	"reserve-interbank-settlement/services/payments-svc/ent/webhooksubscription"
	"reserve-interbank-settlement/services/payments-svc/internal/biz"
)

// ─── Files ───

type fileRepo struct{ d *Data }

// NewFileRepo creates the payment-file repository.
func NewFileRepo(d *Data) biz.FileRepo { return &fileRepo{d: d} }

func (r *fileRepo) Create(ctx context.Context, f *biz.PaymentFile) error {
	var rj []schema.Rejection
	for _, x := range f.Rejections {
		rj = append(rj, schema.Rejection{EndToEndID: x.EndToEndID, Reason: x.Reason, Message: x.Message})
	}
	_, err := r.d.client(ctx).PaymentFile.Create().SetID(f.ID).SetOrganizationID(f.Org).SetMessageID(f.MessageID).
		SetRequestHash(f.RequestHash).SetIdempotencyKey(f.IdempotencyKey).SetTransactions(f.Transactions).
		SetControlSum(f.ControlSum).SetCreatedBy(f.CreatedBy).SetCreatedAt(f.CreatedAt).SetRejections(rj).Save(ctx)
	if ent.IsConstraintError(err) {
		return biz.ErrConflict("a file with this message id or idempotency key exists")
	}
	return err
}

func toFile(row *ent.PaymentFile) *biz.PaymentFile {
	f := &biz.PaymentFile{ID: row.ID, Org: row.OrganizationID, MessageID: row.MessageID, RequestHash: row.RequestHash,
		IdempotencyKey: row.IdempotencyKey, Transactions: row.Transactions, ControlSum: row.ControlSum,
		CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt}
	for _, x := range row.Rejections {
		f.Rejections = append(f.Rejections, biz.FileRejection{EndToEndID: x.EndToEndID, Reason: x.Reason, Message: x.Message})
	}
	return f
}

func (r *fileRepo) Get(ctx context.Context, id string) (*biz.PaymentFile, error) {
	row, err := r.d.client(ctx).PaymentFile.Get(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return toFile(row), nil
}

func (r *fileRepo) GetByIdempotencyKey(ctx context.Context, org, key string) (*biz.PaymentFile, error) {
	row, err := r.d.client(ctx).PaymentFile.Query().Where(paymentfile.OrganizationID(org), paymentfile.IdempotencyKey(key)).Only(ctx)
	if err != nil {
		return nil, notFound(err)
	}
	return toFile(row), nil
}

func (r *fileRepo) GetByMessageID(ctx context.Context, org, msgID string) (*biz.PaymentFile, error) {
	row, err := r.d.client(ctx).PaymentFile.Query().Where(paymentfile.OrganizationID(org), paymentfile.MessageID(msgID)).Only(ctx)
	if err != nil {
		return nil, notFound(err)
	}
	return toFile(row), nil
}

// ─── Webhooks and the outbox ───

type webhookRepo struct {
	d   *Data
	box biz.SecretBox
}

// NewWebhookRepo creates the webhook repository; secrets are sealed with box.
func NewWebhookRepo(d *Data, box biz.SecretBox) biz.WebhookRepo { return &webhookRepo{d: d, box: box} }

func (r *webhookRepo) CreateSubscription(ctx context.Context, s *biz.Subscription) error {
	sealed, err := r.box.Seal(s.Secret)
	if err != nil {
		return err
	}
	_, err = r.d.client(ctx).WebhookSubscription.Create().SetID(s.ID).SetOrganizationID(s.Org).SetURL(s.URL).
		SetSecretSealed(sealed).SetCreatedBy(s.CreatedBy).SetCreatedAt(s.CreatedAt).Save(ctx)
	return err
}

func (r *webhookRepo) toSub(row *ent.WebhookSubscription) (*biz.Subscription, error) {
	secret, err := r.box.Open(row.SecretSealed)
	if err != nil {
		return nil, fmt.Errorf("webhook %s: %w", row.ID, err)
	}
	return &biz.Subscription{ID: row.ID, Org: row.OrganizationID, URL: row.URL, Secret: secret, CreatedBy: row.CreatedBy, CreatedAt: row.CreatedAt}, nil
}

func (r *webhookRepo) ListSubscriptions(ctx context.Context, org string) ([]*biz.Subscription, error) {
	rows, err := r.d.client(ctx).WebhookSubscription.Query().Where(webhooksubscription.OrganizationID(org)).
		Order(webhooksubscription.ByCreatedAt()).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*biz.Subscription, 0, len(rows))
	for _, row := range rows {
		s, err := r.toSub(row)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (r *webhookRepo) GetSubscription(ctx context.Context, id string) (*biz.Subscription, error) {
	row, err := r.d.client(ctx).WebhookSubscription.Get(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return r.toSub(row)
}

func (r *webhookRepo) DeleteSubscription(ctx context.Context, id string) error {
	return r.d.InTx(ctx, func(ctx context.Context) error {
		c := r.d.client(ctx)
		if _, err := c.WebhookDelivery.Update().
			Where(webhookdelivery.SubscriptionID(id), webhookdelivery.Status("PENDING")).
			SetStatus("FAILED").SetLastError("subscription removed").Save(ctx); err != nil {
			return err
		}
		return c.WebhookSubscription.DeleteOneID(id).Exec(ctx)
	})
}

func (r *webhookRepo) Enqueue(ctx context.Context, org, eventType string, payload func(string) []byte, at time.Time) error {
	c := r.d.client(ctx)
	subs, err := c.WebhookSubscription.Query().Where(webhooksubscription.OrganizationID(org)).All(ctx)
	if err != nil {
		return err
	}
	for _, s := range subs {
		id := "msg_" + strings.ReplaceAll(uuid.Must(uuid.NewV7()).String(), "-", "")
		if _, err := c.WebhookDelivery.Create().SetID(id).SetSubscriptionID(s.ID).SetOrganizationID(org).
			SetEventType(eventType).SetPayload(payload(id)).SetNextAttemptAt(time.Now()).SetCreatedAt(time.Now()).
			Save(ctx); err != nil {
			return err
		}
	}
	return nil
}

func toDelivery(row *ent.WebhookDelivery) *biz.Delivery {
	return &biz.Delivery{ID: row.ID, Subscription: row.SubscriptionID, Org: row.OrganizationID, EventType: row.EventType,
		Payload: row.Payload, Status: row.Status, Attempts: row.Attempts, LastCode: row.LastCode, LastError: row.LastError,
		NextAttempt: row.NextAttemptAt, CreatedAt: row.CreatedAt}
}

func (r *webhookRepo) Pending(ctx context.Context, limit int) ([]*biz.Delivery, error) {
	rows, err := r.d.client(ctx).WebhookDelivery.Query().Where(webhookdelivery.Status("PENDING")).
		Order(webhookdelivery.ByID()).Limit(limit).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*biz.Delivery, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDelivery(row))
	}
	return out, nil
}

func (r *webhookRepo) UpdateDelivery(ctx context.Context, d *biz.Delivery) error {
	return r.d.client(ctx).WebhookDelivery.UpdateOneID(d.ID).SetStatus(d.Status).SetAttempts(d.Attempts).
		SetLastCode(d.LastCode).SetLastError(d.LastError).SetNextAttemptAt(d.NextAttempt).Exec(ctx)
}

func (r *webhookRepo) ListDeliveries(ctx context.Context, sub string) ([]*biz.Delivery, error) {
	rows, err := r.d.client(ctx).WebhookDelivery.Query().Where(webhookdelivery.SubscriptionID(sub)).
		Order(webhookdelivery.ByID()).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*biz.Delivery, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDelivery(row))
	}
	return out, nil
}

// ─── Defunds ───

type defundRepo struct{ d *Data }

// NewDefundRepo creates the defund repository.
func NewDefundRepo(d *Data) biz.DefundRepo { return &defundRepo{d: d} }

func toDefund(row *ent.Defund) *biz.Defund {
	return &biz.Defund{ID: row.ID, Bank: row.BankID, Amount: row.Amount, RequestedByID: row.RequestedByID,
		RequestedBy: row.RequestedBy, ApprovedBy: row.ApprovedBy, Status: row.Status, LedgerRef: row.LedgerRef,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func (r *defundRepo) Create(ctx context.Context, d *biz.Defund) error {
	_, err := r.d.client(ctx).Defund.Create().SetID(d.ID).SetBankID(d.Bank).SetAmount(d.Amount).
		SetRequestedByID(d.RequestedByID).SetRequestedBy(d.RequestedBy).SetStatus(d.Status).SetLedgerRef(d.LedgerRef).
		SetCreatedAt(d.CreatedAt).SetUpdatedAt(d.UpdatedAt).Save(ctx)
	return err
}

func (r *defundRepo) Get(ctx context.Context, id string) (*biz.Defund, error) {
	row, err := r.d.client(ctx).Defund.Get(ctx, id)
	if err != nil {
		return nil, notFound(err)
	}
	return toDefund(row), nil
}

func (r *defundRepo) Update(ctx context.Context, d *biz.Defund) error {
	return r.d.client(ctx).Defund.UpdateOneID(d.ID).SetApprovedBy(d.ApprovedBy).SetStatus(d.Status).
		SetUpdatedAt(d.UpdatedAt).Exec(ctx)
}

func (r *defundRepo) list(ctx context.Context, q *ent.DefundQuery) ([]*biz.Defund, error) {
	rows, err := q.Order(defund.ByCreatedAt(sql.OrderDesc())).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*biz.Defund, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDefund(row))
	}
	return out, nil
}

func (r *defundRepo) ListByBank(ctx context.Context, bank string) ([]*biz.Defund, error) {
	return r.list(ctx, r.d.client(ctx).Defund.Query().Where(defund.BankID(bank)))
}

func (r *defundRepo) ListByStatus(ctx context.Context, status string) ([]*biz.Defund, error) {
	return r.list(ctx, r.d.client(ctx).Defund.Query().Where(defund.Status(status)))
}

// ─── Alerts ───

type alertRepo struct{ d *Data }

// NewAlertRepo creates the alert repository.
func NewAlertRepo(d *Data) biz.AlertRepo { return &alertRepo{d: d} }

func toAlert(row *ent.Alert) *biz.Alert {
	return &biz.Alert{ID: row.ID, Bank: row.BankID, Kind: row.Kind, Message: row.Message, Open: row.Open, At: row.At, ClosedAt: row.ClosedAt}
}

func (r *alertRepo) Create(ctx context.Context, a *biz.Alert) error {
	_, err := r.d.client(ctx).Alert.Create().SetID(a.ID).SetBankID(a.Bank).SetKind(a.Kind).SetMessage(a.Message).
		SetOpen(a.Open).SetAt(a.At).Save(ctx)
	return err
}

func (r *alertRepo) all(ctx context.Context, q *ent.AlertQuery) ([]*biz.Alert, error) {
	rows, err := q.Order(alert.ByAt(sql.OrderDesc()), alert.ByID(sql.OrderDesc())).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*biz.Alert, 0, len(rows))
	for _, row := range rows {
		out = append(out, toAlert(row))
	}
	return out, nil
}

func (r *alertRepo) ListByBank(ctx context.Context, bank string) ([]*biz.Alert, error) {
	return r.all(ctx, r.d.client(ctx).Alert.Query().Where(alert.BankID(bank)))
}

func (r *alertRepo) OpenOfKind(ctx context.Context, bank, kind string) ([]*biz.Alert, error) {
	return r.all(ctx, r.d.client(ctx).Alert.Query().Where(alert.BankID(bank), alert.Kind(kind), alert.Open(true)))
}

func (r *alertRepo) Close(ctx context.Context, id string, at time.Time) error {
	return r.d.client(ctx).Alert.UpdateOneID(id).SetOpen(false).SetClosedAt(at).Exec(ctx)
}

// ─── Netting cycles and reconciliations ───

type networkRepo struct{ d *Data }

// NewNetworkRepo creates operator operations repository.
func NewNetworkRepo(d *Data) biz.NetworkRepo { return &networkRepo{d: d} }

func (r *networkRepo) CreateCycle(ctx context.Context, c *biz.NettingCycle) error {
	_, err := r.d.client(ctx).NettingCycle.Create().SetID(c.ID).SetCycleRef(c.CycleRef).SetAt(c.At).SetTrigger(c.Trigger).
		SetDischarged(c.Discharged).SetDeferred(c.Deferred).SetGross(c.Gross).SetNet(c.Net).Save(ctx)
	return err
}

func (r *networkRepo) ListCycles(ctx context.Context, limit int) ([]*biz.NettingCycle, error) {
	rows, err := r.d.client(ctx).NettingCycle.Query().Order(nettingcycle.ByAt(sql.OrderDesc()), nettingcycle.ByID(sql.OrderDesc())).
		Limit(limit).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*biz.NettingCycle, 0, len(rows))
	for _, x := range rows {
		out = append(out, &biz.NettingCycle{ID: x.ID, CycleRef: x.CycleRef, At: x.At, Trigger: x.Trigger,
			Discharged: x.Discharged, Deferred: x.Deferred, Gross: x.Gross, Net: x.Net})
	}
	return out, nil
}

func (r *networkRepo) CreateReconciliation(ctx context.Context, x *biz.Reconciliation) error {
	_, err := r.d.client(ctx).Reconciliation.Create().SetID(x.ID).SetAt(x.At).SetFedBalance(x.FedBalance).
		SetReservePool(x.ReservePool).SetReconciliationBreak(x.Break).SetInvariants(x.Invariants).Save(ctx)
	return err
}

func (r *networkRepo) ListReconciliations(ctx context.Context, limit int) ([]*biz.Reconciliation, error) {
	rows, err := r.d.client(ctx).Reconciliation.Query().Order(reconciliation.ByAt(sql.OrderDesc()), reconciliation.ByID(sql.OrderDesc())).
		Limit(limit).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*biz.Reconciliation, 0, len(rows))
	for _, x := range rows {
		out = append(out, &biz.Reconciliation{ID: x.ID, At: x.At, FedBalance: x.FedBalance, ReservePool: x.ReservePool,
			Break: x.ReconciliationBreak, Invariants: x.Invariants})
	}
	return out, nil
}

func (r *networkRepo) CreateDistribution(ctx context.Context, x *biz.InterestDistribution) error {
	_, err := r.d.client(ctx).InterestDistribution.Create().SetID(x.ID).SetAt(x.At).SetRunBy(x.RunBy).SetAmount(x.Amount).
		SetPaid(x.Paid).SetRetained(x.Retained).SetShares(x.Shares).Save(ctx)
	return err
}

func (r *networkRepo) ListDistributions(ctx context.Context, limit int) ([]*biz.InterestDistribution, error) {
	rows, err := r.d.client(ctx).InterestDistribution.Query().
		Order(interestdistribution.ByAt(sql.OrderDesc()), interestdistribution.ByID(sql.OrderDesc())).Limit(limit).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*biz.InterestDistribution, 0, len(rows))
	for _, x := range rows {
		out = append(out, &biz.InterestDistribution{ID: x.ID, At: x.At, RunBy: x.RunBy, Amount: x.Amount, Paid: x.Paid,
			Retained: x.Retained, Shares: x.Shares})
	}
	return out, nil
}

// NewRepos bundles the repositories for the engine.
func NewRepos(d *Data, box biz.SecretBox) biz.Repos {
	return biz.Repos{Orders: NewOrderRepo(d), Files: NewFileRepo(d), Webhooks: NewWebhookRepo(d, box),
		Defunds: NewDefundRepo(d), Alerts: NewAlertRepo(d), Network: NewNetworkRepo(d), Tx: d}
}
