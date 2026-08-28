// Package safedelete removes a path that the runner user owns, escalating to a
// guarded privileged wrapper only when the unprivileged delete is blocked by a
// root-owned file.
//
// Why this exists (docs/specs/civm-runner-reliability, DT-v2-1/3/5/7/8):
// CI Docker steps that run as root write files into the mounted runner _work
// (e.g. generated docs/events-catalog.json). The runner user (emdev) then
// cannot unlink them, so both the GitHub runner checkout-cleanup AND the civm
// job-completed hook fail with EACCES (unlinkat ... permission denied). The job
// fails at "Complete runner" and every subsequent job on that runner breaks.
// This is repo-agnostic and the #1 reliability killer on the box.
//
// The escalation is bounded by defense-in-depth: an internal fixed validation
// (abs, no NUL, not /, $HOME or a bare /home/<x>), a caller-supplied GuardFn
// proving the path is a direct child of a validated _work / cleanup root, and
// an EvalSymlinks + Lstat ownership check that re-asserts the resolved target
// still lives under a real runner-owned tree before any sudo. The ONLY binary
// sudo authorizes is the wrapper (civm.DefaultSafeDeleteWrapperPath); chown/rm
// absolutes are invoked from inside it, already root.
package safedelete

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/emersonbusson/civm/internal/civm"
)

// ErrUnsafePath is returned (without ever invoking sudo) when a path fails the
// internal validation, the caller GuardFn, or the symlink/ownership re-check.
var ErrUnsafePath = errors.New("safedelete: unsafe path")

// rootUID is the only non-runner owner an escalation target may have: a
// Docker-as-root CI step is the reason root-owned entries appear under _work.
const rootUID = 0

// Options injects every side effect so unit tests never touch real sudo or the
// real filesystem outside t.TempDir(). Defaults wrap os / exec.
type Options struct {
	// WrapperPath is the absolute path of the privileged validated wrapper that
	// sudo whitelists. Defaults to civm.DefaultSafeDeleteWrapperPath.
	WrapperPath string
	// GuardFn proves path is a direct child of a validated runner _work /
	// cleanup root. It runs BEFORE any removal. A nil GuardFn is a hard reject:
	// the caller must always scope the escalation. See DT-v2-9.
	GuardFn func(path string) error
	// RunFn runs the privileged wrapper via sudo. Injected so tests do not call
	// real sudo. Defaults to exec.CommandContext output.
	RunFn func(ctx context.Context, name string, args ...string) ([]byte, error)
	// RemoveAllFn is the unprivileged delete tried first. Defaults to
	// os.RemoveAll.
	RemoveAllFn func(path string) error
	// EvalSymlinksFn resolves symlinks before the ownership re-check. Defaults
	// to filepath.EvalSymlinks.
	EvalSymlinksFn func(path string) (string, error)
	// LstatFn stats the resolved target without following a final symlink.
	// Defaults to os.Lstat.
	LstatFn func(path string) (fs.FileInfo, error)
	// OwnerUIDFn reports the uid that must own the resolved target (the runner
	// user). Defaults to os.Getuid. Tests inject a value matching their fixture.
	OwnerUIDFn func() int
	// FileOwnerUIDFn extracts the uid of a stat result. Defaults to a
	// syscall.Stat_t reader. Tests inject to control ownership outcomes without
	// real files.
	FileOwnerUIDFn func(fs.FileInfo) (int, bool)
}

// Result reports what happened so callers can classify the outcome without
// re-parsing error strings (DT-v2-12).
type Result struct {
	// Escalated is true when the privileged wrapper was invoked because the
	// unprivileged delete hit a permission error.
	Escalated bool
	// Err is the terminal error, or nil on success. A non-nil Err after a guard
	// rejection is ErrUnsafePath (sudo was never invoked).
	Err error
}

// Remove deletes path. It first validates and guards the path, then tries an
// unprivileged RemoveAllFn; only on a permission error does it escalate to the
// privileged wrapper (chown -R, retry delete, then rm). The wrapper is the only
// thing sudo authorizes and it re-validates the path on the root side.
//
// Remove never panics and never invokes sudo for a path that fails validation,
// the GuardFn, or the symlink/ownership re-check.
func Remove(ctx context.Context, opts Options, path string) Result {
	applyDefaults(&opts)

	target, err := opts.resolveSafeTarget(path)
	if err != nil {
		return Result{Err: err}
	}

	// Unprivileged delete first — the common case where the runner user owns
	// every file. Success short-circuits without any sudo.
	if err := opts.RemoveAllFn(target); err == nil {
		return Result{}
	} else if !errors.Is(err, fs.ErrPermission) {
		// A non-permission error (ENOSPC, EBUSY, a stale mount) is NOT a
		// root-owned-file situation. Do not escalate; surface it so the caller
		// classifier can decide whether it is fatal (DT-v2-12).
		return Result{Err: err}
	}

	return opts.escalate(ctx, target)
}

