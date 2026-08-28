---
slug: civm-v1-finalization
title: civm v1 Operational Finalization IMPL
milestone: v1.0.0
issues: [3]
---

# IMPL — civm v1 Operational Finalization

**Status:** local v1 tag ready
**Issue:** https://github.com/emersonbusson/civm/issues/3
**PRD:** `docs/specs/civm-v1-finalization/PRD.md`
**SPEC:** `docs/specs/civm-v1-finalization/SPEC.md`
**Validation timestamp:** 2026-05-11T00:21:41Z

## 1. Commits

| Item | Valor |
|---|---|
| Base antes da formalizacao | `0fdf5430ff500ba6219412860495ae2609044105` |
| Commit de formalizacao | commit apontado pela tag `v1.0.0` |
| Tag local | `v1.0.0`, criada localmente sobre o commit que contem este IMPL |
| Release remota | `https://github.com/emersonbusson/civm/releases/tag/v1.0.0` |

## 2. Validacao local

| Comando | Resultado |
|---|---|
| `git status --short --branch` | `main...origin/main`; apenas `docs/specs/civm-v1-finalization/` novo |
| `git branch --merged main` | apenas `main` |
| `git branch -r --merged origin/main` | apenas `origin/main` |
| `gh pr list --state open` | `[]` |
| `gh issue list --state open` | apenas `#3` desta formalizacao |
| `go vet ./...` | passou |
| `go build ./...` | passou |
| `go test -race -count=1 ./...` | passou em todos os pacotes |
| `go test -count=1 -cover ./internal/...` | passou; todos os pacotes `internal/**` >= 80% |
| `git diff --check` | passou |

## 3. CI real

| Item | Resultado |
|---|---|
| Ultimo run em `main` | `25641375952`, `success`, commit `0fdf5430ff500ba6219412860495ae2609044105` |
| Build + test civmctl | passou |
| Validate templates and runbooks | passou |
| Self-hosted runner smoke | passou |

## 4. VM operacional

| Item | Resultado |
|---|---|
| `/usr/local/bin/civmctl` | instalado; sha256 `cbdc1534a3a89653eae7e5400309dbe39a0925720a8fcd408cdfe5875ff7e9bd` |
| `civmctl health` | exit `1` por warning `LAST`; DISK/MEM/RUNNERS/TIMER_* OK |
| `civmctl doctor --json` | exit `1` apenas por warning `LAST`; runners `civm-self`, `civm-peer`, `civm-app` online e `busy=false` |
| `civmctl idle-check` | `idle`, exit `0`, revalidado em `2026-05-11T00:21:41Z` |
| Cleanup preservando `_work/_tool` e `_work/_actions` | validado no rollout do PR `#2`; nenhum cleanup destrutivo novo executado nesta formalizacao |

## 5. Escopo fora da v1

Itens abaixo permanecem em `CODEX.md` como `DEFERRED` e nao bloqueiam a v1:

- GitHub App para `runner add`.
- Deploy para multiplas VMs.
- Snapshot/restore de cache.
- Exporter Prometheus proprio.
- Suporte `windows-latest` e `macos-latest`.

## 6. Veredito

Validacoes locais e CI do `civm` passaram. A VM esta operacional e ociosa.
A tag `v1.0.0` aponta para o commit de formalizacao. A release remota usa
o mesmo tag e fecha a issue `#3`.
