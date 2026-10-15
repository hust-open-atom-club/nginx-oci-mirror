package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/random"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// resilienceRegistry records actual HTTP GETs, including any transport-level
// retries, rather than only counting calls to the downloader's retry helper.
type resilienceRegistry struct {
	server *httptest.Server
	mu     sync.Mutex
	gets   map[string]int
	hook   func(http.ResponseWriter, *http.Request, int) bool
}

func newResilienceRegistry(t *testing.T) *resilienceRegistry {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("registry digest filenames require a Linux filesystem")
	}
	r := &resilienceRegistry{gets: make(map[string]int)}
	upstream := registry.New(registry.Logger(log.New(io.Discard, "", 0)))
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && (strings.Contains(request.URL.Path, "/manifests/") || strings.Contains(request.URL.Path, "/blobs/")) {
			r.mu.Lock()
			r.gets[request.URL.Path]++
			attempt := r.gets[request.URL.Path]
			hook := r.hook
			r.mu.Unlock()
			if hook != nil && hook(w, request, attempt) {
				return
			}
		}
		upstream.ServeHTTP(w, request)
	}))
	t.Cleanup(r.server.Close)
	return r
}

func (r *resilienceRegistry) tag(t *testing.T, repository, tag string) name.Tag {
	t.Helper()
	ref, err := name.NewTag(strings.TrimPrefix(r.server.URL, "http://") + "/" + repository + ":" + tag)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func (r *resilienceRegistry) intercept(hook func(http.ResponseWriter, *http.Request, int) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gets = make(map[string]int)
	r.hook = hook
}

func (r *resilienceRegistry) count(path string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gets[path]
}

func (r *resilienceRegistry) snapshot() map[string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make(map[string]int, len(r.gets))
	maps.Copy(result, r.gets)
	return result
}

func resilienceImage(t *testing.T, layers int) v1.Image {
	t.Helper()
	image, err := random.Image(256, int64(layers))
	if err != nil {
		t.Fatal(err)
	}
	return image
}

func resiliencePush(t *testing.T, ref name.Tag, image v1.Image) {
	t.Helper()
	if err := remote.Write(ref, image, remote.WithContext(t.Context())); err != nil {
		t.Fatal(err)
	}
}

func resilienceManifest(t *testing.T, image v1.Image) *v1.Manifest {
	t.Helper()
	manifest, err := image.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func resilienceDigest(t *testing.T, image interface{ Digest() (v1.Hash, error) }) string {
	t.Helper()
	digest, err := image.Digest()
	if err != nil {
		t.Fatal(err)
	}
	return digest.String()
}

func resiliencePath(ref name.Reference, kind, id string) string {
	return "/v2/" + ref.Context().RepositoryStr() + "/" + kind + "/" + id
}

func resilienceLocalPath(dir string, ref name.Reference, kind, id string) string {
	return filepath.Join(dir, "v2", filepath.FromSlash(ref.Context().RepositoryStr()), kind, id)
}

func resilienceAssertAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unexpected published file %s: %v", path, err)
	}
}

