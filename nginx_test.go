package main

import (
	"path/filepath"
	"testing"
)

func TestNginxRootValidation(t *testing.T) {
	for _, dir := range []string{"bad\npath", "bad\rpath", "bad$path"} {
		if err := nginxConfig(t.Context(), filepath.Join(t.TempDir(), dir), filepath.Join(t.TempDir(), "oci.conf"), defaultParallelVerify); err == nil {
			t.Fatalf("accepted invalid nginx root %q", dir)
		}
	}
}
