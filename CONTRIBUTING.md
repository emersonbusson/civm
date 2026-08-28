# Contributing to civm

Thanks for helping improve civm — a toolkit to provision and operate
**self-hosted GitHub Actions runners** (guest Linux + optional Windows host).

## Quick start (dev)

```bash
# Go 1.26+ (see go.mod / toolchain)
go test ./internal/... ./cmd/...
go build -o /tmp/civmctl ./cmd/civmctl
/tmp/civmctl --help
```

CI also runs `go vet`, `golangci-lint`, `govulncheck`, a secret-pattern scan,
**gitleaks** (working tree + git history), and a coverage gate on
`./internal/...` (≥80% per package).

Locally (optional, if you have [gitleaks](https://github.com/gitleaks/gitleaks) installed):

```bash
gitleaks detect --source . --no-git   # working tree
gitleaks detect --source .           # + git history
```

## Pull requests

1. Prefer **small, reviewable** PRs (one responsibility).
2. **Conventional Commits** titles, English, imperative (`fix(runner): …`).
3. If you change behavior, add/adjust tests in the same PR.
4. Do **not** commit secrets, lab IPs, PATs, SSH keys, or host state under
   `C:\ProgramData\civm\` / `/etc/civm/`.
5. Do not add product/tenant-specific fleets as code defaults. Use env /
   CLI flags (`--repos=…`, host `TokenPaths` / `Repos`).

## What belongs where

| Layer | Repo | Language |
| --- | --- | --- |
| Guest tooling (`civmctl`, hooks, systemd units) | **this repo** | Go + shell |
| Windows Hyper-V orchestrator (scale-to-zero) | sibling `civm-host` (optional) | C# |
| Product application CI workflows | **your** app repos | YAML templates from `templates/` |

## Security

- Report vulnerabilities **privately** (see [SECURITY.md](SECURITY.md)).
- Never open a public issue with live tokens or private keys.
- CI fails if high-confidence secret patterns land in the tree.

## Code of conduct (lightweight)

Be respectful. Assume good intent. Prefer evidence and reproducible steps over
adjectives when discussing performance or disk-safety.

## License

By contributing, you agree that your contributions are licensed under the
MIT License (see [LICENSE](LICENSE)).