func resilienceAssertNoTemps(t *testing.T, dir string) {
	t.Helper()
	if err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".download-") {
			t.Errorf("temporary download was not removed: %s", path)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadSucceedsOnFourthHTTPAttempt(t *testing.T) {
	for _, resource := range []string{"manifest", "blob"} {
		t.Run(resource, func(t *testing.T) {
			r := newResilienceRegistry(t)
			ref := r.tag(t, "retry/image", "latest")
			image := resilienceImage(t, 1)
			resiliencePush(t, ref, image)
			path := resiliencePath(ref, "manifests", ref.Identifier())
			if resource == "blob" {
				path = resiliencePath(ref, "blobs", resilienceManifest(t, image).Layers[0].Digest.String())
			}
			r.intercept(func(w http.ResponseWriter, request *http.Request, attempt int) bool {
				if request.URL.Path == path && attempt < 4 {
					http.Error(w, "temporary upstream failure", http.StatusServiceUnavailable)
					return true
				}
				return false
			})
			dir := t.TempDir()
			if err := download(t.Context(), ref, dir); err != nil {
				t.Fatalf("fourth attempt should succeed: %v", err)
			}
			if got := r.count(path); got != 4 {
				t.Fatalf("HTTP GET attempts = %d, want 4", got)
			}
			if _, err := os.Stat(resilienceLocalPath(dir, ref, "manifests", ref.Identifier())); err != nil {
				t.Fatalf("completed tag was not published: %v", err)
			}
			resilienceAssertNoTemps(t, dir)
		})
	}
}

func TestExhaustedDownloadContinuesIndependentTreeFiles(t *testing.T) {
	for _, resource := range []string{"child manifest", "blob"} {
		t.Run(resource, func(t *testing.T) {
			r := newResilienceRegistry(t)
			ref := r.tag(t, "continue/tree", "latest")
			first, later := resilienceImage(t, 2), resilienceImage(t, 1)
			index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: first}, mutate.IndexAddendum{Add: later})
			if err := remote.WriteIndex(ref, index, remote.WithContext(t.Context())); err != nil {
				t.Fatal(err)
			}
			failedDigest := resilienceDigest(t, first)
			failedPath := resiliencePath(ref, "manifests", failedDigest)
			if resource == "blob" {
				failedDigest = resilienceManifest(t, first).Layers[0].Digest.String()
				failedPath = resiliencePath(ref, "blobs", failedDigest)
			}
			r.intercept(func(w http.ResponseWriter, request *http.Request, _ int) bool {
				if request.URL.Path == failedPath {
					http.Error(w, "unavailable", http.StatusNotFound)
					return true
				}
				return false
			})
			dir := t.TempDir()
			err := download(t.Context(), ref, dir)
			if err == nil || !strings.Contains(err.Error(), failedDigest) {
				t.Fatalf("failed digest must be reported: %v", err)
			}
			if got := r.count(failedPath); got != 4 {
				t.Fatalf("exhausted HTTP GET attempts = %d, want exactly 4", got)
			}
			laterManifest := resilienceManifest(t, later)
			for _, desc := range append([]v1.Descriptor{laterManifest.Config}, laterManifest.Layers...) {
				if !validContent(resilienceLocalPath(dir, ref, "blobs", desc.Digest.String()), desc) {
					t.Errorf("did not finish independent later child's blob %s", desc.Digest)
				}
			}
			if resource == "blob" {
				lastLayer := resilienceManifest(t, first).Layers[1]
				if !validContent(resilienceLocalPath(dir, ref, "blobs", lastLayer.Digest.String()), lastLayer) {
					t.Error("did not finish a later layer from the failed image")
				}
				resilienceAssertAbsent(t, resilienceLocalPath(dir, ref, "blobs", failedDigest))
			}
			resilienceAssertAbsent(t, resilienceLocalPath(dir, ref, "manifests", resilienceDigest(t, first)))
			resilienceAssertAbsent(t, resilienceLocalPath(dir, ref, "manifests", resilienceDigest(t, index)))
			resilienceAssertAbsent(t, resilienceLocalPath(dir, ref, "manifests", ref.Identifier()))
			resilienceAssertNoTemps(t, dir)
		})
	}
}

func TestBatchContinuesAfterExhaustedManifestAndBlob(t *testing.T) {
	for _, resource := range []string{"manifest", "blob"} {
		t.Run(resource, func(t *testing.T) {
			r := newResilienceRegistry(t)
			first, later := r.tag(t, "batch/first", "latest"), r.tag(t, "batch/later", "latest")
			firstImage, laterImage := resilienceImage(t, 2), resilienceImage(t, 1)
			resiliencePush(t, first, firstImage)
			resiliencePush(t, later, laterImage)
			failedPath := resiliencePath(first, "manifests", first.Identifier())
			if resource == "blob" {
				failedPath = resiliencePath(first, "blobs", resilienceManifest(t, firstImage).Layers[0].Digest.String())
			}
			r.intercept(func(w http.ResponseWriter, request *http.Request, _ int) bool {
				if request.URL.Path == failedPath {
					http.Error(w, "unavailable", http.StatusServiceUnavailable)
					return true
				}
				return false
			})
			dir := t.TempDir()
			input := filepath.Join(t.TempDir(), "images.txt")
			if err := os.WriteFile(input, []byte(first.String()+"\n"+later.String()+"\n"), 0644); err != nil {
				t.Fatal(err)
			}
			err := run(t.Context(), []string{"--parallel-downloads", "2", "--image-file", input, dir})
			if err == nil || !strings.Contains(err.Error(), first.String()) {
				t.Fatalf("batch must return an error identifying failed image: %v", err)
			}
			if got := r.count(failedPath); got != 4 {
				t.Fatalf("failed file GET count = %d, want 4", got)
			}
			if _, err := os.Stat(resilienceLocalPath(dir, later, "manifests", later.Identifier())); err != nil {
				t.Fatalf("later batch image was not published: %v", err)
			}
			resilienceAssertAbsent(t, resilienceLocalPath(dir, first, "manifests", first.Identifier()))
			resilienceAssertNoTemps(t, dir)
		})
	}
}

