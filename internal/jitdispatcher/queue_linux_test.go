//go:build linux

package jitdispatcher

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireQueueSerializesAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dispatcher.lock")
	first, err := AcquireQueue(context.Background(), path, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := AcquireQueue(ctx, path, time.Millisecond); !errors.Is(err, ErrBusy) {
		t.Fatalf("second AcquireQueue() error = %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := AcquireQueue(context.Background(), path, time.Millisecond)
	if err != nil {
		t.Fatalf("AcquireQueue() after release error = %v", err)
	}
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
}
