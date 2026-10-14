package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadImageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.txt")
	data := "  alpine:latest  \r\n\n # comment\r\n busybox \n ghcr.io/owner/image@sha256:" + strings.Repeat("a", 64) + "\r\n"
	if err := os.WriteFile(path, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	refs, err := readImageFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"index.docker.io/library/alpine:latest", "index.docker.io/library/busybox:latest", "ghcr.io/owner/image@sha256:" + strings.Repeat("a", 64)}
	if len(refs) != len(want) {
		t.Fatalf("got %d images, want %d", len(refs), len(want))
	}
	for i, ref := range refs {
		if ref.Name() != want[i] {
			t.Errorf("image %d: got %s, want %s", i, ref.Name(), want[i])
		}
	}
}

func TestReadImageFileErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "images.txt")
	if _, err := readImageFile(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	for _, check := range []struct{ text, message string }{
		{"", "contains no images"},
		{"\n # comment\n", "contains no images"},
		{"alpine\ninvalid image\n", ":2:"},
		{"alpine\nexample.com/../escape:latest\n", ":2:"},
		{strings.Repeat("a", 70<<10), ":1: read image file"},
	} {
		if err := os.WriteFile(path, []byte(check.text), 0644); err != nil {
			t.Fatal(err)
		}
		refs, err := readImageFile(path)
		if err == nil || !strings.Contains(err.Error(), check.message) || refs != nil {
			t.Errorf("input %q: got %v, %v", check.text[:min(len(check.text), 40)], refs, err)
		}
	}
}
