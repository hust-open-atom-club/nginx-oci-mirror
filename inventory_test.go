package main

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

func TestNginxInventoryAfterFailedSync(t *testing.T) {
	r := newResilienceRegistry(t)
	old := r.tag(t, "inventory/old", "stable")
	resiliencePush(t, old, resilienceImage(t, 1))
	dir := t.TempDir()
	if err := download(t.Context(), old, dir); err != nil {
		t.Fatal(err)
	}
	missing := r.tag(t, "inventory/new", "latest")
	config := filepath.Join(t.TempDir(), "includes", "registry.conf")
	r.intercept(func(w http.ResponseWriter, req *http.Request, _ int) bool {
		if strings.Contains(req.URL.Path, "/manifests/") {
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	if err := run(t.Context(), []string{"--nginx-config", config, missing.String(), dir}); err == nil {
		t.Fatal("upstream failure was not reported")
	}
	data, err := os.ReadFile(config)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "location = /v2/inventory/old/manifests/stable {") {
		t.Fatal("failed current sync hid a previously downloaded image")
	}
	if strings.Contains(string(data), "location = /v2/inventory/new/manifests/latest {") {
		t.Fatal("failed image published")
	}
}

func TestNginxInventoryUsesCompleteDiskGraphs(t *testing.T) {
	for _, damage := range []string{"missing blob", "corrupt blob", "missing child", "corrupt tag", "symlink blob", "symlink blob directory"} {
		t.Run(damage, func(t *testing.T) {
			r := newResilienceRegistry(t)
			ref := r.tag(t, "inventory/damaged", "latest")
			child := resilienceImage(t, 1)
			index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: child})
			if err := remote.WriteIndex(ref, index, remote.WithContext(t.Context())); err != nil {
				t.Fatal(err)
			}
			healthy := r.tag(t, "inventory/healthy", "stable")
			resiliencePush(t, healthy, resilienceImage(t, 1))
			dir := t.TempDir()
			if err := download(t.Context(), ref, dir); err != nil {
				t.Fatal(err)
			}
			if err := download(t.Context(), healthy, dir); err != nil {
				t.Fatal(err)
			}
			blob := resilienceManifest(t, child).Layers[0]
			blobPath := resilienceLocalPath(dir, ref, "blobs", blob.Digest.String())
			var err error
			switch damage {
			case "missing blob":
				err = os.Remove(blobPath)
			case "corrupt blob":
				err = os.WriteFile(blobPath, []byte("corrupt"), 0644)
			case "missing child":
				err = os.Remove(resilienceLocalPath(dir, ref, "manifests", resilienceDigest(t, child)))
			case "corrupt tag":
				err = os.WriteFile(resilienceLocalPath(dir, ref, "manifests", "latest"), []byte("{broken"), 0644)
			case "symlink blob":
				outside := filepath.Join(t.TempDir(), "blob")
				if err = os.Rename(blobPath, outside); err == nil {
					err = os.Symlink(outside, blobPath)
				}
			case "symlink blob directory":
				outside := filepath.Join(t.TempDir(), "blobs")
				if err = os.Rename(filepath.Dir(blobPath), outside); err == nil {
					err = os.Symlink(outside, filepath.Dir(blobPath))
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			config := filepath.Join(dir, "nginx.conf")
			if err := nginxConfig(dir, config); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(config)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "location = /v2/inventory/healthy/manifests/stable {") {
				t.Fatal("invalid image prevented publication of healthy image")
			}
			if strings.Contains(string(data), "location = /v2/inventory/damaged/manifests/latest {") {
				t.Fatal("incomplete graph or corrupt tag published")
			}
		})
	}
}

func TestNginxInventoryDoesNotDependOnMetadata(t *testing.T) {
	for _, metadata := range []string{"missing", "{broken", "null"} {
		t.Run(metadata, func(t *testing.T) {
			r := newResilienceRegistry(t)
			ref := r.tag(t, "inventory/metadata", "latest")
			resiliencePush(t, ref, resilienceImage(t, 1))
			dir := t.TempDir()
			if err := download(t.Context(), ref, dir); err != nil {
				t.Fatal(err)
			}
			path := resilienceLocalPath(dir, ref, "manifests", ".metadata.json")
			var err error
			if metadata == "missing" {
				err = os.Remove(path)
			} else {
				err = os.WriteFile(path, []byte(metadata), 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			// Ignore both abandoned temporary files and false digest filenames.
			raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "latest"))
			if err != nil {
				t.Fatal(err)
			}
			wrong := "sha256:" + strings.Repeat("a", 64)
			for _, name := range []string{wrong, ".download-interrupted"} {
				if err := os.WriteFile(filepath.Join(filepath.Dir(path), name), raw, 0644); err != nil {
					t.Fatal(err)
				}
			}
			inventory, err := localManifests(dir)
			if err != nil {
				t.Fatal(err)
			}
			headers := inventory[ref.Context().RepositoryStr()]
			if _, ok := headers["latest"]; !ok {
				t.Fatal("healthy image hidden by missing or corrupt metadata")
			}
			if _, ok := headers[wrong]; ok {
				t.Fatal("mislabelled digest file published")
			}
			if len(headers) != 2 {
				t.Fatalf("want exactly tag and digest, got %d entries", len(headers))
			}
		})
	}
}
