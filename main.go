package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	configPath := flag.String("config", defaultConfigPath(), "config file path")
	listen := flag.String("listen", "", "listen address, overrides config")
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if len(cfg.APIKeys) == 0 {
		return fmt.Errorf("no upstream API keys: set apiKeys in %s or export %s=<key>", *configPath, envAPIKeys)
	}

	gw := NewGateway(cfg)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	gw.StartUsageLoop(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	log.Printf("glms listening on http://%s (keys: %d, models: %s)", cfg.Listen, len(cfg.APIKeys), strings.Join(cfg.ModelNames(), ", "))

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	log.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