func TestBlobIntegrityAndTruncationAreRetried(t *testing.T) {
	for _, damage := range []string{"wrong digest", "truncated response"} {
		for _, recover := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/recover=%t", damage, recover), func(t *testing.T) {
				r := newResilienceRegistry(t)
				ref := r.tag(t, "integrity/image", "latest")
				image := resilienceImage(t, 1)
				resiliencePush(t, ref, image)
				desc := resilienceManifest(t, image).Layers[0]
				path := resiliencePath(ref, "blobs", desc.Digest.String())
				r.intercept(func(w http.ResponseWriter, request *http.Request, attempt int) bool {
					if request.URL.Path != path || (recover && attempt == 4) {
						return false
					}
					w.Header().Set("Content-Type", "application/octet-stream")
					w.Header().Set("Content-Length", strconv.FormatInt(desc.Size, 10))
					n := desc.Size
					if damage == "truncated response" {
						n /= 2
					}
					_, _ = w.Write(bytes.Repeat([]byte{'x'}, int(n)))
					return true
				})
				dir := t.TempDir()
				err := download(t.Context(), ref, dir)
				if (err == nil) != recover {
					t.Fatalf("recover=%t: %v", recover, err)
				}
				if got := r.count(path); got != 4 {
					t.Fatalf("integrity failure GET count = %d, want exactly 4", got)
				}
				local := resilienceLocalPath(dir, ref, "blobs", desc.Digest.String())
				if recover {
					if !validContent(local, desc) {
						t.Fatal("successful retry did not produce a verified blob")
					}
				} else {
					resilienceAssertAbsent(t, local)
					resilienceAssertAbsent(t, resilienceLocalPath(dir, ref, "manifests", ref.Identifier()))
				}
				resilienceAssertNoTemps(t, dir)
			})
		}
	}
}

func TestFailedTagUpdatePreservesPublishedFiles(t *testing.T) {
	r := newResilienceRegistry(t)
	ref := r.tag(t, "atomic/image", "latest")
	oldImage := resilienceImage(t, 1)
	resiliencePush(t, ref, oldImage)
	dir := t.TempDir()
	if err := download(t.Context(), ref, dir); err != nil {
		t.Fatal(err)
	}
	tagPath := resilienceLocalPath(dir, ref, "manifests", ref.Identifier())
	metadataPath := resilienceLocalPath(dir, ref, "manifests", ".metadata.json")
	oldTag, err := os.ReadFile(tagPath)
	if err != nil {
		t.Fatal(err)
	}
	oldMetadata, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	newImage := resilienceImage(t, 2)
	resiliencePush(t, ref, newImage)
	failed := resilienceManifest(t, newImage).Layers[0]
	failedPath := resiliencePath(ref, "blobs", failed.Digest.String())
	r.intercept(func(w http.ResponseWriter, request *http.Request, _ int) bool {
		if request.URL.Path == failedPath {
			http.Error(w, "unavailable", http.StatusNotFound)
			return true
		}
		return false
	})
	if err := downloadWithConcurrency(t.Context(), ref, dir, 3); err == nil {
		t.Fatal("incomplete update must fail")
	}
	for path, want := range map[string][]byte{tagPath: oldTag, metadataPath: oldMetadata} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("failed update modified %s: %v", path, err)
		}
	}
	oldManifest := resilienceManifest(t, oldImage)
	for _, desc := range append([]v1.Descriptor{oldManifest.Config}, oldManifest.Layers...) {
		if !validContent(resilienceLocalPath(dir, ref, "blobs", desc.Digest.String()), desc) {
			t.Errorf("failed update damaged old blob %s", desc.Digest)
		}
	}
	resilienceAssertAbsent(t, resilienceLocalPath(dir, ref, "manifests", resilienceDigest(t, newImage)))
	resilienceAssertAbsent(t, resilienceLocalPath(dir, ref, "blobs", failed.Digest.String()))
	resilienceAssertNoTemps(t, dir)
}

