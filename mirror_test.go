package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestMirror(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("registry digest filenames require a Linux filesystem")
	}
	for _, media := range []struct{ image, index types.MediaType }{
		{types.DockerManifestSchema2, types.DockerManifestList},
		{types.OCIManifestSchema1, types.OCIImageIndex},
	} {
		t.Run(string(media.index), func(t *testing.T) {
			var blocked atomic.Bool
			var requests atomic.Int64
			upstream := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
					requests.Add(1)
					if blocked.Load() {
						w.WriteHeader(http.StatusNotFound)
						return
					}
				}
				upstream.ServeHTTP(w, r)
			}))
			defer server.Close()
			ref, err := name.NewTag(strings.TrimPrefix(server.URL, "http://") + "/test/image:v1.json")
			if err != nil {
				t.Fatal(err)
			}
			var images []v1.Image
			for range 2 {
				image, err := random.Image(256, 1)
				if err != nil {
					t.Fatal(err)
				}
				images = append(images, mutate.MediaType(image, media.image))
			}
			nested := mutate.AppendManifests(mutate.IndexMediaType(empty.Index, media.index), mutate.IndexAddendum{Add: images[1]})
			index := mutate.AppendManifests(mutate.IndexMediaType(empty.Index, media.index),
				mutate.IndexAddendum{Add: images[0]}, mutate.IndexAddendum{Add: nested})
			if err := remote.WriteIndex(ref, index, remote.WithContext(t.Context())); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := run(t.Context(), []string{ref.String(), dir}); err != nil {
				t.Fatal(err)
			}
			manifestDir := filepath.Join(dir, "v2", "test", "image", "manifests")
			metadataPath := filepath.Join(manifestDir, ".metadata.json")
			metadata, err := os.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			var headers map[string]v1.Descriptor
			if err := json.Unmarshal(metadata, &headers); err != nil {
				t.Fatal(err)
			}
			if len(headers) != 5 {
				t.Fatalf("missing nested manifests: %d entries", len(headers))
			}
			for ref, desc := range headers {
				if !validContent(filepath.Join(manifestDir, ref), desc) {
					t.Fatalf("manifest %s is not byte-for-byte preserved", ref)
				}
			}
			var layerPath string
			var layerDesc v1.Descriptor
			for _, image := range images {
				manifest, err := image.Manifest()
				if err != nil {
					t.Fatal(err)
				}
				for _, desc := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
					path := filepath.Join(dir, "v2", "test", "image", "blobs", desc.Digest.String())
					if !validContent(path, desc) {
						t.Fatalf("missing or invalid blob: %s", desc.Digest)
					}
					layerPath, layerDesc = path, desc
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "nginx.conf")); !os.IsNotExist(err) {
				t.Fatalf("generated nginx config without flag: %v", err)
			}
			initialRequests := requests.Load()
			if err := download(t.Context(), ref, dir); err != nil {
				t.Fatal(err)
			}
			if requests.Load() != initialRequests {
				t.Fatal("downloaded already verified blobs again")
			}
			second := ref.Context().Tag("second")
			if err := remote.WriteIndex(second, index, remote.WithContext(t.Context())); err != nil {
				t.Fatal(err)
			}
			if err := download(t.Context(), second, dir); err != nil {
				t.Fatal(err)
			}
			if err := download(t.Context(), ref.Context().Digest(headers[ref.Identifier()].Digest.String()), dir); err != nil {
				t.Fatal(err)
			}
			metadata, err = os.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := run(t.Context(), []string{"--nginx", ref.String(), dir}); err != nil {
				t.Fatal(err)
			}
			config, err := os.ReadFile(filepath.Join(dir, "nginx.conf"))
			if err != nil || !strings.Contains(string(config), string(media.index)) || !strings.Contains(string(config), string(media.image)) {
				t.Fatalf("missing manifest content types: %v", err)
			}
			if !strings.Contains(string(config), "/manifests/second {") || !strings.Contains(string(config), "/manifests/v1.json {") {
				t.Fatal("import did not preserve both tags")
			}
			for _, directive := range []string{"events {", "http {", "server {", "listen ", "pid ", "access_log ", "error_log "} {
				if strings.Contains(string(config), directive) {
					t.Fatalf("fragment contains host directive %q", directive)
				}
			}
			customPath := filepath.Join(t.TempDir(), "includes", "registry.conf")
			if err := run(t.Context(), []string{"--nginx-config", customPath, ref.String(), dir}); err != nil {
				t.Fatal(err)
			}
			custom, err := os.ReadFile(customPath)
			if err != nil || !bytes.Equal(custom, config) {
				t.Fatalf("custom fragment differs from default: %v", err)
			}
			testNginxInclude(t, dir, customPath, headers[ref.Identifier()], layerDesc)
			if err := os.WriteFile(layerPath, []byte("corrupt"), 0644); err != nil {
				t.Fatal(err)
			}
			blocked.Store(true)
			if err := download(t.Context(), ref, dir); err == nil {
				t.Fatal("succeeded with corrupt cache and unavailable upstream blob")
			}
			current, err := os.ReadFile(metadataPath)
			if err != nil || !bytes.Equal(current, metadata) {
				t.Fatalf("failed import replaced metadata: %v", err)
			}
			blocked.Store(false)
			if err := download(t.Context(), ref, dir); err != nil || !validContent(layerPath, layerDesc) {
				t.Fatalf("failed to repair corrupt cache: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "nginx.conf"), []byte("user config"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := nginxConfig(t.Context(), dir, filepath.Join(dir, "nginx.conf"), defaultParallelVerify); err == nil {
				t.Fatal("overwrote user nginx config")
			}
		})
	}
}