// Chown recursively chowns path back to the runner user (OwnerUIDFn) via the
// privileged wrapper, after the SAME validation + guard + owner re-check as
// Remove. It is non-destructive: only ownership changes, no file is removed.
//
// Used at job-started to reclaim a REUSED checkout dir that a prior job's
// containerized (root) step left partially root-owned: actions/checkout deletes
// the previous checkout before fetching, and a root-owned file makes that delete
// fail with EACCES, killing the job at checkout. A plain rmdir by the runner
// user cannot fix it; a recursive chown to the runner user lets checkout clean
// it normally. Chowning a tree owned by the runner OR root is exactly the
// allowed case (resolveAndAffirmOwner); a third user's files are still refused.
func Chown(ctx context.Context, opts Options, path string) Result {
	applyDefaults(&opts)

	target, err := opts.resolveSafeTarget(path)
	if err != nil {
		return Result{Err: err}
	}
	uidgid := fmt.Sprintf("%d:%d", opts.OwnerUIDFn(), opts.OwnerUIDFn())
	if err := opts.runWrapper(ctx, "chown", uidgid, target); err != nil {
		return Result{Escalated: true, Err: fmt.Errorf("wrapper chown failed: %w", err)}
	}
	return Result{Escalated: true}
}

// resolveSafeTarget runs the three independent guards and returns the resolved
// real path that any privileged delete must target. Any failure returns
// ErrUnsafePath and guarantees no sudo runs.
func (opts Options) resolveSafeTarget(path string) (string, error) {
	// (1) Fixed internal validation, independent of the caller.
	clean, err := validateFixed(path)
	if err != nil {
		return "", err
	}
	// (2) Caller scope: direct child of a validated runner _work / cleanup root.
	if opts.GuardFn == nil {
		return "", fmt.Errorf("%w: nil GuardFn (escalation must be scoped)", ErrUnsafePath)
	}
	if err := opts.GuardFn(clean); err != nil {
		return "", fmt.Errorf("%w: guard rejected %q: %s", ErrUnsafePath, clean, err.Error())
	}
	// (3) Symlink + ownership re-check: the resolved target must still live
	// under a real runner-owned tree, never a symlink escaping it (DT-v2-7).
	real, err := opts.resolveAndAffirmOwner(clean)
	if err != nil {
		return "", err
	}
	return real, nil
}

// validateFixed enforces the caller-independent invariants: absolute, no NUL,
// not "/", not $HOME, not a bare /home/<user>. Mirrors validateCleanupRoot /
// removePath / safeWorkRoot so the wrapper and Go caller share one contract.
func validateFixed(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%w: empty path", ErrUnsafePath)
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%w: path contains NUL", ErrUnsafePath)
	}
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) {
		return "", fmt.Errorf("%w: path not absolute: %q", ErrUnsafePath, clean)
	}
	if clean == "/" {
		return "", fmt.Errorf("%w: refusing to delete root", ErrUnsafePath)
	}
	if home := os.Getenv("HOME"); home != "" && clean == filepath.Clean(home) {
		return "", fmt.Errorf("%w: refusing to delete $HOME: %q", ErrUnsafePath, clean)
	}
	if isBareHome(clean) {
		return "", fmt.Errorf("%w: refusing to delete a bare home dir: %q", ErrUnsafePath, clean)
	}
	return clean, nil
}

// isBareHome reports whether clean is exactly /home/<user> (one segment under
// /home), which must never be deleted even if a guard mis-scoped it.
func isBareHome(clean string) bool {
	if !strings.HasPrefix(clean, "/home/") {
		return false
	}
	return strings.Count(strings.Trim(clean, "/"), "/") == 1
}

