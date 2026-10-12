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

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

var repositoryPattern = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
var tagPattern = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)

type mirror struct {
	dir     string
	repo    name.Repository
	puller  *remote.Puller
	headers map[string]v1.Descriptor
	seen    map[string]bool
}

func validReference(repo, ref string) bool {
	_, err := v1.NewHash(ref)
	return repositoryPattern.MatchString(repo) && (tagPattern.MatchString(ref) || err == nil)
}

func download(ctx context.Context, ref name.Reference, dir string) error {
	// shortcut: imports into one directory must run serially; add a lock for concurrent writers.
	if !validReference(ref.Context().RepositoryStr(), ref.Identifier()) {
		return fmt.Errorf("invalid repository or reference: %s", ref)
	}
	puller, err := remote.NewPuller(remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err != nil {
		return err
	}
	m := mirror{dir, ref.Context(), puller, make(map[string]v1.Descriptor), make(map[string]bool)}
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
	desc, err := m.manifest(ctx, ref, nil)
	if err != nil {
		return err
	}
	// Publish the tag only after every referenced manifest and blob is complete.
	if err := storeContent(m.path("manifests", ref.Identifier()), bytes.NewReader(desc.Manifest), desc.Descriptor); err != nil {
		return err
	}
	m.headers[ref.Identifier()] = desc.Descriptor
	return atomicFile(metadata, func(w io.Writer) error { return json.NewEncoder(w).Encode(m.headers) })
}

func (m *mirror) path(kind, ref string) string {
	return filepath.Join(m.dir, "v2", filepath.FromSlash(m.repo.RepositoryStr()), kind, ref)
}

func (m *mirror) manifest(ctx context.Context, ref name.Reference, expected *v1.Descriptor) (*remote.Descriptor, error) {
	desc, err := m.puller.Get(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("fetch manifest %s: %w", ref, err)
	}
	if expected != nil && (desc.Digest != expected.Digest || desc.Size != expected.Size) {
		return nil, fmt.Errorf("manifest %s does not match index descriptor", ref)
	}
	if err := copyContent(io.Discard, bytes.NewReader(desc.Manifest), desc.Descriptor); err != nil {
		return nil, err
	}
	if m.seen[desc.Digest.String()] {
		return desc, nil
	}
	var doc struct {
		SchemaVersion int             `json:"schemaVersion"`
		MediaType     types.MediaType `json:"mediaType"`
		Config        v1.Descriptor   `json:"config"`
		Layers        []v1.Descriptor `json:"layers"`
		Manifests     []v1.Descriptor `json:"manifests"`
	}
	if err := json.Unmarshal(desc.Manifest, &doc); err != nil {
		return nil, err
	}
	if doc.SchemaVersion != 2 || (doc.MediaType != "" && doc.MediaType != desc.MediaType) {
		return nil, fmt.Errorf("invalid manifest schema or media type: %s", ref)
	}
	m.seen[desc.Digest.String()] = true
	fmt.Fprintf(os.Stderr, "manifest %s\n", ref)
	switch desc.MediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		for _, child := range doc.Manifests {
			if _, err := m.manifest(ctx, m.repo.Digest(child.Digest.String()), &child); err != nil {
				return nil, err
			}
		}
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		for _, blob := range append([]v1.Descriptor{doc.Config}, doc.Layers...) {
			if err := m.blob(ctx, blob); err != nil {
				return nil, fmt.Errorf("fetch blob %s: %w", blob.Digest, err)
			}
		}
	default:
		return nil, fmt.Errorf("unsupported manifest media type %q", desc.MediaType)
	}
	if err := storeContent(m.path("manifests", desc.Digest.String()), bytes.NewReader(desc.Manifest), desc.Descriptor); err != nil {
		return nil, err
	}
	m.headers[desc.Digest.String()] = desc.Descriptor
	return desc, nil
}

func (m *mirror) blob(ctx context.Context, desc v1.Descriptor) error {
	path := m.path("blobs", desc.Digest.String())
	if validContent(path, desc) {
		return nil
	}
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
}
