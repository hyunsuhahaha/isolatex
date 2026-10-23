package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"isolatex/internal/config"
	"isolatex/internal/gateway"
	"isolatex/internal/supervisor"
)

func main() {
	if err := run(); err != nil {
		slog.Error("runtime_failed", "error", err)
		os.Exit(1)
	}
}

func run() error {
	filename := flag.String("config", "isolatex.yaml", "YAML configuration path (commands resolve from current directory)")
	check := flag.Bool("check", false, "validate configuration without starting servers or workers")
	debug := flag.Bool("debug", false, "include request outcome logs")
	flag.Parse()
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	c, err := config.Load(*filename)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if *check {
		fmt.Println("configuration valid")
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	publicLn, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer publicLn.Close()
	adminLn, err := net.Listen("tcp", c.AdminListen)
	if err != nil {
		return err
	}
	defer adminLn.Close()
	s := supervisor.New(c, logger)
	// Worker lifetime outlives the drain grace; cancel only after HTTP shutdown.
	if err = s.Start(context.Background()); err != nil {
		return err
	}
	defer s.Stop()
	g := gateway.New(c, s.Pools, logger)
	defer g.Close()
	server := func(handler http.Handler) *http.Server {
		return &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10}
	}
	public, admin := server(g), server(g.Admin())
	admin.WriteTimeout = 5 * time.Second
	errors := make(chan error, 2)
	go func() { errors <- public.Serve(publicLn) }()
	go func() { errors <- admin.Serve(adminLn) }()
	logger.Info("runtime_started", "listen", c.Listen, "admin_listen", c.AdminListen)
	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errors:
	}
	g.Drain()
	shutdown, cancel := context.WithTimeout(context.Background(), c.ShutdownGrace)
	defer cancel()
	if err := public.Shutdown(shutdown); err != nil {
		logger.Warn("drain_deadline", "error", err)
		_ = public.Close()
	}
	_ = admin.Close()
	if serveErr != nil && serveErr != http.ErrServerClosed {
		return serveErr
	}
	return nil
}
