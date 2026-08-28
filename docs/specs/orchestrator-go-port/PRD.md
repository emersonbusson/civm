---
slug: orchestrator-go-port
title: Port do orchestrator scale-to-zero de PowerShell para Go (civmctl orchestrate)
milestone: —
issues: ["#267"]
---

# PRD — Port do orchestrator scale-to-zero de PowerShell para Go

> Pipeline SSDV3: este é o **Passo 1 (PRD)**. Fonte de verdade do **comportamento** a preservar:
> [`docs/specs/orchestrator-scale-to-zero/SPEC.md`](../orchestrator-scale-to-zero/SPEC.md). Este PRD governa o
> **port** (mover a lógica para Go sem mudar o comportamento), não a política de scale-to-zero.
>
> **Revisado (2026-06-27) após auditoria completa do PRD** (3 red-teams): a decisão de **processo-pai do loop
> (Model A vs Model B)** foi **adiada para a Fase 2** — a Fase 1 (decisão pura) é **model-agnostic**. Correções de
> exatidão aplicadas: RF-7 reusa `internal/runner` (não `runreaper`); "round-trip semântico" (não "byte-compatível");
> riscos novos R6 (BOM) e R7 (paridade não-executável); `powershell.exe` reclassificado para **Confirmado**;
> cobertura "**100% statement**". Baseline anterior no histórico git.

## 1. Resumo

A decisão do orchestrator scale-to-zero (start/stop/compact/admissão) e a fila FIFO por-PR vivem hoje em
**PowerShell no host** (`deploy/windows/civm-orchestrator-decision.ps1`, `civm-pr-queue.ps1`,
`civm-reclaim-gate.ps1` + o loop `civm-vm-orchestrator.ps1`). Isso **duplica conceitos que já existem em Go** —
o gate de slack do reclaim (`Test-OptimizeSlack`) já tem gêmeo Go (`civm.EmergencyAdmits`), e a query de fila do
GitHub tem base reusável (`internal/activeruns`, via `gh`). Hoje a decisão de disk-safety roda **só** no `.ps1`,
testada por arquivos `*.test.ps1` executados **à mão** (fora do CI). **[Confirmado no codebase]**

**Problema:** lógica crítica de disk-safety duplicada e fora do CI gera risco de drift PS↔Go nos thresholds e
impede `go test -race`/cobertura sobre as decisões. **Valor operacional:** consolidar a decisão em Go dá
**fonte única**, testes de tabela no CI, e reduz o PowerShell do host a uma fina camada de **atuação Hyper-V**
(~6 cmdlets), que é o que genuinamente não tem API Go. **[Inferência]** (benefício esperado; medível pela queda de
linhas `.ps1` e pela cobertura do novo pacote).

## 2. Contexto técnico

**Topologia [Confirmado em docs — `CLAUDE.md` § Visão geral / `orchestrator-scale-to-zero/SPEC.md`]:**
host Windows (Hyper-V, volume `V:` NTFS hospedando o VHDX dinâmico) → VM Linux Ubuntu 24.04 (7 runners label
`civm`). `civmctl` (Go 1.26, stdlib-only) roda no guest. Disco é o recurso-gargalo; o orchestrator é uma
Scheduled Task SYSTEM que roda a cada ~2min.

**Camada PowerShell envolvida [Confirmado no codebase]:**

| Script | Papel | Pureza |
|---|---|---|
| `civm-orchestrator-decision.ps1` | `Get-OrchestratorDecision` (start/stop/compact/warn/panic), `Resolve-AdmitTransition`, `Update-AdmitAttempts` | **pura** (sem Hyper-V) |
| `civm-pr-queue.ps1` | `Resolve-PrSlot` (FIFO por-PR: grant/hold/boundary_advance/idle) | **pura** |
| `civm-reclaim-gate.ps1` | `Test-OptimizeSlack`, `Test-ReclaimStuck`, `Test-ReclaimCooldown` | **pura** |
| `civm-vm-orchestrator.ps1` | loop: query GitHub → lê estado `V:` → chama decisão → atua (`Start/Stop-VM`, SSH, `Optimize-VHD`) | impura (Hyper-V) |
| `civm-host-metrics.ps1` | snapshot de `V:`/VHDX + guest-free via SSH → JSON | impura (`Get-VHD`/`Get-Volume`) |
| `serialize-runner.ps1` | `civmctl doctor --json` via SSH + remove runners redundantes via GitHub API | impura (SSH) |

