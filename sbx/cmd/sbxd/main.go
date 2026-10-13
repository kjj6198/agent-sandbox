//go:build linux

// sbxd runs Firecracker sandboxes for AI agents and serves the sbx API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/daemon"
	"github.com/kjj6198/agent-sandbox-eval/sbx/internal/proto"
)

func main() {
	configPath := flag.String("config", "/etc/sbx/sbxd.json", "config file")
	version := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *version {
		fmt.Println(proto.Version)
		return
	}
	if err := run(*configPath); err != nil {
		slog.Error("sbxd", "err", err)
		os.Exit(1)
	}
}

func run(configPath string) error {
	cfg, err := daemon.LoadConfig(configPath)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	d, err := daemon.New(cfg)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := d.Start(ctx); err != nil {
		return err
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: d.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("sbxd listening", "addr", cfg.Listen, "version", proto.Version)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// Sandboxes keep running; the next sbxd adopts them.
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil && !errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return nil
}
