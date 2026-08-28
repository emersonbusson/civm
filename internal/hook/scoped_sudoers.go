package hook

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/emersonbusson/civm/internal/civm"
)

// File modes for the two installed artifacts. The wrapper must be world-readable
// and executable (sudo execs it as root); the sudoers drop-in must be 0440
// root:root or sudo refuses to load it.
const (
	safeDeleteWrapperPerm = os.FileMode(0o755)
	scopedSudoersPerm     = os.FileMode(0o440)
	// Suffix for the staging file written before visudo validates the sudoers.
	sudoersTempSuffix = ".tmp"
)

// installScopedSudoers installs the privileged fixed-command wrappers and the
// scoped sudoers drop-in that whitelists exactly those wrappers under NOPASSWD.
//
// Single source of truth: both artifacts are read from DeploySourceDir via
// opts.ReadFileFn (mirrors DefaultUnitsSourceDir / ScriptContent). go:embed is
// forbidden across the package boundary (DT-v2-5), so the content lives only in
// deploy/ on disk and is shipped to /opt/civm/deploy at provisioning time.
//
// Fail-closed activation of the sudoers: the file is written to a temp path,
// validated with `visudo -c -f <temp>`, and only renamed into place on success.
// A visudo rejection leaves NO active drop-in (the temp is removed) and returns
// an error. Idempotent: re-running overwrites the wrapper and re-validates the
// sudoers, converging on the same bytes/perms.
//
// Every side effect is injected (ReadFileFn/WriteFileFn/MkdirAllFn/RunFn/
// RenameFn/RemoveFn) so unit tests never touch real /etc, sudo, or visudo.
func installScopedSudoers(ctx context.Context, opts InstallOptions) error {
	if err := installSafeDeleteWrapper(opts); err != nil {
		return err
	}
	if err := installGenerationBoundaryWrapper(opts); err != nil {
		return err
	}
	return installSudoersDropIn(ctx, opts)
}

// installSafeDeleteWrapper copies the versioned wrapper from DeploySourceDir to
// civm.DefaultSafeDeleteWrapperPath, root-owned 0755. Ownership is the install
// process's (the bootstrap runs `hook install --execute` under sudo), so we only
// set the mode here; the parent dir is ensured first.
func installSafeDeleteWrapper(opts InstallOptions) error {
	return installPrivilegedWrapper(
		opts,
		civm.DefaultSafeDeleteWrapperSource,
		civm.DefaultSafeDeleteWrapperPath,
		"safedelete",
	)
}

func installGenerationBoundaryWrapper(opts InstallOptions) error {
	return installPrivilegedWrapper(
		opts,
		civm.DefaultGenerationBoundaryWrapperSource,
		civm.DefaultGenerationBoundaryWrapperPath,
		"generation-boundary",
	)
}

func installPrivilegedWrapper(opts InstallOptions, source, destination, name string) error {
	src := filepath.Join(opts.DeploySourceDir, source)
	content, err := opts.ReadFileFn(src)
	if err != nil {
		return fmt.Errorf("read %s wrapper source %s: %w", name, src, err)
	}
	if len(content) == 0 {
		return fmt.Errorf("%s wrapper source %s is empty", name, src)
	}
	if err := opts.MkdirAllFn(filepath.Dir(destination), 0o755); err != nil {
		return fmt.Errorf("mkdir for %s wrapper %s: %w", name, destination, err)
	}
	if err := opts.WriteFileFn(destination, content, safeDeleteWrapperPerm); err != nil {
		return fmt.Errorf("write %s wrapper %s: %w", name, destination, err)
	}
	return nil
}

// installSudoersDropIn writes the scoped sudoers to a temp file, validates it
// with visudo, and atomically renames it into place. On any failure the temp is
// best-effort removed and the existing active drop-in (if any) is left untouched.
func installSudoersDropIn(ctx context.Context, opts InstallOptions) error {
	src := filepath.Join(opts.DeploySourceDir, civm.DefaultScopedSudoersSource)
	content, err := opts.ReadFileFn(src)
	if err != nil {
		return fmt.Errorf("read scoped sudoers source %s: %w", src, err)
	}
	if len(content) == 0 {
		return fmt.Errorf("scoped sudoers source %s is empty", src)
	}
	final := civm.DefaultScopedSudoersDropIn
	tmp := final + sudoersTempSuffix
	if err := opts.MkdirAllFn(filepath.Dir(final), 0o755); err != nil {
		return fmt.Errorf("mkdir for sudoers drop-in %s: %w", final, err)
	}
	// Write the staging file at the final 0440 perm so a rename does not widen it.
	if err := opts.WriteFileFn(tmp, content, scopedSudoersPerm); err != nil {
		return fmt.Errorf("write sudoers temp %s: %w", tmp, err)
	}
	// visudo -c -f validates syntax without activating. Fail closed: a rejection
	// removes the temp and aborts before any rename, so the box is never left
	// with a broken sudoers that could lock out sudo entirely.
	if out, err := opts.RunFn(ctx, "visudo", "-c", "-f", tmp); err != nil {
		_ = opts.RemoveFn(tmp)
		return fmt.Errorf("visudo rejected scoped sudoers (%s): %w", trimVisudoOutput(out), err)
	}
	if err := opts.RenameFn(tmp, final); err != nil {
		_ = opts.RemoveFn(tmp)
		return fmt.Errorf("activate sudoers drop-in %s: %w", final, err)
	}
	return nil
}

// trimVisudoOutput keeps the visudo diagnostic short and free of embedded NULs
// for the wrapped error message.
func trimVisudoOutput(out []byte) string {
	const max = 200
	s := string(out)
	if len(s) > max {
		s = s[:max]
	}
	return s
}
