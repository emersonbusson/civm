# Runbook — Windows host orchestrator (scale-to-zero)

> Day-0 host path for Hyper-V. **Secrets never enter git.** Product/org fleet is
> **operator config**, not repository defaults (open-source hygiene).

## Topology

| Layer | Role |
| --- | --- |
| Guest Linux (`civmctl`) | Runners, cleanup, reaper, doctor |
| Host Windows (this runbook) | Scale-to-zero: start/stop VM, `Optimize-VHD`, GitHub queue poll |
| Sibling **civm-host** | Owner C# ativo após F4; PowerShell permanece somente como rollback |

## Preconditions

1. Hyper-V VM exists (name default `gha-ubuntu-2404`).
2. Volume for VHDX (default `V:`) with headroom.
3. **SYSTEM-readable** SSH key: `C:\ProgramData\civm\ssh\id_ed25519` (ACL: SYSTEM only).
4. One fine-grained PAT per GitHub owner with **actions:read**, as files:
   `C:\ProgramData\civm\gh-token-<owner>.txt` (single line, no log echo).
5. Guest user + IP/DNS reachable from SYSTEM SSH (prefer stable LAN IP if Tailscale DNS is stale).
6. Capability guest `civm-generation-boundary/v1` instalada antes do owner C#.

## Owner ativo e rollback

Produção usa `civm-host-orchestrator` em modo active. Os scripts PowerShell
abaixo são o caminho de rollback e devem permanecer `Disabled` enquanto o C#
estiver ativo. Nunca mantenha os dois owners em `Ready`/`Running`.

## Memória fixa da VM

O padrão é `12 GiB` fixos. A alteração é operação de boundary: aguarde fila
vazia, confirme `Runner.Worker=0`, deixe o owner concluir cleanup/compactação e
confirme a VM `Off`. O helper não inicia nem desliga a VM.

Em PowerShell elevado, primeiro revise o plano e só depois aplique:

```powershell
Get-VM -Name gha-ubuntu-2404 | Select-Object Name, State

.\configure-civm-vm-memory.ps1
.\configure-civm-vm-memory.ps1 -Execute

Get-VMMemory -VMName gha-ubuntu-2404 |
  Select-Object DynamicMemoryEnabled, Startup, Minimum, Maximum
Get-VM -Name gha-ubuntu-2404 | Select-Object Name, State
```

Pós-condição: `DynamicMemoryEnabled=False`, `Startup=12884901888` e VM ainda
`Off`. Reaplicar deve retornar `noop`. O owner C# continua sendo o único
responsável pelo próximo start.

Rollback, também somente com a VM `Off`:

```powershell
Set-VMMemory -VMName gha-ubuntu-2404 `
  -DynamicMemoryEnabled $true `
  -MinimumBytes 7GB -StartupBytes 7.5GB -MaximumBytes 12GB
```

Rollback trigger: qualquer um dos 3 primeiros jobs terminar por OOM/exit 137,
ou o host permanecer com menos de 1 GiB livre por 5 minutos.

## Deploy scripts PowerShell (rollback)

From an elevated PowerShell, in the repo `deploy/windows` (or a staging copy):

```powershell
# Copies decision/reclaim/pr-queue/orchestrator/host-metrics into C:\civm-deploy
# and registers SYSTEM task. Default registration uses -Observe (safe).
pwsh -NoProfile -ExecutionPolicy Bypass -File .\activate-orchestrator.ps1
```

`register-orchestrator.ps1` alone registers **Observe** only. Production EnforceQueue
must pass `-EnforceQueue` **and** non-empty `Repos` + `TokenPaths`.

## Host-local lab wrapper pattern (recommended)

Public `civm-vm-orchestrator.ps1` defaults:

- `Repos = @()`
- `TokenPaths = @{}`
- `GuestSshTarget = 'emdev@gha-ubuntu-2404'` (example host; override)

Keep **lab fleet out of git**. Example host-only wrapper (not committed):

```powershell
# C:\civm-deploy\civm-vm-orchestrator-lab.ps1
param([switch]$Observe, [switch]$EnforceQueue)
$invoke = @{
  TokenPaths     = @{ 'myorg' = 'C:\ProgramData\civm\gh-token-myorg.txt' }
  Repos          = @('myorg/app', 'myorg/other')
  GuestSshTarget = 'myuser@<GUEST_IP>'   # stable reachability
}
if ($Observe)      { $invoke['Observe'] = $true }
if ($EnforceQueue) { $invoke['EnforceQueue'] = $true }
& "$PSScriptRoot\civm-vm-orchestrator.ps1" @invoke
```

