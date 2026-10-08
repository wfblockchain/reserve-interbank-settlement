package biz

import (
	"context"
	"errors"
	"time"
)

// ─── Persistence ───

// Transaction runs fn in one database transaction; repositories called with
// the context fn receives join it.
type Transaction interface {
	InTx(ctx context.Context, fn func(ctx context.Context) error) error
}

// OrderFilter selects orders for a list.
type OrderFilter struct {
	Org       string
	Bank      string
	Statuses  []Status
	PageSize  int
	PageToken string
}

// OrderRepo stores payment orders, their audit trail and approvals.
type OrderRepo interface {
	Create(ctx context.Context, o *Order) error
	// Update stores o's fields when its version still matches, appends
	// History[SavedEvents:] and Approvals[SavedApprovals:], and bumps the
	// version. A stale version is a conflict.
	Update(ctx context.Context, o *Order) error
	Get(ctx context.Context, id string) (*Order, error)
	GetByIdempotencyKey(ctx context.Context, org, key string) (*Order, error)
	GetByEndToEndID(ctx context.Context, org, e2e string) (*Order, error)
	List(ctx context.Context, f OrderFilter) ([]*Order, string, error)
	ListByFile(ctx context.Context, fileID string) ([]*Order, error)
	ListCases(ctx context.Context, bank string) ([]*Order, error)
	// SumSince is what an organization has ordered since t, excluding orders
	// that moved nothing (rejected, cancelled, expired).
	SumSince(ctx context.Context, org string, t time.Time) (int64, error)
}

// ErrNoRows is what repositories return for a missing record.
var ErrNoRows = errors.New("not found")

// FileRepo stores pain.001 files.
type FileRepo interface {
	Create(ctx context.Context, f *PaymentFile) error
	Get(ctx context.Context, id string) (*PaymentFile, error)
	GetByIdempotencyKey(ctx context.Context, org, key string) (*PaymentFile, error)
	GetByMessageID(ctx context.Context, org, msgID string) (*PaymentFile, error)
}

// WebhookRepo stores endpoints and the delivery outbox.
type WebhookRepo interface {
	CreateSubscription(ctx context.Context, s *Subscription) error
	ListSubscriptions(ctx context.Context, org string) ([]*Subscription, error)
	GetSubscription(ctx context.Context, id string) (*Subscription, error)
	DeleteSubscription(ctx context.Context, id string) error
	// Enqueue adds one delivery per endpoint of the organization.
	Enqueue(ctx context.Context, org, eventType string, payload func(deliveryID string) []byte, at time.Time) error
	// Pending lists pending deliveries, oldest first.
	Pending(ctx context.Context, limit int) ([]*Delivery, error)
	UpdateDelivery(ctx context.Context, d *Delivery) error
	ListDeliveries(ctx context.Context, subscription string) ([]*Delivery, error)
}

// DefundRepo stores defund requests.
type DefundRepo interface {
	Create(ctx context.Context, d *Defund) error
	Get(ctx context.Context, id string) (*Defund, error)
	Update(ctx context.Context, d *Defund) error
	ListByBank(ctx context.Context, bank string) ([]*Defund, error)
	ListByStatus(ctx context.Context, status string) ([]*Defund, error)
}

// AlertRepo stores treasury alerts.
type AlertRepo interface {
	Create(ctx context.Context, a *Alert) error
	ListByBank(ctx context.Context, bank string) ([]*Alert, error)
	OpenOfKind(ctx context.Context, bank, kind string) ([]*Alert, error)
	Close(ctx context.Context, id string, at time.Time) error
}

// NetworkRepo stores netting cycles, reconciliations and reserve-interest
// distributions.
type NetworkRepo interface {
	CreateCycle(ctx context.Context, c *NettingCycle) error
	ListCycles(ctx context.Context, limit int) ([]*NettingCycle, error)
	CreateReconciliation(ctx context.Context, r *Reconciliation) error
	ListReconciliations(ctx context.Context, limit int) ([]*Reconciliation, error)
	CreateDistribution(ctx context.Context, d *InterestDistribution) error
	ListDistributions(ctx context.Context, limit int) ([]*InterestDistribution, error)
}

// ─── The settlement rails ───

// Conversion states.
const (
	ConversionSettled  = "SETTLED"
	ConversionRejected = "REJECTED"
)

// Obligation states in the netting engine. EXPIRED is the hub's own: an
// obligation still queued after the hub's time-to-live is cancelled on chain
// and refunded.
const (
	ObligationQueued    = "QUEUED"
	ObligationSettled   = "SETTLED"
	ObligationCancelled = "CANCELLED"
	ObligationExpired   = "EXPIRED"
)

// ErrInsufficientFunds is a deposit account that cannot cover a debit.
var ErrInsufficientFunds = errors.New("insufficient funds")

// ErrAccountClosed is a payee account that does not exist or is closed at
// the receiving bank (AC04).
var ErrAccountClosed = errors.New("no such open account")

// ErrOutcomeUnknown is a chain transaction that was sent but whose result
// could not be established. It may still execute, so the caller must not
// compensate (refund, reverse) as if it had failed; operations resolve it.
var ErrOutcomeUnknown = errors.New("outcome unknown")

// BankInfo describes a member bank.
type BankInfo struct {
	MemberID     string
	Name         string
	DepositToken string // the bank's deposit token symbol
	Routing      string
}

