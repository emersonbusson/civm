// Package cleanup implements disk hygiene for the civm runner host.
// All actions are dry-run by default; --execute flag flips to mutating.
package cleanup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/emersonbusson/civm/internal/cachetrim"
	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/dockerlock"
	"github.com/emersonbusson/civm/internal/idle"
	"github.com/emersonbusson/civm/internal/safedelete"
)

// deferredByDockerHeavyLock is the no-op Action name emitted when a docker-heavy
// job currently holds the box-wide serialization lock (ITEM-10 / DT-v2-16).
// Cleanup returns early without error so the cron/next hook re-runs later.
const deferredByDockerHeavyLock = "deferred-by-docker-heavy-lock"

// deferredByHostBusy is the no-op Action name emitted when a runner/build job is
// active: the privileged file cleanup and the aggressive docker/apt prune wait
// for an idle tick, but dockerPruneSafe still reclaims unused space. It carries
// no error — a busy host is a benign deferral, not a failure (issue #70, mirrors
// the deferred-by-docker-heavy-lock contract).
const deferredByHostBusy = "deferred-by-host-busy"

// IsDeferral reports whether an Action name marks a benign deferral (host busy
// or a docker-heavy build holding the lock) rather than work that ran. Render
// surfaces these as "deferido", not "(dry-run)"/"skip", during --execute.
func IsDeferral(name string) bool {
	return strings.HasPrefix(name, "deferred-by-")
}

type deleteCandidate struct {
	path string
	size int64
}

// Activity is evidence that a CI job or build is currently active on the host.
type Activity = idle.Activity

var dangerousAbsoluteRoots = map[string]struct{}{
	"/":     {},
	"/home": {},
	"/root": {},
}

var allowedTopLevelCleanupRoots = map[string]struct{}{
	civm.DefaultTmpDir: {},
}

var protectedWorkCacheDirs = map[string]struct{}{
	"_actions": {},
	"_tool":    {},
}

// Action is one cleanup step result.
type Action struct {
	Name       string
	Path       string
	BytesFound int64
	BytesFreed int64
	Executed   bool
	Err        error
}

// Options control which steps run.
type Options struct {
	Execute            bool
	WorkDir            string
	TmpDir             string
	CodespaceDir       string
	TmpThreshold       time.Duration
	WorkThreshold      time.Duration
	CodespaceThreshold time.Duration
	DockerPrune        bool
	// ManagedVolumePrune enables scoped named-volume removal only at a
	// generation boundary while the host has closed admission. Routine timers
	// and hooks keep it disabled.
	ManagedVolumePrune bool
	AptClean           bool
	SkipIdleGuard      bool
	// EmergencyBypassIdle stops deferring the SAFE reclaim (old /tmp, cache
	// trim) when the host is busy. Set by the disk-watchdog at
	// civm.DefaultEmergencyBypassPct: a busy host filling its own disk is
	// exactly when the deferral is wrong (2026-06-10: watchdog fired at 83%,
	// deferred everything, guest ran to 0% free and wedged sshd). The
	// privileged work-dir sweep and aggressive docker/apt prune stay deferred —
	// they can break the live job.
	EmergencyBypassIdle bool
	IdleProbeDelay      time.Duration
	Now                 time.Time

	WalkFn     func(root string, fn fs.WalkDirFunc) error
	StatFn     func(path string) (fs.FileInfo, error)
	GlobFn     func(pattern string) ([]string, error)
	RunFn      func(ctx context.Context, name string, args ...string) ([]byte, error)
	ActivityFn func(ctx context.Context) ([]Activity, error)
	// RemoveAllFn removes one regenerable cache file during the cache-trim step.
	// Caches live under the runner user's home and cleanup runs as root, so a
	// plain os.RemoveAll suffices (no safedelete escalation, unlike root-owned
	// _work leftovers). Injected so tests never touch disk.
	RemoveAllFn func(path string) error
	// LockActiveFn reports whether a docker-heavy job holds the serialization
	// lock right now; ReclaimStaleFn removes a stale (.hb of a dead holder) lock;
	// LockHolderFn labels the current holder. All injected so unit tests never
	// touch /run/civm. Defaults wrap dockerlock (DT-v2-16).
	LockActiveFn   func() (bool, error)
	ReclaimStaleFn func() (bool, error)
	LockHolderFn   func() string
	// SafeDeleteFn removes one stale candidate, escalating to the privileged
	// wrapper only when a root-owned file (a containerized CI step ran as root
	// and wrote into the mounted _work) blocks the unprivileged delete. The
	// GuardFn scopes the escalation to a direct child of a validated cleanup
	// root. Injected so unit tests never call real sudo (DT-v2-9).
	SafeDeleteFn func(ctx context.Context, path string) safedelete.Result
}

