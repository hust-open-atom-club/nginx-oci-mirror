package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// graphSize uses already verified descriptor lengths. Only graphs accepted
// into the final inventory are added; successful reads from discarded graphs
// do not contribute. Tags share their canonical manifest's backing path.
type graphSize struct {
	files map[string]bool
	bytes int64
}

func (size *graphSize) addFile(path string, bytes int64) error {
	if size.files[path] {
		return nil
	}
	if bytes < 0 || bytes > math.MaxInt64-size.bytes {
		return fmt.Errorf("invalid total size for %s", path)
	}
	size.files[path] = true
	size.bytes += bytes
	return nil
}

func (size *graphSize) addManifest(ctx context.Context, node *localManifest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if size.files[node.path] {
		return nil
	}
	if err := size.addFile(node.path, node.size); err != nil {
		return err
	}
	for _, blob := range node.blobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := size.addFile(blob.path, blob.desc.Size); err != nil {
			return err
		}
	}
	for _, child := range node.children {
		if err := size.addManifest(ctx, child); err != nil {
			return err
		}
	}
	return nil
}

// localMirrorSize counts retained regular-file lengths, not allocated disk
// blocks or bytes read during verification. Walking paths counts shared blobs
// once without following manifest references or reading any file contents.
func localMirrorSize(ctx context.Context, dir string) (int64, error) {
	root := filepath.Join(dir, "v2")
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A new mirror may have no data, for example if every download failed.
		// Missing entries inside an existing tree must instead fail the count.
		if path == root && errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".download-") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// WalkDir does not follow symlinks, including a symlinked v2 root.
		if path == root && !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			return fmt.Errorf("mirror root is not a directory: %s", root)
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Size() > math.MaxInt64-total {
			return fmt.Errorf("mirror size exceeds %d bytes", int64(math.MaxInt64))
		}
		total += info.Size()
		return nil
	})
	if err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	return total, nil
}

// formatSize uses binary units and at most two decimal places. Round before
// selecting the final unit so a boundary value never appears as 1024K, etc.
func formatSize(bytes int64) string {
	if bytes < 1024 {
		return strconv.FormatInt(bytes, 10)
	}
	value := float64(bytes)
	const units = "KMGTP"
	unit := -1
	for value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	value = math.Round(value*100) / 100
	if value >= 1024 && unit < len(units)-1 {
		value /= 1024
		unit++
	}
	return strconv.FormatFloat(value, 'f', -1, 64) + string(units[unit])
}
