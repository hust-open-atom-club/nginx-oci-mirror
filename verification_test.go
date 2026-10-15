package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func TestLocalScanDeduplicatesAndRechecksEveryRun(t *testing.T) {
	fixture := makeScanFixture(t, 2, 6, 4, 8, 4096, true)
	var previous map[string]map[string]v1.Descriptor
	for _, workers := range []int{1, 2, 4} {
		scan := &localScan{}
		var progress bytes.Buffer
		inventory, err := scan.manifests(t.Context(), fixture.dir, workers, &progress)
		if err != nil {
			t.Fatal(err)
		}
		if scan.checked.Load() != fixture.blobChecks || scan.bytes.Load() != fixture.blobBytes {
			t.Fatalf("workers=%d: checks=%d bytes=%d, want %d/%d", workers, scan.checked.Load(), scan.bytes.Load(), fixture.blobChecks, fixture.blobBytes)
		}
		if previous != nil && !reflect.DeepEqual(previous, inventory) {
			t.Fatal("worker count changed inventory")
		}
		previous = inventory
		if !strings.Contains(progress.String(), "Scanning local images") || !strings.Contains(progress.String(), "Local verification finished") {
			t.Fatalf("missing progress: %s", &progress)
		}
	}
	// Same-length corruption with the original mtime must still be detected on
	// the next scan. Other repositories' identical config bytes stay valid.
	configDigest, _, _ := v1.SHA256(strings.NewReader(`{}`))
	path := filepath.Join(fixture.dir, "v2", "fixture", "repo0", "blobs", configDigest.String())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`[]`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	scan := &localScan{}
	inventory, err := scan.manifests(t.Context(), fixture.dir, 4, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(inventory["fixture/repo0"]) != 0 || len(inventory["fixture/repo1"]) != fixture.references/2 {
		t.Fatal("corruption was missed or propagated between repositories")
	}
	if scan.checked.Load() != fixture.blobChecks || scan.bytes.Load() != fixture.blobBytes {
		t.Fatal("shared corrupt blob was reread or full byte verification skipped")
	}
}

func TestLocalVerificationPoolBoundAndCancellation(t *testing.T) {
	for _, workers := range []int{1, 2, 4, maxParallelVerify} {
		t.Run(fmt.Sprint(workers), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			jobs := make([]*localBlob, max(20, workers*2))
			for i := range jobs {
				jobs[i] = &localBlob{}
			}
			var active, peak, started atomic.Int64
			occupied := make(chan struct{}, len(jobs))
			done := make(chan error, 1)
			go func() {
				done <- runLocalBlobChecks(ctx, jobs, workers, func(ctx context.Context, _ *localBlob) error {
					current := active.Add(1)
					defer active.Add(-1)
					for old := peak.Load(); current > old; old = peak.Load() {
						if peak.CompareAndSwap(old, current) {
							break
						}
					}
					started.Add(1)
					occupied <- struct{}{}
					<-ctx.Done()
					return ctx.Err()
				})
			}()
			for range workers {
				select {
				case <-occupied:
				case <-time.After(5 * time.Second):
					t.Fatal("workers did not overlap")
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("workers did not terminate")
			}
			if peak.Load() != int64(workers) || active.Load() != 0 || started.Load() != int64(workers) {
				t.Fatalf("unbounded or leaked workers: peak=%d active=%d started=%d", peak.Load(), active.Load(), started.Load())
			}
		})
	}
}

type cancelRead struct {
	cancel context.CancelFunc
	reads  int
}

func (reader *cancelRead) Read(p []byte) (int, error) {
	reader.reads++
	reader.cancel()
	return copy(p, "bytes"), nil
}

func TestVerificationCancelsBetweenReads(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	source := &cancelRead{cancel: cancel}
	var bytes atomic.Int64
	_, err := io.Copy(io.Discard, &verificationReader{ctx: ctx, reader: source, bytes: &bytes})
	if !errors.Is(err, context.Canceled) || source.reads != 1 || bytes.Load() != 5 {
		t.Fatalf("read cancellation: err=%v reads=%d bytes=%d", err, source.reads, bytes.Load())
	}
}

func TestCanceledNginxScanPreservesConfig(t *testing.T) {
	fixture := makeScanFixture(t, 1, 1, 1, 2, 4096, false)
	config := filepath.Join(fixture.dir, "nginx.conf")
	original := []byte(nginxHeader + "# original\n")
	if err := os.WriteFile(config, original, 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := nginxConfig(ctx, fixture.dir, config, 2, io.Discard); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan: %v", err)
	}
	got, err := os.ReadFile(config)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("canceled scan overwrote config: %s, %v", got, err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	if err := atomicFileContext(ctx, config, func(w io.Writer) error {
		_, err := w.Write([]byte("replacement"))
		cancel()
		return err
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled config write: %v", err)
	}
	got, err = os.ReadFile(config)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("canceled write overwrote config")
	}
}

func TestParallelVerifyArguments(t *testing.T) {
	for _, value := range []string{"0", "-1", "33", "invalid"} {
		if err := run(t.Context(), []string{"--parallel-verify", value, "localhost:1/image", t.TempDir()}); err == nil {
			t.Fatalf("accepted --parallel-verify %s", value)
		}
	}
}

func TestParallelScanConfigStable(t *testing.T) {
	fixture := makeScanFixture(t, 2, 4, 3, 3, 4096, true)
	config := filepath.Join(fixture.dir, "nginx.conf")
	var previous []byte
	for _, workers := range []int{1, 2, 4} {
		if _, err := nginxConfig(t.Context(), fixture.dir, config, workers, io.Discard); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(config)
		if err != nil {
			t.Fatal(err)
		}
		if previous != nil && !bytes.Equal(previous, data) {
			t.Fatal("worker count changed generated config")
		}
		previous = data
	}
}

type scanProgressWriter struct {
	bytes.Buffer
	updates chan struct{}
}

func (w *scanProgressWriter) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "Local verification:") {
		select {
		case w.updates <- struct{}{}:
		default:
		}
	}
	return w.Buffer.Write(p)
}

func TestLocalScanPeriodicProgress(t *testing.T) {
	scan := &localScan{}
	scan.discovered.Store(10)
	scan.checked.Store(3)
	scan.bytes.Store(5 << 20)
	output := &scanProgressWriter{updates: make(chan struct{}, 1)}
	stop := scan.progress(output, 2)
	select {
	case <-output.updates:
	case <-time.After(10 * time.Second):
		stop()
		t.Fatal("no periodic scan progress")
	}
	stop()
	if !strings.Contains(output.String(), "3/10 discovered blobs, 5.0 MiB read") {
		t.Fatalf("inaccurate progress: %s", output.String())
	}
}

func TestLocalScanCancellationDuringHash(t *testing.T) {
	fixture := makeScanFixture(t, 1, 1, 1, 8, 8<<20, false)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	scan := &localScan{}
	done := make(chan error, 1)
	go func() {
		inventory, err := scan.manifests(ctx, fixture.dir, 1, io.Discard)
		if inventory != nil {
			done <- errors.New("canceled scan returned an inventory")
			return
		}
		done <- err
	}()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for scan.bytes.Load() == 0 {
		select {
		case err := <-done:
			t.Fatalf("scan finished before cancellation: %v", err)
		case <-deadline.C:
			t.Fatal("scan did not start reading blobs")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("active scan cancellation: %v", err)
		}
	case <-deadline.C:
		t.Fatal("active scan did not terminate")
	}
}
