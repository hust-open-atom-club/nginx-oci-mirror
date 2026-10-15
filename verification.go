package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

const defaultParallelVerify = 2
const maxParallelVerify = 32

type localBlob struct {
	path string
	desc v1.Descriptor
	err  error
}

type localScan struct {
	blobs      map[string]*localBlob
	jobs       []*localBlob
	discovered atomic.Int64
	checked    atomic.Int64
	bytes      atomic.Int64
}

func (scan *localScan) blob(path string, desc v1.Descriptor) *localBlob {
	// The path includes the repository. Copies in different repositories must
	// be checked independently, and conflicting sizes must remain independent.
	key := fmt.Sprintf("%s/%s/%d", path, desc.Digest, desc.Size)
	if previous, ok := scan.blobs[key]; ok {
		return previous
	}
	blob := &localBlob{path: path, desc: desc}
	scan.blobs[key] = blob
	scan.jobs = append(scan.jobs, blob)
	scan.discovered.Add(1)
	return blob
}

func runLocalBlobChecks(ctx context.Context, blobs []*localBlob, workers int, check func(context.Context, *localBlob) error) error {
	jobs := make(chan *localBlob)
	var wg sync.WaitGroup
	for range min(workers, len(blobs)) {
		wg.Go(func() {
			for blob := range jobs {
				blob.err = ctx.Err()
				if blob.err == nil {
					blob.err = check(ctx, blob)
				}
			}
		})
	}
enqueue:
	for _, blob := range blobs {
		select {
		case <-ctx.Done():
			break enqueue
		case jobs <- blob:
		}
	}
	close(jobs)
	wg.Wait()
	return ctx.Err()
}

func (scan *localScan) verifyBlob(ctx context.Context, root string, blob *localBlob) error {
	if err := safeMirrorFile(root, blob.path); err != nil {
		return err
	}
	f, err := os.Open(blob.path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	// A wrong size is already invalid. Avoid reading a potentially enormous
	// file just to establish the same failure at the end of hashing.
	if !info.Mode().IsRegular() || info.Size() != blob.desc.Size {
		return fmt.Errorf("content does not match size of %s", blob.desc.Digest)
	}
	return copyContent(io.Discard, &verificationReader{ctx: ctx, reader: f, bytes: &scan.bytes}, blob.desc)
}

type verificationReader struct {
	ctx    context.Context
	reader io.Reader
	bytes  *atomic.Int64
}

func (r *verificationReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.reader.Read(p)
	r.bytes.Add(int64(n))
	return n, err
}

func (scan *localScan) progress(output io.Writer, workers int) func() {
	fmt.Fprintf(output, "Scanning local images for nginx (%d verification workers)...\n", workers)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				fmt.Fprintf(output, "Local verification: %d/%d discovered blobs, %.1f MiB read\n",
					scan.checked.Load(), scan.discovered.Load(), float64(scan.bytes.Load())/(1<<20))
			}
		}
	}()
	return func() { close(stop); <-done }
}
