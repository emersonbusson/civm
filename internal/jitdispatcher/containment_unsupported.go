//go:build !linux

package jitdispatcher

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"time"
)

type CgroupContainment struct{}

func (CgroupContainment) Prepare(*exec.Cmd, string) (io.Closer, error) {
	return nil, fmt.Errorf("%w", ErrUnsupportedPlatform)
}

func (CgroupContainment) Attach(int, string) (ProcessIdentity, error) {
	return ProcessIdentity{}, fmt.Errorf("%w", ErrUnsupportedPlatform)
}

func (CgroupContainment) Terminate(context.Context, ProcessIdentity, time.Duration) error {
	return fmt.Errorf("%w", ErrUnsupportedPlatform)
}

func (CgroupContainment) Recover(context.Context, string, ProcessIdentity, time.Duration) error {
	return fmt.Errorf("%w", ErrUnsupportedPlatform)
}

func (CgroupContainment) Alive(ProcessIdentity) (bool, error) {
	return false, fmt.Errorf("%w", ErrUnsupportedPlatform)
}
