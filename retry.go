package main

import (
	"context"
	"fmt"
	"os"
	"time"
)

const downloadAttempts = 4 // The initial attempt, followed by three retries.

func retryDownload(ctx context.Context, file string, download func() error) error {
	var err error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err = download(); err == nil {
			return nil
		}
		fmt.Fprintf(os.Stderr, "%s: attempt %d/%d failed: %v\n", file, attempt, downloadAttempts, err)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt < downloadAttempts {
			timer := time.NewTimer(100 * time.Millisecond << (attempt - 1))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("skipping %s after %d attempts: %w", file, downloadAttempts, err)
}