**Estado em Go que será reutilizado/estendido [Confirmado no codebase]:**

- `internal/civm/reclaim.go::EmergencyAdmits(liveFreeGB, hardFloorGB, scratchBudgetGB int64)` — **mesma fórmula** que
  `Test-OptimizeSlack`. Será reusado; **não reescrever**. (Nota: o PS recebe `[double]` e não tem a guarda
  `scratchBudget<=0 → false`; ver R8.)
- `internal/civm/civm.go` — já tem `DefaultHostVolumeHardFloorGB=1`, `DefaultHostVolumeScratchBudgetGB=11`,
  `DefaultReclaimMinIntervalMin=30`. Os thresholds **do orchestrator** (`AdmitFloorGB=55`, `GuestFloorGB=40`,
  `WarnFloorGB=28`, `PanicFloorGB=18`, `IdleStopMinutes=10`, `DoneGraceMinutes=3`, `PanicCooldownMinutes=15`,
  `MinRecoverGB=3`) **estão ausentes** (confirmado por leitura) — serão adicionados.
- `internal/activeruns.Collect()` → `Summary{InProgress, Queued}` — query de fila via **`gh`**. Reusável no
  **guest**; o orchestrator decide no **host** (sem `gh`), que usa fetcher `net/http`+PAT próprio (R5). Reuso real:
  o *shape* `Summary` + o idioma worker-pool, não a função.
- `internal/hostdisk.Metrics` — struct do snapshot de host (Go já **consome**; o produtor migra para cá).
- `internal/idle.Check()` — probe "job ativo" do guest. `internal/runner.DetectCollisions`/`Remove`/`List` —
  detecção e remoção de runners redundantes (o que `serialize-runner.ps1` faz; RF-7).
- `internal/capacity` / `internal/maintenance` — idiomas `RunFn`/`StatfsFn` injetáveis (exec + statfs) e o drain
  Enter/Exit que o host já aciona via SSH. `cmd/civmctl/main.go` — dispatch `switch` plano.
- **Pacote novo `internal/orchestrator` justificado:** a decisão scale-to-zero é domínio distinto de
  `internal/admit` (admissão de RAM via flock-slots) e de `internal/civm` (constantes/gates compartilhados), e não
  há símbolo Go `orchestrat*` a estender. Os *gates* ficam em `reclaim.go` e as *constantes* em `civm.go` (reuso);
  só a máquina de decisão é código novo.

## 3. Opção recomendada

**Port faseado; decisão pura primeiro; atuação Hyper-V permanece em PowerShell (sem WMI).** A **Fase 1**
(decisão pura) é **model-agnostic**: `civmctl orchestrate decide|pr-slot` são funções puras invocáveis de qualquer
lado. O **processo-pai do loop** — `civmctl.exe` como Scheduled Task shellando `powershell.exe` (**Model B**, mais
programático) **vs** o `.ps1`-shim continuar a task chamando `civmctl` (**Model A**, menor risco) — é **decisão
ABERTA da Fase 2** (ver §9 R3), não resolvida aqui. Em ambos os modelos, a atuação Hyper-V (~6 cmdlets) fica em
`powershell.exe` (PS 5.1). **[Confirmado no codebase — `activate-orchestrator.ps1:25` fixa `powershell.exe`; docs `validation.md`]**

**Por que é a recomendada no civm:** (a) reusa o idioma `RunFn` já pervasivo (`internal/capacity`,
`internal/maintenance`, `internal/runner`); (b) preserva o invariante **zero-deps** (`go.mod` sem `require`, sem
`go.sum`, zero `golang.org/x/sys`); (c) single-sourcing da decisão remove o drift PS↔Go; (d) põe a disk-safety no
CI com `-race`.

**Alternativas descartadas:**

- **Bindings WMI/CIM** (`github.com/microsoft/wmi`) — **descartada**: primeira dep de terceiros (quebra zero-deps),
  adiciona polling de `Msvm_ConcreteJob`, sem ganho sobre um cmdlet de uma linha. **[Confirmado no codebase — `go.mod` sem deps]**
