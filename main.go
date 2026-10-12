package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/google/go-containerregistry/pkg/name"
)

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("tool", flag.ContinueOnError)
	nginx := flags.Bool("nginx", false, "generate nginx.conf in the output directory (Linux, port 8080)")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: tool [--nginx] IMAGE DIRECTORY")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 2 {
		return errors.New("usage: tool [--nginx] IMAGE DIRECTORY")
	}
	ref, err := name.ParseReference(flags.Arg(0))
	if err != nil {
		return err
	}
	if err := download(ctx, ref, flags.Arg(1)); err != nil {
		return err
	}
	if *nginx {
		if err := nginxConfig(flags.Arg(1)); err != nil {
			return err
		}
	}
	fmt.Printf("Saved %s to %s\n", ref, flags.Arg(1))
	return nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
