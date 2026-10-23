package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hyunsuhahaha/isolatex/internal/config"
	"github.com/hyunsuhahaha/isolatex/internal/gateway"
	"github.com/hyunsuhahaha/isolatex/internal/logging"
	"github.com/hyunsuhahaha/isolatex/internal/supervisor"
)

// Set by the distribution build; source builds keep explicit development values.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	logs := logging.NewWriter(os.Stderr, 256, 64<<10)
	exitCode := 0
	if err := run(logs); err != nil {
		slog.Error("runtime_failed", "error", err)
		exitCode = 1
	}
	flush, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	_ = logs.Close(flush)
	cancel()
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

func run(output io.Writer) error {
	filename := flag.String("config", "isolatex.yaml", "YAML configuration path (commands resolve from current directory)")
	check := flag.Bool("check", false, "validate configuration without starting servers or workers")
	debug := flag.Bool("debug", false, "include request outcome logs")
	showVersion := flag.Bool("version", false, "print build version and exit without loading configuration")
	flag.Parse()
	if *showVersion {
		fmt.Printf("isolatex %s\ncommit: %s\nbuilt: %s\n", version, commit, buildDate)
		return nil
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: level}))
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
	if logs, ok := output.(interface{ Dropped() uint64 }); ok {
		g.RegisterLogDrops(logs.Dropped)
	}
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