- **Reescrever `Optimize-VHD -Mode Full` via syscall `virtdisk`** — **descartada**: operação mais perigosa do host
  (scratch em `V:`, **ininterruptível**); reimplementar contra precondições não-documentadas é risco/recompensa
  ruim. Fica em PowerShell. **[Confirmado em docs — Hyper-V]**
- **Manter PowerShell e só expandir os testes Pester** — **descartada**: não resolve a duplicação PS↔Go nem põe a
  decisão no CI Go.
- **Reescrita big-bang** — **descartada**: viola o staged-cutover fail-safe; a decisão pura é isolável e portável
  com paridade provada antes de tocar a atuação.

## 4. Requisitos funcionais (RF)

- **RF-1 — Decisão pura em Go, bit-idêntica ao PS.** Portar `Get-OrchestratorDecision`, `Resolve-AdmitTransition`,
  `Update-AdmitAttempts` (de `civm-orchestrator-decision.ps1`) e `Resolve-PrSlot` (de `civm-pr-queue.ps1`) para
  `internal/orchestrator/`, devolvendo **exatamente** as mesmas ações/objetos. **[Confirmado no codebase]**
- **RF-2 — Gates de reclaim em Go.** Adicionar `ReclaimCooldownOK` (= `Test-ReclaimCooldown`) e `ReclaimStuck`
  (= `Test-ReclaimStuck`) a `internal/civm/reclaim.go`, ao lado de `EmergencyAdmits` (já cobre `Test-OptimizeSlack`
  — **não reescrever**). `ReclaimCooldownOK` usa `time.Parse`; data ilegível → "pode reclamar" (fail-safe; espelha
  `catch { return $true }`). **[Confirmado no codebase]**
- **RF-3 — Thresholds como constantes Go.** Adicionar os 8 ausentes a `internal/civm/civm.go` com os **valores vivos
  verbatim** (55/40/28/18/10/3/15/3). Reusar os existentes (`DefaultHostVolumeHardFloorGB`/`ScratchBudgetGB`). **[Confirmado no codebase]**
- **RF-4 — Subcomando `civmctl orchestrate`.** `decide --json` e `pr-slot --json` (puros) na Fase 1; `tick` na Fase
  2. Registrar `case "orchestrate"` no `switch` de `main.go` + help; espelhar `cmd/civmctl/activeruns.go`. **[Confirmado no codebase]**
- **RF-5 — Loop + estado em Go (Fase 2).** `internal/orchestrator/queue.go` (filtro *ghost-queued*: só `run.status`
  real + idade < 12h, sobre um fetcher injetável — **fetcher do host é `net/http`+PAT novo**; `activeruns.Collect`
  é `gh`-only, reusável só no guest) e `state.go` (read/write **round-trip semântico** dos JSON de `V:` — ver §7).
  **[Confirmado no codebase — filtro lido no loop; fetcher do host detalhado no SPEC]**
- **RF-6 — Produtor de host-metrics em Go (Fase 3).** Mover a produção do snapshot para `internal/hostdisk.Metrics`
  (schema já existe); guest-free via `civmctl idle-check`/SSH; sobra em PS só `Get-VHD`/`Get-Volume`. **[Confirmado no codebase]**
- **RF-7 — `civmctl runner serialize` (Fase 3).** Substituir `serialize-runner.ps1` por um subcomando que reusa
  `internal/runner` (`DetectCollisions` + `Remove` + `List`) para detectar e remover runners redundantes.
  *(Correção da auditoria: `runreaper` cancela **runs**, não remove **runners**.)* **[Confirmado no codebase]**
