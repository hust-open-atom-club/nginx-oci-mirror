package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
)

func readImageFile(path string) ([]name.Reference, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open image file %q: %w", path, err)
	}
	defer f.Close()
	var refs []name.Reference
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		image := strings.TrimSpace(scanner.Text())
		if image == "" || strings.HasPrefix(image, "#") {
			continue
		}
		ref, err := name.ParseReference(image)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if !validReference(ref.Context().RepositoryStr(), ref.Identifier()) {
			return nil, fmt.Errorf("%s:%d: invalid image reference %q", path, line, image)
		}
		refs = append(refs, ref)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%s:%d: read image file: %w", path, line+1, err)
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("%s: image file contains no images", path)
	}
	return refs, nil
}