func TestParallelFileDownloadsAreBounded(t *testing.T) {
	for _, workers := range []int{1, 3} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			r := newResilienceRegistry(t)
			ref := r.tag(t, "parallel/tree", "latest")
			index := mutate.AppendManifests(empty.Index,
				mutate.IndexAddendum{Add: resilienceImage(t, 1)},
				mutate.IndexAddendum{Add: resilienceImage(t, 1)},
				mutate.IndexAddendum{Add: resilienceImage(t, 1)})
			if err := remote.WriteIndex(ref, index, remote.WithContext(t.Context())); err != nil {
				t.Fatal(err)
			}
			var active, maximum atomic.Int64
			started := make(chan struct{})
			var release sync.Once
			r.intercept(func(_ http.ResponseWriter, request *http.Request, _ int) bool {
				if strings.Contains(request.URL.Path, "/blobs/") {
					n := active.Add(1)
					defer active.Add(-1)
					for previous := maximum.Load(); n > previous; previous = maximum.Load() {
						if maximum.CompareAndSwap(previous, n) {
							break
						}
					}
					if n == int64(workers) {
						release.Do(func() { close(started) })
					}
					select {
					case <-started:
					case <-time.After(3 * time.Second):
						t.Errorf("pool did not start %d independent downloads across the image tree", workers)
					}
					time.Sleep(20 * time.Millisecond)
				}
				return false
			})
			args := []string{ref.String(), t.TempDir()}
			if workers != 1 {
				args = append([]string{"--parallel-downloads", strconv.Itoa(workers)}, args...)
			}
			if err := run(t.Context(), args); err != nil {
				t.Fatal(err)
			}
			got := maximum.Load()
			if got < 1 || got > int64(workers) {
				t.Fatalf("observed %d simultaneous blob GETs, allowed 1..%d", got, workers)
			}
			if got != int64(workers) {
				t.Fatalf("parallel option overlapped %d files, want %d across the image tree", got, workers)
			}
			t.Logf("workers=%d, observed peak simultaneous blob GETs=%d", workers, got)
		})
	}
}

func TestParallelDownloadDeduplicatesSharedDigests(t *testing.T) {
	r := newResilienceRegistry(t)
	ref := r.tag(t, "parallel/shared", "latest")
	original := resilienceImage(t, 1)
	layers, err := original.Layers()
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := mutate.AppendLayers(original, layers[0], layers[0])
	if err != nil {
		t.Fatal(err)
	}
	config, err := original.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	config.Architecture = "arm64"
	other, err := mutate.ConfigFile(original, config)
	if err != nil {
		t.Fatal(err)
	}
	index := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: repeated},
		mutate.IndexAddendum{Add: repeated},
		mutate.IndexAddendum{Add: other})
	if err := remote.WriteIndex(ref, index, remote.WithContext(t.Context())); err != nil {
		t.Fatal(err)
	}
	r.intercept(func(_ http.ResponseWriter, request *http.Request, _ int) bool {
		if strings.Contains(request.URL.Path, "/blobs/") {
			time.Sleep(10 * time.Millisecond)
		}
		return false
	})
	dir := t.TempDir()
	if err := downloadWithConcurrency(t.Context(), ref, dir, 8); err != nil {
		t.Fatal(err)
	}
	for path, n := range r.snapshot() {
		if n != 1 {
			t.Errorf("shared file fetched %d times: %s", n, path)
		}
	}
	for _, image := range []v1.Image{repeated, other} {
		manifest := resilienceManifest(t, image)
		for _, desc := range append([]v1.Descriptor{manifest.Config}, manifest.Layers...) {
			if !validContent(resilienceLocalPath(dir, ref, "blobs", desc.Digest.String()), desc) {
				t.Errorf("shared blob missing or invalid: %s", desc.Digest)
			}
		}
	}
	resilienceAssertNoTemps(t, dir)
}

