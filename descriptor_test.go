package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func TestConflictingDescriptorsDoNotPoisonHealthySiblings(t *testing.T) {
	for _, conflict := range []string{"blob size", "manifest size", "manifest media type"} {
		t.Run(conflict, func(t *testing.T) {
			r := newResilienceRegistry(t)
			ref := r.tag(t, "isolation/conflict", "latest")
			image := resilienceImage(t, 1)
			resiliencePush(t, ref.Context().Tag("good"), image)
			goodRaw, err := image.RawManifest()
			if err != nil {
				t.Fatal(err)
			}
			good := v1.Descriptor{Digest: mustHash(t, goodRaw), Size: int64(len(goodRaw)), MediaType: types.DockerManifestSchema2}
			bad := good
			put := func(tag string, data []byte, media types.MediaType) {
				t.Helper()
				request, err := http.NewRequest("PUT", r.server.URL+resiliencePath(ref, "manifests", tag), bytes.NewReader(data))
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Content-Type", string(media))
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if response.StatusCode >= 300 {
					body, _ := io.ReadAll(response.Body)
					t.Fatalf("PUT %s: %s %s", tag, response.Status, body)
				}
			}
			switch conflict {
			case "blob size":
				manifest := resilienceManifest(t, image)
				manifest.Layers[0].Size++
				raw, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				bad.Digest, bad.Size = mustHash(t, raw), int64(len(raw))
				put("bad", raw, bad.MediaType)
			case "manifest size":
				bad.Size++
			case "manifest media type":
				bad.MediaType = types.OCIManifestSchema1
			}
			index := v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{bad, good}}
			raw, err := json.Marshal(index)
			if err != nil {
				t.Fatal(err)
			}
			put("latest", raw, index.MediaType)
			dir := t.TempDir()
			if err := run(t.Context(), []string{"--parallel-downloads", "3", "--nginx", ref.String(), dir}); err == nil {
				t.Fatal("invalid child unexpectedly succeeded")
			}
			config, err := os.ReadFile(filepath.Join(dir, "nginx.conf"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(config), "/manifests/"+good.Digest.String()+" {") {
				t.Fatal("invalid sibling descriptor prevented healthy sibling publication")
			}
			resilienceAssertAbsent(t, resilienceLocalPath(dir, ref, "manifests", "latest"))
			resilienceAssertNoTemps(t, dir)
		})
	}
}

func mustHash(t *testing.T, data []byte) v1.Hash {
	t.Helper()
	digest, _, err := v1.SHA256(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return digest
}
