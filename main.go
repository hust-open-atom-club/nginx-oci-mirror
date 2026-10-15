package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/google/go-containerregistry/pkg/name"
)

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("tool", flag.ContinueOnError)
	nginx := flags.Bool("nginx", false, "generate a server include fragment at DIRECTORY/nginx.conf")
	nginxPath := flags.String("nginx-config", "", "generate a server include fragment at this path")
	parallel := flags.Int("parallel-downloads", 1, "maximum simultaneous blob file downloads (1-128)")
	imageFile := flags.String("image-file", "", "download images listed one per line in this file")
	const usage = "tool [OPTIONS] IMAGE DIRECTORY\n       tool [OPTIONS] --image-file PATH DIRECTORY"
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage:", usage)
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *parallel < 1 || *parallel > maxParallelDownloads {
		return fmt.Errorf("parallel downloads must be between 1 and %d", maxParallelDownloads)
	}
	var refs []name.Reference
	var dir string
	if *imageFile != "" {
		if flags.NArg() != 1 {
			return fmt.Errorf("usage: %s", usage)
		}
		var err error
		refs, err = readImageFile(*imageFile)
		if err != nil {
			return err
		}
		dir = flags.Arg(0)
	} else {
		if flags.NArg() != 2 {
			return fmt.Errorf("usage: %s", usage)
		}
		ref, err := name.ParseReference(flags.Arg(0))
		if err != nil {
			return err
		}
		refs = []name.Reference{ref}
		dir = flags.Arg(1)
	}
	var failures []error
	for _, ref := range refs {
		if err := ctx.Err(); err != nil {
			failures = append(failures, err)
			break
		}
		if err := downloadWithConcurrency(ctx, ref, dir, *parallel); err != nil {
			err = fmt.Errorf("download %s: %w", ref, err)
			fmt.Fprintln(os.Stderr, err)
			failures = append(failures, err)
			continue
		}
		fmt.Printf("Saved %s to %s\n", ref, dir)
	}
	if *nginx || *nginxPath != "" {
		if *nginxPath == "" {
			*nginxPath = filepath.Join(dir, "nginx.conf")
		}
		if err := nginxConfig(dir, *nginxPath); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
