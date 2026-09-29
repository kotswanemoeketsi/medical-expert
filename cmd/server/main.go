// Command server runs the Medical Expert System API.
//
// Configuration comes from the environment, or from a .env file in the
// working directory (see .env.example). It refuses to start with pending
// migrations unless MES_AUTO_MIGRATE is on (the default outside production).
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yourorg/medical-expert-system/internal/ai"
	"github.com/yourorg/medical-expert-system/internal/config"
	"github.com/yourorg/medical-expert-system/internal/controller"
	"github.com/yourorg/medical-expert-system/internal/db"
	"github.com/yourorg/medical-expert-system/internal/job"
	"github.com/yourorg/medical-expert-system/internal/service"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "server:", err)
		os.Exit(1)
	}
}

func run() error {
	if err := config.LoadDotEnv(config.EnvFile()); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := newLogger(cfg)
	slog.SetDefault(logger)
	if err := cfg.Validate(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := db.Open(ctx, cfg.DSN, db.Options{
		MaxOpen: cfg.DBMaxOpen, MaxIdle: cfg.DBMaxIdle, ConnMaxLife: cfg.DBConnMaxLife, ConnMaxIdle: cfg.DBConnMaxIdle,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	pending, err := db.Pending(ctx, conn)
	if err != nil {
		return err
	}
	if len(pending) > 0 {
		if !cfg.AutoMigrate {
			return fmt.Errorf("%d migration(s) pending (%s): run `go run ./cmd/migrate`, or set MES_AUTO_MIGRATE=true",
				len(pending), strings.Join(pending, ", "))
		}
		applied, err := db.Migrate(ctx, conn, func(f string, a ...any) { logger.Info(fmt.Sprintf(f, a...)) })
		if err != nil {
			return err
		}
		logger.Info("migrations applied", "count", len(applied))
	}

	var drafter ai.Drafter
	if cfg.AnthropicAPIKey != "" {
		client, err := ai.New(ai.Config{APIKey: cfg.AnthropicAPIKey, Model: cfg.AIModel, BaseURL: cfg.AIBaseURL, Timeout: cfg.AITimeout})
		if err != nil {
			return err
		}
		drafter = client
		logger.Info("note drafting enabled", "model", client.Model(), "daily_limit_per_practice", cfg.AIDailyLimit)
	} else {
		logger.Info("note drafting disabled: ANTHROPIC_API_KEY is not set")
	}

	reg, err := service.NewRegistry(service.Deps{
		DB: conn, Logger: logger, Drafter: drafter,
		Config: service.Config{
			SessionIdle: cfg.SessionIdle, SessionMax: cfg.SessionMax, MFAChallengeTTL: cfg.MFAChallengeTTL,
			MaxLoginAttempts: cfg.MaxLoginAttempts, Lockout: cfg.Lockout, RequireMFA: cfg.RequireMFA,
			MFAIssuer: cfg.MFAIssuer, AIDailyLimit: cfg.AIDailyLimit, DefaultReviewMonths: cfg.DefaultReviewMonths,
		},
	})
	if err != nil {
		return err
	}
	api := controller.New(reg, logger, controller.Options{
		Production: cfg.Production(), TrustProxyHeaders: cfg.TrustProxyHeaders, LoginPerMinute: cfg.LoginPerMinute,
	})

	(&job.Scheduler{Log: logger, Interval: time.Hour, Tasks: []job.Task{
		{Name: "purge_sessions", Run: func(ctx context.Context) (string, error) {
			n, err := reg.PurgeSessions(ctx)
			return fmt.Sprintf("%d removed", n), err
		}},
		{Name: "expire_prescriptions", Run: func(ctx context.Context) (string, error) {
			n, err := reg.ExpirePrescriptions(ctx)
			return fmt.Sprintf("%d expired", n), err
		}},
	}}).Start(ctx)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		// Long enough for a note draft, which waits on the model provider.
		WriteTimeout:   150 * time.Second,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 64 << 10,
		ErrorLog:       slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	logger.Info("listening", "addr", cfg.Addr, "env", cfg.Env, "require_mfa", cfg.RequireMFA)

	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		logger.Info("shutting down")
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

func newLogger(cfg *config.Config) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.LogLevel}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
