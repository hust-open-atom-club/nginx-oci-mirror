package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

var repositoryPattern = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

const maxParallelDownloads = 128

type manifestTask struct {
	desc     *remote.Descriptor
	children []*manifestTask
	blobs    []v1.Descriptor
	err      error
}

type blobTask struct {
	desc v1.Descriptor
	err  error
}

type mirror struct {
	dir       string
	repo      name.Repository
	puller    *remote.Puller
	headers   map[string]v1.Descriptor
	manifests map[string]*manifestTask
	ordered   []*manifestTask // Children precede their parents.
	blobs     map[string]*blobTask
	jobs      []*blobTask
}

func validReference(repo, ref string) bool {
	_, err := v1.NewHash(ref)
	return repositoryPattern.MatchString(repo) && (tagPattern.MatchString(ref) || err == nil)
}

func downloadWithConcurrency(ctx context.Context, ref name.Reference, dir string, workers int) error {
	// Separate processes importing into one directory must still run serially.
	if workers < 1 || workers > maxParallelDownloads {
		return fmt.Errorf("parallel downloads must be between 1 and %d", maxParallelDownloads)
	}
	if !validReference(ref.Context().RepositoryStr(), ref.Identifier()) {
		return fmt.Errorf("invalid repository or reference: %s", ref)
	}
	puller, err := remote.NewPuller(remote.WithAuthFromKeychain(authn.DefaultKeychain),
		// Retry whole files, including body reads and integrity checks, exactly four times.
		remote.WithRetryBackoff(remote.Backoff{Steps: 1}))
	if err != nil {
		return err
	}
	m := mirror{
		dir: dir, repo: ref.Context(), puller: puller,
		headers: make(map[string]v1.Descriptor), manifests: make(map[string]*manifestTask),
		blobs: make(map[string]*blobTask),
	}
	metadata := m.path("manifests", ".metadata.json")
	data, err := os.ReadFile(metadata)
	if err == nil {
		if err := json.Unmarshal(data, &m.headers); err != nil {
			return fmt.Errorf("read manifest metadata: %w", err)
		}
		if m.headers == nil {
			return errors.New("invalid manifest metadata: null")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}

	root := m.collect(ctx, ref, nil)
	m.downloadBlobs(ctx, workers)
	var failures []error
	for _, job := range m.jobs {
		if job.err != nil {
			failures = append(failures, job.err)
		}
	}
	for _, task := range m.ordered {
		if task.err == nil {
			for _, child := range task.children {
				if child.err != nil {
					task.err = fmt.Errorf("incomplete child manifest: %w", child.err)
					break
				}
			}
		}
		if task.err == nil {
			for _, desc := range task.blobs {
				job := m.blobs[blobKey(desc)]
				if job.err != nil || job.desc.Size != desc.Size {
					task.err = fmt.Errorf("incomplete blob %s", desc.Digest)
					break
				}
			}
		}
		if task.err == nil {
			task.err = ctx.Err()
		}
		if task.err == nil {
			task.err = storeContent(m.path("manifests", task.desc.Digest.String()), bytes.NewReader(task.desc.Manifest), task.desc.Descriptor)
		}
		if task.err != nil {
			failures = append(failures, task.err)
			continue
		}
		m.headers[task.desc.Digest.String()] = task.desc.Descriptor
	}
	if root.err == nil {
		// Publish a tag only after its complete dependency graph is on disk.
		if err := storeContent(m.path("manifests", ref.Identifier()), bytes.NewReader(root.desc.Manifest), root.desc.Descriptor); err != nil {
			failures = append(failures, err)
		} else {
			m.headers[ref.Identifier()] = root.desc.Descriptor
		}
	}
	// Completed sibling manifests remain usable even when the root is incomplete.
	if len(m.headers) > 0 {
		if err := atomicFile(metadata, func(w io.Writer) error { return json.NewEncoder(w).Encode(m.headers) }); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (m *mirror) path(kind, ref string) string {
	return filepath.Join(m.dir, "v2", filepath.FromSlash(m.repo.RepositoryStr()), kind, ref)
}

// Discovery is serial: the worker pool only handles independent blob files, so
// manifest bookkeeping is never mutated concurrently and workers cannot deadlock
// while waiting for children in a nested index.
func (m *mirror) collect(ctx context.Context, ref name.Reference, expected *v1.Descriptor) *manifestTask {
	key := ref.Identifier()
	if expected != nil {
		key = fmt.Sprintf("%s/%d/%s", key, expected.Size, expected.MediaType)
	}
	if previous, ok := m.manifests[key]; ok {
		return previous
	}
	task := &manifestTask{}
	var doc manifestDocument
	m.manifests[key] = task
	task.err = retryDownload(ctx, "manifest "+ref.String(), func() error {
		var err error
		task.desc, err = m.puller.Get(ctx, ref)
		if err != nil {
			return err
		}
		if expected != nil && !sameDescriptor(task.desc.Descriptor, *expected) {
			return fmt.Errorf("manifest %s does not match index descriptor", ref)
		}
		if err := copyContent(io.Discard, bytes.NewReader(task.desc.Manifest), task.desc.Descriptor); err != nil {
			return err
		}
		doc, err = parseManifest(task.desc.Manifest, task.desc.MediaType)
		return err
	})
	if task.err == nil {
		fmt.Fprintf(os.Stderr, "manifest %s\n", ref)
		for _, child := range doc.Manifests {
			task.children = append(task.children, m.collect(ctx, m.repo.Digest(child.Digest.String()), &child))
		}
		task.blobs = doc.blobs()
		for _, desc := range task.blobs {
			if _, ok := m.blobs[blobKey(desc)]; !ok {
				job := &blobTask{desc: desc}
				m.blobs[blobKey(desc)] = job
				m.jobs = append(m.jobs, job)
			}
		}
	}
	m.ordered = append(m.ordered, task)
	return task
}

func (m *mirror) downloadBlobs(ctx context.Context, workers int) {
	jobs := make(chan *blobTask)
	var wg sync.WaitGroup
	for range min(workers, len(m.jobs)) {
		wg.Go(func() {
			for job := range jobs {
				job.err = ctx.Err()
				if job.err == nil {
					job.err = m.blob(ctx, job.desc)
				}
			}
		})
	}
	for _, job := range m.jobs {
		jobs <- job
	}
	close(jobs)
	wg.Wait()
}

func (m *mirror) blob(ctx context.Context, desc v1.Descriptor) error {
	path := m.path("blobs", desc.Digest.String())
	if validContent(path, desc) {
		return nil
	}
	return retryDownload(ctx, "blob "+desc.Digest.String(), func() error {
		layer, err := m.puller.Layer(ctx, m.repo.Digest(desc.Digest.String()))
		if err != nil {
			return err
		}
		body, err := layer.Compressed()
		if err != nil {
			return err
		}
		defer body.Close()
		fmt.Fprintf(os.Stderr, "blob %s (%d bytes)\n", desc.Digest, desc.Size)
		return storeContent(path, body, desc)
	})
}

// Conflicting sizes must not let one malformed descriptor poison a healthy
// sibling. Identical descriptors still share one transfer; failed writes are
// temporary and cannot replace a verified blob at the common digest path.
func blobKey(desc v1.Descriptor) string {
	return fmt.Sprintf("%s/%d", desc.Digest, desc.Size)
}