// resolveAndAffirmOwner resolves symlinks and confirms the resolved target is
// owned by the runner user, refusing a symlink that escapes the validated tree.
// EvalSymlinks failing with not-exist is benign (already deleted) — we keep the
// cleaned path so the caller's RemoveAllFn no-ops. Any other resolution error,
// or an owner mismatch, is ErrUnsafePath with no sudo.
func (opts Options) resolveAndAffirmOwner(clean string) (string, error) {
	real, err := opts.EvalSymlinksFn(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return clean, nil
		}
		return "", fmt.Errorf("%w: cannot resolve %q: %s", ErrUnsafePath, clean, err.Error())
	}
	// When a symlink was followed, the resolved real path must STILL satisfy the
	// fixed invariants AND the caller scope (GuardFn). A symlink whose target
	// escapes the validated _work tree — pointing at /, $HOME or /etc — is
	// rejected here even though the cleaned link path passed (DT-v2-7).
	if real != clean {
		if _, err := validateFixed(real); err != nil {
			return "", err
		}
		if err := opts.GuardFn(real); err != nil {
			return "", fmt.Errorf("%w: resolved target %q escapes the validated tree: %s", ErrUnsafePath, real, err.Error())
		}
	}
	info, err := opts.LstatFn(real)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return real, nil
		}
		return "", fmt.Errorf("%w: cannot lstat %q: %s", ErrUnsafePath, real, err.Error())
	}
	uid, ok := opts.FileOwnerUIDFn(info)
	if !ok {
		// Ownership unknowable (non-Unix FileInfo). Fail closed: a privileged
		// delete with no ownership proof is exactly what we refuse.
		return "", fmt.Errorf("%w: cannot determine owner of %q", ErrUnsafePath, real)
	}
	// The target must be owned by the runner (the happy path, removed without
	// sudo) OR by root. A root-owned entry is the exact Docker-as-root leftover
	// the escalation exists to remove — including when the _work entry ITSELF is
	// root-owned, not just a nested file (DT-v2-20). Refusing it here re-wedges
	// "Complete runner" for every later job on the runner, defeating the tool's
	// purpose. Any OTHER uid is still refused: the runner must never
	// escalate-delete a third user's files. A symlink that escapes the _work tree
	// is already rejected above by the GuardFn re-check on the resolved path, and
	// the root-side wrapper independently re-validates the realpath under _work,
	// so allowing root here does not widen the blast radius beyond _work.
	if want := opts.OwnerUIDFn(); uid != want && uid != rootUID {
		return "", fmt.Errorf("%w: %q owned by uid %d, runner is uid %d", ErrUnsafePath, real, uid, want)
	}
	return real, nil
}

// escalate runs the privileged wrapper: chown -R the tree back to the runner
// user, retry the unprivileged delete, and only if that still fails ask the
// wrapper to rm. Every invoke targets the validated wrapper path; the wrapper
// re-validates and places "--" before the path on the root side (DT-v2-8).
func (opts Options) escalate(ctx context.Context, target string) Result {
	uidgid := fmt.Sprintf("%d:%d", opts.OwnerUIDFn(), opts.OwnerUIDFn())

	chownErr := opts.runWrapper(ctx, "chown", uidgid, target)
	if chownErr == nil {
		if err := opts.RemoveAllFn(target); err == nil {
			return Result{Escalated: true}
		}
	}

	// chown insufficient (or the retry still hit EACCES on an immutable/odd
	// entry): ask the wrapper to remove the tree as root.
	if rmErr := opts.runWrapper(ctx, "rm", target); rmErr != nil {
		// Wrap the terminal rm failure (callers may errors.Is on the underlying
		// exec error) and carry the chown context as a message.
		if chownErr != nil {
			return Result{Escalated: true, Err: fmt.Errorf("chown failed (%s) and rm failed: %w", chownErr.Error(), rmErr)}
		}
		return Result{Escalated: true, Err: fmt.Errorf("wrapper rm failed: %w", rmErr)}
	}
	return Result{Escalated: true}
}

// runWrapper invokes `sudo -n <wrapper> <op> <args...>`. The wrapper is the
// only path sudo whitelists, so this is the single point where privilege is
// requested. Context cancellation propagates through RunFn.
func (opts Options) runWrapper(ctx context.Context, op string, args ...string) error {
	cmd := append([]string{"-n", opts.WrapperPath, op}, args...)
	if _, err := opts.RunFn(ctx, "sudo", cmd...); err != nil {
		return err
	}
	return nil
}

func applyDefaults(opts *Options) {
	if opts.WrapperPath == "" {
		opts.WrapperPath = civm.DefaultSafeDeleteWrapperPath
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.RemoveAllFn == nil {
		opts.RemoveAllFn = os.RemoveAll
	}
	if opts.EvalSymlinksFn == nil {
		opts.EvalSymlinksFn = filepath.EvalSymlinks
	}
	if opts.LstatFn == nil {
		opts.LstatFn = os.Lstat
	}
	if opts.OwnerUIDFn == nil {
		opts.OwnerUIDFn = os.Getuid
	}
	if opts.FileOwnerUIDFn == nil {
		opts.FileOwnerUIDFn = defaultFileOwnerUID
	}
}
