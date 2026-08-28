//go:build linux

package jitdispatcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"
)

type queueLock struct {
	file *os.File
}

func AcquireQueue(ctx context.Context, path string, poll time.Duration) (io.Closer, error) {
	if poll <= 0 {
		return nil, fmt.Errorf("%w: queue poll must be positive", ErrInvalid)
	}
	descriptor, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("queue open: %w", err)
	}
	file := os.NewFile(uintptr(descriptor), path)
	if file == nil {
		_ = syscall.Close(descriptor)
		return nil, fmt.Errorf("queue file allocation failed")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByTrustedAdmin(info) {
		_ = file.Close()
		return nil, errors.Join(fmt.Errorf("queue file metadata is unsafe"), err)
	}
	for {
		err = syscall.Flock(descriptor, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &queueLock{file: file}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("queue flock: %w", err)
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, fmt.Errorf("%w: %v", ErrBusy, ctx.Err())
		case <-timer.C:
		}
	}
}

func (lock *queueLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	descriptor := int(lock.file.Fd())
	unlockErr := syscall.Flock(descriptor, syscall.LOCK_UN)
	closeErr := lock.file.Close()
	lock.file = nil
	return errors.Join(unlockErr, closeErr)
}