// DefaultOptions returns sane defaults: dry-run, 1d /tmp, 3d _work.
// Hook job-completed already wipes _work per job; these are a safety net for
// orphaned dirs from crashes or runner restarts, kept short to free SSD space.
func DefaultOptions() Options {
	return Options{
		Execute:            false,
		WorkDir:            civm.DefaultWorkDir,
		TmpDir:             civm.DefaultTmpDir,
		CodespaceDir:       civm.DefaultCodespaceDir,
		TmpThreshold:       24 * time.Hour,
		WorkThreshold:      3 * 24 * time.Hour,
		CodespaceThreshold: 7 * 24 * time.Hour,
		DockerPrune:        true,
		AptClean:           true,
		IdleProbeDelay:     2 * time.Second,
		Now:                time.Now(),
		WalkFn:             filepath.WalkDir,
		StatFn:             defaultStat,
		GlobFn:             filepath.Glob,
		RemoveAllFn:        os.RemoveAll,
		RunFn:              defaultRun,
		ActivityFn:         defaultActivities,
		LockActiveFn:       defaultLockActive,
		ReclaimStaleFn:     defaultReclaimStale,
		LockHolderFn:       defaultLockHolder,
		SafeDeleteFn:       defaultSafeDelete,
	}
}

// defaultSafeDelete removes one candidate via safedelete with the cleanup-scoped
// guard. The escalation can only ever target a direct child of a root that
// validateCleanupRoot accepts, so a root-owned _work leftover is reclaimed
// without ever widening the blast radius.
func defaultSafeDelete(ctx context.Context, path string) safedelete.Result {
	return safedelete.Remove(ctx, safedelete.Options{GuardFn: cleanupChildGuard}, path)
}

// cleanupChildGuard rejects any path that is not a direct child of a cleanup
// root that validateCleanupRoot accepts. validateCleanupRoot validates the ROOT
// (rejects /, dangerous roots, bare home); this adapter derives that root from
// the candidate and confirms the parent/child relation (DT-v2-9). It is never
// passed directly as GuardFn because validateCleanupRoot has a (string,error)
// signature over the root, not the child.
func cleanupChildGuard(path string) error {
	clean := filepath.Clean(path)
	parent := filepath.Dir(clean)
	if _, err := validateCleanupRoot(parent); err != nil {
		return fmt.Errorf("parent %q is not a valid cleanup root: %w", parent, err)
	}
	if filepath.Dir(clean) != parent || filepath.Base(clean) == "" {
		return fmt.Errorf("%q is not a direct child of a cleanup root", clean)
	}
	return nil
}

// defaultLockActive / defaultReclaimStale / defaultLockHolder wrap dockerlock so
// cleanup never duplicates the staleness/PID-reuse logic. Import direction is
// cleanup -> dockerlock only.
func defaultLockActive() (bool, error)   { return dockerlock.IsActive(dockerlock.DefaultOptions()) }
func defaultReclaimStale() (bool, error) { return dockerlock.ReclaimStale(dockerlock.DefaultOptions()) }
func defaultLockHolder() string          { return dockerlock.Holder(dockerlock.DefaultOptions()) }

