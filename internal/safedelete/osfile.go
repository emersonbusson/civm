package safedelete

import (
	"context"
	"io/fs"
	"os/exec"
	"syscall"
)

// defaultRun executes the privileged wrapper via sudo without a shell.
func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// defaultFileOwnerUID reads the owning uid from a Unix stat result. Returns
// ok=false when the FileInfo carries no syscall.Stat_t (non-Unix), so the
// caller fails closed instead of assuming ownership.
func defaultFileOwnerUID(info fs.FileInfo) (int, bool) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || st == nil {
		return 0, false
	}
	return int(st.Uid), true
}
