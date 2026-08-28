---
name: observability
description: Observabilidade do civm — civmctl read-only, slog estruturado, host-metrics, log de manutenção, Prometheus textfile.
paths:
  - "cmd/civmctl/**"
  - "internal/**"
  - "deploy/windows/*.ps1"
---

# Observability rules

civm é infra Go (runner self-hosted + camada host Hyper-V). Observabilidade aqui
é sobre **estado da VM/runner e da limpeza de disco**, não sobre HTTP/tenant/DB.

## Estado da VM/runner (read-only)

`civmctl doctor --repos=auto --json` e `civmctl capacity --json` são a rota
read-only canônica para estado da VM/runner. `capacity` usa 90% de disco como
hard-fail para `accepting_jobs=false`; pressão antes do job começa em 60% via
`disk-watchdog` e hook `job-started` (`civm.DefaultPreCleanupPct`).

`civmctl disk-audit --json` reporta ownership seguro de disco: `_work`,
`_work/_tool`, `_work/_actions`, `$HOME/.cache`, `$HOME/go/pkg`,
`$HOME/codespace`, Docker reclaimable, `/var/log` e `/var/cache`. Clones em
`$HOME/codespace` são observabilidade-only e não são removidos automaticamente.

`civmctl health` agrega o estado dos timers. `civmctl-metrics.timer` deve ficar
habilitado junto com cleanup, disk-watchdog, mem-watchdog, runner-watchdog e
reverse-watchdog. Metrics missing é warning; cleanup e disk-watchdog missing
continuam críticos.

O runner-watchdog emite `runner-restart-skipped/maintenance-active` com exit 0
quando o snapshot `/var/lib/civm/maintenance.json` existe. Erro ao consultar o
snapshot emite `maintenance-state-unknown`, sai 1 e não toca rede, hooks nem
systemd. Um `runner-restarted` durante maintenance é violação crítica.

## Logs estruturados

**Go (civmctl):** `slog.JSONHandler` é o handler default. Nunca `fmt.Println` ou
`log.Printf` em produção — sempre `slog` com contexto.

```go
slog.InfoContext(ctx, "hook job-started",
    slog.String("repo", repo),
    slog.String("work_root", workRoot),
    slog.Int("disk_pct", pct),
)
```

**Camada host (PowerShell):** as tasks `deploy/windows/*.ps1` emitem **uma linha
JSON por evento** (campos `ts`/`timestamp`, `level`, `event`, `vm`, + dados).
ERROR/CRITICAL também vão pra stderr.

- **Orchestrator scale-to-zero (dono vivo C#):** a task
  `civm-host-orchestrator` escreve JSONL em **`V:\civm-host-shadow.jsonl`**.
  Cada tick registra decisão, estado da VM, fila, geração
  `<contexto>@<head_sha>`, resultado da limpeza/compactação, broker-ready,
  publicação e latch de processo. O watchdog independente grava o snapshot
  atual em **`V:\civm-watchdog-status.txt`** e o histórico em
  **`V:\civm-watchdog.log`**; owner zero/dual, heartbeat >45 min, last result
  falho ou `processBlockedReason` produzem `DRIFT`. Ele é detect-only.
- **Orchestrator PowerShell legado (rollback, `Disabled`):**
  `civm-vm-orchestrator.ps1` escrevia em **`V:\civm-orchestrator.log`** com
  eventos `tick`, `vm_started`, `disk_warn`, `disk_panic`, `reclaim_*`,
  `guest_full_clean` e `orchestrator_error`. O catálogo permanece para leitura
  histórica; não é o emissor vivo após o cutover C#.
- **Mecanismo de reclaim antigo (SUPERSEDED 2026-06-17, tasks `Disabled`):** o
  `civm-vhdx-autoreclaim`/`optimize`/`optimize-watchdog` escreviam em
  `V:\civm-hyperv-maintenance.log` com eventos `autoreclaim_*`, `optimize_*`,
  `emergency_reclaim_*`, `watchdog_*`. Catálogo preservado para leitura
  histórica; esses eventos não saem mais em operação normal.

**Dispatcher JIT:** cada transição emite `request_id`, `repository`, `status`,
`run_id`, `job_id` e `runner_id`. O ledger 0600 persiste também lease,
PID/start time, process group, cgroup e identidade/base do isolamento; nunca a
chave de idempotência raw. Stdout/stderr do driver pinado são redigidos no
diretório do request. Token, JIT config, Authorization e body remoto de erro
nunca entram em log, erro, ledger ou métrica. `ambiguous` e cleanup sem as 3
postconditions (VM destruída, run terminal, runner ausente) mantêm o slot do
Guard e exigem reconciliação/inspeção, sem retry de POST.

**Hooks de job:** registram em `hooks.jsonl` (uma linha por job-started/finished,
com `WorkRoot`, disco, cleanup aplicado).

## Métricas

`civmctl metrics dump --stdout` e o **Prometheus textfile collector** (via
`civmctl-metrics.timer`) expõem contadores de capacidade/disco/cleanup para
scrape local. `host-metrics.json` (no host, `V:\`) carrega `v_free_gb` e o gap do
VHDX, consumido pelo guard de headroom do reclaim.

O runner-watchdog expõe `queue-stall-armed`, `runner-restarted`,
`queue-stall-recovered` e `queue-stall-unresolved`, com unit, contagem e idade,
sem IDs como labels. `capacity` lê o marker semântico: qualquer stall ativo ou
marker inválido torna `accepting_jobs=false`. Prometheus expõe
`civm_runner_queue_stalled`; `health` verifica timer e resultado do service.

## Log de validação empírica (`validation.md`)

`validation.md` na raiz é o log append-only de **toda validação empírica de
infra** — a fonte de verdade para "isso está de fato funcionando agora?". A
definição, a taxonomia de categorias e o framing Kahneman #13 vivem no **header
do `validation.md`**; complementa o `vm.md` (inventário da máquina). Validação de
app vive no `validation.md` do **acme** (independente); não logue app aqui.

Regras de uso:

- Append-only: entrada mais recente no fim; nunca delete, reescreva nem reordene.
  Leia de baixo para cima.
- Toda entrada carrega DADOS medidos (número real, sem adjetivo antes do número)
  e um veredito explícito.
- Schema: `## YYYY-MM-DD HH:MM -03 — <titulo>`, depois `**O que:**`,
  `**Dados medidos:**`, `**Veredito:**` (✅/🔴/🟡) e `**Proxima acao:**`.
  Opcionais: `**Categoria:**` (tag da taxonomia) e `**Como medir:**` (comando de repro).
- Nunca persista secret/token/PAT/chave, valor de env ou PII.

## Não logar segredo

Nunca logar token/PAT/chave raw (GitHub App key, SSH key, `gh` token). Mascarar
ou omitir. civm é infra: não há PII de usuário final no caminho.

## Don't

- ❌ `fmt.Println` / `log.Printf` em produção (use `slog`).
- ❌ Engolir erro sem log de contexto (`%w` + `slog`).
- ❌ Logar token/chave/secret raw.
- ❌ Métrica/evento órfão sem consumidor (`civmctl health`, runbook, scrape).
- ❌ Task host que muta sem emitir evento no log estruturado da sua camada
  (`V:\civm-orchestrator.log` para o orchestrator vivo;
  `V:\civm-hyperv-maintenance.log` para o mecanismo de reclaim antigo).