Register the wrapper as the Scheduled Task action (SYSTEM, every ~2 min, boot trigger).
Disable legacy `civm-vhdx-autoreclaim`, `civm-vhdx-optimize` e
`civm-vhdx-optimize-watchdog` para que **um owner** detenha stop/compact
(Kahneman #15). `civm-watchdog` é somente detector e permanece ativo.

## Owner-aware watchdog

O watchdog de disponibilidade é separado do owner e nunca inicia, habilita ou
troca o processo de orquestração. Ele aceita exatamente um owner ativo: C# em
modo active ou PowerShell `-EnforceQueue` durante rollback.

```powershell
# Elevado: valida AST, copia para C:\civm-deploy e registra SYSTEM.
powershell -NoProfile -ExecutionPolicy Bypass `
  -File .\register-civm-watchdog.ps1

Get-Content V:\civm-watchdog-status.txt
Get-ScheduledTaskInfo civm-watchdog
```

Heartbeat C# maior que `45 min`, owner zero/dual, last result não zero ou
`processBlockedReason` preenchido produzem DRIFT. A task roda a cada `20 min`,
no startup, com `ExecutionTimeLimit=5 min`.

The task contract is also part of availability: `AtStartup`, `StartWhenAvailable`,
`DisallowStartIfOnBatteries=false` and `StopIfGoingOnBatteries=false`. Registration
must replace the existing definition with `Register-ScheduledTask -Force`; never
unregister the last valid definition before deployment validation finishes.

## host-metrics

```powershell
# Register metrics task (separate cadence) — override GuestSshTarget if needed:
pwsh -File C:\civm-deploy\civm-host-metrics.ps1 -GuestSshTarget 'myuser@<GUEST_IP>'
```

Snapshot: `V:\civm-host-metrics.json`. When the VM is Off, guest `df` over SSH fails by design;
the orchestrator treats missing/`<=0` guest free as **999 (unknown)** — does not block admit.

## Validate (numbers before adjectives)

| Check | Expect |
| --- | --- |
| Task `civm-host-orchestrator` | `Ready`/`Running`, modo active e `LastTaskResult=0` |
| Task `civm-vm-orchestrator` | `Disabled`; somente rollback |
| Log `V:\civm-host-shadow.jsonl` | heartbeat <45 min; ticks com geração e decisão |
| Idle ≥ `IdleStopMinutes` (default 10) + empty queue | `reclaim_start` → VM Off; optional `reclaim_mount_retry` then `reclaim_done` |
| Queue while Off | `vm_started` → guest runners online |
| Lock | Only one of PS orch / civm-host **active** holds `V:\civm-reclaim.lock` |
| Reboot/power policy | boot trigger present; start/stop-on-battery both `false`; `StartWhenAvailable=true` |
| Task `civm-watchdog` | `LastTaskResult=0`; status `OK`; owner exatamente `1` |
| Guest boundary | probe imprime exatamente `civm-generation-boundary/v1` |
| Reaper | timer `enabled+active`; eventos classificáveis no journal |

Inspect the effective definition from an elevated shell (a non-elevated query can
hide SYSTEM tasks):

```powershell
$t = Get-ScheduledTask -TaskName 'civm-host-orchestrator'
$t | Select-Object State, Actions, Triggers, Settings
```

## Rollback trigger

- `orchestrator_error` rate dominates ticks, or VM thrash (start/stop loop) under normal queue.
- Dual active reclaimers (PS + `civm-host --active`) without F4 cutover — disable one immediately.

## Related

- Behavior SPEC: `docs/specs/orchestrator-scale-to-zero/`
- Rollout guest→host: `runbooks/GENERATION-CLEAN-BOUNDARY-ROLLOUT.md`
- PR-queue canary: `runbooks/PR-QUEUE-ENABLE.md`
- VHDX maintenance: `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md`
- Go host port: **superseded** — `docs/specs/orchestrator-go-port/STATUS.md`
- C# rewrite: sibling `civm-host` ROADMAP F3/F4