// AccountRecord is what a bank's core system knows about an account.
type AccountRecord struct {
	Name   string
	Closed bool
}

// Conversion is the result of a cross-bank conversion: burned at the
// sending bank, settlement money moved between the banks, minted at the
// receiving bank, in one transaction or not at all.
type Conversion struct {
	Ref    string // transaction hash
	State  string // SETTLED | REJECTED
	Reason string
}

// CycleReport is what a netting cycle settled.
type CycleReport struct {
	CycleRef   string
	Discharged int
	Deferred   int
	Gross      int64
	Net        int64
}

// Draw is an intraday draw from the liquidity pool against collateral.
type Draw struct {
	Ref        string
	Principal  int64
	Collateral int64 // demo T-bills posted, at par
	RateBps    int
	OpenedAt   time.Time
	Interest   int64 // owed now while open; paid once repaid
	Overdue    bool
	Open       bool
}

// PoolState is the intraday liquidity pool's standing.
type PoolState struct {
	Assets         int64
	Drawn          int64
	UtilizationBps int
}

// Rails are the clearing contracts and the systems around them: the chain,
// the Fed with the reserve account, and each member bank's core ledger.
// Amounts are in cents.
type Rails interface {
	Now() time.Time
	SetTime(ctx context.Context, t time.Time) error
	FedwireOpen(t time.Time) bool
	NextFedwireOpen(t time.Time) time.Time

	Banks() []BankInfo
	BankByRouting(routing string) (BankInfo, bool)
	Bank(memberID string) (BankInfo, bool)
	Member(ctx context.Context, memberID string) (Member, error)

	// Core banking.
	Account(bank, account string) (AccountRecord, bool)
	Balance(bank, account string) int64
	Postings(bank, account string) []Posting
	Post(bank, account string, delta int64, ref string) error // idempotent per (account, ref)
	EnsureAccount(bank, account, name string)
	InFlight(ctx context.Context, bank, account string) int64

	// Gross, 24x7: a customer's deposit becomes the bank's deposit token, and
	// converts to the payee's bank's deposit token over settlement money.
	//
	// Every side effect is exactly once per uetr, the order's end-to-end id:
	// a payment replayed after a lost database write repeats nothing.
	Tokenize(ctx context.Context, bank, account string, amount int64, uetr string) error
	Convert(ctx context.Context, bank, account string, to Party, amount int64, uetr string) (Conversion, error)
	// Converted reports a conversion already made for uetr.
	Converted(uetr string) (Conversion, bool)
	// Refund takes the order's deposit token back into the payer's account.
	Refund(ctx context.Context, bank, account string, amount int64, uetr string) error
	ConversionCredited(receivingBank, ref string) bool

	// Netted: deposit-funded obligations between banks.
	SubmitObligation(ctx context.Context, bank, account string, to Party, amount int64, uetr string) (string, error)
	ObligationState(ctx context.Context, ref string) (string, error)
	CancelObligation(ctx context.Context, bank, ref string) error
	ObligationCredited(receivingBank, ref string) bool
	QueuedObligations(ctx context.Context) int
	RunCycle(ctx context.Context) (CycleReport, error)
	ExpireStaleObligations(ctx context.Context) error
	EfficiencyBps(ctx context.Context) int

	// Bank treasury: settlement money against reserves, and intraday
	// liquidity against collateral.
	Fund(ctx context.Context, bank string, amount int64) FundResult
	RequestDefund(ctx context.Context, bank string, amount int64) (string, error)
	ApproveDefund(ctx context.Context, bank, ref string) error
	DefundState(ctx context.Context, ref string) (string, error)
	DrawIntraday(ctx context.Context, bank string, amount int64) (Draw, error)
	RepayIntraday(ctx context.Context, bank, ref string) (Draw, error)
	Draws(ctx context.Context, bank string) ([]Draw, error)
	Pool(ctx context.Context) (PoolState, error)

	// The operator: the reserve account against the token, and reserve
	// interest passed through to holders.
	FedReserveBalance() int64
	ReservePool(ctx context.Context) (int64, error)
	Reconcile(ctx context.Context) (Reconciliation, error)
	Health(ctx context.Context) (brk, invariants bool)
	DistributeInterest(ctx context.Context, amount int64) (InterestDistribution, error)

	// Process runs everything asynchronous on the rails: approved defunds
	// waiting for Fedwire, payees credited from arrived deposit tokens, and
	// payees credited for settled obligations.
	Process(ctx context.Context) error
}

// ─── Screening ───

// SanctionsMatch is a screening hit.
type SanctionsMatch struct {
	List     string // us_ofac
	SourceID string // SDN entry number
	Name     string
	Programs []string
	Score    float64
}

// Screener checks a name against sanctions lists.
type Screener interface {
	Screen(ctx context.Context, name string) (SanctionsMatch, bool)
}

// ─── Notifications ───

// SecretBox encrypts webhook signing secrets at rest.
type SecretBox interface {
	Seal(plain []byte) ([]byte, error)
	Open(sealed []byte) ([]byte, error)
}

// ─── Calendar ───

// Calendar counts business days (weekends and Federal Reserve holidays off).
type Calendar interface {
	AddBusinessDays(t time.Time, n int) time.Time
}
