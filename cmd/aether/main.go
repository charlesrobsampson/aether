// Command aether is the Aether messaging router.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/charlesrobsampson/aether/internal/app"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "aether:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 {
		switch args[0] {
		case "serve", "token":
			command = args[0]
			args = args[1:]
		}
	}

	switch command {
	case "token":
		return app.RunToken(args)
	default:
		return runServe(args)
	}
}

func runServe(args []string) error {
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(flag.CommandLine.Output())

	configDir := flags.String("config-dir", "", "config root (default AETHER_CONFIG_DIR or ~/.config/aether)")
	addr := flags.String("addr", "", "HTTP listen address (default server.port)")
	if err := flags.Parse(args); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return app.Run(ctx, app.Options{
		ConfigDir: *configDir,
		Addr:      *addr,
	})
}
