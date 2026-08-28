//go:build !linux

package jitdispatcher

import (
	"context"
	"fmt"
	"time"
)

type ExecResourceGate struct{}

func NewExecResourceGate(string) *ExecResourceGate { return &ExecResourceGate{} }

func (*ExecResourceGate) Preflight(string) error {
	return fmt.Errorf("%w", ErrUnsupportedPlatform)
}

func (*ExecResourceGate) Acquire(context.Context, string, string, time.Duration, time.Duration) (ResourceLease, error) {
	return nil, fmt.Errorf("%w", ErrUnsupportedPlatform)
}

func (*ExecResourceGate) Orphan() (LeaseMarker, bool, error) {
	return LeaseMarker{}, false, fmt.Errorf("%w", ErrUnsupportedPlatform)
}

func (*ExecResourceGate) ReleaseOrphan(context.Context, LeaseMarker, time.Duration) error {
	return fmt.Errorf("%w", ErrUnsupportedPlatform)
}
