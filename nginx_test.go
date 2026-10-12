package main

import (
	"path/filepath"
	"testing"
)

func TestNginxRootValidation(t *testing.T) {
	for _, dir := range []string{"bad\npath", "bad\rpath", "bad$path"} {
		if err := nginxConfig(filepath.Join(t.TempDir(), dir)); err == nil {
			t.Fatalf("accepted invalid nginx root %q", dir)
		}
	}
}