// Run executes every enabled step and returns one Action per step.
// Errors are captured per-Action; the function itself returns nil.
func Run(ctx context.Context, opts Options) []Action {
	applyDefaults(&opts)
	// Defer to a live docker-heavy job: pruning the daemon while a build holds
	// the lock would fight it for resources. A fresh holder -> no-op early
	// return (exit 0, cron re-runs); a stale holder -> reclaim and proceed.
	// Never hard-fails on lock read errors (DT-v2-16).
	if opts.Execute {
		if active, err := opts.LockActiveFn(); err == nil && active {
			return []Action{{Name: deferredByDockerHeavyLock, Path: opts.LockHolderFn()}}
		}
		_, _ = opts.ReclaimStaleFn()
	}
	if opts.Execute && !opts.SkipIdleGuard {
		if err := ensureIdle(ctx, opts); err != nil {
			// Host busy: defer the privileged file cleanup and the aggressive
			// docker/apt prune to an idle tick (benign, exit 0), but still
			// reclaim UNUSED docker space now — the disk-pressure consumer on a
			// perpetually busy box. Os dois safe prunes só removem recursos
			// órfãos, nunca um que um build ativo segura (issue #70):
			// dockerPruneSafe = imagens dangling + build cache velho;
			// dockerVolumePruneSafe = volumes sem container (fechava a lacuna dos
			// ~86 volumes stale do E2E).
			//
			// O dockerImagePruneOld (`image prune -a --filter until=168h`) foi
			// REMOVIDO daqui: o `until` casa a data de build do VENDOR, não a do
			// pull, então uma imagem recém-baixada vendor-antiga (redis/minio/
			// postgres) era apagada debaixo de um sibling em build->up no daemon
			// compartilhado ("No such image", a corrida que derrubava o
			// tenant-isolation-smoke). Nenhum branch faz mais `-a`: o branch idle
			// agora também usa `system prune -f` (sem `-a`), porque a deteccao de
			// idle teve falso "idle" no meio de um deploy de 66min e o `-af` rodou
			// mesmo assim. O reclaim de imagens taggeadas migra pro teardown
			// per-job (cada job derruba o proprio stack).
			// No Err: a busy host is a deferral, not a failure.
			var out []Action
			if opts.DockerPrune {
				out = append(out, dockerPruneSafe(ctx, opts))
				out = append(out, dockerVolumePruneSafe(ctx, opts))
			}
			if opts.EmergencyBypassIdle {
				// Disk at emergency level: the busy job is the thing filling
				// the disk, so "wait for idle" never comes. Run only the safe
				// reclaim — old /tmp entries (age-gated by TmpThreshold) and
				// cache trim under InFlightFloor: trims only genuinely-stale
				// caches and SKIPS any dir with a fresh write (a live install) —
				// MinProtect alone is not enough here, the hard-ceiling Pass 2
				// overrides it. The work-dir sweep stays deferred.
				out = append(out, scanAndMaybeDelete(ctx, opts, "tmp_old", opts.TmpDir, opts.TmpThreshold))
				out = append(out, cacheTrimActions(opts)...)
				out = append(out, Action{Name: "emergency-bypass-idle", Path: "(disk emergency: safe reclaim while busy)"})
				return out
			}
			out = append(out, Action{Name: deferredByHostBusy, Path: "(runner/build activity)"})
			return out
		}
	}
	var out []Action
	out = append(out, scanAndMaybeDelete(ctx, opts, "tmp_old", opts.TmpDir, opts.TmpThreshold))
	out = append(out, scanWorkAndMaybeDelete(ctx, opts))
	// Clones parados em /home/*/codespace: o CI clona em _work, nunca aqui; este dir era
	// um ponto cego (nenhuma rotina o limpava) e acumulava drift de paridade-pago. Gated
	// pelo mesmo ensureIdle acima, entao nunca roda mid-job; alem disso, apagar codespace
	// nao toca _work, entao e seguro mesmo se um job estivesse ativo.
	out = append(out, scanCodespaceAndMaybeDelete(ctx, opts))
	// Bound the regenerable CI caches (the named-dir gap). The job hook trims
	// these at job-started, but a disk-watchdog tick (idle runner, disk filled by
	// caches) must trim them too — same cachetrim policy, applied as root across
	// all /home/* runner homes. Reached only past ensureIdle, so it never trims a
	// cache a live build is using; cachetrim's MinProtect double-guards hot files.
	out = append(out, cacheTrimActions(opts)...)
	if opts.DockerPrune {
		out = append(out, dockerPrune(ctx, opts))
	}
	if opts.ManagedVolumePrune {
		out = append(out, dockerManagedVolumePrune(ctx, opts))
	}
	if opts.AptClean {
		out = append(out, aptClean(ctx, opts))
	}
	return out
}

