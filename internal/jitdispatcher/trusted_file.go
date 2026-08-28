package jitdispatcher

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

func validateTrustedExecutable(path, expectedSHA256 string) error {
	file, err := openTrustedExecutable(path, expectedSHA256)
	if err != nil {
		return err
	}
	return file.Close()
}

// openTrustedExecutable validates the already-open file descriptor. Callers
// can execute that descriptor through /proc/self/fd so a path replacement
// cannot swap the binary between digest validation and exec.
func openTrustedExecutable(path, expectedSHA256 string) (*os.File, error) {
	if err := validateAbsoluteExecutable("executable", path); err != nil {
		return nil, err
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	file, err := openNoFollow(path)
	if err != nil {
		return nil, fmt.Errorf("trusted executable open: %w", err)
	}
	ok := false
	defer func() {
		if !ok {
			_ = file.Close()
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("trusted executable stat: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 ||
		info.Mode().Perm()&0o022 != 0 || !ownedByTrustedAdmin(info) {
		return nil, fmt.Errorf("trusted executable metadata is unsafe")
	}
	if expectedSHA256 == "" {
		ok = true
		return file, nil
	}
	if !digestRE.MatchString(expectedSHA256) {
		return nil, fmt.Errorf("%w: trusted executable digest is invalid", ErrInvalid)
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, (256<<20)+1)); err != nil {
		return nil, fmt.Errorf("trusted executable hash: %w", err)
	}
	if info.Size() > 256<<20 || hex.EncodeToString(hash.Sum(nil)) != expectedSHA256 {
		return nil, fmt.Errorf("trusted executable digest mismatch")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("trusted executable rewind: %w", err)
	}
	ok = true
	return file, nil
}

func validateTrustedDirectory(name, path string) error {
	if err := validateAbsoluteDir(name, path); err != nil {
		return err
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%s lstat: %w", name, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || !ownedByTrustedAdmin(info) {
		return fmt.Errorf("%s metadata is unsafe", name)
	}
	return nil
}

func readOwnerOnlyRegular(path string, maxBytes int64) ([]byte, bool, error) {
	if maxBytes <= 0 {
		return nil, false, fmt.Errorf("%w: file size limit is invalid", ErrInvalid)
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, false, err
	}
	file, err := openNoFollow(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return nil, false, fmt.Errorf("file is not an owner-only regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) == 0 || int64(len(data)) > maxBytes {
		return nil, false, fmt.Errorf("file size is invalid")
	}
	return data, true, nil
}
