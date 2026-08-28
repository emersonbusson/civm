# Security policy

civm is operational infrastructure: a self-hosted GitHub Actions runner
provisioning toolkit (`civmctl`) plus systemd timers and managed runner
hook scripts. This doc describes the threat surface, the validations that
defend it, and how to report issues.

## Reporting a vulnerability

For anything that could let an unprivileged actor escalate to runner
privileges or compromise the VM, contact the maintainer privately first
— **do not** open a public issue. Contact the repository owner via a
**private** channel (security advisory / DM) before public disclosure.
Do not paste live tokens or private keys into issues.

For ordinary bugs that are not security-relevant, regular GitHub issues
are fine.

## Public repository readiness (credentials)

**Invariant:** no secret *values* in git (tokens, private keys, passwords).
Paths and *names* of env vars / GitHub Actions secrets are OK.

| Class | In repo? | Where real values live |
| --- | --- | --- |
| `RELEASE_APP_ID` / `RELEASE_APP_PRIVATE_KEY` | name only | GitHub Actions secrets |
| `RELEASE_PLEASE_TOKEN` (optional) | name only | GitHub Actions secrets |
| Host `C:\ProgramData\civm\gh-token-*.txt` | path only in docs | Windows host filesystem |
| SSH host→guest key | path only | `C:\ProgramData\civm\ssh\` (SYSTEM) |
| Guest `/etc/civm/*.env`, `~/.config/gh` | never | guest host state |
| Runner `.credentials` / registration tokens | never | ephemeral / guest dirs |

**Before flipping the repo to public:**

1. Confirm CI **Secret pattern scan** and **Gitleaks** are green on default branch.
2. Keep lab session logs gitignored and **local-only**: `MEMORY.md`, `validation.md`
   (agents append here; never publish). `vm.md` may stay tracked as generic runner inventory.
3. Prefer `ubuntu-latest` for public free CI (default in this repo). Optional lab smoke:
   set repository variable `CIVM_SELF_HOSTED_SMOKE=true` when a `civm` runner exists.
4. Never commit scratch scripts that `echo` registration tokens or hardcode guest IPs.
5. History: gitleaks scans full git history on every full CI run. If a real token is
   ever found, **rotate** it immediately; rewriting public history is hard once mirrored.
6. Module path is still `github.com/emersonbusson/civm` until the Go module / GitHub transfer is intentional.
7. Host secrets (`C:\ProgramData\civm\gh-token-*.txt`, SSH under `ProgramData\civm\ssh`)
   must never be copied into this repository.

## Threat model

The civm runner is a **shared resource** across peer repos (`peer`,
`acme`, etc.). Multiple jobs from different repos can run
concurrently on the same VM. Each job ships with whatever code its
authors push — so untrusted input includes:

- Repository source code at checkout time
- Action payloads (`actions/checkout`, third-party actions)
- Environment variables propagated by the GitHub Actions runner
- Files written under `_work` during the job
- Anything an action chooses to run via shell

The trusted set is:

- `civmctl` binary at `/usr/local/bin/civmctl` (only operators can
  install or replace; see `civmctl self-upgrade`)
- systemd unit files in `/etc/systemd/system/civmctl-*.{service,timer}`
- Target-state hook scripts at `/opt/civm/hooks/job-{started,completed}.sh`
  executing the trusted binary. Some legacy VMs can still have stale
  symlinks or custom wrappers until `civmctl hook install --execute` is
  run with a fresh binary; see `runbooks/MULTI-PROJECT-RUNNER.md`.

Implicit assumptions:

- The runner OS is Ubuntu 24.04 LTS; `civmctl bootstrap` enforces this
  before any apt operation.
- The hook process runs with the runner user's privileges, escalated
  via `sudo` only for specific allowed commands (apt-get clean,
  journalctl --vacuum-time, fstrim).

## Defended surfaces

### Path traversal in hook workspace cleanup

`internal/hook/safeWorkRoot` validates every candidate work-root path
before `os.RemoveAll`. The historical bug (caught by `FuzzSafeWorkRoot`
in PR #26) was that `filepath.Clean` does **not** resolve `..` at the
start of a relative path — so `../home/x/actions-runner/_work` slipped
through a `strings.Contains(clean, "/home/")` check. The fix enforces:

1. `filepath.IsAbs(clean)` — must be absolute
2. `strings.HasPrefix(clean, "/home/")` — prefix, not substring
3. `strings.Contains(clean, "/actions-runner")` — runner-shaped
4. `strings.HasSuffix(clean, "/_work")` — work-root literal

The fuzz harness asserts no traversal component (`..` as a path element,
not as a substring like `..0`) survives in the cleaned path. The
crashing input that uncovered the bug is committed at
`internal/hook/testdata/fuzz/FuzzSafeWorkRoot/`.

### Subprocess argument injection

`internal/civm` exposes `Validate*` regex functions that gate any CLI
flag value that ever appears in a subprocess argv:

| Validator | Pattern | Used by |
|-----------|---------|---------|
| `ValidateRepo` | `^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$` | runner, billing, cireport, peerstatus |
| `ValidateShort` | `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` | runner directory suffix |
| `ValidateLabels` | comma-split `^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$` | runner labels |
| `ValidateSemver` | `^[0-9]+[.][0-9]+[.][0-9]+$` | runner version, Go version |
| `ValidateServiceUnit` | `^[A-Za-z0-9_.@-]+[.]service$` (no `..`) | systemctl restart targets |
| `ValidateUserName` | `^[A-Za-z_][A-Za-z0-9_-]{0,63}$` | `--run-as` user |
| `ValidateWorkflowFile` | `^[A-Za-z0-9.][A-Za-z0-9._/-]{0,127}[.]ya?ml$` (no `/` prefix, no `..`) | `gh workflow` selectors |

Any caller skipping validation is a regression — `gosec G204` is
acknowledged but excluded globally because subprocess argv is always
gated by these validators (see `.golangci.yml` for the rationale).

### Privilege boundary

Hook policy in `internal/hook` is exercised through managed scripts at
`/opt/civm/hooks/job-{started,completed}.sh`. Each script contains only a
static `exec /usr/local/bin/civmctl hook <event> --execute "$@"` adapter
because the GitHub Actions runner executes `.sh` hooks through bash.
This means:

- The runner can never invoke arbitrary code via the hook env vars,
  only the managed script and validated civmctl binary.
- A compromise of the hook path or binary path requires write access to
  `/opt/civm/hooks/` or `/usr/local/bin/` (root-only).
- `civmctl self-upgrade` performs the binary swap via `os.Rename`
  inside the same directory (atomic per POSIX) so concurrent
  invocations never see a half-written file.

### Disk hygiene without destroying valuable state

`internal/hook/cleanup` differentiates routine cleanup (`job-completed`)
from disk-pressure cleanup (`job-started` with disk >= threshold). The
former preserves `$HOME/.cache/go-build` and similar build caches; the
latter purges them. Conflating the two cost recurring CI failures
(PR #31 fixed it). Tests
`TestJobCompletedPreservesHotCachesUnderHome` and
`TestJobStartedPurgesHotCachesUnderDiskPressure` lock that behavior.

### Hook event log

`/var/log/civm/hooks.jsonl` is emitted via `slog.JSONHandler` with
`level` derived from decision (ERROR for `error`, WARN for `rejected`,
INFO otherwise). World-readable (`0644`) by design — operators and log
shippers (Vector/Loki) consume it. `//nolint:gosec` annotation on the
open call documents the intent.

## Linter excludes (justified)

`.golangci.yml` excludes a small set of `gosec` rules with rationale:

| Rule | Reason |
|------|--------|
| G115 | Disk arithmetic (`uint64 → int` percent, `uint64 → int64` GB) is bounded by realistic filesystem sizes and percent ranges. |
| G204 | All subprocess argv values are validated by `internal/civm.Validate*` regexes before reaching `exec.CommandContext`. |
| G304 | Path traversal: paths come from validated CLI flags (`CleanDir`) or from a whitelisted glob in `internal/hook` (`safeWorkRoot`/`safeRunnerDir`). |

When in doubt, prefer a per-line `//nolint:gosec // motivo` annotation
over expanding the global exclude list.

## Operational response

If a deployed civmctl version is found to have a security issue:

1. **Stop the bleed.** On the runner host, downgrade by disabling the
   hook env vars in every `/home/*/actions-runner*/.env`:
   ```bash
   sudo sed -i '/^ACTIONS_RUNNER_HOOK_/s/^/# /' /home/*/actions-runner*/.env
   sudo systemctl restart actions.runner.*
   ```
   The runner keeps working without the hook (no cleanup between jobs,
   but no compromised hook either).
2. **Fix.** Land the patch on `main`. Conventional Commits + the
   `release-please` automation produces a release PR.
3. **Roll forward.** Once a release with the fix is cut:
   ```bash
   cd /opt/civm && git pull --ff-only
   sudo civmctl self-upgrade --execute
   ```
   If the host predates `self-upgrade` or `/opt/civm` is not a Git
   checkout, first verify the runner is idle (`civmctl idle-check`),
   build the release binary from a trusted checkout, copy it to the VM,
   and install it atomically with `sudo install -m 0755 <binary>
   /usr/local/bin/civmctl`. Then run `sudo civmctl hook install
   --execute` to replace legacy hook symlinks or custom wrappers with
   managed `.sh` scripts that execute the trusted binary.
4. **Re-enable hooks.** Reverse step 1 on each runner.

## Known operational notes

- `release-please` requires either repo setting **"Allow GitHub Actions
  to create and approve pull requests"** to be enabled, or a PAT with
  `repo` scope stored as secret `RELEASE_PLEASE_TOKEN`. The workflow at
  `.github/workflows/release.yml` reads the secret with fallback.
- The hook is intentionally idempotent and tolerant: every
  `civmctl hook install --execute` is safe to re-run; legacy `.sh`
  wrappers from before PR #26 are cleaned up automatically.
