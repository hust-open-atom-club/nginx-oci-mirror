package main

import (
	"errors"
	"flag"
	"testing"
)

func TestRunArguments(t *testing.T) {
	if err := run(t.Context(), []string{"--help"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help: %v", err)
	}
	for _, args := range [][]string{
		nil,
		{"alpine:latest"},
		{"--unknown", "alpine:latest", t.TempDir()},
		{"example.com/../alpine:latest", t.TempDir()},
		{"--image-file", "images.txt"},
		{"--image-file", "images.txt", "alpine:latest", t.TempDir()},
	} {
		if err := run(t.Context(), args); err == nil {
			t.Errorf("accepted invalid arguments: %v", args)
		}
	}
}
