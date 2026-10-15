package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type scanFixture struct {
	dir        string
	blobBytes  int64
	blobChecks int64
	references int
}

// Real files, with deterministic unique payloads. Setup and writes are excluded
// from timings; repeated scans normally measure warm page-cache throughput.
func makeScanFixture(tb testing.TB, repositories, manifests, tags, layers, layerSize int, shared bool) scanFixture {
	tb.Helper()
	fixture := scanFixture{dir: tb.TempDir()}
	for repo := range repositories {
		base := filepath.Join(fixture.dir, "v2", fmt.Sprintf("fixture/repo%d", repo))
		for _, kind := range []string{"manifests", "blobs"} {
			if err := os.MkdirAll(filepath.Join(base, kind), 0755); err != nil {
				tb.Fatal(err)
			}
		}
		writeBlob := func(data []byte, mediaType types.MediaType) v1.Descriptor {
			digest, size, err := v1.SHA256(bytes.NewReader(data))
			if err != nil {
				tb.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(base, "blobs", digest.String()), data, 0644); err != nil {
				tb.Fatal(err)
			}
			fixture.blobBytes += size
			fixture.blobChecks++
			return v1.Descriptor{Digest: digest, Size: size, MediaType: mediaType}
		}
		config := writeBlob([]byte(`{}`), types.OCIConfigJSON)
		var layerDescs []v1.Descriptor
		for manifest := range manifests {
			if !shared || manifest == 0 {
				layerDescs = nil
				for layer := range layers {
					payload := bytes.Repeat([]byte{byte(layer + 1)}, layerSize)
					copy(payload, fmt.Sprintf("repo=%d manifest=%d layer=%d", repo, manifest, layer))
					layerDescs = append(layerDescs, writeBlob(payload, types.OCILayer))
				}
			}
			doc := manifestDocument{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, Config: config, Layers: layerDescs}
			data, err := json.Marshal(doc)
			if err != nil {
				tb.Fatal(err)
			}
			// Different byte-level manifests may legitimately share every layer.
			data = append(data, bytes.Repeat([]byte("\n"), manifest+1)...)
			digest, _, err := v1.SHA256(bytes.NewReader(data))
			if err != nil {
				tb.Fatal(err)
			}
			refs := []string{digest.String()}
			for tag := range tags {
				refs = append(refs, fmt.Sprintf("image%d-tag%d", manifest, tag))
			}
			for _, ref := range refs {
				if err := os.WriteFile(filepath.Join(base, "manifests", ref), data, 0644); err != nil {
					tb.Fatal(err)
				}
				fixture.references++
			}
		}
	}
	return fixture
}

func BenchmarkLocalInventory(b *testing.B) {
	for _, shared := range []bool{true, false} {
		name, manifests, tags, layers, layerSize := "many_blobs", 32, 1, 4, 4<<20
		if shared {
			name, manifests, tags, layers, layerSize = "shared_tags", 64, 4, 32, 16<<20
		}
		b.Run(name, func(b *testing.B) {
			fixture := makeScanFixture(b, 1, manifests, tags, layers, layerSize, shared)
			for _, workers := range []int{1, 2, 4} {
				b.Run(fmt.Sprintf("workers_%d", workers), func(b *testing.B) {
					b.SetBytes(fixture.blobBytes)
					for b.Loop() {
						scan := &localScan{}
						inventory, err := scan.manifests(b.Context(), fixture.dir, workers, io.Discard)
						if err != nil {
							b.Fatal(err)
						}
						if len(inventory["fixture/repo0"]) != fixture.references || scan.bytes.Load() != fixture.blobBytes || scan.checked.Load() != fixture.blobChecks {
							b.Fatalf("incomplete scan: refs=%d bytes=%d checks=%d", len(inventory["fixture/repo0"]), scan.bytes.Load(), scan.checked.Load())
						}
					}
					b.ReportMetric(float64(fixture.blobBytes), "blob_bytes/op")
					b.ReportMetric(float64(fixture.blobChecks), "blob_checks/op")
				})
			}
		})
	}
}