func TestParallelDownloadsHonorCancellation(t *testing.T) {
	r := newResilienceRegistry(t)
	ref := r.tag(t, "cancel/image", "latest")
	resiliencePush(t, ref, resilienceImage(t, 4))
	started := make(chan struct{}, 1)
	r.intercept(func(_ http.ResponseWriter, request *http.Request, _ int) bool {
		if strings.Contains(request.URL.Path, "/blobs/") {
			select {
			case started <- struct{}{}:
			default:
			}
			<-request.Context().Done()
			return true
		}
		return false
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	dir := t.TempDir()
	result := make(chan error, 1)
	go func() { result <- downloadWithConcurrency(ctx, ref, dir, 3) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download never started")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation result = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled download did not stop promptly")
	}
	for path, n := range r.snapshot() {
		if n > 1 {
			t.Errorf("retried a canceled request %d times: %s", n, path)
		}
	}
	resilienceAssertAbsent(t, resilienceLocalPath(dir, ref, "manifests", ref.Identifier()))
	resilienceAssertNoTemps(t, dir)
}

func TestParallelDownloadsRejectInvalidLimitBeforeNetworking(t *testing.T) {
	r := newResilienceRegistry(t)
	ref := r.tag(t, "parallel/invalid", "latest")
	for _, limit := range []string{"0", "-1", "129", "1.5", "invalid"} {
		if err := run(t.Context(), []string{"--parallel-downloads", limit, ref.String(), t.TempDir()}); err == nil {
			t.Errorf("accepted invalid --parallel-downloads %q", limit)
		}
	}
	if gets := r.snapshot(); len(gets) != 0 {
		t.Fatalf("invalid concurrency should be rejected before downloads: %v", gets)
	}
}

// This intentionally documents the existing repository-scoped cache. It does
// not assert cross-repository deduplication, which is a separate storage change.
func TestCrossRepositoryBlobReuseEvidence(t *testing.T) {
	r := newResilienceRegistry(t)
	first, other := r.tag(t, "reuse/first", "latest"), r.tag(t, "reuse/other", "latest")
	image := resilienceImage(t, 1)
	resiliencePush(t, first, image)
	resiliencePush(t, other, image)
	manifest := resilienceManifest(t, image)
	blobs := append([]v1.Descriptor{manifest.Config}, manifest.Layers...)
	dir := t.TempDir()
	r.intercept(nil)
	if err := download(t.Context(), first, dir); err != nil {
		t.Fatal(err)
	}
	firstGets := r.snapshot()
	if err := download(t.Context(), other, dir); err != nil {
		t.Fatal(err)
	}
	afterOther := r.snapshot()
	beforeRepeatInfo := make(map[string]os.FileInfo)
	for _, desc := range blobs {
		id := desc.Digest.String()
		a, err := os.Stat(resilienceLocalPath(dir, first, "blobs", id))
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.Stat(resilienceLocalPath(dir, other, "blobs", id))
		if err != nil {
			t.Fatal(err)
		}
		firstCount := firstGets[resiliencePath(first, "blobs", id)]
		otherCount := afterOther[resiliencePath(other, "blobs", id)]
		sameFile := os.SameFile(a, b)
		t.Logf("blob %s: first repository GETs=%d, second repository GETs=%d, os.SameFile=%t (first stat=%+v, second stat=%+v)", id, firstCount, otherCount, sameFile, a.Sys(), b.Sys())
		if firstCount != 1 || otherCount != 1 || sameFile {
			t.Errorf("repository-scoped cache behavior changed: first GETs=%d, second GETs=%d, same physical file=%t", firstCount, otherCount, sameFile)
		}
		beforeRepeatInfo[id] = a
	}
	if err := download(t.Context(), first, dir); err != nil {
		t.Fatal(err)
	}
	for _, desc := range blobs {
		id := desc.Digest.String()
		path := resiliencePath(first, "blobs", id)
		after, err := os.Stat(resilienceLocalPath(dir, first, "blobs", id))
		if err != nil {
			t.Fatal(err)
		}
		additional := r.count(path) - firstGets[path]
		sameFile := os.SameFile(beforeRepeatInfo[id], after)
		t.Logf("same-repository repeat for %s: additional blob GETs=%d, os.SameFile=%t", id, additional, sameFile)
		if additional != 0 || !sameFile {
			t.Errorf("same-repository repeat did not reuse verified blob: additional GETs=%d, same physical file=%t", additional, sameFile)
		}
	}
	metadata, err := os.ReadFile(resilienceLocalPath(dir, first, "manifests", ".metadata.json"))
	if err != nil {
		t.Fatal(err)
	}
	var headers map[string]v1.Descriptor
	if err := json.Unmarshal(metadata, &headers); err != nil || len(headers) == 0 {
		t.Fatalf("repeat import did not retain usable manifest metadata: %v", err)
	}
}

func TestChildManifestIntegrityFailureIsRetried(t *testing.T) {
	r := newResilienceRegistry(t)
	ref := r.tag(t, "integrity/manifest", "latest")
	child := resilienceImage(t, 1)
	index := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: child})
	if err := remote.WriteIndex(ref, index, remote.WithContext(t.Context())); err != nil {
		t.Fatal(err)
	}
	wrong := resilienceImage(t, 1)
	wrongBytes, err := wrong.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	mediaType, err := wrong.MediaType()
	if err != nil {
		t.Fatal(err)
	}
	childDigest := resilienceDigest(t, child)
	wrongDigest := resilienceDigest(t, wrong)
	path := resiliencePath(ref, "manifests", childDigest)
	r.intercept(func(w http.ResponseWriter, request *http.Request, attempt int) bool {
		if request.URL.Path == path && attempt < 4 {
			w.Header().Set("Content-Type", string(mediaType))
			w.Header().Set("Docker-Content-Digest", wrongDigest)
			_, _ = w.Write(wrongBytes)
			return true
		}
		return false
	})
	dir := t.TempDir()
	if err := download(t.Context(), ref, dir); err != nil {
		t.Fatalf("child manifest should recover on fourth verified response: %v", err)
	}
	if got := r.count(path); got != 4 {
		t.Fatalf("child manifest GETs = %d, want exactly 4", got)
	}
	want, err := child.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(resilienceLocalPath(dir, ref, "manifests", childDigest))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("child manifest is not byte-for-byte verified: %v", err)
	}
	resilienceAssertNoTemps(t, dir)
}

