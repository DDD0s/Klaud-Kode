// Command kloud-kode is the single Kloud Kode binary. This rewrite slice
// implements the api subcommand; body and helper subcommands land in later
// slices behind the same entrypoint.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/api"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/childenv"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kloud-kode <api|body|bridge|hook|shim>")
	fmt.Fprintln(os.Stderr, "this Go rewrite slice implements: api")
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "kloud-kode:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "api"
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "api":
		cfg, err := config.Load(childenv.FromList(os.Environ()))
		if err != nil {
			return err
		}
		server, err := api.NewServer(cfg)
		if err != nil {
			return err
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return server.ListenAndServe(ctx)
	case "body", "bridge", "hook", "shim":
		return fmt.Errorf("the %q subcommand is not yet implemented in the Go rewrite", command)
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", command)
	}
}