- **RF-8 — Cutover fail-safe (exceção Day-0 time-boxed).** Os `.ps1` de decisão passam a **chamar**
  `civmctl orchestrate decide`, com fallback dot-source à função PS se o binário faltar/erro (Kahneman #15). É um
  **dual-path declarado como exceção Day-0**, removido quando a paridade ficar verde + N ticks (N no SPEC; SPECv2 =
  48) — os `.ps1` de decisão ficam **marcados para deleção**. A troca de atuação só ocorre após paridade verde;
  tasks legadas desabilitadas **atomically**. **Single-owner: nem dois atuadores, nem dois escritores de estado** (o
  observe escreve só em `*.observe.json`). **[Confirmado no codebase]**

## 5. Requisitos não-funcionais (RNF)

- **RNF-1 — Paridade total.** Harness **executável** alimenta os mesmos vetores a `pwsh <script>` e a
  `civmctl orchestrate decide --json` e afirma ação idêntica sobre **toda** a tabela. Exige **PowerShell 7 instalado
  no CI** (R7), com o golden **gerado mecanicamente dos `.ps1`** via pwsh (não transcrito à mão). **[Confirmado em
  docs — `disciplines/KAHNEMAN-DISCIPLINES.md` #13]**
- **RNF-2 — Cobertura + corrida.** `go test -race`; **cobertura 100% statement** nas funções puras novas
  (`internal/orchestrator` e os gates novos de `reclaim.go`) — Go **não mede branch nativo**, então a meta
  verificável é statement, enforçada por regra de CI dedicada. **Não cobrir código inexistente/inalcançável**; o
  ≥80% do repo é só **piso** para glue com I/O. **[Confirmado em docs — `rules/testing.md` (piso); feedback do
  usuário 2026-06-27, vale p/ todo o civm]**
- **RNF-3 — Zero deps de terceiros.** Manter stdlib (`os/exec`, `encoding/json`, `net/http`, `time`, `syscall`).
  Nenhuma entrada em `go.mod`/`go.sum`. **[Confirmado no codebase]**
- **RNF-4 — Threshold freeze.** **Refactor, não recalibração**: nenhum número de disk-safety muda; valores copiados
  verbatim do `.ps1`. **[Confirmado em docs — `orchestrator-scale-to-zero/SPEC.md`]**
- **RNF-5 — Privilégio/runtime.** A atuação Hyper-V roda como task SYSTEM (acesso a `V:`, chave SSH em
  `C:\ProgramData\civm`); pin **`powershell.exe`** (PS 5.1, onde o módulo Hyper-V vive), não `pwsh`. Quem **é** a
  task (Model A/B) é decisão da Fase 2. **[Confirmado no codebase + docs — `validation.md`]**

## 6. Fluxos

**Fluxo de decisão (tick) — Fase 2 [DIRECIONAL]:** (1) query GitHub ghost-filtrada (Go); (2) lê `V:\civm-*.json`
(Go, tolerante a BOM); (3) `orchestrator.Decide()` + `ResolvePrSlot` (Go puro); (4) persiste estado (Go, **escritor
único**); (5) emite a **ação** → o cmdlet Hyper-V é executado em `powershell.exe`. **Quem invoca quem (Model A/B) é
decisão da Fase 2.**

**Fluxo de cutover (fail-safe, RF-8):** decisão Go validada em `--observe` (loga `would_*` sem atuar; escreve só em
`*.observe.json`) contra a box viva → quando os `would_*` batem com o PS por **N ticks** (SPEC; SPECv2 = 48), a
nova assume e as `.ps1` legadas são desabilitadas atomically.

## 7. Modelo de dados

**State files no host `V:` (NTFS), efêmeros — política Day-0, sem migração [Confirmado no codebase]:**

- `V:\civm-orchestrator-state.json`: `admitReclaimAttempts` (int), `lastBusyUtc`, `lastPanicUtc`, `prevRunning`
  (int). (A ociosidade é **derivada de `lastBusyUtc`** — **não há** campo `idleSinceUtc` aqui.)
- `V:\civm-pr-queue.json`: `contexts[]` (ordem FIFO), `currentPr` (string), `currentIdleSinceUtc`.
- `V:\civm-current-context`: linha única **ascii, sem newline, sem BOM** (slot publicado, lido pelo gate runner).

Go deve fazer **round-trip semântico** (mesmos nomes/tipos de campo; tolerante a BOM/ordem de chaves/HTML-escape),
**não** byte-igualdade — a PS 5.1 grava com BOM e ordena chaves por inserção. **Encoding (R6):** o port grava os
state files **sem BOM** (`UTF8Encoding($false)`, igual ao `civm-host-metrics.ps1` que o Go já consome) — a PS 5.1
`Set-Content -Encoding UTF8` adiciona BOM, que quebra o `json.Unmarshal`. Day-0: forma final, sem shim. Golden test
com fixture **real da 5.1 (BOM-ful)** nos dois sentidos.

## 8. API / Interfaces

`civmctl orchestrate <sub> [--json]` (lista canônica de flags no SPEC) **[Confirmado no codebase — padrão de `activeruns.go`]:**

- `decide` — flags com **defaults verbatim do `.ps1`**: `--vm-state {Off|Running}`, `--queued`, `--running`,
  `--idle-min`, `--idle-stop-min`, `--v-free-gb` (**999**), `--guest-free-gb` (**999**), `--warn-floor-gb`,
  `--panic-floor-gb`, `--admit-floor-gb`, `--guest-floor-gb`, `--can-panic` (**default `true`** — `false`
  desabilitaria o `panic_compact`), `--prev-running`, `--admit-attempts`, `--has-active-job`. Saída JSON:
  `{action, admitReclaimAttempts}`.
- `pr-slot` — flags: `--prs <json>`, `--current-pr`, `--idle-since`, `--now`, `--grace-min`. Saída JSON:
  `{action, currentPr, idleSinceUtc, reason}`.
- `tick` (Fase 2, DIRECIONAL) — flags: `--observe`, `--state-dir`, `--repos`. Faz query→estado→decisão→emite ação.

## 9. Dependências e riscos

- **R1 — Drift de comportamento no port.** Mitigação: RNF-1 (harness executável) + traduzir os `.test.ps1` como
  oráculo; sub-risco: formato de timestamp `.ToString('o')` (7 frações) — passar verbatim, `RFC3339Nano`. **[Confirmado em docs — #13]**
- **R2 — `Optimize-VHD` ininterruptível.** `context` cancel do Go tem a **mesma** semântica do `Stop-Job`; sem
  regressão; controle real é o gate pré-flight (`EmergencyAdmits`). **[Confirmado no codebase — `reclaim.go`]**
- **R3 — Lock no Windows (decisão da Fase 2).** Sob **Model A** o lock `V:\civm-reclaim.lock` (`FileShare::None`)
  **permanece em PowerShell** (auto-release batido em produção); sob **Model B** seria reproduzido em Go com
  `syscall.CreateFile` sem share flags (não PID-file). A/B é da Fase 2. **[Confirmado no codebase — `civm-vm-orchestrator.ps1:320`]**
- **R4 — `pwsh` vs `powershell.exe` — RESOLVIDO.** `powershell.exe` (PS 5.1) já roda os cmdlets Hyper-V como task
  SYSTEM hoje. Pin `powershell.exe`. **[Confirmado no codebase + docs — `validation.md`]**
- **R5 — GitHub sem `gh` no host.** O host consulta via PAT (`Invoke-RestMethod`); o fetcher Go do host é
  `net/http`+PAT **novo** (`activeruns.Collect` é `gh`-only, reusável só no guest). **[Confirmado no codebase]**
- **R6 — Encoding/BOM dos state files (FUNDACIONAL).** PS 5.1 `Set-Content -Encoding UTF8` grava **com BOM** →
  `json.Unmarshal` Go falha → estado lido como vazio → `lastPanicUtc` perdido → **cooldown de panic anulado**
  (re-mata jobs). Mitigação: gravar **sem BOM** + `TrimPrefix` no reader. **[Confirmado no codebase —
  `civm-vm-orchestrator.ps1:194,589` vs `civm-host-metrics.ps1:139`]**
- **R7 — Paridade não-executável no CI (FUNDACIONAL).** `pwsh` não existe no CI → o harness "pula" → vira
  Go-testa-Go (oráculo circular). Mitigação: **instalar PowerShell 7 no CI** + gerar o golden mecanicamente dos
  `.ps1` via pwsh (`validation.md` prova pwsh 7 Linux rodando os `.test.ps1`). **[Confirmado no codebase — `ci.yml` sem pwsh; `validation.md`]**
- **R8 — Paridade fracionária do `EmergencyAdmits`.** `Test-OptimizeSlack` recebe `[double]`; `EmergencyAdmits`
  recebe `int64` e tem guarda `scratchBudget<=0 → false` que o PS não tem. Em `V:` fracionário podem divergir na
  fronteira; o SPEC fixa o ponto de truncamento e o tratamento de budget≤0 no harness. **[Confirmado no codebase — `reclaim.go:19-23`]**

## 10. Estratégia de implementação

Três fases, ordenadas por risco/valor (detalhe operacional no SPEC):

- **Fase 1 (RF-1, RF-2, RF-3, RF-4 parcial, RF-8 parcial):** decisão pura → Go + `decide`/`pr-slot` + paridade.
  Risco quase-zero (não toca Hyper-V). **Model-agnostic.**
- **Fase 2 (RF-5, resto de RF-4):** loop + estado → `tick --observe`; encolher `civm-vm-orchestrator.ps1` a shim;
  **decisão Model A/B**.
- **Fase 3 (RF-6, RF-7):** métricas → `hostdisk.Metrics`; `serialize-runner.ps1` → `civmctl runner serialize`.

> **Fronteira SPEC-por-fase:** a **Fase 1** é a única unidade **SPEC-ável** sob este PRD (`SPEC.md`/`SPECv2.md`).
> **Fase 2 e Fase 3 são direcionais** — cada uma abre seu próprio PRD→SPEC→Passo 2.5 (incluindo a decisão Model A/B).
> Este PRD descreve as 3 fases como estratégia; só a Fase 1 vira código sob ele.

## 11. Documentos a atualizar (sync rule #14)

No(s) mesmo(s) commit(s) estrutural(is): o **help** de `civmctl` (`cmd/civmctl/main.go`);
**`.github/workflows/ci.yml`** (gate de paridade com pwsh + regra de 100% statement — ITEM-8b do SPEC); `README.md`/
`AGENTS.md`/`CODEX.md`/`rules/*` quando o contrato do host mudar (Fase 2); os runbooks afetados
(`runbooks/RUNNER-SERIALIZATION.md`, `runbooks/MULTI-PROJECT-RUNNER.md` — Fase 2/3); e `validation.md` (prova
empírica de cada cutover). **[Confirmado em docs — `CLAUDE.md` § Sync rule]**

## 12. Fora de escopo

- Reimplementar `Optimize-VHD -Mode Full` / `Mount-VHD` sem PowerShell (R2).
- **Recalibrar qualquer threshold** de disk-safety (RNF-4 — é refactor).
- Eliminar os `register-*.ps1` / `activate-orchestrator.ps1` (possível via `schtasks.exe` do Go; fase futura,
  opcional — e dependente de Model B).
- **Decidir Model A vs Model B** (adiado para a Fase 2).
- Mudar a política de scale-to-zero (governada por `orchestrator-scale-to-zero/SPEC.md`).

## 13. Critérios de aceitação

1. `internal/orchestrator` e os gates novos de `reclaim.go` existem, **100% statement** (sem cobrir código
   inexistente/inalcançável) e `go test -race` verde.
2. Harness de paridade `pwsh ↔ civmctl orchestrate decide` passa sobre **toda** a tabela (ação idêntica), **com
   PowerShell 7 instalado no CI** e o golden gerado dos `.ps1`.
3. `civmctl orchestrate {decide,pr-slot}` registrados e documentados no help; `golangci-lint` limpo.
4. Os 8 thresholds em `civm.go` batem **verbatim** com os defaults do `.ps1` (diff mecânico — teste Go puro, sem
   pwsh; zero drift).
5. (Fase 2) Golden test do state-shape com fixture **real da 5.1 (BOM-ful)** nos dois sentidos.
6. Cutover documentado em `validation.md` com single-owner garantido (sem dois atuadores nem dois escritores).

## 14. Validação

- `go build ./... && go vet ./... && golangci-lint run && go test -race -cover ./...`.
- **CI instala PowerShell 7** → golden gerado dos `.ps1` → harness de paridade (RNF-1) verde; regra de **100%
  statement** em `internal/orchestrator` + gates novos de `reclaim.go`.
- (Fase 2) `civmctl orchestrate tick --observe` na box viva: `would_*` batem com o PS por N ticks **antes** do
  cutover de atuação.
- E2E na box (host Windows): task nova em paralelo (observe em paths-sombra) → comparar logs JSONL → desabilitar
  atomically as `.ps1` legadas. Anexar prova a `validation.md`.

---

> **Próximo passo SSDV3:** o `SPECv2.md` (já gerado após o Passo 2.5) é o candidato ativo, **cercado à Fase 1**.
> Re-auditar o `SPECv2.md` (Passo 2.5) → `go` → IMPL Fase 1. Fase 2/3 abrem seus próprios PRD→SPEC→2.5.