func applyDefaults(opts *Options) {
	if opts.WalkFn == nil {
		opts.WalkFn = filepath.WalkDir
	}
	if opts.StatFn == nil {
		opts.StatFn = defaultStat
	}
	if opts.GlobFn == nil {
		opts.GlobFn = filepath.Glob
	}
	if opts.RemoveAllFn == nil {
		opts.RemoveAllFn = os.RemoveAll
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.ActivityFn == nil {
		opts.ActivityFn = defaultActivities
	}
	if opts.LockActiveFn == nil {
		opts.LockActiveFn = defaultLockActive
	}
	if opts.ReclaimStaleFn == nil {
		opts.ReclaimStaleFn = defaultReclaimStale
	}
	if opts.LockHolderFn == nil {
		opts.LockHolderFn = defaultLockHolder
	}
	if opts.SafeDeleteFn == nil {
		opts.SafeDeleteFn = defaultSafeDelete
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
}

func scanWorkAndMaybeDelete(ctx context.Context, opts Options) Action {
	roots := workCleanupRoots(opts)
	if len(roots) == 1 {
		return scanAndMaybeDelete(ctx, opts, "work_old", roots[0], opts.WorkThreshold)
	}
	a := Action{Name: "work_old", Path: strings.Join(roots, ", ")}
	for _, root := range roots {
		part := scanAndMaybeDelete(ctx, opts, "work_old", root, opts.WorkThreshold)
		a.BytesFound += part.BytesFound
		a.BytesFreed += part.BytesFreed
		a.Executed = a.Executed || part.Executed
		if part.Err != nil && a.Err == nil {
			a.Err = part.Err
		}
	}
	return a
}

func workCleanupRoots(opts Options) []string {
	workDir := filepath.Clean(opts.WorkDir)
	if workDir != filepath.Clean(civm.DefaultWorkDir) {
		return []string{opts.WorkDir}
	}
	matches, err := opts.GlobFn("/home/*/actions-runner-*/_work")
	if err != nil || len(matches) == 0 {
		return []string{opts.WorkDir}
	}
	sort.Strings(matches)
	return matches
}

// codespaceCleanupRoots descobre os dirs /home/*/codespace (espelha workCleanupRoots).
// Quando CodespaceDir e o sentinel (DefaultCodespaceDir), faz glob; senao usa o literal.
// Sao clones manuais parados — o CI clona em _work, nunca em codespace; nada operacional
// referencia esse dir, entao um clone parado alem do threshold e drift seguro de remover.
func codespaceCleanupRoots(opts Options) []string {
	dir := filepath.Clean(opts.CodespaceDir)
	if dir != filepath.Clean(civm.DefaultCodespaceDir) {
		return []string{opts.CodespaceDir}
	}
	matches, err := opts.GlobFn("/home/*/codespace")
	if err != nil || len(matches) == 0 {
		return nil
	}
	sort.Strings(matches)
	return matches
}

// scanCodespaceAndMaybeDelete varre cada /home/*/codespace e remove os clones parados
// (mtime > CodespaceThreshold, com a guarda de 2h herdada de scanAndMaybeDelete). Diferente
// de work_old, nao ha filhos protegidos (_tool/_actions) — todo clone parado e descartavel.
func scanCodespaceAndMaybeDelete(ctx context.Context, opts Options) Action {
	roots := codespaceCleanupRoots(opts)
	if len(roots) == 0 {
		return Action{Name: "codespace_stale", Path: "(nenhum /home/*/codespace)"}
	}
	if len(roots) == 1 {
		return scanAndMaybeDelete(ctx, opts, "codespace_stale", roots[0], opts.CodespaceThreshold)
	}
	a := Action{Name: "codespace_stale", Path: strings.Join(roots, ", ")}
	for _, root := range roots {
		part := scanAndMaybeDelete(ctx, opts, "codespace_stale", root, opts.CodespaceThreshold)
		a.BytesFound += part.BytesFound
		a.BytesFreed += part.BytesFreed
		a.Executed = a.Executed || part.Executed
		if part.Err != nil && a.Err == nil {
			a.Err = part.Err
		}
	}
	return a
}

// cacheHomeRoots derives the runner user home(s) from the discovered _work roots
// (/home/<user>/actions-runner-X/_work -> /home/<user>). cleanup runs as root, so
// os.Getenv("HOME") is /root, not the user whose caches we must bound — the
// caches live next to the runner installs.
func cacheHomeRoots(opts Options) []string {
	roots := workCleanupRoots(opts)
	seen := make(map[string]struct{}, len(roots))
	var homes []string
	for _, r := range roots {
		home := filepath.Dir(filepath.Dir(r)) // strip /_work then /actions-runner-X
		if home == "" || home == "/" || home == "." {
			continue
		}
		if _, ok := seen[home]; ok {
			continue
		}
		seen[home] = struct{}{}
		homes = append(homes, home)
	}
	return homes
}

// cacheTrimActions trims the regenerable CI caches across the runner homes to
// their family budgets, via the shared cachetrim policy (same source as the job
// hook). One Action per cache dir.
func cacheTrimActions(opts Options) []Action {
	caps := cachetrim.Caps(cacheHomeRoots(opts), cachetrim.Deps{GlobFn: opts.GlobFn, StatFn: opts.StatFn})
	if len(caps) == 0 {
		return nil
	}
	trimOpts := cachetrim.Options{Execute: opts.Execute, Now: opts.Now, WalkDirFn: opts.WalkFn, RemoveAllFn: opts.RemoveAllFn}
	if opts.EmergencyBypassIdle {
		// Emergency path: sem idle guard, o trim precisa do floor in-flight para
		// não deletar o working-set de um install vivo. O path normal (idle-gated)
		// mantém InFlightFloor=0 e o teto-hard de sempre.
		trimOpts.InFlightFloor = time.Duration(civm.DefaultCacheInFlightFloorMinutes) * time.Minute
	}
	out := make([]Action, 0, len(caps))
	for _, c := range caps {
		if opts.EmergencyBypassIdle && c.WipeWhole {
			// Emergency disk pressure means headroom matters more than preserving a
			// cache hit. WipeWhole caches (go-build/golangci) cannot be safely
			// sub-trim, so remove the whole dir unless InFlightFloor detects a live
			// writer. Non-WipeWhole caches keep their normal caps.
			c.MaxBytes = 0
		}
		r := cachetrim.TrimByAge(trimOpts, c)
		a := Action{Name: "cache_trim", Path: r.Path, BytesFound: r.BytesFound, BytesFreed: r.BytesFreed, Executed: r.Executed, Err: r.Err}
		if r.SkippedInFlight {
			a.Path = r.Path + " (skipped: in-flight install)"
		}
		out = append(out, a)
	}
	return out
}

func scanAndMaybeDelete(ctx context.Context, opts Options, name, root string, threshold time.Duration) Action {
	a := Action{Name: name, Path: root}
	cleanRoot, err := validateCleanupRoot(root)
	if err != nil {
		a.Err = err
		return a
	}
	root = cleanRoot
	a.Path = cleanRoot
	cutoff := opts.Now.Add(-threshold)
	var toDelete []deleteCandidate
	err = opts.WalkFn(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		if name == "work_old" && isProtectedWorkCacheDir(root, path) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if info.ModTime().After(cutoff) {
			return nil
		}
		// Anti-jobs-em-curso: skip arquivos com mtime <2h.
		if opts.Now.Sub(info.ModTime()) < 2*time.Hour {
			return nil
		}
		size := dirSize(opts, path, info)
		a.BytesFound += size
		toDelete = append(toDelete, deleteCandidate{path: path, size: size})
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		a.Err = err
		return a
	}
	if !opts.Execute {
		return a
	}
	// Double-check idleness right before deleting — except under the disk
	// emergency bypass: the candidates here are already age-gated (mtime older
	// than the threshold AND >2h, never a live job's files), and at emergency
	// usage waiting for idle is what let the disk run to 0 (2026-06-10).
	if !opts.EmergencyBypassIdle {
		if err := ensureIdle(ctx, opts); err != nil {
			a.Err = err
			return a
		}
	}
	for _, candidate := range toDelete {
		// safedelete tries an unprivileged remove first, escalating to the
		// guarded wrapper only for a root-owned _work leftover (a CI Docker step
		// ran as root). Accumulate the first error but keep going so one stuck
		// candidate never wedges the whole sweep (DT-v2-9).
		res := opts.SafeDeleteFn(ctx, candidate.path)
		if res.Err != nil {
			// A safedelete REFUSAL (ErrUnsafePath: cross-user owner, path
			// escaping the validated tree, etc.) is the guard correctly
			// declining — skip this candidate and keep sweeping, but do NOT
			// fail the action. Otherwise a stray cross-user file (e.g. a
			// uid-1000 leftover in /tmp swept by the root disk-watchdog) turns
			// routine disk hygiene into a FAILED service. Only a GENUINE delete
			// failure (a root-owned _work leftover whose escalation itself
			// failed — the broken-runner sentinel) stays fatal (issue #70 family).
			if errors.Is(res.Err, safedelete.ErrUnsafePath) {
				continue
			}
			if a.Err == nil {
				a.Err = res.Err
			}
			continue
		}
		a.BytesFreed += candidate.size
	}
	a.Executed = true
	return a
}

func isProtectedWorkCacheDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." || rel == "" {
		return false
	}
	first := strings.Split(filepath.ToSlash(rel), "/")[0]
	_, ok := protectedWorkCacheDirs[first]
	return ok
}

