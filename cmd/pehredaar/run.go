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

	"github.com/akkuraina/pehredaar/internal/config"
	"github.com/akkuraina/pehredaar/internal/dashboard"
	"github.com/akkuraina/pehredaar/internal/fetcher"
	"github.com/akkuraina/pehredaar/internal/normalize"
	"github.com/akkuraina/pehredaar/internal/scheduler"
	"github.com/akkuraina/pehredaar/internal/store/sqlite"
)

// runServe is `pehredaar run`: sync sources.yaml, then run the scheduler
// and the local dashboard until Ctrl+C.
func runServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	// Each flag defaults to its environment variable, so the same binary
	// works from a terminal (flags) and from a service manager (env).
	dbPath := fs.String("db", envOr("PEHREDAAR_DB", filepath.Join("data", "pehredaar.db")), "SQLite database file")
	sourcesPath := fs.String("sources", envOr("PEHREDAAR_SOURCES", "sources.yaml"), "curated sources file")
	addr := fs.String("http", envOr("PEHREDAAR_HTTP", "127.0.0.1:8787"), "dashboard address (localhost only)")
	workers := fs.Int("workers", envInt("PEHREDAAR_WORKERS", 4), "concurrent checks")
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

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

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

	// errgroup runs the scheduler and the web server side by side. If either
	// fails, gctx is cancelled and the other one shuts down too; Wait
	// returns the first error.
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return sched.Run(gctx) })
	g.Go(func() error {
		log.Info("dashboard listening", "url", "http://"+*addr)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error {
		<-gctx.Done()
		// A fresh context: gctx is already cancelled, and Shutdown needs time
		// to let open requests (like a running "check now") finish.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	})
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
