package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func atomicFile(path string, write func(io.Writer) error) error {
	return atomicFileContext(context.Background(), path, write)
}

func atomicFileContext(ctx context.Context, path string, write func(io.Writer) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".download-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := write(f); err != nil {
		return err
	}
	if err := f.Chmod(0644); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func copyContent(dst io.Writer, src io.Reader, desc v1.Descriptor) error {
	h, err := v1.Hasher(desc.Digest.Algorithm)
	if err != nil {
		return err
	}
	n, err := io.Copy(io.MultiWriter(dst, h), src)
	if err != nil {
		return err
	}
	if n != desc.Size || hex.EncodeToString(h.Sum(nil)) != desc.Digest.Hex {
		return fmt.Errorf("content does not match digest/size of %s", desc.Digest)
	}
	return nil
}

func validContent(path string, desc v1.Descriptor) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	return copyContent(io.Discard, f, desc) == nil
}

func storeContent(path string, src io.Reader, desc v1.Descriptor) error {
	return atomicFile(path, func(w io.Writer) error {
		return copyContent(w, src, desc)
	})
}