func validateCleanupRoot(root string) (string, error) {
	clean, err := civm.CleanDir(root, "cleanup root")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(clean) {
		return clean, nil
	}
	if _, ok := allowedTopLevelCleanupRoots[clean]; ok {
		return clean, nil
	}
	if _, ok := dangerousAbsoluteRoots[clean]; ok {
		return "", fmt.Errorf("cleanup root perigoso: %s", clean)
	}
	if home, err := os.UserHomeDir(); err == nil && clean == filepath.Clean(home) {
		return "", fmt.Errorf("cleanup root nao pode ser home inteiro: %s", clean)
	}
	if strings.HasPrefix(clean, "/home/") && strings.Count(strings.Trim(clean, "/"), "/") == 1 {
		return "", fmt.Errorf("cleanup root nao pode ser home inteiro: %s", clean)
	}
	return clean, nil
}

func dirSize(opts Options, root string, info fs.FileInfo) int64 {
	if !info.IsDir() {
		return info.Size()
	}
	var total int64
	_ = opts.WalkFn(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		if !fi.IsDir() {
			total += fi.Size()
		}
		return nil
	})
	return total
}

// dockerPrune faz o reclaim de docker do branch idle: containers parados, networks
// sem uso, imagens DANGLING, build cache e volumes orfaos (`system prune -f
// --volumes`). NAO usa `-a`: o `-a` apaga TODA imagem taggeada sem uso, e numa box
// com um daemon compartilhado por 8 runners isso wipa as imagens vendor recem-
// PUXADAS (redis/alpine/postgres) de um sibling em `compose up --build` debaixo
// dele — "No such image", a corrida que derrubava o tenant-isolation-smoke. O
// idle-gate sozinho nao basta: a deteccao de idle teve um falso "idle" no meio de
// um deploy de 66min (build com lulls) e o `-af` rodou mesmo assim. O reclaim de
// imagens taggeadas antigas migra para o teardown per-job (cada job derruba o
// proprio stack), nao para um prune global que corre com deploys.
func dockerPrune(ctx context.Context, opts Options) Action {
	a := Action{Name: "docker_prune", Path: "(docker daemon)"}
	if !opts.Execute {
		// Best-effort estimate without execute: parse `docker system df`.
		out, err := opts.RunFn(ctx, "docker", "system", "df", "--format", "{{.Reclaimable}}")
		if err == nil {
			a.BytesFound = parseReclaimable(string(out))
		}
		return a
	}
	if err := ensureIdle(ctx, opts); err != nil {
		a.Err = err
		return a
	}
	out, err := opts.RunFn(ctx, "docker", "system", "prune", "-f", "--volumes")
	if err != nil {
		a.Err = err
		return a
	}
	a.BytesFreed = parseTotalReclaimed(string(out))
	a.Executed = true
	return a
}

