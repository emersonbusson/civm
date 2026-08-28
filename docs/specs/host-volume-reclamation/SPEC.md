---
slug: host-volume-reclamation
title: Reclamação de volume do host (VHDX) — guest-free vira host-free com segurança
milestone: —
issues: []
---

# SPEC — Reclamação de volume do host (VHDX): guest-free vira host-free com segurança

> **SUPERSEDED-BY (2026-06-17): orchestrator scale-to-zero.** O reclaim do VHDX
> agora pertence ao `civm-vm-orchestrator.ps1` (único dono do stop/compact/
> power-state; tasks `autoreclaim`/`optimize`/`optimize-watchdog` `Disabled`).
> Fonte de verdade viva: `docs/specs/orchestrator-scale-to-zero/`. Esta cadeia
> (`SPEC.md` → `SPECv2.md` → `SPECv3.md`) é preservada como histórico — não a
> reimplemente.

> Gerado a partir de `docs/specs/host-volume-reclamation/PRD.md` (PASSO 2 SSDV3).
> Disciplinas: `disciplines/KAHNEMAN-DISCIPLINES.md`. Validação: `go test`, `golangci-lint run`, `npm run docs:check`.

## Escopo fechado desta implementação

**Entra agora:**

- `civmctl disk-doctor` (guest): diagnóstico do pipeline de discard (controlador, `discard`, TRIM, efetividade) — RF-1.
- Auto-shrink online (VHDX em SCSI + discard efetivo): procedimento + verificação — RF-2 (runbook one-time + verificação por `disk-doctor`).
- Observabilidade do host: `deploy/windows/civm-host-metrics.ps1` + Scheduled Task → JSON entregue ao guest; `civmctl host-disk` consome — RF-3.
- Compactação offline segura e não-interativa: `deploy/windows/civm-vhdx-optimize.ps1` + Scheduled Task SYSTEM — RF-4.
- `civmctl maintenance enter|exit` (guest): drain idempotente — RF-5.
- Right-sizing/headroom: invariante reportado + procedimento — RF-6.
- Runbook + docs — RF-7.

**Fica fora agora:** reescrever civmctl em Windows; trocar hypervisor; expandir disco como solução única; multi-project isolation (outro spec).

**Dependências assumidas prontas:**

- `fstrim -av` já roda no hook (`internal/hook/hook.go:200`); RF-2 o torna efetivo.
- `idle.Check` (`internal/idle/idle.go:116-169`) detecta busy/idle — reusado por `maintenance`/optimize.
- Dispatch por `switch` (`cmd/civmctl/main.go:40-95`); padrão `Options{...Fn}` injetável (`internal/diskaudit`, `internal/capacity`).
- Convenção `deploy/systemd/` (guest) espelhada em `deploy/windows/` (host).

## Matriz de rastreabilidade PRD → SPEC

| PRD | Implementação no SPEC |
| --- | --- |
| RF-1 disk-doctor | ITEM-3 (`internal/diskdoctor`), ITEM-7 (`cmd/civmctl/diskdoctor.go`), ITEM-9 (dispatch) |
| RF-2 auto-shrink SCSI/discard | ITEM-1 (Slice 0 diag), ITEM-12 (runbook one-time) + verificação por disk-doctor |
| RF-3 host observabilidade | ITEM-2 (constantes), ITEM-5 (`internal/hostdisk`), ITEM-8 (`cmd/civmctl/hostdisk.go`), ITEM-10 (`deploy/windows/civm-host-metrics.ps1`), ITEM-9 |
| RF-4 compactação segura | ITEM-11 (`deploy/windows/civm-vhdx-optimize.ps1` + task SYSTEM), ITEM-2 (headroom) |
| RF-5 maintenance drain | ITEM-4 (`internal/maintenance`), ITEM-6 (`cmd/civmctl/maintenance.go`), ITEM-9 |
| RF-6 headroom | ITEM-2 (constantes), ITEM-3/ITEM-5 (campos de violação), ITEM-12 (runbook) |
| RF-7 docs | ITEM-12 (runbook), ITEM-13 (MULTI-PROJECT-RUNNER + help) |

## Decisões técnicas

