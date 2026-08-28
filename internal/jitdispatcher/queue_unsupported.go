//go:build !linux

package jitdispatcher

import (
	"context"
	"fmt"
	"io"
	"time"
)

func AcquireQueue(context.Context, string, time.Duration) (io.Closer, error) {
	return nil, fmt.Errorf("%w", ErrUnsupportedPlatform)
}