// dockerPruneSafe reclaims only UNUSED docker space and is safe to run while a
// build/job is active: `docker image prune -f` removes dangling (untagged,
// unreferenced) images, and `docker builder prune -f -a` removes ALL unused
// build cache — dropping the former until=24h filter, because this runs under
// disk pressure and today's <24h cache is exactly the bulk that filled the
// guest to 95% mid-build (2026-06-16). BuildKit excludes an active build's
// in-use cache graph regardless, so -a stays concurrency-safe: it only drops a
// finished sibling's reusable cache (a cache-HIT, never correctness). Neither can
// remove a resource a running container or build holds, so it needs no idle
// guard (issue #70). Called only from the host-busy branch of Run, which is
// reached only when opts.Execute is true (ensureIdle is a no-op otherwise), so
// there is no dry-run path here — the idle-path dockerPrune handles dry-run.
func dockerPruneSafe(ctx context.Context, opts Options) Action {
	a := Action{Name: "docker_prune_safe", Path: "(docker unused: dangling images + all unused build cache)"}
	images, err := opts.RunFn(ctx, "docker", "image", "prune", "-f")
	if err != nil {
		a.Err = err
		return a
	}
	cache, err := opts.RunFn(ctx, "docker", "builder", "prune", "-f", "-a")
	if err != nil {
		a.Err = err
		return a
	}
	a.BytesFreed = parseTotalReclaimed(string(images)) + parseTotalReclaimed(string(cache))
	a.Executed = true
	return a
}

// dockerVolumePruneSafe remove apenas volumes NÃO-usados (`docker volume prune
// -f`). É seguro rodar com um build/job ativo: o docker recusa remover qualquer
// volume anexado a um container vivo — só os órfãos (de containers já removidos)
// são reclamados. Era a lacuna do reclaim seguro: o prune dangling-only deixava
// para trás dezenas de volumes stale acumulando GBs no disco do E2E. Sem idle
// guard, pelo mesmo motivo de dockerPruneSafe (issue #70). Chamada só do branch
// host-busy de Run, sempre com opts.Execute true — não há path de dry-run aqui.
func dockerVolumePruneSafe(ctx context.Context, opts Options) Action {
	a := Action{Name: "docker_volume_prune", Path: "(docker unused volumes)"}
	out, err := opts.RunFn(ctx, "docker", "volume", "prune", "-f")
	if err != nil {
		a.Err = err
		return a
	}
	a.BytesFreed = parseTotalReclaimed(string(out))
	a.Executed = true
	return a
}

var (
	managedComposeProjectPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*-[1-9][0-9]{5,}$`)
	dockerVolumeNamePattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]+$`)
)

type dockerVolumeMetadata struct {
	Name   string            `json:"Name"`
	Driver string            `json:"Driver"`
	Scope  string            `json:"Scope"`
	Labels map[string]string `json:"Labels"`
}

type dockerSystemDFRow struct {
	Type        string `json:"Type"`
	Reclaimable string `json:"Reclaimable"`
}

// managedComposeVolumeNames classifies only local volumes created by a
// run-scoped Compose project. Name, labels, and numeric suffix must agree;
// ambiguous data is preserved.
func managedComposeVolumeNames(payload []byte) ([]string, error) {
	var volumes []dockerVolumeMetadata
	if err := json.Unmarshal(payload, &volumes); err != nil {
		return nil, fmt.Errorf("parse docker volume inspect: %w", err)
	}

	names := make([]string, 0, len(volumes))
	seen := make(map[string]struct{}, len(volumes))
	for _, volume := range volumes {
		project := volume.Labels["com.docker.compose.project"]
		composeVolume := volume.Labels["com.docker.compose.volume"]
		if volume.Driver != "local" || volume.Scope != "local" ||
			!managedComposeProjectPattern.MatchString(project) ||
			volume.Labels["com.docker.compose.config-hash"] == "" ||
			volume.Labels["com.docker.compose.version"] == "" ||
			composeVolume == "" ||
			!dockerVolumeNamePattern.MatchString(volume.Name) ||
			volume.Name != project+"_"+composeVolume {
			continue
		}
		if _, duplicate := seen[volume.Name]; duplicate {
			continue
		}
		seen[volume.Name] = struct{}{}
		names = append(names, volume.Name)
	}
	sort.Strings(names)
	return names, nil
}

