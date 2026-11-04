// Command seller runs the external seller service: the VTEX seller protocol,
// the dispatch worker and the public demo (panel, docs, marketplace simulator).
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/fabriciossouza/poc-vtex-seller-go/internal/bootstrap"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
	if err := run(); err != nil {
		slog.Error("seller stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadSettings(os.Getenv)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.redisAddr})
	defer rdb.Close()
	cfg.app.Redis = rdb
	app, err := bootstrap.New(ctx, cfg.app)
	if err != nil {
		return err
	}
	defer app.Close()

	srv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           app.Handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	var background sync.WaitGroup
	background.Add(1)
	go func() {
		defer background.Done()
		if err := app.Worker.Run(ctx); err != nil {
			slog.Error("worker stopped", "error", err)
		}
	}()
	if cfg.resetInterval > 0 {
		background.Add(1)
		go func() {
			defer background.Done()
			resetEvery(ctx, cfg.resetInterval, app.Reset)
		}()
	}

	serveErr := make(chan error, 1)
	go func() {
		slog.Info("seller listening", "addr", cfg.listenAddr)
		serveErr <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		stop()
		background.Wait()
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	background.Wait()
	return err
}

func resetEvery(ctx context.Context, interval time.Duration, reset func(context.Context) error) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := reset(ctx); err != nil {
				slog.Error("scheduled demo reset failed", "error", err)
				continue
			}
			slog.Info("scheduled demo reset done")
		}
	}
}

// healthcheck lets the distroless image (no curl) report its own health.
func healthcheck() int {
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	client := http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
