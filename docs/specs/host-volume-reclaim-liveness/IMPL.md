# IMPL — Host Volume Reclaim Liveness

> **SUPERSEDED-BY (2026-06-17): orchestrator scale-to-zero.** O reclaim do VHDX
> agora pertence ao `civm-vm-orchestrator.ps1` (único dono do stop/compact/
> power-state; tasks `autoreclaim`/`optimize`/`optimize-watchdog` `Disabled`).
> Fonte de verdade viva: `docs/specs/orchestrator-scale-to-zero/`. O conteúdo
> abaixo é preservado como histórico do mecanismo anterior — não o reimplemente.

> SSDV3 PASSO 3. Implementa estritamente o `SPECv2.md` (ativo pós-auditoria 2.5).
> Escopo: RF-1 + RF-3 + backstop ExecutionTimeLimit. RF-2/RF-4 fora de escopo.

## Commits (branch `fix/autoreclaim-exec-time-limit`)

| Commit | RF | O quê |
| --- | --- | --- |
| `6ffbfee` | RF-1 (backstop) | `register-civm-vhdx-autoreclaim.ps1`: ExecutionTimeLimit 72h→2h após o `schtasks /create` (o default era P3D). Aplicado também no task vivo via `Set-ScheduledTask`. |
| _(este)_ | RF-1, RF-3 | `internal/hostdisk/phantom.go`+`_test.go` (predicado `IsPhantomReclaim`); `register-civm-vhdx-optimize.ps1` (watchdog detecta+limpa fantasma); `civm-vhdx-autoreclaim.ps1` (guest-prune antes do fstrim); docs SSDV3. |

## RF-1 — detector de fantasma (≤5 min)

- Predicado puro `IsPhantomReclaim(taskRunning, scriptProcessAlive,
  reclaimLockHeld)` em `internal/hostdisk/phantom.go`. Fantasma == Running +
  sem processo + lock órfão. Exigir AMBOS (sem processo E sem lock) evita matar
  recém-iniciado (janela de corrida).
- Orquestração: corpo do `civm-vhdx-optimize-watchdog` (a cada 5 min) — ANTES do
  `if state -eq Running { exit 0 }` (o fantasma ocorre com a VM Running). Win32
  process-scan + `Test-LockHeld` → `Stop-ScheduledTask civm-vhdx-autoreclaim` +
  remove locks órfãos + `reclaim_liveness_phantom_cleared`.
- Watchdog HABILITADO (estava Disabled; #16: a cura não fica desligada).
- Backstop: ExecutionTimeLimit=PT2H (`6ffbfee`).

## RF-3 — prune do guest antes do Optimize

- `civm-vhdx-autoreclaim.ps1`, após `Wait-GuestIdle` (guest idle → não
  interrompe job) e antes do `fstrim`: `Invoke-Guest 'civmctl disk-watchdog
--threshold-pct=0 --execute'` + `reclaim_liveness_guest_prune`. Best-effort.

## Deploy (host EMEDEV via sudo.exe — UAC off)

1. Backup de `C:\civm-deploy\{civm-vhdx-autoreclaim,civm-vhdx-optimize-watchdog,register-civm-vhdx-optimize}.ps1`
   → `.bak-liveness-20260615`.
2. Copiados os scripts editados para `C:\civm-deploy\`.
3. `register-civm-vhdx-optimize.ps1` rodado elevado → materializou o
   watchdog `.ps1` (RF-1) + recriou as tasks.
4. `civm-vhdx-optimize` → Disabled (estado deliberado anterior preservado);
   `civm-vhdx-optimize-watchdog` → **Enabled**.

Estados finais verificados no host:

```
civm-vhdx-autoreclaim         State=Ready    ExecTimeLimit=PT2H
civm-vhdx-optimize            State=Disabled ExecTimeLimit=PT72H
civm-vhdx-optimize-watchdog   State=Ready    ExecTimeLimit=PT72H  (Enabled)
```

## Validação (critérios do SPECv2)

| Critério | Resultado |
| --- | --- |
| RF-1 unit (Go, predicado) | `go test ./internal/hostdisk -race -run TestIsPhantomReclaim` PASS (5 casos: fantasma=true; processo vivo=false; lock segurado=false; recém-iniciado=false; not-running=false). |
| RF-1 host — par #13 (negativo, o crítico) | Watchdog rodado no estado normal (autoreclaim Ready) → lastResult=0, **SEM** `phantom_cleared`, autoreclaim **segue Ready** (não matou reclaim legítimo). |
| RF-1 host — positivo (limpa fantasma) | Coberto por: predicado testado + body deployado espelha-o (parse OK; `phantom_cleared`+lógica proc/lock presentes) + o clear manual idêntico (`Stop-ScheduledTask`) foi executado no firefight 2026-06-15 e recuperou o V:. Um fantasma real (quirk do Task Scheduler) não é injetável sob demanda. |
| RF-3 host (efeito) | Demonstrado ao vivo no firefight: sem prune `skip_low_gap gap=2.4GB`; com `docker_prune` (17.5GB) o gap virou ~28GB e o Optimize liberou V: 5.9→28GB. |
| Regressão | `go test ./... -race` (civm) verde; `golangci-lint` 0 issues; parse PS dos 2 scripts OK. |
| Firefight (incidente) | V: recuperado 5.9→28GB; VM Running; runner online. |

## Correção pós-validação (disciplina SSDV3: efeito > código)

A validação ao vivo (re-run do CI acme #1155 que dirigiu o V: ao piso, F3)
expôs um **bug de placement no RF-3**: o guest-prune estava DEPOIS do
`Wait-GuestIdle`, ou seja, DEPOIS do `autoreclaim_skip_low_gap`. Com o gap baixo
(0.82GB, guest cheio), o reclaim saía em `skip_low_gap` ANTES de chegar ao
prune — então o prune nunca corrigia o gap (o caso exato que deveria curar).
Kahneman #13: o código "existia" mas não funcionava no efeito.

**Fix (re-implementado + re-deployado):** o prune passou para DENTRO do
gap-check — se `gap < MinReclaimable`, pruna o guest, RE-busca o guest-free e
RE-computa o gap; só se AINDA baixo (dados ATIVOS, F3) faz `skip_low_gap`
(`after_prune=true`). Roda antes do `Wait-GuestIdle` (prune de docker UNUSED não
interrompe job). Provado ao vivo: prune liberou ~13GB de docker leftover, o gap
subiu de <1GB para >20GB, e o Optimize liberou o V: (2.8→recuperando).

## Limitações conhecidas (do PRD)

- **F3 (fora de escopo):** working set ativo de uma rajada concorrente pesada >
  capacidade do V: (119GB) é limite de **hardware**; nenhum reclaim compacta
  dados em uso. Mitigação: disco maior ou menor concorrência de CI (repo acme).
  A recusa de job no piso crítico permanece o fail-safe correto (#15).
- RF-1 positivo validado por unit+deploy+lived, não por injeção de fantasma real
  (não reproduzível sob demanda).

## Rastreabilidade

RF-1 → `phantom.go`/`_test.go` + watchdog body + watchdog Enabled + ExecTimeLimit.
RF-3 → `civm-vhdx-autoreclaim.ps1` guest-prune.
RF-2/RF-4 → fora de escopo (F3), documentado no PRD/SPECv2.
