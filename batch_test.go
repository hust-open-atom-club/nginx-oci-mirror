package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestBatchDownload(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("registry digest filenames require a Linux filesystem")
	}
	for _, check := range []struct {
		name                            string
		missing, invalid, defaultConfig bool
		completed                       int
	}{
		{"success custom config", false, false, false, 3},
		{"success default config", false, false, true, 3},
		{"stop on download error", true, false, false, 1},
		{"validate before downloading", false, true, false, 0},
	} {
		t.Run(check.name, func(t *testing.T) {
			var mu sync.Mutex
			var requested []string
			upstream := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/manifests/") {
					mu.Lock()
					requested = append(requested, r.URL.Path)
					mu.Unlock()
					if check.missing && strings.HasSuffix(r.URL.Path, "/two") {
						w.WriteHeader(http.StatusNotFound)
						return
					}
				}
				upstream.ServeHTTP(w, r)
			}))
			defer server.Close()
			var images []string
			for _, tag := range []string{"one", "two", "three"} {
				ref, err := name.NewTag(strings.TrimPrefix(server.URL, "http://") + "/test/image:" + tag)
				if err != nil {
					t.Fatal(err)
				}
				if err := remote.Write(ref, empty.Image, remote.WithContext(t.Context())); err != nil {
					t.Fatal(err)
				}
				images = append(images, ref.String())
			}
			if check.invalid {
				images[1] = "invalid image"
			}
			input := filepath.Join(t.TempDir(), "images.txt")
			if err := os.WriteFile(input, []byte(strings.Join(images, "\n")), 0644); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			configPath := filepath.Join(t.TempDir(), "registry.conf")
			args := []string{"--image-file", input}
			if check.defaultConfig {
				configPath = filepath.Join(dir, "nginx.conf")
				args = append(args, "--nginx")
			} else {
				args = append(args, "--nginx-config", configPath)
			}
			args = append(args, dir)
			previous := nginxHeader + "# previous export\n"
			if err := os.WriteFile(configPath, []byte(previous), 0644); err != nil {
				t.Fatal(err)
			}
			err := run(t.Context(), args)
			if (err != nil) != (check.missing || check.invalid) {
				t.Fatalf("batch result: %v", err)
			}
			wantRequests := []string{"/v2/test/image/manifests/one", "/v2/test/image/manifests/two", "/v2/test/image/manifests/three"}
			if check.missing {
				wantRequests = wantRequests[:2]
				if !strings.Contains(err.Error(), images[1]) {
					t.Fatalf("error does not identify failed image: %v", err)
				}
			} else if check.invalid {
				wantRequests = nil
			}
			mu.Lock()
			actualRequests := slices.Clone(requested)
			mu.Unlock()
			if !slices.Equal(actualRequests, wantRequests) {
				t.Fatalf("download order: got %v, want %v", actualRequests, wantRequests)
			}
			config, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			for i, tag := range []string{"one", "two", "three"} {
				path := filepath.Join(dir, "v2", "test", "image", "manifests", tag)
				if _, err := os.Stat(path); (err == nil) != (i < check.completed) {
					t.Fatalf("unexpected completion of %s: %v", tag, err)
				}
				if check.completed == 3 && !strings.Contains(string(config), "/manifests/"+tag+" {") {
					t.Fatalf("config is missing image %s", tag)
				}
			}
			if check.completed != 3 && string(config) != previous {
				t.Fatal("failed batch replaced the previous nginx config")
			}
		})
	}
}
