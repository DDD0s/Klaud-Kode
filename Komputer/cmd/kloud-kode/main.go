// Command kloud-kode is the single Kloud Kode binary. This rewrite slice
// implements the api and body subcommands; helper subcommands land in
// later slices behind the same entrypoint.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/DDD0s/Kloud-Kode/Komputer/internal/api"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/backend"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/body"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/childenv"
	"github.com/DDD0s/Kloud-Kode/Komputer/internal/config"
)

// Build metadata is injected by the release workflow with -ldflags -X.
var (
	buildVersion = "dev"
	buildCommit  = "unknown"
	buildDate    = "unknown"
)

func printVersion() {
	fmt.Printf("kloud-kode %s (commit %s, built %s)\n", buildVersion, buildCommit, buildDate)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: kloud-kode <api|body|bridge|hook|shim|version>")
	fmt.Fprintln(os.Stderr, "this Go rewrite slice implements: api, body, bridge, hook, shim, version")
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
	case "version", "-version", "--version", "-v":
		printVersion()
		return nil
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
	case "body":
		mode := "http"
		if len(args) > 1 {
			mode = args[1]
		}
		return runBody(mode)
	case "bridge":
		name := "komputer_use"
		if len(args) > 1 {
			name = args[1]
		}
		backend.RunAGYBridge(context.Background(), name, os.Stdin, os.Stdout, os.Getenv)
		return nil
	case "hook":
		home, _ := os.UserHomeDir()
		if v := os.Getenv("USERPROFILE"); v != "" {
			home = v
		}
		os.Exit(backend.RunAGYHook(os.Stdin, os.Stdout, os.Getenv, home))
		return nil
	case "shim":
		cwd, _ := os.Getwd()
		os.Exit(backend.RunAGYShim(context.Background(), args[1:], os.Stdout, os.Stderr, os.Getenv, cwd))
		return nil
	default:
		usage()
		return fmt.Errorf("unknown subcommand %q", command)
	}
}

func runBody(mode string) error {
	cfg, err := body.LoadConfig()
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "", 0)
	logf := func(format string, args ...any) {
		logger.Printf("[%s] %s", time.Now().UTC().Format(time.RFC3339Nano), fmt.Sprintf(format, args...))
	}
	service := body.NewService(cfg, logf)
	defer service.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return service.Run(ctx, mode)
}
