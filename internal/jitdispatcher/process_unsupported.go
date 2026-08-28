//go:build !linux

package jitdispatcher

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

func configureProcess(*exec.Cmd)           {}
func configurePersistentProcess(*exec.Cmd) {}

func currentProcessStart() (uint64, error)  { return 0, fmt.Errorf("%w", ErrUnsupportedPlatform) }
func processStartTicks(int) (uint64, error) { return 0, fmt.Errorf("%w", ErrUnsupportedPlatform) }
func processIdentityAlive(int, uint64) (bool, error) {
	return false, fmt.Errorf("%w", ErrUnsupportedPlatform)
}
func terminateExactProcess(<-chan struct{}, int, uint64, time.Duration) error {
	return fmt.Errorf("%w", ErrUnsupportedPlatform)
}

func signalProcessGroup(int, bool) error {
	return fmt.Errorf("%w", ErrUnsupportedPlatform)
}

func openNoFollow(string) (*os.File, error) {
	return nil, fmt.Errorf("%w", ErrUnsupportedPlatform)
}

func openFileNoFollow(string, int, os.FileMode) (*os.File, error) {
	return nil, fmt.Errorf("%w", ErrUnsupportedPlatform)
}
