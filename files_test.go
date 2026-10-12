package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestStoreContent(t *testing.T) {
	data := []byte("verified content")
	h, size, err := v1.SHA256(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	desc := v1.Descriptor{Digest: h, Size: size}
	path := filepath.Join(t.TempDir(), "blobs", "content")
	if err := storeContent(path, bytes.NewReader(data), desc); err != nil {
		t.Fatal(err)
	}
	for _, bad := range [][]byte{[]byte("corrupt content!"), data[:3]} {
		if err := storeContent(path, bytes.NewReader(bad), desc); err == nil {
			t.Fatal("accepted corrupt or truncated data")
		}
		if !validContent(path, desc) {
			t.Fatal("failed download replaced the valid file")
		}
	}
	if err := os.WriteFile(path, []byte("corrupt"), 0644); err != nil {
		t.Fatal(err)
	}
	if validContent(path, desc) {
		t.Fatal("accepted corrupt cache")
	}
	files, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".download-*"))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files left behind: %v, %v", files, err)
	}
}

func TestAtomicFileFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "partial")
	err := atomicFile(path, func(w io.Writer) error {
		if _, err := io.WriteString(w, "partial"); err != nil {
			return err
		}
		return errors.New("interrupted download")
	})
	if err == nil {
		t.Fatal("ignored write error")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("published partial file: %v", err)
	}
}
