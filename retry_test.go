package main

import (
	"context"
	"errors"
	"testing"
)

func TestRetryDownload(t *testing.T) {
	failure := errors.New("download failed")
	for _, succeed := range []bool{true, false} {
		attempts := 0
		err := retryDownload(t.Context(), "fixture", func() error {
			attempts++
			if succeed && attempts == downloadAttempts {
				return nil
			}
			return failure
		})
		if attempts != 4 || (err == nil) != succeed || (!succeed && !errors.Is(err, failure)) {
			t.Fatalf("attempts=%d, succeed=%v, error=%v", attempts, succeed, err)
		}
	}
}

func TestRetryDownloadCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	attempts := 0
	err := retryDownload(ctx, "fixture", func() error {
		attempts++
		cancel()
		return errors.New("interrupted")
	})
	if attempts != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("attempts=%d, error=%v", attempts, err)
	}
}
