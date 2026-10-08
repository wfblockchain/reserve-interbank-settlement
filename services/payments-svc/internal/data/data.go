// Package data implements biz's repositories with Ent over PostgreSQL (pgx)
// in production, or SQLite (modernc, pure Go) for tests and local runs.
package data

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/go-kratos/kratos/v2/log"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"

	"reserve-interbank-settlement/services/payments-svc/ent"
	"reserve-interbank-settlement/services/payments-svc/internal/biz"
	"reserve-interbank-settlement/services/payments-svc/internal/conf"
)

// Data holds the Ent client.
type Data struct {
	db    *ent.Client
	sqlDB *sql.DB
	log   *log.Helper
}

// NewData opens the database and creates or updates the schema.
func NewData(c *conf.Data, logger log.Logger) (*Data, func(), error) {
	h := log.NewHelper(logger)
	var (
		driver, dia string
	)
	switch c.Driver {
	case "postgres":
		driver, dia = "pgx", dialect.Postgres
	case "sqlite":
		driver, dia = "sqlite", dialect.SQLite
	default:
		return nil, nil, fmt.Errorf("unsupported database driver %q", c.Driver)
	}
	db, err := sql.Open(driver, c.Source)
	if err != nil {
		return nil, nil, fmt.Errorf("open database: %w", err)
	}
	if c.Driver == "sqlite" {
		// One writer: SQLite serializes writes anyway, and a single
		// connection keeps an in-memory database alive.
		db.SetMaxOpenConns(1)
		db.SetConnMaxLifetime(0)
	} else {
		maxOpen, maxIdle := c.MaxOpenConns, c.MaxIdleConns
		if maxOpen <= 0 {
			maxOpen = 20
		}
		if maxIdle <= 0 {
			maxIdle = 5
		}
		db.SetMaxOpenConns(maxOpen)
		db.SetMaxIdleConns(maxIdle)
		db.SetConnMaxLifetime(5 * time.Minute)
	}
	if err := db.PingContext(context.Background()); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("ping database: %w", err)
	}
	client := ent.NewClient(ent.Driver(entsql.OpenDB(dia, db)))
	if err := client.Schema.Create(context.Background()); err != nil {
		db.Close()
		return nil, nil, fmt.Errorf("create schema: %w", err)
	}
	h.Infof("database ready (%s)", c.Driver)
	d := &Data{db: client, sqlDB: db, log: h}
	return d, func() {
		if err := client.Close(); err != nil {
			h.Errorf("close database: %v", err)
		}
	}, nil
}

// PingDB checks the database for readiness probes.
func (d *Data) PingDB(ctx context.Context) error { return d.sqlDB.PingContext(ctx) }

type txKey struct{}

// InTx runs fn in a transaction; repositories called with fn's context join
// it. A nested call joins the outer transaction.
func (d *Data) InTx(ctx context.Context, fn func(ctx context.Context) error) error {
	if _, ok := ctx.Value(txKey{}).(*ent.Tx); ok {
		return fn(ctx)
	}
	tx, err := d.db.Tx(ctx)
	if err != nil {
		return err
	}
	if err := fn(context.WithValue(ctx, txKey{}, tx)); err != nil {
		if rerr := tx.Rollback(); rerr != nil {
			return fmt.Errorf("%w (rollback: %v)", err, rerr)
		}
		return err
	}
	return tx.Commit()
}

// client is the transaction's client when ctx carries one.
func (d *Data) client(ctx context.Context) *ent.Client {
	if tx, ok := ctx.Value(txKey{}).(*ent.Tx); ok {
		return tx.Client()
	}
	return d.db
}

// NewTransaction exposes Data as biz.Transaction.
func NewTransaction(d *Data) biz.Transaction { return d }

// notFound maps Ent's not-found to biz.ErrNoRows.
func notFound(err error) error {
	if ent.IsNotFound(err) {
		return biz.ErrNoRows
	}
	return err
}
