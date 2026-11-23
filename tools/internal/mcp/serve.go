// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// Serve runs s on addr until SIGTERM/SIGINT.
func Serve(s *Server, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 2 * time.Minute}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	s.Log.Info("listening", "server", s.Name, "version", s.Version, "addr", addr, "tools", len(s.Tools))
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	}
	shut, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return srv.Shutdown(shut)
}

// Env returns an environment variable or a default.
func Env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// NewLogger is the JSON audit logger all servers use (stdout).
func NewLogger() *slog.Logger { return slog.New(slog.NewJSONHandler(os.Stdout, nil)) }
