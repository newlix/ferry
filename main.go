package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if err := dispatch(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "glms:", err)
		os.Exit(1)
	}
}

func dispatch(args []string) error {
	// Bare glms and glms with run flags (-v, -listen, -config) both run the
	// gateway; only words select a verb.
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return cmdRun(args)
	}
	switch args[0] {
	case "run":
		return cmdRun(args[1:])
	case "status":
		cfg, err := LoadConfig(defaultConfigPath(), false)
		if err != nil {
			return err
		}
		return cmdStatus(cfg, NewKeyStore(configDir()), os.Stdout)
	case "install":
		return cmdInstall(NewKeyStore(configDir()))
	case "uninstall":
		return cmdUninstall()
	case "key":
		if len(args) >= 2 {
			switch args[1] {
			case "add":
				cfg, err := LoadConfig(defaultConfigPath(), false)
				if err != nil {
					return err
				}
				return cmdKeyAdd(cfg, NewKeyStore(configDir()), os.Stdin, os.Stdout)
			case "remove":
				return cmdKeyRemove(NewKeyStore(configDir()), args[2:])
			case "list":
				return cmdKeyList(NewKeyStore(configDir()))
			}
		}
		return errors.New("usage: glms key add | glms key remove NAME | glms key list")
	default:
		printUsage(os.Stdout)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func serve(cfg Config, log Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gw := NewGateway(cfg, log)
	gw.StartUsageLoop(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           gw,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.ListenAndServe() }()

	log.Quiet.Printf("glms listening on http://%s (%d key(s), models: %s)", cfg.Listen, len(cfg.APIKeys), strings.Join(cfg.ModelNames(), ", "))

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}

	log.Quiet.Printf("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	return nil
}
