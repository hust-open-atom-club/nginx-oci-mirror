package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

// The files on disk are authoritative. Metadata is only a media-type hint for
// older manifests that omit mediaType; missing or stale metadata cannot hide a
// valid image or make a broken dependency graph publishable.
func localManifests(dir string) (map[string]map[string]v1.Descriptor, error) {
	root := filepath.Join(dir, "v2")
	files := make(map[string][]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
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
	result := make(map[string]map[string]v1.Descriptor)
	for repo, refs := range files {
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
		validator := localValidator{dir: dir, repo: repo, checked: make(map[string]error), visiting: make(map[string]bool), blobs: make(map[string]bool)}
		for _, ref := range refs {
			path := filepath.Join(manifestDir, ref)
			desc, err := localDescriptor(dir, path, ref, hints[ref])
			if err == nil {
				err = validator.manifest(desc)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "skipping invalid local manifest %s: %v\n", path, err)
				continue
			}
			if result[repo] == nil {
				result[repo] = make(map[string]v1.Descriptor)
			}
			result[repo][ref] = desc
		}
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

type localValidator struct {
	dir      string
	repo     string
	checked  map[string]error
	visiting map[string]bool
	blobs    map[string]bool
}

func (v *localValidator) manifest(desc v1.Descriptor) (err error) {
	key := fmt.Sprintf("%s/%d/%s", desc.Digest, desc.Size, desc.MediaType)
	if result, ok := v.checked[key]; ok {
		return result
	}
	if v.visiting[desc.Digest.String()] {
		return fmt.Errorf("manifest cycle at %s", desc.Digest)
	}
	v.visiting[desc.Digest.String()] = true
	defer func() {
		delete(v.visiting, desc.Digest.String())
		v.checked[key] = err
	}()
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
		if err := v.manifest(child); err != nil {
			return fmt.Errorf("child %s: %w", child.Digest, err)
		}
	}
	for _, blob := range doc.blobs() {
		key := fmt.Sprintf("%s/%d", blob.Digest, blob.Size)
		if v.blobs[key] {
			continue
		}
		path := filepath.Join(v.dir, "v2", filepath.FromSlash(v.repo), "blobs", blob.Digest.String())
		if err := safeMirrorFile(v.dir, path); err != nil {
			return err
		}
		if !validContent(path, blob) {
			return fmt.Errorf("missing or corrupt blob %s", blob.Digest)
		}
		v.blobs[key] = true
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
