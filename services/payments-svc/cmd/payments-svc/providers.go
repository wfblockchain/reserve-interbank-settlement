package main

import (
	"context"
	"fmt"

	"github.com/go-kratos/kratos/v2/log"

	"reserve-interbank-settlement/services/payments-svc/internal/auth"
	"reserve-interbank-settlement/services/payments-svc/internal/biz"
	"reserve-interbank-settlement/services/payments-svc/internal/conf"
	"reserve-interbank-settlement/services/payments-svc/internal/rails"
)

func provideServerConf(bc *conf.Bootstrap) *conf.Server               { return bc.Server }
func provideDataConf(bc *conf.Bootstrap) *conf.Data                   { return bc.Data }
func provideNetworkConf(bc *conf.Bootstrap) *conf.Network             { return bc.Network }
func provideSanctionsConf(bc *conf.Bootstrap) *conf.Sanctions         { return bc.Sanctions }
func provideNotificationsConf(bc *conf.Bootstrap) *conf.Notifications { return bc.Notifications }
func provideJobsConf(bc *conf.Bootstrap) *conf.Jobs                   { return bc.Jobs }
func provideContext() context.Context                                 { return context.Background() }
func provideCalendar() biz.Calendar                                   { return biz.FedCalendar{} }
func provideWebhookRepo(r biz.Repos) biz.WebhookRepo                  { return r.Webhooks }

func provideAuthenticator(bc *conf.Bootstrap) (auth.Authenticator, error) {
	if bc.Auth.Mode == "static" {
		return auth.NewStatic(bc.Auth.StaticUsers)
	}
	return auth.NewOIDC(context.Background(), bc.Auth.Issuer, bc.Auth.Audience)
}

// provideOptions turns configuration into the engine's policy.
func provideOptions(bc *conf.Bootstrap) (biz.Options, error) {
	opts := biz.Options{Watermarks: map[string]biz.Watermark{}, AllowLoopbackHTTP: bc.Notifications.AllowLoopbackHTTP}
	if bc.Jobs != nil {
		opts.NettingEvery = bc.Jobs.NettingEvery.Std()
	}
	amount := func(what, v string) (int64, error) {
		if v == "" {
			return 0, nil
		}
		c, err := biz.ParseAmount(v)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", what, err)
		}
		return c, nil
	}
	for _, b := range bc.Network.Banks {
		low, err := amount(b.MemberID+" low_watermark", b.LowWatermark)
		if err != nil {
			return opts, err
		}
		normal, err := amount(b.MemberID+" normal_watermark", b.NormalWatermark)
		if err != nil {
			return opts, err
		}
		opts.Watermarks[b.MemberID] = biz.Watermark{Low: low, Normal: normal}
	}
	for _, o := range bc.Organizations {
		org := biz.Org{ID: o.ID, Name: o.Name, Bank: o.Bank, Account: o.Account}
		var err error
		if org.PerPaymentLimit, err = amount(o.ID+" per_payment_limit", o.PerPaymentLimit); err != nil {
			return opts, err
		}
		if org.DailyLimit, err = amount(o.ID+" daily_limit", o.DailyLimit); err != nil {
			return opts, err
		}
		if org.SecondApprovalAbove, err = amount(o.ID+" second_approval_above", o.SecondApprovalAbove); err != nil {
			return opts, err
		}
		opts.Orgs = append(opts.Orgs, org)
	}
	return opts, nil
}

// provideRails connects the rails, refusing first on a database with payment
// history. The rails keep their state in the process (the core ledgers,
// customer wallets, conversion and obligation records) and start fresh, so
// orders from an earlier run would be paired with ledgers that never saw
// them: balances would silently reset and in-flight payments would never
// resolve. The check runs before the rails touch the chain, so a refused
// restart changes nothing. Failing closed is the only safe answer until the
// rails' state is durable.
func provideRails(ctx context.Context, c *conf.Network, r biz.Repos, logger log.Logger) (*rails.Reserve, func(), error) {
	prior, _, err := r.Orders.List(ctx, biz.OrderFilter{PageSize: 1})
	if err != nil {
		return nil, nil, fmt.Errorf("check for payment history: %w", err)
	}
	if len(prior) > 0 {
		return nil, nil, fmt.Errorf("refusing to start: the database holds payment orders from an earlier run, " +
			"but the rails' state (core ledgers, wallets, conversions, obligations) lives in the process and " +
			"would start fresh; start on a new database, or make the rails' state durable first")
	}
	return rails.New(ctx, c, logger)
}
