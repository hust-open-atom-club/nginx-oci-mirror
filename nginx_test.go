package main

import (
	"io"
	"path/filepath"
	"testing"
)

func TestNginxRootValidation(t *testing.T) {
	for _, dir := range []string{"bad\npath", "bad\rpath", "bad$path"} {
		if _, err := nginxConfig(t.Context(), filepath.Join(t.TempDir(), dir), filepath.Join(t.TempDir(), "oci.conf"), defaultParallelVerify, io.Discard); err == nil {
			t.Fatalf("accepted invalid nginx root %q", dir)
		}
	}
}
