package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestNginxSizeUsesFinalGraphs(t *testing.T) {
	fixture := makeScanFixture(t, 2, 3, 4, 2, 4096, true)
	want := storedFixtureBytes(t, fixture.dir, true)
	// This valid blob is never discovered by verification.
	orphan := []byte("unreferenced layer")
	orphanDesc := scanTestDescriptor(orphan, types.OCILayer)
	writeScanTestContent(t, fixture.dir, "fixture/repo0", "blobs", orphanDesc.Digest.String(), orphan)
	// This config is read and verified, but only an incomplete manifest uses it.
	configBytes := []byte(`{"source":"incomplete image"}`)
	config := scanTestDescriptor(configBytes, types.OCIConfigJSON)
	missing := scanTestDescriptor([]byte("missing layer"), types.OCILayer)
	writeScanTestContent(t, fixture.dir, "fixture/repo0", "blobs", config.Digest.String(), configBytes)
	writeScanTestManifest(t, fixture.dir, "fixture/repo0", "incomplete", config, missing)
	scan := &localScan{}
	if _, err := scan.manifests(t.Context(), fixture.dir, 2, io.Discard); err != nil {
		t.Fatal(err)
	}
	if scan.totalSize != want || scan.bytes.Load() != fixture.blobBytes+config.Size {
		t.Fatalf("graph size=%d, bytes read=%d; want %d and %d", scan.totalSize, scan.bytes.Load(), want, fixture.blobBytes+config.Size)
	}
	path := filepath.Join(fixture.dir, "nginx.conf")
	total, err := nginxConfig(t.Context(), fixture.dir, path, 2, io.Discard)
	if err != nil || total != want {
		t.Fatalf("nginx total=%d, %v; want %d", total, err, want)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "/manifests/incomplete {") {
		t.Fatalf("incomplete image included in config: %v", err)
	}
}

// The scan's final progress line is emitted only after its inventory and size
// are complete. Mutate the fixture at that exact boundary to prove that config
// generation consumes the in-memory snapshot without another walk/stat/read.
type afterScanWriter struct {
	callback func() error
	err      error
	called   bool
}

func (writer *afterScanWriter) Write(data []byte) (int, error) {
	if bytes.HasPrefix(data, []byte("Local verification finished:")) {
		writer.called = true
		writer.err = writer.callback()
	}
	return len(data), nil
}

func TestNginxSizeNeedsNoIOAfterScan(t *testing.T) {
	fixture := makeScanFixture(t, 1, 2, 3, 2, 4096, true)
	want := storedFixtureBytes(t, fixture.dir, true)
	progress := &afterScanWriter{callback: func() error {
		return os.Rename(filepath.Join(fixture.dir, "v2"), filepath.Join(fixture.dir, "moved-after-scan"))
	}}
	total, err := nginxConfig(t.Context(), fixture.dir, filepath.Join(t.TempDir(), "nginx.conf"), 2, progress)
	if err != nil || total != want || !progress.called || progress.err != nil {
		t.Fatalf("post-scan IO or lost snapshot: total=%d want=%d err=%v callback=%v/%v", total, want, err, progress.called, progress.err)
	}
}

func TestNginxSizeOnlyOnPublishedConfig(t *testing.T) {
	for _, kind := range []string{"write failure", "cancellation"} {
		t.Run(kind, func(t *testing.T) {
			fixture := makeScanFixture(t, 1, 1, 1, 1, 1024, false)
			path := filepath.Join(t.TempDir(), "nginx.conf")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			progress := &afterScanWriter{callback: func() error {
				if kind == "cancellation" {
					cancel()
					return nil
				}
				return os.Mkdir(path, 0755) // Atomic rename of the config must fail.
			}}
			total, err := nginxConfig(ctx, fixture.dir, path, 2, progress)
			if err == nil || total != 0 || !progress.called || progress.err != nil {
				t.Fatalf("failed publication returned size: %d, %v, callback=%v/%v", total, err, progress.called, progress.err)
			}
			if kind == "cancellation" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}

func TestNginxSizeRechecksChangedFiles(t *testing.T) {
	fixture := makeScanFixture(t, 1, 1, 2, 1, 4096, false)
	want := storedFixtureBytes(t, fixture.dir, true)
	config := filepath.Join(fixture.dir, "nginx.conf")
	desc := scanTestDescriptor([]byte("{}"), types.OCIConfigJSON)
	path := filepath.Join(fixture.dir, "v2", "fixture/repo0", "blobs", desc.Digest.String())
	for _, change := range []struct {
		contents string
		size     int64
	}{{"{}", want}, {"[]", 0}, {"{}", want}} {
		if err := os.WriteFile(path, []byte(change.contents), 0644); err != nil {
			t.Fatal(err)
		}
		total, err := nginxConfig(t.Context(), fixture.dir, config, 2, io.Discard)
		if err != nil || total != change.size {
			t.Fatalf("after writing %q: size=%d, %v; want %d", change.contents, total, err, change.size)
		}
	}
}