func listComposeVolumes(ctx context.Context, opts Options) ([]string, error) {
	out, err := opts.RunFn(
		ctx,
		"docker",
		"volume",
		"ls",
		"--filter",
		"dangling=true",
		"--filter",
		"label=com.docker.compose.project",
		"--format",
		"{{.Name}}",
	)
	if err != nil {
		return nil, fmt.Errorf("list compose volumes: %w", err)
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		name := strings.TrimSpace(line)
		if name == "" {
			continue
		}
		if !dockerVolumeNamePattern.MatchString(name) {
			return nil, fmt.Errorf("docker returned unsafe volume name %q", name)
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func inspectManagedComposeVolumes(ctx context.Context, opts Options, names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	args := append([]string{"volume", "inspect"}, names...)
	out, err := opts.RunFn(ctx, "docker", args...)
	if err != nil {
		return nil, fmt.Errorf("inspect compose volumes: %w", err)
	}
	return managedComposeVolumeNames(out)
}

func dockerLocalVolumeReclaimable(ctx context.Context, opts Options) (int64, error) {
	out, err := opts.RunFn(ctx, "docker", "system", "df", "--format", "{{json .}}")
	if err != nil {
		return 0, fmt.Errorf("measure local volume reclaimable: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var row dockerSystemDFRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return 0, fmt.Errorf("parse docker system df: %w", err)
		}
		if row.Type == "Local Volumes" {
			fields := strings.Fields(row.Reclaimable)
			if len(fields) == 0 {
				return 0, errors.New("docker system df returned empty local volume size")
			}
			return parseHumanBytes(fields[0]), nil
		}
	}
	return 0, errors.New("docker system df omitted Local Volumes")
}

// dockerManagedVolumePrune explicitly removes only managed run-scoped Compose
// volumes. Unlike `docker volume prune -af`, it does not broaden the mutation
// to every unreferenced named volume in the daemon.
func dockerManagedVolumePrune(ctx context.Context, opts Options) Action {
	a := Action{Name: "docker_managed_volumes", Path: "(docker managed compose volumes)"}
	beforeBytes, err := dockerLocalVolumeReclaimable(ctx, opts)
	if err != nil {
		a.Err = err
		return a
	}
	a.BytesFound = beforeBytes
	listed, err := listComposeVolumes(ctx, opts)
	if err != nil {
		a.Err = err
		return a
	}
	targets, err := inspectManagedComposeVolumes(ctx, opts, listed)
	if err != nil {
		a.Err = err
		return a
	}
	a.Path = fmt.Sprintf("(docker managed compose volumes: %d)", len(targets))
	if !opts.Execute {
		return a
	}
	if len(targets) == 0 {
		a.Executed = true
		return a
	}

	// Recheck host idleness and the lock immediately before mutation. The
	// earlier inventory does not authorize removal if a job started meanwhile.
	if err := ensureIdle(ctx, opts); err != nil {
		a.Err = fmt.Errorf("managed volume prune idle recheck: %w", err)
		return a
	}
	if active, err := opts.LockActiveFn(); err != nil {
		a.Err = fmt.Errorf("managed volume prune lock recheck: %w", err)
		return a
	} else if active {
		a.Err = errors.New("managed volume prune blocked by docker-heavy lock")
		return a
	}

	args := append([]string{"volume", "rm"}, targets...)
	if _, err := opts.RunFn(ctx, "docker", args...); err != nil {
		a.Err = fmt.Errorf("remove managed compose volumes: %w", err)
		return a
	}

	remaining, err := listComposeVolumes(ctx, opts)
	if err != nil {
		a.Err = fmt.Errorf("managed volume postcondition: %w", err)
		return a
	}
	remainingSet := make(map[string]struct{}, len(remaining))
	for _, name := range remaining {
		remainingSet[name] = struct{}{}
	}
	for _, target := range targets {
		if _, found := remainingSet[target]; found {
			a.Err = fmt.Errorf("managed volume postcondition: %q still exists", target)
			return a
		}
	}
	afterBytes, err := dockerLocalVolumeReclaimable(ctx, opts)
	if err != nil {
		a.Err = fmt.Errorf("managed volume byte postcondition: %w", err)
		return a
	}
	if afterBytes > beforeBytes {
		a.Err = fmt.Errorf(
			"managed volume byte postcondition: reclaimable grew from %d to %d",
			beforeBytes,
			afterBytes,
		)
		return a
	}
	a.BytesFreed = beforeBytes - afterBytes
	a.Executed = true
	return a
}

func aptClean(ctx context.Context, opts Options) Action {
	a := Action{Name: "apt_cache", Path: "/var/cache/apt"}
	if !opts.Execute {
		return a
	}
	if err := ensureIdle(ctx, opts); err != nil {
		a.Err = err
		return a
	}
	if _, err := opts.RunFn(ctx, "apt-get", "clean"); err != nil {
		a.Err = err
		return a
	}
	if _, err := opts.RunFn(ctx, "apt-get", "autoremove", "-y"); err != nil {
		a.Err = err
		return a
	}
	a.Executed = true
	return a
}

func ensureIdle(ctx context.Context, opts Options) error {
	if opts.SkipIdleGuard || !opts.Execute {
		return nil
	}
	idleOpts := idle.DefaultOptions()
	idleOpts.ActivityFn = opts.ActivityFn
	idleOpts.ProbeDelay = opts.IdleProbeDelay
	return idle.Ensure(ctx, idleOpts, "cleanup")
}

func formatActivities(activities []Activity) string {
	return idle.FormatActivities(activities)
}

func defaultActivities(ctx context.Context) ([]Activity, error) {
	return idle.DefaultActivities(ctx)
}

func parseActiveProcesses(psOutput string, currentPID int) []Activity {
	return idle.ParseActiveProcesses(psOutput, currentPID)
}

func isActiveBuildProcess(comm, args string) bool {
	return idle.IsActiveBuildProcess(comm, args)
}

// parseReclaimable parses output of `docker system df --format {{.Reclaimable}}`.
// Each line looks like "1.234GB (100%)". We sum bytes.
func parseReclaimable(s string) int64 {
	var total int64
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		total += parseHumanBytes(fields[0])
	}
	return total
}

// parseTotalReclaimed parses the reclaimed-space summary line emitted by docker
// prune commands. `docker image/system prune` print "Total reclaimed space:
// 1.234GB"; `docker builder prune` prints "Total:  1.234GB". Both are handled.
func parseTotalReclaimed(s string) int64 {
	for _, line := range strings.Split(s, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.Contains(trimmed, "reclaimed space:") && !strings.HasPrefix(trimmed, "Total:") {
			continue
		}
		idx := strings.LastIndex(trimmed, ":")
		if idx == -1 {
			continue
		}
		return parseHumanBytes(strings.TrimSpace(trimmed[idx+1:]))
	}
	return 0
}

// parseHumanBytes accepts "1.5GB", "200MB", "10kB", "100B".
func parseHumanBytes(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "GB"):
		mult = 1 << 30
		s = strings.TrimSuffix(s, "GB")
	case strings.HasSuffix(s, "MB"):
		mult = 1 << 20
		s = strings.TrimSuffix(s, "MB")
	case strings.HasSuffix(s, "kB"), strings.HasSuffix(s, "KB"):
		mult = 1 << 10
		s = s[:len(s)-2]
	case strings.HasSuffix(s, "B"):
		s = strings.TrimSuffix(s, "B")
	}
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	if err != nil {
		return 0
	}
	return int64(f * float64(mult))
}

