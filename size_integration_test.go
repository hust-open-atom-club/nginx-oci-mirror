package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures below have two-component repository names. Read their actual stored
// bytes independently of the stat-only implementation under test.
func storedFixtureBytes(t *testing.T, dir string) int64 {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "v2", "*", "*", "*", "*"))
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		total += int64(len(data))
	}
	return total
}

func assertTotalSizeOutput(t *testing.T, output string, total int64) {
	t.Helper()
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	want := "Total size is " + formatSize(total)
	if strings.Count(output, "Total size is ") != 1 || lines[len(lines)-1] != want || !totalSizeLine.MatchString(lines[len(lines)-1]) {
		t.Fatalf("want exactly one final line %q; got %q", want, output)
	}
}

func TestRunTotalSize(t *testing.T) {
	for _, config := range []string{"none", "default", "custom"} {
		t.Run(config, func(t *testing.T) {
			r := newResilienceRegistry(t)
			ref := r.tag(t, "size/current", "latest")
			image := resilienceImage(t, 1)
			resiliencePush(t, ref, image)
			alias := ref.Context().Tag("alias")
			resiliencePush(t, alias, image)
			dir := t.TempDir()
			input := writeSizeFixture(t, t.TempDir(), "images.txt", ref.String()+"\n"+alias.String()+"\n")
			args := []string{"--image-file", input}
			switch config {
			case "default":
				args = append(args, "--nginx")
			case "custom":
				args = append(args, "--nginx-config", filepath.Join(t.TempDir(), "registry.conf"))
			}
			args = append(args, dir)
			for range 2 { // Reusing cached files must still report the full retained size.
				var output bytes.Buffer
				if err := runWithOutput(t.Context(), args, &output); err != nil {
					t.Fatal(err)
				}
				assertTotalSizeOutput(t, output.String(), storedFixtureBytes(t, dir))
			}
		})
	}
}

func TestRunTotalSizeAfterFailedSync(t *testing.T) {
	for _, check := range []struct {
		name                  string
		previous, partialSync bool
	}{
		{"partial sync with previous mirror", true, true},
		{"all downloads failed with previous mirror", true, false},
		{"all downloads failed in new mirror", false, false},
	} {
		t.Run(check.name, func(t *testing.T) {
			r := newResilienceRegistry(t)
			dir := filepath.Join(t.TempDir(), "mirror")
			if check.previous {
				old := r.tag(t, "size/old", "stable")
				resiliencePush(t, old, resilienceImage(t, 2))
				if err := download(t.Context(), old, dir); err != nil {
					t.Fatal(err)
				}
			}
			missing := r.tag(t, "size/missing", "latest")
			images := missing.String() + "\n"
			if check.partialSync {
				good := r.tag(t, "size/new", "latest")
				resiliencePush(t, good, resilienceImage(t, 1))
				images += good.String() + "\n"
			}
			input := writeSizeFixture(t, t.TempDir(), "images.txt", images)
			var output bytes.Buffer
			if err := runWithOutput(t.Context(), []string{"--nginx", "--image-file", input, dir}, &output); err == nil {
				t.Fatal("download failure was hidden")
			}
			assertTotalSizeOutput(t, output.String(), storedFixtureBytes(t, dir))
		})
	}
}

func TestRunDoesNotPrintIncompleteTotal(t *testing.T) {
	for _, kind := range []string{"invalid arguments", "invalid repository", "invalid tag", "invalid image list", "canceled before run", "canceled during download", "size read error"} {
		t.Run(kind, func(t *testing.T) {
			r := newResilienceRegistry(t)
			ref := r.tag(t, "size/image", "latest")
			resiliencePush(t, ref, resilienceImage(t, 1))
			dir := t.TempDir()
			args := []string{ref.String(), dir}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch kind {
			case "invalid arguments":
				args = []string{"--parallel-verify", "0", ref.String(), dir}
			case "invalid repository":
				args = []string{"example.com/../alpine:latest", dir}
			case "invalid tag":
				args = []string{"alpine:..", dir}
			case "invalid image list":
				input := writeSizeFixture(t, t.TempDir(), "images.txt", "invalid image\n")
				args = []string{"--image-file", input, dir}
			case "canceled before run":
				cancel()
			case "canceled during download":
				r.intercept(func(w http.ResponseWriter, _ *http.Request, _ int) bool {
					cancel()
					w.WriteHeader(http.StatusServiceUnavailable)
					return true
				})
			case "size read error":
				if os.Geteuid() == 0 {
					t.Skip("root can bypass filesystem permissions")
				}
				blocked := filepath.Dir(writeSizeFixture(t, dir, "v2/z-blocked/data", "must not count partial data"))
				if err := os.Chmod(blocked, 0100); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(blocked, 0755) })
			}
			var output bytes.Buffer
			err := runWithOutput(ctx, args, &output)
			if err == nil || strings.Contains(output.String(), "Total size is ") {
				t.Fatalf("incomplete run printed total or hid error: %q, %v", &output, err)
			}
			if strings.HasPrefix(kind, "canceled") && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
		})
	}
}
