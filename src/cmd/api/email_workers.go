package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"

	"skein-api/internal/config"
	"skein-api/internal/invitations"
	"skein-api/internal/mail"
	"skein-api/internal/tokens"
)

// startEmailWorkers starts the background workers of outgoing mail
// (sk-674.18 T022, K-9) and returns a channel closed once every one of them
// has returned after ctx is cancelled:
//
//   - the mail worker (mail.Runner) — only while SKEIN_EMAIL_ENABLED=true:
//     it delivers outgoing_emails over the SMTP relay of SKEIN_SMTP_*, with
//     links on SKEIN_APP_BASE_URL and tokens from the token protocol.
//   - the mail cleanup (mail.CleanupRunner) — always: it ends emails past
//     their deadline as expired (with mail off nothing else would) and
//     deletes final emails after their retention.
//   - the invitation expiry (invitations.Expirer, T040) — always: it gives
//     overdue pending invitations their outcome `expired` whether or not
//     mail is on, and sweeps the journal of sends.
//
// It is THE wiring of these workers: main calls it, and the wiring test
// calls the same function, so a worker built but not started, or started by
// the wrong flag, is visible to a test. The goroutines are started in the
// caller's goroutine context (a test under testing/synctest gets them in
// its bubble).
func startEmailWorkers(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config) (<-chan struct{}, error) {
	var runner *mail.Runner
	if cfg.EmailEnabled {
		sender, err := mail.NewSMTPSender(cfg, nil)
		if err != nil {
			return nil, fmt.Errorf("building the SMTP sender: %w", err)
		}
		runner, err = mail.NewRunner(pool, sender, tokens.EmailIssuer(pool), cfg.AppBaseURL, mail.Options{})
		if err != nil {
			return nil, fmt.Errorf("building the mail runner: %w", err)
		}
	}
	cleanup := mail.NewCleanupRunner(pool, mail.CleanupOptions{})
	expirer := invitations.NewExpirer(pool, invitations.ExpiryOptions{})

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		cleanup.Run(ctx)
	}()
	go func() {
		defer wg.Done()
		expirer.Run(ctx)
	}()
	if runner != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runner.Run(ctx)
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	return done, nil
}