| # | Decisão | Justificativa |
| --- | --- | --- |
| DT-1 | O guest **não vê** o `V:`. O host-metrics task escreve `V:\civm-host-metrics.json` **e entrega uma cópia ao guest** em `/var/lib/civm/host-metrics.json` pelo canal SSH que o host já usa; `civmctl host-disk` lê a cópia guest-local. | Mantém `civmctl` guest-side autoritativo; dá awareness ao guest sem montar `V:`. Freshness via `timestamp` + warn se stale. |
| DT-2 | Compactação como **Scheduled Task SYSTEM** acionada por `schtasks /run`, não por `Start-Process -Verb RunAs`. | SYSTEM tem direito de Hyper-V sem UAC interativo; elimina o "pendurado no UAC" observado. |
| DT-3 | A task de compactação **proíbe zero-fill** e **aborta** se `v_free_gb < DefaultHostVolumeHeadroomGB`. | Zero-fill cresce o VHDX; sob baixo headroom estoura o `V:` (risco real observado a 3 GB). |
| DT-4 | Caminho **primário é online** (RF-2: SCSI+discard→VHDX encolhe via `fstrim` existente). Offline `Optimize-VHD` é **fallback**. | Conserta a raiz; evita downtime recorrente; Codex tratava só o sintoma offline. |
| DT-5 | `disk-doctor` é o **gate de diagnóstico** antes de afirmar qualquer coisa sobre discard (Kahneman #3). | Codex nunca diagnosticou por que `fstrim` liberou só 737 MiB; medir antes de agir. |
| DT-6 | `maintenance` grava o estado anterior (labels/serviços) em `/var/lib/civm/maintenance.json` para `exit` idempotente. | Substitui a remoção/recolocação manual de label por restore determinístico. |
| DT-7 | Componente host vive em `deploy/windows/` (scripts PS + registro de task), versionado, sem segredo, sem rede. | Espelha `deploy/systemd/`; isola o boundary host; reversível por `schtasks /delete`. |

## Fronteira de atomicidade e política de rollback

- **Atômico nesta entrega:** cada escrita de `/var/lib/civm/maintenance.json` e `host-metrics.json` (`os.WriteFile` substitui o arquivo); cada `Optimize-VHD` é uma operação Hyper-V única; o par `maintenance enter/exit` é idempotente mas **não** transacional entre si.
- **Fora da atomicidade:** o ciclo drain→shutdown→optimize→start→restore (multi-passo, VM mudando de estado); a entrega SSH das métricas (best-effort). Estados parciais aceitos: VM drenada mas não compactada (restore traz de volta); métricas ausentes/stale (civm degrada para guest-only).
- **Política de rollback:**
  - **App:** `civmctl self-upgrade` anterior; subcomandos novos viram no-op.
  - **Host:** `schtasks /delete /tn civm-host-metrics` e `/tn civm-vhdx-optimize`; reverter SCSI→IDE (janela) se a troca quebrar boot.
  - **Dados:** N/A.
  - **Proibido:** zero-fill sob baixo headroom; deixar a VM Off ao fim de qualquer caminho.
  - **`forward-only`?** Não — tudo reversível por remoção de task/binário; a troca SCSI é reversível em janela.

## Mapa Kahneman por etapa crítica

| Etapa / ITEM | Disciplina | Link | Pergunta obrigatória | Evidência mínima | Abort trigger |
| --- | --- | --- | --- | --- | --- |
| ITEM-1/ITEM-3 (diagnóstico) | #3 Número não adjetivo | `disciplines/KAHNEMAN-DISCIPLINES.md` #3 | Por que o `fstrim` liberou só 737 MiB — IDE? sem discard? TRIM ausente? | `disk-doctor --json` com controlador, `/proc/mounts` discard, `lsblk -D` DISC-MAX, delta de teste | Afirmar causa sem medir |
| ITEM-12 (SCSI/discard RF-2) | #5 Availability heuristic | idem #5 | Trocar para SCSI muda device name/quebra boot/fstab? | `fstab` por UUID + boot OK + `disk-doctor trim_effective=true` + VHDX encolhe ≈N GB após liberar N GB | VHDX não encolher após N GB liberados em 3 medições |
| ITEM-11 (Optimize-VHD task RF-4) | #5 Availability heuristic | idem #5 | A task pode estourar o `V:` (zero-fill) ou deixar a VM Off? | Run com headroom-guard abortando sob `< headroom`; teste de falha do Optimize religando a VM; log antes/depois | VM ficar Off ao fim; zero-fill sob baixo headroom |
| ITEM-2/ITEM-5 (headroom/obs RF-3/RF-6) | #2 Counterfactual | idem #2 | Qual número dispara alarme/rollback? | `host-disk` warn a 30 GB / crit a 10 GB; invariante `v_size-vhdx_max ≥ headroom` reportado | `v_free_gb` cruzar 10 GB sem alarme prévio |
| ITEM-4 (maintenance RF-5) | #5 Availability heuristic | idem #5 | Drain interrompe build em andamento? | `idle.Check` antes de drenar; teste idempotência enter/exit | Drain matar job ativo |

**Rollback trigger numérico (fecha o PRD §9):** reverter a slice se, após RF-2, liberar N GB no guest + `fstrim` **não** reduzir o VHDX FileSize em ≈N GB (±20%) em 3 medições; OU a task de compactação deixar a VM Off ≥1 vez; OU `v_free_gb` cruzar 10 GB sem alarme prévio de 30 GB.

## Checklist de segurança (pré-implementação)

- [ ] **Tenant isolation:** N/A (infra de runner).
- [ ] **SQL injection:** N/A.
- [ ] **Privilégio host:** task `civm-vhdx-optimize` roda como SYSTEM com direito Hyper-V — privilégio mínimo para `Optimize-VHD` sem UAC; documentado.
- [ ] **Exec safety:** scripts em `deploy/windows/` versionados, sem `Invoke-Expression` de input externo; civmctl usa `exec.CommandContext` sem shell.
- [ ] **Input validation:** `host-disk` valida JSON + timestamp (stale → warn); `maintenance` valida flags; headroom-guard valida `v_free_gb` numérico.
- [ ] **Secrets:** nenhum segredo em `deploy/windows/`; usa `gh`/SSH já presentes. Logs sem PII.
- [ ] **Error messages:** task nunca deixa a VM Off; erros logados em `V:\civm-hyperv-maintenance.log` sem segredo.
- [ ] **Zero-fill:** proibido por contrato sob baixo headroom.

## Migrações SQL

**N/A — sem banco.** Estado novo: `/var/lib/civm/maintenance.json` (guest), `/var/lib/civm/host-metrics.json` (cópia entregue), `V:\civm-host-metrics.json` + `V:\civm-hyperv-maintenance.log` (host). Backfill = **N/A — Day-0**.

## Arquivos a CRIAR

### `internal/diskdoctor/diskdoctor.go` (+ `diskdoctor_test.go`) — ITEM-3

- **Propósito:** diagnosticar por que o discard/`fstrim` recupera (ou não) espaço para o VHDX.
- **Requisitos:** RF-1, RF-6 (campo de headroom quando host-metrics disponível), DT-5.
- **Structs/Funções:**
  - `type Options struct { RootPath string; ReadFileFn func(string)([]byte,error); RunFn func(ctx,name,...string)([]byte,error); HostMetricsPath string }`
  - `type Report struct { Device string `json:"device"`; Controller string `json:"controller"` /* scsi|ide|virtio|unknown */; MountDiscard bool `json:"mount_discard"`; DiscGranBytes int64 `json:"disc_gran_bytes"`; DiscMaxBytes int64 `json:"disc_max_bytes"`; TrimEffective bool `json:"trim_effective"`; RootCause string `json:"root_cause"`; HostHeadroomViolation bool `json:"host_headroom_violation,omitempty"` }`
  - `func DefaultOptions() Options`; `func Diagnose(ctx, opts) (Report, error)` — passos: resolver device de `RootPath` (`findmnt`/`/proc/mounts`); ler `discard` em `/proc/mounts`; `lsblk -D -b -o NAME,DISC-GRAN,DISC-MAX` para o device; classificar controlador (`/sys/block/.../device` ou `lsblk -o TRAN`); compor `RootCause` (ex.: "controlador IDE não repassa UNMAP" / "discard não suportado" / "TRIM ok — VHDX deve encolher online"); `TrimEffective = DiscMaxBytes>0`.
  - `RenderJSON/RenderText`.
- **Padrão de referência:** `internal/capacity/capacity.go` (Statfs/Options), `internal/diskaudit` (RunFn injetável).
- **Testes:** `/proc/mounts` com/sem discard; `lsblk -D` DISC-MAX 0 vs >0; controlador ide vs scsi; `RootCause` por caso.
- **Disciplina Kahneman:** #3 — ver Mapa.

### `internal/maintenance/maintenance.go` (+ test) — ITEM-4

- **Propósito:** drenar/restaurar este runner idempotentemente.
- **Requisitos:** RF-5, DT-6.
- **Structs/Funções:**
  - `type Options struct { Execute bool; StatePath string; Repos []string; RunFn func(ctx,name,...string)([]byte,error); GHFn func(ctx,args...string)([]byte,error); ReadFileFn/WriteFileFn ... }`
  - `type State struct { DrainedAt string `json:"drained_at"`; StoppedUnits []string `json:"stopped_units"`; RemovedLabels map[string]string `json:"removed_labels"` /* runnerID->label */ }`
  - `func Enter(ctx, opts) (State, error)` — checa `idle.Check` (não interrompe build), para `actions.runner.*` (`systemctl stop`) e/ou remove label `civm` via `gh api -X DELETE .../labels/civm`, grava `State` em `StatePath`.
  - `func Exit(ctx, opts) error` — lê `State`, re-adiciona labels (`gh api -X POST .../labels`), `systemctl start` das units, remove `StatePath`.
- **Padrão de referência:** `internal/idle` (Check), `internal/runner` (gh/systemctl), `internal/hook/install.go` (WriteFile state).
- **Testes:** enter grava state; exit restaura; re-run no-op; dry-run não muta; idle-busy bloqueia enter.
- **Disciplina Kahneman:** #5 — ver Mapa.

### `internal/hostdisk/hostdisk.go` (+ test) — ITEM-5

- **Propósito:** consumir as métricas do host entregues ao guest e avaliar pisos/headroom.
- **Requisitos:** RF-3, RF-6, DT-1.
- **Structs/Funções:**
  - `type Metrics struct { VFreeGB int64 `json:"v_free_gb"`; VSizeGB int64 `json:"v_size_gb"`; VHDXFileSizeGB int64 `json:"vhdx_file_size_gb"`; VHDXMinSizeGB int64 `json:"vhdx_min_size_gb"`; VHDXMaxSizeGB int64 `json:"vhdx_max_size_gb"`; GuestFreeGB int64 `json:"guest_free_gb"`; GapGB int64 `json:"gap_gb"`; Timestamp string `json:"timestamp"` }`
  - `type Report struct { Metrics; Stale bool `json:"stale"`; Level string `json:"level"` /* ok|warn|crit */; HeadroomViolation bool `json:"headroom_violation"`; Reason string `json:"reason,omitempty"` }`
  - `func DefaultOptions() Options` (Path `/var/lib/civm/host-metrics.json`, `MaxAge` from `DefaultHostMetricsMaxAgeMinutes`, warn/crit/headroom from `civm.Default*`).
  - `func Check(opts) (Report, error)` — lê JSON; `Stale` se `Timestamp` > `MaxAge`; `Level` por `VFreeGB` vs warn/crit; `HeadroomViolation` se `VSizeGB-VHDXMaxSizeGB < headroom`.
- **Padrão de referência:** `internal/capacity` (Report+Render).
- **Testes:** níveis ok/warn/crit; stale; headroom violation; arquivo ausente → `Stale`+reason.

### `cmd/civmctl/diskdoctor.go` / `maintenance.go` / `hostdisk.go` — ITEM-6/7/8

- `func runDiskDoctor(args) int` — `--json`; chama `diskdoctor.Diagnose`; exit 0 (diagnóstico).
- `func runMaintenance(args) int` — subgrupo `enter|exit` + `--execute`/`--json`/`--repos`; exit 0/erro.
- `func runHostDisk(args) int` — `--json`; `hostdisk.Check`; exit 0 (ok)/`1` (crit ou headroom violation) para uso em guard.
- **Padrão:** `cmd/civmctl/capacity.go`, `cmd/civmctl/runner.go` (subgrupo).
- **Testes:** dispatch + exit codes em `main_test.go`.

### `deploy/windows/civm-host-metrics.ps1` (+ registro de Scheduled Task) — ITEM-10

- **Propósito:** emitir métricas do host e entregá-las ao guest.
- **Requisitos:** RF-3, DT-1, DT-7.
- **Lógica (passos):** `Get-Volume -DriveLetter V` (free/size); `Get-VHD -Path <vhdx>` (FileSize/MinimumSize/Size); `df`/`ssh gha-ubuntu-2404 'df -B1 /'` para `guest_free`; calcular `gap`; escrever `V:\civm-host-metrics.json` (JSON com `timestamp` ISO); `scp`/`ssh ... 'cat > /var/lib/civm/host-metrics.json'` entrega ao guest. Read-only no host (sem `Optimize`/`Stop`).
- **Task:** trigger a cada N min (ex.: 15), com direito de leitura Hyper-V; registro versionado (XML ou `schtasks /create`).
- **Testes:** validação manual (rodar a task; checar JSON no host e no guest); PSScriptAnalyzer se disponível.

### `deploy/windows/civm-vhdx-optimize.ps1` (+ Scheduled Task SYSTEM) — ITEM-11

- **Propósito:** compactação offline segura, não-interativa.
- **Requisitos:** RF-4, DT-2, DT-3, DT-7.
- **Lógica (passos, abort-safe):**
  1. Ler métricas; **se `v_free_gb < DefaultHostVolumeHeadroomGB` → abort** com log `headroom` (nunca zero-fill).
  2. `ssh gha-ubuntu-2404 'civmctl maintenance enter --execute'` (drain) + `civmctl idle-check` até idle (timeout).
  3. `ssh gha-ubuntu-2404 'sudo fstrim -av'` (garante TRIM antes de compactar).
  4. `ssh gha-ubuntu-2404 'sudo shutdown -h now'`; aguardar `Get-VM` State=Off (timeout).
  5. `Optimize-VHD -Path <vhdx> -Mode Full -ErrorAction Stop` (timeout/try).
  6. **finally:** `Start-VM`; aguardar Running; `ssh ... 'civmctl maintenance exit --execute'`; **garantir VM nunca fica Off**.
  7. Log estruturado antes/depois em `V:\civm-hyperv-maintenance.log`.
- **Task:** SYSTEM, acionável `schtasks /run /tn civm-vhdx-optimize`; sem trigger interativo.
- **Testes:** validação manual em janela (headroom-guard abortando sob baixo espaço; falha simulada do Optimize religando a VM; idempotência).
- **Disciplina Kahneman:** #5 — ver Mapa (abort triggers: VM Off; zero-fill sob baixo headroom).

## Arquivos a MODIFICAR

### `internal/civm/civm.go` — ITEM-2

- **O que muda:** adicionar ao bloco `const (...)` (linhas 15-62).
- **Requisitos:** RF-3, RF-4, RF-6.
- **Depois (acrescentar):**
  ```go
  // Reclamação de volume do host (docs/specs/host-volume-reclamation).
  DefaultHostVolumeWarnFreeGB    = 30 // alinhado ao runbook ">30GB livres"
  DefaultHostVolumeCritFreeGB    = 10 // alinhado ao runbook "<10GB"
  DefaultHostVolumeHeadroomGB    = 8  // Optimize-VHD scratch; abaixo disso, abort (sem zero-fill)
  DefaultHostMetricsPath         = "/var/lib/civm/host-metrics.json" // cópia entregue ao guest
  DefaultHostMetricsMaxAgeMinutes = 30 // stale acima disso
  DefaultMaintenanceStatePath    = "/var/lib/civm/maintenance.json"
  ```
- **Impacto:** aditivo; nenhum caller quebra.

### `cmd/civmctl/main.go` — ITEM-9

- **O que muda:** registrar 3 subcomandos no `switch` (40-95) + `printHelp`.
- **Requisitos:** RF-1, RF-3, RF-5.
- **Depois (acrescentar cases):**
  ```go
  case "disk-doctor":
      os.Exit(runDiskDoctor(args))
  case "maintenance":
      os.Exit(runMaintenance(args))
  case "host-disk":
      os.Exit(runHostDisk(args))
  ```
  + linhas em `COMANDOS`/`EXEMPLOS`.
- **Impacto:** aditivo; padrão `switch` existente.
- **Testes:** `main_test.go` — dispatch dos 3.

### `internal/capacity/capacity.go` — ITEM-5b (opcional, aditivo)

- **O que muda:** `Check` pode incluir, best-effort, o nível do host se `hostdisk` disponível.
- **Requisitos:** RF-3.
- **Depois:** acrescentar campo `HostLevel string `json:"host_level,omitempty"`` ao `Report` (ok/warn/crit/unknown), populado via `hostdisk.Check` (erro→`unknown`, omitido). Sem acoplar capacity à presença do arquivo (degrada para `unknown`).
- **Impacto:** aditivo; consumidores antigos ignoram. Evitar import cycle (`capacity`→`hostdisk`→`civm`).

### `runbooks/MULTI-PROJECT-RUNNER.md` — ITEM-13

- **O que muda:** §Disk pressure / §Rollback trigger ganham bloco "Host VHDX e volume `V:`": gap guest×host, observabilidade (`host-disk`), pipeline SCSI/discard, Scheduled Task de compactação, `civmctl maintenance`, e a **proibição de zero-fill sob baixo headroom**. Cross-link para o novo runbook.
- **Requisitos:** RF-7.
- **Impacto:** documentação; `npm run docs:check`.

## Arquivos a CRIAR (docs) — ITEM-12

### `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md`

- **Propósito:** procedimento canônico de reclamação de volume do host.
- **Requisitos:** RF-2, RF-4, RF-6, RF-7.
- **Conteúdo:** diagnóstico (`civmctl disk-doctor`), correção SCSI/discard one-time (`fstab` por UUID), instalação/uso/segurança das Scheduled Tasks (`civm-host-metrics`, `civm-vhdx-optimize` SYSTEM), `civmctl maintenance enter|exit`, invariante de headroom + como aplicar (`Resize-VHD`/expandir `V:`/mover), e a **ordem segura** (online discard primeiro; offline só com headroom; nunca zero-fill sob baixo espaço). Inclui o caminho de emergência observado (host a 3 GB) com o passo seguro correto.

## Arquivos a DELETAR (se houver)

| Arquivo | Motivo |
| --- | --- |
| — | Nenhum. Aditivo; o procedimento manual ad-hoc do Codex não estava versionado. |

## Observabilidade

**`host-disk`/`host-metrics` JSON:** `v_free_gb`, `v_size_gb`, `vhdx_file_size_gb`, `vhdx_min_size_gb`, `vhdx_max_size_gb`, `guest_free_gb`, `gap_gb`, `timestamp`, `level` (ok/warn/crit), `stale`, `headroom_violation`.

**Eventos/log (host `V:\civm-hyperv-maintenance.log`):** `optimize_start`/`optimize_end` (before/after FileSize, duração), `abort_headroom`, `vm_restarted_on_error`, `drain_enter`/`drain_exit`.

**`capacity --json`:** campo aditivo `host_level`.

Sem PII, sem segredo, sem label de alta cardinalidade.

## Contratos e documentação viva

| Documento | Atualização | Motivo |
| --- | --- | --- |
| `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md` | Criar | procedimento host VHDX |
| `runbooks/MULTI-PROJECT-RUNNER.md` | Alterar | §Disk: host VHDX/observabilidade/zero-fill proibido |
| `deploy/windows/*` | Criar | scripts PS + Scheduled Task |
| `deploy/systemd/README.md` | Alterar | cross-ref guest↔host |
| `cmd/civmctl/main.go` printHelp | Alterar | disk-doctor/maintenance/host-disk |
| `docs/INDEX.md` | Regenerar | novo spec |
| `AGENTS.md`/`CODEX.md` | Alterar | boundary: civm com componente host |
| `disciplines/KAHNEMAN-DISCIPLINES.md` | N/A | sem nova disciplina |
| `docs/openapi/*`/SDK/eventos | N/A | sem contrato de produto |

## Ordem de implementação

1. **ITEM-1 — Diagnóstico/baseline (Slice 0):** `disk-doctor` manual + medir VHDX × guest free (sem código); colar no IMPL.
2. **ITEM-2 — Constantes** (`internal/civm/civm.go`).
3. **ITEM-3 — `internal/diskdoctor`** + testes.
4. **ITEM-4 — `internal/maintenance`** + testes.
5. **ITEM-5 — `internal/hostdisk`** (+ ITEM-5b capacity) + testes.
6. **ITEM-6/7/8 — `cmd/civmctl/{diskdoctor,maintenance,hostdisk}.go`** + **ITEM-9 dispatch/help**.
7. **ITEM-10 — `deploy/windows/civm-host-metrics.ps1`** + task (host observabilidade).
8. **ITEM-12 (parcial) — SCSI/discard one-time (RF-2)** + verificar `disk-doctor trim_effective` + auto-shrink.
9. **ITEM-11 — `deploy/windows/civm-vhdx-optimize.ps1`** + task SYSTEM (compactação segura).
10. **ITEM-12/13 — runbook + MULTI-PROJECT-RUNNER + help** + `npm run docs:index`.
11. **Prova:** ciclo "liberar→fstrim→VHDX encolhe online" + um `civm-vhdx-optimize` seguro com `v_free_gb` recuperado e VM Running.

## Plano de testes

**Go (civm) — unitários:**

- `diskdoctor`: controlador ide/scsi, discard on/off, DISC-MAX 0/>0, `RootCause` por caso.
- `maintenance`: enter grava state, exit restaura, idempotência, dry-run, idle-busy bloqueia.
- `hostdisk`: níveis ok/warn/crit, stale, headroom violation, arquivo ausente.
- `capacity`: `host_level` aditivo serializa; `unknown` quando ausente.
- `cmd/civmctl`: dispatch + exit codes (`host-disk` crit→1).

**Go — integração (guest, `-race`):**

- `disk-doctor` no guest real (lsblk/proc reais) — captura baseline (Slice 0).
- `maintenance enter/exit` com runner fake + gh/systemctl mock.

**Host (manual/scriptado, em janela):**

- `civm-host-metrics` task escreve JSON no host e entrega ao guest; `civmctl host-disk` lê.
- `civm-vhdx-optimize`: headroom-guard aborta sob `v_free_gb < headroom`; falha simulada do `Optimize-VHD` religa a VM; idempotência; VM nunca fica Off.
- RF-2: medir VHDX FileSize antes/depois de liberar N GB + `fstrim` — deve cair ≈N GB sem `Optimize-VHD`.

**Manuais (evidência das etapas críticas):**

- `disk-doctor --json` colado no IMPL (root cause).
- Log `optimize_start/end` com before/after FileSize.
- `host-disk --json` mostrando `level` cruzando 30/10 GB.

## Checklist de validação

**Go (civm)**

- [ ] `gofmt -w ./...`
- [ ] `golangci-lint run -c .golangci.yml ./...`
- [ ] `go test ./... -race -count=1`
- [ ] `go build -o /tmp/civmctl ./cmd/civmctl` (compila com disk-doctor/maintenance/host-disk)

**Host (PowerShell)**

- [ ] PSScriptAnalyzer nos `.ps1` (se disponível) sem erros
- [ ] `schtasks /run /tn civm-host-metrics` produz JSON no host e no guest
- [ ] `schtasks /run /tn civm-vhdx-optimize` em janela: aborta sob baixo headroom; religa em erro; nunca deixa VM Off

**Docs**

- [ ] `npm run docs:index`
- [ ] `npm run docs:check`

**Gates cognitivos**

- [ ] Cada etapa crítica aponta `disciplines/KAHNEMAN-DISCIPLINES.md` (Mapa preenchido)
- [ ] Pergunta obrigatória, evidência mínima e abort trigger por etapa crítica
- [ ] Rollback trigger numérico definido (VHDX não encolhe ≈N GB / VM ficou Off / 10 GB sem alarme prévio)
- [ ] Zero-fill proibido sob baixo headroom documentado e testado
