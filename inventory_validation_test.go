package main

import (
	"bytes"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

func scanTestDescriptor(data []byte, mt types.MediaType) v1.Descriptor {
	digest, size, err := v1.SHA256(bytes.NewReader(data))
	if err != nil {
		panic(err)
	}
	return v1.Descriptor{Digest: digest, Size: size, MediaType: mt}
}

func writeScanTestContent(t *testing.T, dir, repo, kind, name string, data []byte) {
	t.Helper()
	path := filepath.Join(dir, "v2", repo, kind, name)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func writeScanTestManifest(t *testing.T, dir, repo, tag string, config, layer v1.Descriptor) v1.Descriptor {
	t.Helper()
	raw, err := json.Marshal(manifestDocument{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, Config: config, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	desc := scanTestDescriptor(raw, types.OCIManifestSchema1)
	writeScanTestContent(t, dir, repo, "manifests", desc.Digest.String(), raw)
	writeScanTestContent(t, dir, repo, "manifests", tag, raw)
	return desc
}

func TestLocalScanDescriptorIsolation(t *testing.T) {
	dir := t.TempDir()
	configBytes, layerBytes := []byte("{}"), []byte("shared same-size data")
	config := scanTestDescriptor(configBytes, types.OCIConfigJSON)
	layer := scanTestDescriptor(layerBytes, types.OCILayer)
	for _, repo := range []string{"review/a", "review/b"} {
		writeScanTestContent(t, dir, repo, "blobs", config.Digest.String(), configBytes)
		writeScanTestContent(t, dir, repo, "blobs", layer.Digest.String(), layerBytes)
		good := writeScanTestManifest(t, dir, repo, "good", config, layer)
		badLayer := layer
		badLayer.Size++
		bad := writeScanTestManifest(t, dir, repo, "bad", config, badLayer)
		conflicting := good
		conflicting.MediaType = types.DockerManifestSchema2
		raw, err := json.Marshal(manifestDocument{SchemaVersion: 2, MediaType: types.OCIImageIndex, Manifests: []v1.Descriptor{bad, conflicting, good}})
		if err != nil {
			t.Fatal(err)
		}
		index := scanTestDescriptor(raw, types.OCIImageIndex)
		writeScanTestContent(t, dir, repo, "manifests", index.Digest.String(), raw)
		writeScanTestContent(t, dir, repo, "manifests", "index", raw)
	}
	writeScanTestContent(t, dir, "review/b", "blobs", layer.Digest.String(), bytes.Repeat([]byte("x"), len(layerBytes)))
	for _, workers := range []int{1, 2, 4} {
		got, err := localManifests(t.Context(), dir, workers, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		if len(got["review/a"]) != 2 || got["review/a"]["good"].Digest.Algorithm == "" {
			t.Fatalf("good sibling poisoned or bad parent published: %v", got)
		}
		if len(got["review/b"]) != 0 {
			t.Fatalf("corrupt independent repository published: %v", got["review/b"])
		}
	}
}

func TestLocalScanSHA512LegacyHint(t *testing.T) {
	dir, repo := t.TempDir(), "review/sha512"
	configBytes, layerBytes := []byte("{}"), []byte("legacy sha512 bytes")
	config := scanTestDescriptor(configBytes, types.OCIConfigJSON)
	hash := sha512.Sum512(layerBytes)
	layer := v1.Descriptor{Digest: v1.Hash{Algorithm: "sha512", Hex: hex.EncodeToString(hash[:])}, Size: int64(len(layerBytes)), MediaType: types.OCILayer}
	writeScanTestContent(t, dir, repo, "blobs", config.Digest.String(), configBytes)
	writeScanTestContent(t, dir, repo, "blobs", layer.Digest.String(), layerBytes)
	raw, err := json.Marshal(manifestDocument{SchemaVersion: 2, Config: config, Layers: []v1.Descriptor{layer}})
	if err != nil {
		t.Fatal(err)
	}
	manifestHash := sha512.Sum512(raw)
	desc := v1.Descriptor{Digest: v1.Hash{Algorithm: "sha512", Hex: hex.EncodeToString(manifestHash[:])}, Size: int64(len(raw)), MediaType: types.OCIManifestSchema1}
	writeScanTestContent(t, dir, repo, "manifests", desc.Digest.String(), raw)
	writeScanTestContent(t, dir, repo, "manifests", "good", raw)
	metadata, err := json.Marshal(map[string]v1.Descriptor{"good": desc, desc.Digest.String(): desc})
	if err != nil {
		t.Fatal(err)
	}
	writeScanTestContent(t, dir, repo, "manifests", ".metadata.json", metadata)
	got, err := localManifests(t.Context(), dir, 4, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(got[repo]) != 2 || got[repo]["good"].Digest != desc.Digest {
		t.Fatalf("sha512 legacy hint lost: %v", got)
	}
}