// FormatBytes returns a human-friendly size.
func FormatBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/float64(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/float64(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(b)/float64(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

// RenderTable writes actions as a fixed-width table (PT-BR).
func RenderTable(actions []Action, opts Options, w io.Writer) {
	mode := "DRY-RUN"
	if opts.Execute {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "Modo: %s\n", mode)
	fmt.Fprintf(w, "%-14s %-30s %-12s %-12s %s\n", "ACAO", "PATH", "ENCONTRADO", "LIBERADO", "STATUS")
	fmt.Fprintln(w, strings.Repeat("-", 80))
	var totalFound, totalFreed int64
	for _, a := range actions {
		status := "ok"
		if a.Err != nil {
			status = "erro: " + a.Err.Error()
		} else if IsDeferral(a.Name) {
			status = "deferido"
		} else if !a.Executed && opts.Execute {
			status = "skip"
		} else if !opts.Execute {
			status = "(dry-run)"
		}
		fmt.Fprintf(w, "%-14s %-30s %-12s %-12s %s\n", a.Name, truncatePath(a.Path, 30), FormatBytes(a.BytesFound), FormatBytes(a.BytesFreed), status)
		totalFound += a.BytesFound
		totalFreed += a.BytesFreed
	}
	fmt.Fprintln(w, strings.Repeat("-", 80))
	fmt.Fprintf(w, "TOTAL          %-30s %-12s %s\n", "", FormatBytes(totalFound), FormatBytes(totalFreed))
	if !opts.Execute {
		fmt.Fprintln(w, "Para aplicar: rode novamente com --execute")
	}
}

func truncatePath(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n+1:]
}

// ---- defaults ----

func defaultStat(path string) (fs.FileInfo, error) {
	return defaultStatImpl(path)
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}