func TestBatchImagesRemainSequentialWithParallelFiles(t *testing.T) {
	r := newResilienceRegistry(t)
	first, second := r.tag(t, "sequential/first", "latest"), r.tag(t, "sequential/second", "latest")
	resiliencePush(t, first, resilienceImage(t, 3))
	resiliencePush(t, second, resilienceImage(t, 3))
	var mu sync.Mutex
	var order []string
	r.intercept(func(_ http.ResponseWriter, request *http.Request, _ int) bool {
		mu.Lock()
		order = append(order, request.URL.Path)
		mu.Unlock()
		if strings.Contains(request.URL.Path, "/blobs/") {
			time.Sleep(20 * time.Millisecond)
		}
		return false
	})
	input := filepath.Join(t.TempDir(), "images.txt")
	if err := os.WriteFile(input, []byte(first.String()+"\n"+second.String()+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), []string{"--parallel-downloads", "3", "--image-file", input, t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	secondStarted := false
	for _, path := range order {
		if strings.HasPrefix(path, "/v2/sequential/second/") {
			secondStarted = true
		}
		if secondStarted && strings.HasPrefix(path, "/v2/sequential/first/") {
			t.Fatalf("batch image requests were interleaved: %v", order)
		}
	}
	if !secondStarted {
		t.Fatal("second batch image did not start")
	}
}

func TestMalformedManifestResponseIsRetried(t *testing.T) {
	r := newResilienceRegistry(t)
	ref := r.tag(t, "retry/malformed", "latest")
	image := resilienceImage(t, 1)
	resiliencePush(t, ref, image)
	mediaType, err := image.MediaType()
	if err != nil {
		t.Fatal(err)
	}
	path := resiliencePath(ref, "manifests", ref.Identifier())
	r.intercept(func(w http.ResponseWriter, request *http.Request, attempt int) bool {
		if request.URL.Path == path && attempt < 4 {
			w.Header().Set("Content-Type", string(mediaType))
			_, _ = io.WriteString(w, `{"schemaVersion":2,"layers":[`)
			return true
		}
		return false
	})
	dir := t.TempDir()
	if err := download(t.Context(), ref, dir); err != nil {
		t.Fatalf("malformed manifest should recover on fourth attempt: %v (GETs=%d)", err, r.count(path))
	}
	if got := r.count(path); got != 4 {
		t.Fatalf("malformed manifest GETs = %d, want exactly 4", got)
	}
	want, err := image.RawManifest()
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(resilienceLocalPath(dir, ref, "manifests", ref.Identifier()))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("completed manifest does not match the successful response: %v", err)
	}
	resilienceAssertNoTemps(t, dir)
}

// Keep the default-worker shorthand for fixtures exercising single downloads.
func download(ctx context.Context, ref name.Reference, dir string) error {
	return downloadWithConcurrency(ctx, ref, dir, 1)
}
