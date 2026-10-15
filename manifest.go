package main

import (
	"encoding/json"
	"fmt"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/types"
)

type manifestDocument struct {
	SchemaVersion int             `json:"schemaVersion"`
	MediaType     types.MediaType `json:"mediaType"`
	Config        v1.Descriptor   `json:"config"`
	Layers        []v1.Descriptor `json:"layers"`
	Manifests     []v1.Descriptor `json:"manifests"`
}

func sameDescriptor(a, b v1.Descriptor) bool {
	return a.Digest == b.Digest && a.Size == b.Size && a.MediaType == b.MediaType
}

func parseManifest(data []byte, mediaType types.MediaType) (manifestDocument, error) {
	var doc manifestDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, err
	}
	if doc.SchemaVersion != 2 || (doc.MediaType != "" && doc.MediaType != mediaType) {
		return doc, fmt.Errorf("invalid manifest schema or media type %q", mediaType)
	}
	switch mediaType {
	case types.OCIImageIndex, types.DockerManifestList:
		if doc.Manifests == nil || doc.Config.Digest.Algorithm != "" || doc.Layers != nil {
			return doc, fmt.Errorf("invalid image index")
		}
	case types.OCIManifestSchema1, types.DockerManifestSchema2:
		if doc.Manifests != nil || doc.Config.Digest.Algorithm == "" || doc.Layers == nil {
			return doc, fmt.Errorf("invalid image manifest")
		}
	default:
		return doc, fmt.Errorf("unsupported manifest media type %q", mediaType)
	}
	doc.MediaType = mediaType
	for _, desc := range append(doc.blobs(), doc.Manifests...) {
		if _, err := v1.NewHash(desc.Digest.String()); err != nil || desc.Size < 0 {
			return doc, fmt.Errorf("invalid content descriptor %s", desc.Digest)
		}
	}
	return doc, nil
}

func (doc manifestDocument) blobs() []v1.Descriptor {
	if doc.MediaType == types.OCIImageIndex || doc.MediaType == types.DockerManifestList {
		return nil
	}
	return append([]v1.Descriptor{doc.Config}, doc.Layers...)
}
