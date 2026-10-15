package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var totalSizeLine = regexp.MustCompile(`^Total size is ([0-9]+(?:\.[0-9]+)?[KMGTP]?)$`)

func TestFormatSize(t *testing.T) {
	for _, check := range []struct {
		bytes int64
		want  string
	}{
		{0, "0"}, {1, "1"}, {1023, "1023"},
		{1024, "1K"}, {1025, "1K"}, {1536, "1.5K"}, {1264, "1.23K"}, {1265, "1.24K"},
		{1<<20 - 10, "1023.99K"}, {1<<20 - 1, "1M"}, {1 << 20, "1M"},
		{1<<30 - 1, "1G"}, {1 << 30, "1G"}, {3 << 29, "1.5G"},
		{1<<40 - 1, "1T"}, {1 << 40, "1T"},
		{1<<50 - 1, "1P"}, {1 << 50, "1P"}, {math.MaxInt64, "8192P"},
	} {
		t.Run(fmt.Sprint(check.bytes), func(t *testing.T) {
			got := formatSize(check.bytes)
			if got != check.want || !totalSizeLine.MatchString("Total size is "+got) {
				t.Fatalf("formatSize(%d) = %q, want %q matching output format", check.bytes, got, check.want)
			}
		})
	}
}

func writeSizeFixture(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLocalMirrorSize(t *testing.T) {
	for _, empty := range []string{"missing destination", "missing v2", "empty v2"} {
		t.Run(empty, func(t *testing.T) {
			dir := t.TempDir()
			if empty == "missing destination" {
				dir = filepath.Join(dir, "new")
			} else if empty == "empty v2" {
				if err := os.Mkdir(filepath.Join(dir, "v2"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if got, err := localMirrorSize(t.Context(), dir); err != nil || got != 0 {
				t.Fatalf("empty size = %d, %v", got, err)
			}
		})
	}
	t.Run("retained files counted once per path", func(t *testing.T) {
		dir := t.TempDir()
		var want int64
		for _, name := range []string{
			"v2/old/image/blobs/shared", "v2/old/image/manifests/digest",
			"v2/old/image/manifests/tag1", "v2/old/image/manifests/tag2",
			"v2/old/image/manifests/.metadata.json", "v2/new/image/blobs/shared",
			"v2/new/image/blobs/unreferenced", "v2/new/image/blobs/corrupt",
		} {
			writeSizeFixture(t, dir, name, "12345")
			want += 5
		}
		for _, name := range []string{"nginx.conf", "images.txt", "v2/old/image/blobs/.download-abandoned", "v2/.download-directory/file"} {
			writeSizeFixture(t, dir, name, "not mirror data")
		}
		outside := writeSizeFixture(t, t.TempDir(), "file", "external data")
		for _, link := range []struct{ target, name string }{
			{outside, "file-link"}, {filepath.Dir(outside), "directory-link"}, {outside + "-missing", "broken-link"},
		} {
			if err := os.Symlink(link.target, filepath.Join(dir, "v2", link.name)); err != nil {
				t.Fatal(err)
			}
		}
		if got, err := localMirrorSize(t.Context(), dir); err != nil || got != want {
			t.Fatalf("retained size = %d, %v; want %d", got, err, want)
		}
	})
	t.Run("symlink root is not followed", func(t *testing.T) {
		dir := t.TempDir()
		outside := writeSizeFixture(t, t.TempDir(), "file", "external data")
		if err := os.Symlink(filepath.Dir(outside), filepath.Join(dir, "v2")); err != nil {
			t.Fatal(err)
		}
		if got, err := localMirrorSize(t.Context(), dir); err != nil || got != 0 {
			t.Fatalf("symlink root size = %d, %v", got, err)
		}
	})
	t.Run("invalid root is an error", func(t *testing.T) {
		dir := t.TempDir()
		writeSizeFixture(t, dir, "v2", "not a directory")
		if got, err := localMirrorSize(t.Context(), dir); err == nil || got != 0 {
			t.Fatalf("invalid root size = %d, %v", got, err)
		}
	})
}

func TestLocalMirrorSizeReadErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can bypass filesystem permissions")
	}
	for _, check := range []struct {
		name string
		mode os.FileMode
	}{
		{"read directory", 0100},
		{"stat file", 0400}, // Listing works, but stat of a child needs execute permission.
	} {
		t.Run(check.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSizeFixture(t, dir, "v2/a-known", "must not return this partial sum")
			blocked := filepath.Dir(writeSizeFixture(t, dir, "v2/z-blocked/data", "data"))
			if err := os.Chmod(blocked, check.mode); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(blocked, 0755) })
			if got, err := localMirrorSize(t.Context(), dir); !errors.Is(err, os.ErrPermission) || got != 0 {
				t.Fatalf("incomplete size = %d, %v; want no partial total and permission error", got, err)
			}
		})
	}
}

type sizeCancelContext struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (ctx *sizeCancelContext) Err() error {
	ctx.remaining--
	if ctx.remaining == 0 {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestLocalMirrorSizeCancellation(t *testing.T) {
	dir := t.TempDir()
	writeSizeFixture(t, dir, "v2/a", strings.Repeat("x", 10))
	writeSizeFixture(t, dir, "v2/b", strings.Repeat("x", 20))
	for _, checks := range []int{1, 3, 4} { // Before, during, and just after walking.
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		if got, err := localMirrorSize(&sizeCancelContext{ctx, cancel, checks}, dir); !errors.Is(err, context.Canceled) || got != 0 {
			t.Fatalf("canceled after %d checks: %d, %v", checks, got, err)
		}
	}
}
