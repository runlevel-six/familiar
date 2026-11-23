// SPDX-License-Identifier: Apache-2.0

// model-gate is the only component in familiar that holds Anthropic API
// keys. Agents send unauthenticated Messages API requests to their role's
// listener; model-gate checks them, injects the role's key, forwards them to
// api.anthropic.com, meters the usage against USD budgets, and writes one
// audit line per request. Design: docs/DESIGN.md §4.1 (D3, D4, T1, T6).
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
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "/etc/model-gate/config.json", "path to the JSON config")
	checkOnly := flag.Bool("check", false, "validate the config (and key files) and exit")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(*configPath, *checkOnly, log); err != nil {
		log.Error("fatal", "err", err.Error())
		os.Exit(1)
	}
}

func run(configPath string, checkOnly bool, log *slog.Logger) error {
	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	if err := cfg.loadKeys(); err != nil {
		return err
	}
	if checkOnly {
		fmt.Println("config ok")
		return nil
	}
	budgets, err := newBudgets(cfg.StateFile)
	if err != nil {
		return fmt.Errorf("budget state: %w", err)
	}
	g := &Gate{
		cfg: cfg, budgets: budgets, metrics: newMetrics(), log: log, now: time.Now,
		client: &http.Client{
			// No overall timeout: streams can legitimately run many minutes.
			// The caller's context cancels abandoned requests.
			Transport: &http.Transport{
				Proxy:                 nil, // never honour HTTP(S)_PROXY env vars
				DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				TLSHandshakeTimeout:   10 * time.Second,
				ResponseHeaderTimeout: 10 * time.Minute,
				MaxIdleConnsPerHost:   8,
				ForceAttemptHTTP2:     true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	var servers []*http.Server
	errc := make(chan error, len(cfg.Roles)+1)
	for i := range cfg.Roles {
		r := &cfg.Roles[i]
		s := &http.Server{Addr: r.Listen, Handler: g.handler(r), ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, s)
		log.Info("listening", "role", r.Name, "addr", r.Listen, "models", r.Models,
			"hourly_usd", r.HourlyUSD, "daily_usd", r.DailyUSD, "max_tokens", r.MaxTokensPerRequest,
			"debug_capture", r.DebugCaptureDir != "")
		go func() { errc <- s.ListenAndServe() }()
	}
	if cfg.MetricsListen != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
		mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4")
			g.metrics.Write(w, cfg, budgets, func() RoleSpendFor {
				return func(role string) RoleSpend { return budgets.Snapshot(role, time.Now()) }
			})
		})
		s := &http.Server{Addr: cfg.MetricsListen, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, s)
		go func() { errc <- s.ListenAndServe() }()
	}

	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		_ = s.Shutdown(shutdown)
	}
	return nil
}
