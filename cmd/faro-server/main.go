package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Pouya-Amiri/Faro/internal/buildinfo"
	"github.com/Pouya-Amiri/Faro/internal/server"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "faro-server:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := server.ParseConfig(os.Args[1:], os.Stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if cfg.ShowVersion {
		fmt.Println(buildinfo.EffectiveVersion())
		return nil
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{}))
	service, err := server.New(cfg, buildinfo.EffectiveVersion(), logger)
	if err != nil {
		return err
	}
	if cfg.PrintInvite {
		fmt.Println(service.InviteURL())
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return service.Serve(ctx)
}
