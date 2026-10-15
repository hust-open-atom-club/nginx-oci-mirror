package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// The files on disk are authoritative. Metadata is only a media-type hint for
// older manifests that omit mediaType; missing or stale metadata cannot hide a
// valid image or make a broken dependency graph publishable.
func localManifests(ctx context.Context, dir string, workers int, progress io.Writer) (map[string]map[string]v1.Descriptor, error) {
	return (&localScan{}).manifests(ctx, dir, workers, progress)
}

func (scan *localScan) manifests(ctx context.Context, dir string, workers int, progress io.Writer) (result map[string]map[string]v1.Descriptor, err error) {
	if workers < 1 || workers > maxParallelVerify {
		return nil, fmt.Errorf("parallel verification must be between 1 and %d", maxParallelVerify)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	scan.blobs = make(map[string]*localBlob)
	scan.jobs = nil
	scan.discovered.Store(0)
	scan.checked.Store(0)
	scan.bytes.Store(0)
	started := time.Now()
	stopProgress := scan.progress(progress, workers)
	defer func() {
		stopProgress()
		status := "finished"
		if err != nil {
			status = "stopped"
		}
		fmt.Fprintf(progress, "Local verification %s: %d/%d blobs, %.1f MiB read, %s\n", status,
			scan.checked.Load(), scan.discovered.Load(), float64(scan.bytes.Load())/(1<<20), time.Since(started).Round(time.Millisecond))
	}()
	root := filepath.Join(dir, "v2")
	files := make(map[string][]string)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err != nil {
			return err
		}
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || filepath.Base(filepath.Dir(path)) != "manifests" {
			return nil
		}
		repo, err := filepath.Rel(root, filepath.Dir(filepath.Dir(path)))
		if err != nil {
			return err
		}
		repo = filepath.ToSlash(repo)
		if validReference(repo, entry.Name()) {
			files[repo] = append(files[repo], entry.Name())
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		if _, rootErr := os.Lstat(root); errors.Is(rootErr, os.ErrNotExist) {
			return map[string]map[string]v1.Descriptor{}, nil
		}
	}
	if err != nil {
		return nil, err
	}
	type candidate struct {
		repo, ref, path string
		desc            v1.Descriptor
		node            *localManifest
	}
	var candidates []candidate
	for repo, refs := range files {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		manifestDir := filepath.Join(root, filepath.FromSlash(repo), "manifests")
		hints := make(map[string]v1.Descriptor)
		metadata := filepath.Join(manifestDir, ".metadata.json")
		if safeMirrorFile(dir, metadata) == nil {
			data, err := os.ReadFile(metadata)
			if err == nil {
				err = json.Unmarshal(data, &hints)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "ignoring invalid manifest metadata %s: %v\n", metadata, err)
			}
		}
		validator := localValidator{ctx: ctx, dir: dir, repo: repo, checked: make(map[string]*localManifest), visiting: make(map[string]bool), scan: scan}
		for _, ref := range refs {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			path := filepath.Join(manifestDir, ref)
			desc, err := localDescriptor(dir, path, ref, hints[ref])
			node := &localManifest{err: err}
			if err == nil {
				node = validator.manifest(desc)
			}
			candidates = append(candidates, candidate{repo, ref, path, desc, node})
		}
	}
	// Discover all independent files first so one global bounded pool can keep
	// busy across repositories, nested indexes, and single-layer manifests.
	if err := runLocalBlobChecks(ctx, scan.jobs, workers, func(ctx context.Context, blob *localBlob) error {
		err := scan.verifyBlob(ctx, dir, blob)
		scan.checked.Add(1)
		return err
	}); err != nil {
		return nil, err
	}
	result = make(map[string]map[string]v1.Descriptor)
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := candidate.node.validate(); err != nil {
			fmt.Fprintf(os.Stderr, "skipping invalid local manifest %s: %v\n", candidate.path, err)
			continue
		}
		if result[candidate.repo] == nil {
			result[candidate.repo] = make(map[string]v1.Descriptor)
		}
		result[candidate.repo][candidate.ref] = candidate.desc
	}
	return result, nil
}

