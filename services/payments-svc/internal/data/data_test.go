package data

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"testing"
	"time"

	kerrors "github.com/go-kratos/kratos/v2/errors"
	"github.com/go-kratos/kratos/v2/log"

	"reserve-interbank-settlement/services/payments-svc/internal/biz"
	"reserve-interbank-settlement/services/payments-svc/internal/conf"
	"reserve-interbank-settlement/services/payments-svc/internal/notify"
)

func newRepos(t *testing.T) (biz.Repos, *Data) {
	t.Helper()
	d, cleanup, err := NewData(&conf.Data{Driver: "sqlite",
		Source: "file:" + filepath.Join(t.TempDir(), "t.db") + "?_pragma=foreign_keys(1)"}, log.DefaultLogger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	box, _ := notify.NewBox(&conf.Notifications{SecretKey: base64.StdEncoding.EncodeToString(make([]byte, 32))})
	return NewRepos(d, box), d
}

func order(id, key, e2e string) *biz.Order {
	now := time.Now()
	return &biz.Order{ID: id, Org: "northwind", Bank: "BNKAUS30", IdempotencyKey: key, RequestHash: "h", EndToEndID: e2e,
		UETR: "u-" + id, DebtorAccount: "A-1", Creditor: biz.Party{Name: "C", Routing: "234567898", Account: "A"},
		Amount: 100, Priority: biz.Normal, Status: biz.StatusAwaitingApproval, ISO: biz.ISOReceived,
		PayeeCheck: biz.PayeeCheck{Account: "OPEN", Name: "MATCH"}, ApprovalsRequired: 1, CreatedBy: "m", CreatedAt: now, UpdatedAt: now,
		History: []biz.Event{{Seq: 1, At: now, Actor: "m", Status: biz.StatusAwaitingApproval, ISO: biz.ISOReceived, Note: "received"}}}
}

func TestOrdersAreVersionedAndUnique(t *testing.T) {
	r, _ := newRepos(t)
	ctx := context.Background()
	if err := r.Orders.Create(ctx, order("o1", "k1", "e1")); err != nil {
		t.Fatal(err)
	}
	if err := r.Orders.Create(ctx, order("o2", "k1", "e2")); kerrors.FromError(err).Code != 409 {
		t.Fatalf("a second order under the same idempotency key: %v", err)
	}
	if err := r.Orders.Create(ctx, order("o3", "k3", "e1")); kerrors.FromError(err).Code != 409 {
		t.Fatalf("a second order with the same end-to-end id: %v", err)
	}
	a, _ := r.Orders.Get(ctx, "o1")
	b, _ := r.Orders.Get(ctx, "o1")
	a.Status = biz.StatusInProcess
	a.History = append(a.History, biz.Event{Seq: 2, At: time.Now(), Actor: "x", Status: a.Status, ISO: biz.ISOAcceptedCustomer, Note: "approved"})
	a.Approvals = append(a.Approvals, biz.Approval{ApproverID: "x", ApproverName: "X", At: time.Now()})
	if err := r.Orders.Update(ctx, a); err != nil {
		t.Fatal(err)
	}
	b.Status = biz.StatusCancelled
	if err := r.Orders.Update(ctx, b); kerrors.FromError(err).Code != 409 {
		t.Fatalf("a stale write won: %v", err)
	}
	got, _ := r.Orders.Get(ctx, "o1")
	if got.Status != biz.StatusInProcess || len(got.History) != 2 || len(got.Approvals) != 1 || got.Version != 1 {
		t.Fatalf("stored: %+v", got)
	}
	if _, err := r.Orders.Get(ctx, "missing"); !errors.Is(err, biz.ErrNoRows) {
		t.Fatalf("missing: %v", err)
	}
}

func TestOutboxIsWrittenWithTheOrderOrNotAtAll(t *testing.T) {
	r, d := newRepos(t)
	ctx := context.Background()
	if err := r.Webhooks.CreateSubscription(ctx, &biz.Subscription{ID: "s1", Org: "northwind", URL: "https://erp.example/h",
		Secret: []byte("k"), CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	failing := errors.New("boom")
	err := d.InTx(ctx, func(ctx context.Context) error {
		if err := r.Orders.Create(ctx, order("o1", "k1", "e1")); err != nil {
			return err
		}
		if err := r.Webhooks.Enqueue(ctx, "northwind", "payment.x", func(id string) []byte { return []byte(id) }, time.Now()); err != nil {
			return err
		}
		return failing
	})
	if !errors.Is(err, failing) {
		t.Fatal(err)
	}
	if p, _ := r.Webhooks.Pending(ctx, 10); len(p) != 0 {
		t.Fatal("a delivery survived the rolled-back transaction")
	}
	if _, err := r.Orders.Get(ctx, "o1"); !errors.Is(err, biz.ErrNoRows) {
		t.Fatal("the order survived the rolled-back transaction")
	}
	_ = r.Webhooks.Enqueue(ctx, "northwind", "payment.a", func(id string) []byte { return []byte(id) }, time.Now())
	_ = r.Webhooks.Enqueue(ctx, "northwind", "payment.b", func(id string) []byte { return []byte(id) }, time.Now())
	p, _ := r.Webhooks.Pending(ctx, 10)
	if len(p) != 2 || p[0].EventType != "payment.a" || string(p[0].Payload) != p[0].ID {
		t.Fatalf("outbox order or payload: %+v", p)
	}
	s, _ := r.Webhooks.GetSubscription(ctx, "s1")
	if string(s.Secret) != "k" {
		t.Fatal("secret did not round-trip through the box")
	}
}
