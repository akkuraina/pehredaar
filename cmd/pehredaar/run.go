package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/akkuraina/pehredaar/internal/auth"
	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/dashboard"
	"github.com/akkuraina/pehredaar/internal/email"
	"github.com/akkuraina/pehredaar/internal/fetcher"
	"github.com/akkuraina/pehredaar/internal/normalize"
	"github.com/akkuraina/pehredaar/internal/notifier"
	"github.com/akkuraina/pehredaar/internal/outbox"
	"github.com/akkuraina/pehredaar/internal/scheduler"
	"github.com/akkuraina/pehredaar/internal/store"
	"github.com/akkuraina/pehredaar/internal/store/sqlite"
	"github.com/akkuraina/pehredaar/internal/summarizer"
	"github.com/akkuraina/pehredaar/internal/web"
)

// runServe is `pehredaar run`: sync sources.yaml, then run the scheduler,
// the outbox, the Telegram bot, the public website and the local operator
// dashboard until Ctrl+C.
func runServe(ctx context.Context, args []string) error {
	// First, because the flag defaults below read the environment.
	fromDotEnv, err := config.LoadDotEnv(".env")
	if err != nil {
		return err
	}

	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	// Each flag defaults to its environment variable, so the same binary
	// works from a terminal (flags) and from a service manager (env).
	dbPath := fs.String("db", envOr("PEHREDAAR_DB", filepath.Join("data", "pehredaar.db")), "SQLite database file")
	sourcesPath := fs.String("sources", envOr("PEHREDAAR_SOURCES", "sources.yaml"), "curated sources file")
	addr := fs.String("http", envOr("PEHREDAAR_HTTP", "127.0.0.1:8787"), "dashboard address (localhost only)")
	workers := fs.Int("workers", envInt("PEHREDAAR_WORKERS", 4), "concurrent checks")
	public := fs.String("public", envOr("PEHREDAAR_PUBLIC", "127.0.0.1:8788"), "public website address")
	baseURL := fs.String("base-url", os.Getenv("PEHREDAAR_BASE_URL"), "the website's public URL, for links in emails and Telegram (default http://<public>)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "Usage: pehredaar run [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := requireLoopback(*addr); err != nil {
		return err
	}
	if *baseURL == "" {
		*baseURL = "http://" + *public
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if len(fromDotEnv) > 0 {
		log.Info("loaded .env", "vars", fromDotEnv) // names only, never values
	}

	sources, err := config.LoadSources(*sourcesPath, time.Now())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o755); err != nil {
		return err
	}
	st, err := sqlite.Open(ctx, *dbPath)
	if err != nil {
		return err
	}
	defer st.Close()

	disabled, err := config.Sync(ctx, st, sources)
	if err != nil {
		return fmt.Errorf("syncing %s: %w", *sourcesPath, err)
	}
	log.Info("sources synced", "file", *sourcesPath, "curated", len(sources.List), "disabled", disabled, "db", *dbPath)

	norm := normalize.New()
	if p := os.Getenv("PDFTOTEXT_PATH"); p != "" {
		norm.PDFToText = p
	}
	sched := scheduler.New(scheduler.Config{
		Store:      st,
		Fetcher:    fetcher.New(fetcher.DefaultConfig()),
		Normalizer: norm,
		Sources:    sources,
		Workers:    *workers,
		Logger:     log,
	})

	handler, err := dashboard.New(st, sched, log)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Alert channels. Each is optional; the outbox skips channels without
	// a sender, and the website only offers the channels that exist.
	senders := map[store.Channel]outbox.Sender{}
	var tg *notifier.Bot
	if token := os.Getenv("TELEGRAM_BOT_TOKEN"); token != "" {
		tg, err = notifier.New(token, st, sources.Categories(), *baseURL, log)
		if err != nil {
			return err
		}
		senders[store.ChannelTelegram] = tg
	} else {
		log.Warn("TELEGRAM_BOT_TOKEN is not set: running without the Telegram bot")
	}
	signer, err := auth.LoadSigner(ctx, st)
	if err != nil {
		return err
	}
	var mailer *email.Sender
	if key := os.Getenv("RESEND_API_KEY"); key != "" {
		mailer = email.New(email.Config{
			APIKey: key, From: os.Getenv("PEHREDAAR_EMAIL_FROM"), BaseURL: *baseURL, Signer: signer, Logger: log,
		})
		senders[store.ChannelEmail] = mailer
		log.Info("email via Resend", "from", envOr("PEHREDAAR_EMAIL_FROM", email.DefaultFrom))
	} else {
		log.Warn("RESEND_API_KEY is not set: no email sign-ups or alerts")
	}

	// Claude writes the summaries when a key is set; otherwise alerts use
	// the plain template (which is also the fallback when the API is down).
	var sum summarizer.Summarizer = summarizer.Template{}
	if key := os.Getenv("ANTHROPIC_API_KEY"); key != "" {
		model := envOr("PEHREDAAR_LLM_MODEL", summarizer.DefaultModel)
		sum = summarizer.NewClaude(key, model, log)
		log.Info("summaries by Claude", "model", model)
	} else {
		log.Info("ANTHROPIC_API_KEY is not set: alerts use the plain template")
	}

	site, err := web.New(web.Config{
		Store: st, Signer: signer, Categories: sources.Categories(), BaseURL: *baseURL, Logger: log,
		Mailer: mailerOrNil(mailer),
		BotUsername: func() string {
			if tg == nil {
				return ""
			}
			return tg.Username()
		},
	})
	if err != nil {
		return err
	}
	pub := &http.Server{
		Addr:              *public,
		Handler:           site.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No WriteTimeout: /events streams for as long as a page is open.
	}

	// The outbox always runs, even with no channel configured: it also
	// writes the summaries the website shows. errgroup runs everything side
	// by side; if one part fails, gctx is cancelled, the others shut down,
	// and Wait returns the first error.
	proc := outbox.New(outbox.Config{
		Store:      st,
		Summarizer: sum,
		Senders:    senders,
		Categories: sources.Categories(),
		Logger:     log,
	})
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return sched.Run(gctx) })
	g.Go(func() error { return proc.Run(gctx) })
	g.Go(func() error { return site.Hub().Run(gctx) })
	if tg != nil {
		g.Go(func() error { return tg.Run(gctx) })
	}
	for _, s := range []struct {
		name string
		srv  *http.Server
		url  string
	}{{"dashboard", srv, "http://" + *addr}, {"website", pub, *baseURL}} {
		g.Go(func() error {
			log.Info(s.name+" listening", "url", s.url)
			if err := s.srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				return fmt.Errorf("%s: %w", s.name, err)
			}
			return nil
		})
		g.Go(func() error {
			<-gctx.Done()
			// A fresh context: gctx is already cancelled, and Shutdown needs
			// time to let open requests (like a running "check now") finish.
			// Open /events streams don't end on their own, hence the cap.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.srv.Shutdown(shutdownCtx); err != nil {
				s.srv.Close()
			}
			return nil
		})
	}
	err = g.Wait()
	log.Info("stopped")
	return err
}

// requireLoopback refuses to serve the dashboard beyond this machine: it has
// no login.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("bad -http address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return fmt.Errorf("-http %q: the dashboard has no login, so it only listens on localhost (e.g. 127.0.0.1:8787)", addr)
}

// mailerOrNil keeps a nil *email.Sender from becoming a non-nil
// web.Mailer interface holding a nil pointer (a classic Go gotcha: the
// website would think email is configured, then crash using it).
func mailerOrNil(m *email.Sender) web.Mailer {
	if m == nil {
		return nil
	}
	return m
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return n
	}
	return def
}