func localDescriptor(root, path, ref string, hint v1.Descriptor) (v1.Descriptor, error) {
	var desc v1.Descriptor
	if err := safeMirrorFile(root, path); err != nil {
		return desc, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return desc, err
	}
	var doc manifestDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return desc, err
	}
	desc.Digest, desc.Size, err = v1.SHA256(bytes.NewReader(data))
	if err != nil {
		return desc, err
	}
	if digest, err := v1.NewHash(ref); err == nil {
		desc.Digest = digest
	}
	desc.MediaType = doc.MediaType
	if copyContent(io.Discard, bytes.NewReader(data), hint) == nil {
		// A verified historical descriptor also preserves sha512 tag imports.
		if tagPattern.MatchString(ref) {
			desc.Digest = hint.Digest
		}
		if desc.MediaType == "" {
			desc.MediaType = hint.MediaType
		}
	}
	if desc.MediaType == "" {
		// Docker schema 2 permits omitting mediaType. Infer it only from the
		// manifest shape and its typed descriptors when no verified hint exists.
		desc.MediaType = types.DockerManifestSchema2
		if doc.Config.MediaType == types.OCIConfigJSON {
			desc.MediaType = types.OCIManifestSchema1
		}
		if doc.Manifests != nil {
			desc.MediaType = types.DockerManifestList
			for _, child := range doc.Manifests {
				if child.MediaType == types.OCIManifestSchema1 || child.MediaType == types.OCIImageIndex {
					desc.MediaType = types.OCIImageIndex
				}
			}
		}
	}
	if err := copyContent(io.Discard, bytes.NewReader(data), desc); err != nil {
		return desc, err
	}
	return desc, nil
}

// Graph discovery is serial. Workers only fill localBlob.err, and graph
// completeness is evaluated after they have all joined.
type localManifest struct {
	err       error
	validated bool
	children  []*localManifest
	blobs     []*localBlob
}

func (node *localManifest) validate() error {
	if node.validated || node.err != nil {
		return node.err
	}
	node.validated = true
	for _, child := range node.children {
		if err := child.validate(); err != nil {
			node.err = err
			return err
		}
	}
	for _, blob := range node.blobs {
		if blob.err != nil {
			node.err = fmt.Errorf("missing or corrupt blob %s: %w", blob.desc.Digest, blob.err)
			return node.err
		}
	}
	return nil
}

type localValidator struct {
	ctx      context.Context
	dir      string
	repo     string
	checked  map[string]*localManifest
	visiting map[string]bool
	scan     *localScan
}

func (v *localValidator) manifest(desc v1.Descriptor) *localManifest {
	key := fmt.Sprintf("%s/%d/%s", desc.Digest, desc.Size, desc.MediaType)
	if v.visiting[desc.Digest.String()] {
		return &localManifest{err: fmt.Errorf("manifest cycle at %s", desc.Digest)}
	}
	if previous, ok := v.checked[key]; ok {
		return previous
	}
	node := &localManifest{}
	v.checked[key] = node
	v.visiting[desc.Digest.String()] = true
	defer delete(v.visiting, desc.Digest.String())
	node.err = v.discover(desc, node)
	return node
}

func (v *localValidator) discover(desc v1.Descriptor, node *localManifest) error {
	if err := v.ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(v.dir, "v2", filepath.FromSlash(v.repo), "manifests", desc.Digest.String())
	if err := safeMirrorFile(v.dir, path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := copyContent(io.Discard, bytes.NewReader(data), desc); err != nil {
		return err
	}
	doc, err := parseManifest(data, desc.MediaType)
	if err != nil {
		return err
	}
	for _, child := range doc.Manifests {
		node.children = append(node.children, v.manifest(child))
	}
	for _, desc := range doc.blobs() {
		path := filepath.Join(v.dir, "v2", filepath.FromSlash(v.repo), "blobs", desc.Digest.String())
		node.blobs = append(node.blobs, v.scan.blob(path, desc))
	}
	return nil
}

// Do not follow symlinked files or directories outside the mirror during scans.
func safeMirrorFile(root, path string) error {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("file outside mirror: %s", path)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		root = filepath.Join(root, part)
		info, err := os.Lstat(root)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || (i == len(parts)-1 && !info.Mode().IsRegular()) {
			return fmt.Errorf("not a regular mirror file: %s", root)
		}
	}
	return nil
}
