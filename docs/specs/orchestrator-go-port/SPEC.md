# SPEC — Port do orchestrator scale-to-zero de PowerShell para Go

> Passo 2 do SSDV3. Traduz [`PRD.md`](./PRD.md) em mudanças exatas no repo. Comportamento a preservar:
> [`orchestrator-scale-to-zero/SPEC.md`](../orchestrator-scale-to-zero/SPEC.md). Onde o PRD era ambíguo, este SPEC
> **fecha a decisão** (ver § Decisões técnicas).

## Escopo fechado desta implementação

**Entra agora (Fase 1 — detalhada e implementável sem interpretação):** decisão pura em Go
(`internal/orchestrator/`), gates de reclaim em Go (`internal/civm/reclaim.go`), os 8 thresholds em
`internal/civm/civm.go`, o subcomando `civmctl orchestrate {decide,pr-slot}`, e o harness de paridade.

**Entra como contrato (Fase 2 e Fase 3 — ITEMs de contrato; o wiring interno é refinado quando a fase começar,
SSDV3 "nova decisão → volta ao SPEC"):** `civmctl orchestrate tick` + `internal/orchestrator/{queue,state}.go`
(Fase 2); produtor de métricas em `internal/hostdisk` e `civmctl runner serialize` (Fase 3).

**Fora agora:** reimplementar `Optimize-VHD`/`Mount-VHD` sem PowerShell; recalibrar qualquer threshold; eliminar
`register-*.ps1`; mudar a política de scale-to-zero.

**Dependências assumidas prontas (confirmadas no codebase):** `civm.EmergencyAdmits`; `activeruns.Collect`/
`Options`/`Summary`; o dispatch `switch` de `cmd/civmctl/main.go`; o idioma de subcomando de `cmd/civmctl/activeruns.go`.

## Matriz de rastreabilidade PRD → SPEC

| PRD  | Implementação no SPEC |
| ---- | --------------------- |
| RF-1 | ITEM-2 (decide.go), ITEM-3 (prqueue.go) |
| RF-2 | ITEM-4 (reclaim.go: `ReclaimCooldownOK`, `ReclaimStuck`) |
| RF-3 | ITEM-1 (bloco const em civm.go) |
| RF-4 | ITEM-5 (orchestrate.go: `decide`, `pr-slot`), ITEM-6 (main.go) — `tick` em ITEM-9 (Fase 2) |
| RF-5 | ITEM-9 (queue.go ghost-filter + state.go), Fase 2 |
| RF-6 | ITEM-10 (hostdisk.Metrics producer), Fase 3 |
| RF-7 | ITEM-11 (`civmctl runner serialize`), Fase 3 |
| RF-8 | ITEM-7 (cutover da decisão, Fase 1), ITEM-9 (cutover da atuação, Fase 2) |
| RNF-1 | ITEM-8 (harness de paridade) |
| RNF-2 | ITEM-2/3/4 (`*_test.go`, `-race`, **100%** nas funções puras — ver DT-8) |
| RNF-3 | DT-1 (sem WMI; stdlib) |
| RNF-4 | ITEM-1 (valores verbatim; diff mecânico) |
| RNF-5 | DT-7 (`powershell.exe` 5.1 / SYSTEM) |

## Decisões técnicas

| # | Decisão | Justificativa |
| - | ------- | ------------- |
| DT-1 | **Fetcher do host é `net/http`+PAT novo (Fase 2); `activeruns.Collect` NÃO é reusado no host.** | `activeruns.listRuns` invoca `gh run list` (argv fixo); o host **não tem `gh`** (usa PAT por owner via `Invoke-RestMethod` hoje). `activeruns` permanece como cockpit do guest. O reuso real é o *shape* (`Run`/`Summary`) e o idioma worker-pool, não a função. |
| DT-2 | **Os floors do orchestrator são constantes NOVAS, distintas de `DefaultHostVolumeWarnFreeGB=30`/`DefaultHostVolumeCritFreeGB=10`.** | Valores e propósitos diferentes: `WarnFloorGB=28`/`PanicFloorGB=18` gateiam o scale-to-zero (`warn_clean`/`panic_compact`); os `HostVolume*FreeGB` são limiares do runbook de reclamação. Prefixar `DefaultOrchestrator*` evita o red-team confundir os dois conjuntos. |
| DT-3 | **`Update-AdmitAttempts`/`Resolve-AdmitTransition` viram funções PURAS que retornam o novo `attempts int`** (não mutam struct). | `rules/coding-style.md` (imutabilidade). O PS muta `$State` in-place; o Go idiomático retorna o valor novo. Comportamento idêntico, sem efeito colateral. |
| DT-4 | **`HasActiveJobProbe` é `func() bool` (não `bool`).** | Preserva a laziness do PS (a probe SSH só é chamada no gate de stop, não por tick — evita um SSH por ciclo). A função pura recebe a closure; o CLI `decide` embrulha um `--has-active-job` bool. |
| DT-5 | **O fallback do cutover (PS chama `civmctl`, com dot-source da função PS se o binário faltar) é exceção Day-0 time-boxed.** | Não é compat-shim permanente: é o fail-safe (#15) durante a transição. **Removido** quando a paridade ficar verde e `tick --observe` bater por N ticks; os `.ps1` de decisão são então **marcados para deleção** (rule 20). |
| DT-6 | **Evidência de paridade = tabela golden Go (gate duro) + harness cross-runtime `pwsh↔civmctl` (defesa em profundidade onde `pwsh` existe).** | A tabela Go é sempre reproduzível (`go test`); o cross-check `pwsh` roda onde o runtime existe e prova que o deployado == o testado sem inspeção visual (rule 16). |
| DT-7 | **Atuação Hyper-V via `powershell.exe` (Windows PowerShell 5.1), não `pwsh`.** | O módulo Hyper-V e as Scheduled Tasks SYSTEM vivem no 5.1; a task SYSTEM o alcança. (Validar no Passo 2.5.) |
| DT-8 | **Cobertura 100% (linhas + branches) nas funções puras novas; não cobrir o inexistente/inalcançável.** | Feedback do usuário (2026-06-27, vale p/ todo o civm): `internal/orchestrator` e os gates de `reclaim.go` são puros e 100% alcançáveis → 100% é exigível, não o piso de 80%. O ≥80% de `rules/testing.md` é **piso** só para glue com I/O onde 100% exigiria fabricar teste de caminho inexistente. Não há cobertura artificial. |

## Fronteira de atomicidade e política de rollback

**Atômico nesta implementação:** cada `os.WriteFile` de state file (`V:\civm-orchestrator-state.json`,
`V:\civm-pr-queue.json`) é atômico por arquivo (escrita em tmp + `os.Rename`). A função `Decide` é uma avaliação
pura sem I/O. **Fora da atomicidade:** o ciclo do tick (query→decisão→persist→atua) NÃO é transacional; o
`Optimize-VHD` continua sendo uma operação Hyper-V única **ininterruptível** (R2 do PRD). Estados parciais
aceitos: estado lido antes de uma decisão que não chega a atuar (próximo tick reconcilia).

**Rollback de app:** os subcomandos `orchestrate` novos são aditivos; reverter = `civmctl self-upgrade` para o
binário anterior (os subcomandos viram "comando desconhecido", o `.ps1` cai no fallback dot-source — DT-5).
**Rollback de host:** as tasks `.ps1` legadas permanecem registradas e apenas **desabilitadas** no cutover
(`Disable-ScheduledTask`), nunca deletadas até a Fase 2 fechar; reverter = `Enable-ScheduledTask` + desabilitar a
nova. **Rollback de estado:** N/A — Day-0 (state files efêmeros em `V:`; byte-compatíveis nos dois sentidos).
**Proibido:** dois orchestrators atuando ao mesmo tempo (single-owner); deixar a VM Off ao fim de qualquer caminho.

## Mapa Kahneman por etapa crítica

| ITEM | Disciplina | Pergunta obrigatória | Evidência mínima | Abort trigger |
| ---- | ---------- | -------------------- | ---------------- | ------------- |
| ITEM-1 (constantes) | #3 Número não adjetivo · #2 Counterfactual | Algum threshold mudou de valor no port? | Diff mecânico: cada `Default Orchestrator*`/`Reclaim*` == default do `.ps1` correspondente (script no ITEM-8) | Qualquer número divergir do `.ps1` |
| ITEM-2 (decide.go) | #13 deployado == testado | A decisão Go é idêntica ao PS em toda a tabela? | `go test -race` da tabela golden (traduz `civm-orchestrator-decision.test.ps1`) + harness `pwsh↔Go` (ITEM-8) verde | Qualquer linha divergir |
| ITEM-7 (cutover decisão) | #15 fail-safe | E se `civmctl` faltar/errar no host? | Teste do shim: binário ausente → cai no dot-source da função PS; ação idêntica | Fallback não acionar ou divergir |
| ITEM-9 (state byte-compat) | #15 fail-safe · #5 worst-case | Um host meio-migrado (PS escreve / Go lê) corrompe estado? | Golden test de round-trip do JSON dos state files | Campo perdido/renomeado no round-trip |
| ITEM-9 (cutover atuação) | #5 Availability worst-case | Pode haver dois orchestrators atuando, ou a VM ficar Off? | Janela supervisionada: task nova em `--observe` em paralelo; legada só desabilitada após `would_*` baterem N ticks; log em `validation.md` | Duas tasks habilitadas; VM Off ao fim |

## Checklist de segurança (pré-implementação)

- [x] **Exec safety:** todo `exec` em `orchestrate.go`/tick usa `exec.CommandContext` sem shell, com timeout (espelha `activeruns.defaultRun`). Atuação Hyper-V: `powershell.exe -NoProfile -NonInteractive -Command "<cmdlet>"`, argv-array, sem `Invoke-Expression` de input externo.
- [x] **Input validation:** flags validadas antes de qualquer decisão; `--vm-state` ∈ {Off,Running}; ints parseados; `--prs` é JSON validado; repos via `civm.ValidateRepo` (Fase 2).
- [x] **Fail-closed:** `VFreeGB<=0`/`GuestFreeGB<=0` = "não medi" → não bloqueia a fila (fail-safe, espelha o PS); `ReclaimCooldownOK` em data ilegível → "pode reclamar".
- [x] **Privilégio do host:** `civmctl.exe` roda como a task SYSTEM existente (mesmo direito Hyper-V); sem rede nova além da query GitHub via PAT já usada (Fase 2).
- [x] **Secrets:** nenhuma credencial hardcoded; PAT do host vem do arquivo/token já usado pelo `.ps1`; nada de segredo em `deploy/windows/` ou no Go.
- [x] **Logs:** `log/slog` estruturado / JSONL; o tick nunca deixa a VM Off em silêncio.
- [x] **Int clamp:** nenhum `[math]::Max(0, …)` literal reintroduzido nos `.ps1` (invariante #17); o Go usa `int64` nos GB.

## Mudanças de estado / constantes

**Arquivo:** `internal/civm/civm.go` — **novo bloco** `const (...)` (RF-3). Reusa
`DefaultHostVolumeHardFloorGB`/`DefaultHostVolumeScratchBudgetGB` existentes; adiciona os 8 ausentes:

```go
// Orchestrator scale-to-zero (docs/specs/orchestrator-go-port; comportamento:
// docs/specs/orchestrator-scale-to-zero/SPEC.md). Valores VERBATIM dos defaults de
// deploy/windows/civm-orchestrator-decision.ps1, civm-pr-queue.ps1 e civm-reclaim-gate.ps1
// (port behavior-preserving; RNF-4 threshold freeze). DISTINTOS de DefaultHostVolume*FreeGB
// (runbook de reclamação) — ver SPEC §DT-2. Invariante de ordenação:
// HardFloor(1) < PanicFloor(18) < WarnFloor(28) < GuestFloor(40) < AdmitFloor(55).
DefaultOrchestratorAdmitFloorGB    = 55 // host V: livre p/ admitir o próximo batch (alcançável pós-compact)
DefaultOrchestratorGuestFloorGB    = 40 // guest livre p/ admitir (a VM Ubuntu chega a ~45-63, nunca 70)
DefaultOrchestratorWarnFloorGB     = 28 // V: em zona warn com job rodando -> warn_clean (poda online)
DefaultOrchestratorPanicFloorGB    = 18 // V: em panic -> panic_compact (compacta offline mesmo ocupado)
DefaultOrchestratorIdleStopMinutes = 10 // debounce antes do stop_and_compact por ociosidade
DefaultPrQueueDoneGraceMinutes     = 3  // grace sem check real até o contexto (PR) ser considerado concluído
DefaultReclaimPanicCooldownMinutes = 15 // cooldown entre panics (barra o loop apertado de re-mata-job)
DefaultReclaimMinRecoverGB         = 3  // abaixo disto o Optimize "não ajudou" -> alerta (não finge sucesso)
```

- **Quem lê:** `internal/orchestrator/decide.go` (floors, idle, admit), `internal/orchestrator/prqueue.go`
  (`DefaultPrQueueDoneGraceMinutes`), `internal/civm/reclaim.go` (`ReclaimCooldownOK`/`ReclaimStuck`).
- **Migração de estado:** N/A — Day-0. **Disciplina #3:** os valores são commit de refactor; qualquer mudança
  futura de número é commit separado com evidência (não entra aqui).

---

## ITEM-1 — Constantes do orchestrator em `internal/civm/civm.go` (RF-3)

**O que muda:** adiciona o bloco const acima. **Como:** novo `const (...)` após o bloco "Reclamação de volume do
host". **Por quê:** single-source dos thresholds que hoje só existem como defaults de parâmetro nos `.ps1`.
Reusar os existentes — **não** duplicar `HardFloor`/`ScratchBudget`. Evidência: `go build`; o diff do ITEM-8
prova igualdade verbatim com os `.ps1`.

## ITEM-2 — `internal/orchestrator/decide.go` + `decide_test.go` (RF-1)

**O que muda:** novo pacote `orchestrator` com a decisão pura. **Como:** tipo `Action string` com as constantes de
ação (`ActionNoopOff`, `ActionStart`, `ActionReclaimBeforeAdmit`, `ActionPanicCompact`, `ActionMarkBusy`,
`ActionWarnClean`, `ActionIdleDebounce`, `ActionStopAbortedActiveJob`, `ActionStopAndCompact`); struct
`DecisionInput` (campos espelhando os params de `Get-OrchestratorDecision`: `VMState string`, `Queued`,
`Running int`, `IdleMinutes float64`, `IdleStopMinutes int`, `HasActiveJobProbe func() bool`, `VFreeGB`,
`WarnFloorGB`, `PanicFloorGB int`, `CanPanic bool`, `AdmitFloorGB`, `GuestFreeGB`, `GuestFloorGB`,
`AdmitReclaimAttempts`, `PrevRunning int`); e:

```go
func Decide(in DecisionInput) Action            // porta Get-OrchestratorDecision (mesma ordem de guards)
func UpdateAdmitAttempts(attempts int, vAfter, floor int) int   // porta Update-AdmitAttempts (DT-3: retorna novo valor)
func ResolveAdmitTransition(attempts int, dec Action, running, queued, vAfter, floor int) int // porta Resolve-AdmitTransition
```

A ordem dos guards em `Decide` é **idêntica** ao `.ps1` (Off→fila→admit/start; Running→panic→mark_busy fila
quente→warn→mark_busy→idle_debounce→probe→stop). `HasActiveJobProbe` só é chamada no gate de stop (DT-4).
**Por quê:** RF-1. **Evidência (ITEM-2 do mapa Kahneman #13):** `decide_test.go` traduz **todos** os casos de
`civm-orchestrator-decision.test.ps1` (tabela golden) + `go test -race`, **100% (linhas + branches)** — DT-8.

## ITEM-3 — `internal/orchestrator/prqueue.go` + `prqueue_test.go` (RF-1)

**O que muda:** porta `Resolve-PrSlot`. **Como:** struct `PrSlotInput` (`Prs []PrContext{Number string; RealJobs int}`,
`CurrentPr string`, `CurrentIdleSinceUTC string`, `NowUTC string`, `DoneGraceMinutes int`) e
`func ResolvePrSlot(in PrSlotInput) PrSlotResult` onde `PrSlotResult{Action string; CurrentPr string;
IdleSinceUTC string; Reason string}`, `Action` ∈ {`grant`,`hold`,`boundary_advance`,`idle`}. Parsing de tempo via
`time.Parse(time.RFC3339, …)`. **Por quê:** RF-1. **Evidência:** `prqueue_test.go` traduz `civm-pr-queue.test.ps1`;
`-race`, **100%** (DT-8).

## ITEM-4 — Gates de reclaim em `internal/civm/reclaim.go` + `reclaim_test.go` (RF-2)

**O que muda:** adiciona, ao lado de `EmergencyAdmits` (que já é `Test-OptimizeSlack` — **não tocar**):

```go
// ReclaimStuck espelha Test-ReclaimStuck: reclaim_no_progress só é erro real quando o
// Optimize não recuperou o mínimo E o V: continua abaixo do floor de admissão.
func ReclaimStuck(recoveredGB, vFreeAfterGB, minRecoverGB, admitFloorGB int) bool

// ReclaimCooldownOK espelha Test-ReclaimCooldown: true se pode reclamar agora (fora do
// cooldown). lastReclaimUTC vazio -> true; data ilegível -> true (fail-safe).
func ReclaimCooldownOK(lastReclaimUTC, nowUTC string, cooldownMinutes int) bool
```

`ReclaimStuck` = `(recoveredGB < minRecoverGB) && (vFreeAfterGB < admitFloorGB)`. `ReclaimCooldownOK` usa
`time.Parse(time.RFC3339, …)`; em erro de parse → `true`. **Por quê:** RF-2. **Evidência:** `reclaim_test.go`
traduz `civm-reclaim-gate.test.ps1` (cobrindo o ramo fail-safe de data ilegível); `-race`, **100%** (DT-8).

## ITEM-5 — `cmd/civmctl/orchestrate.go` — subcomandos `decide` e `pr-slot` (RF-4)

**O que muda:** novo runner, **espelhando `cmd/civmctl/activeruns.go`**: `flag.NewFlagSet("orchestrate", ContinueOnError)`,
`fs.SetOutput(io.Discard)`, sub-dispatch por `args[0]` (`decide`|`pr-slot`). **Como — `decide`:** flags
`--vm-state`, `--queued`, `--running`, `--idle-min`, `--idle-stop-min`, `--v-free-gb`, `--guest-free-gb`,
`--warn-floor-gb`, `--panic-floor-gb`, `--admit-floor-gb`, `--guest-floor-gb`, `--can-panic`, `--prev-running`,
`--admit-attempts`, `--has-active-job`, `--json` (defaults = as constantes do ITEM-1). Monta `DecisionInput`
(com `HasActiveJobProbe: func() bool { return *hasActiveJob }`, DT-4), chama `orchestrator.Decide`, imprime
`{"action": "...", "admitReclaimAttempts": N}` (via `ResolveAdmitTransition` quando aplicável). **`pr-slot`:**
flags `--prs` (JSON), `--current-pr`, `--idle-since`, `--now`, `--grace-min`; imprime `PrSlotResult` em JSON.
Exit: `0` ok, `exitUsage`(64) flag inválida, `2` erro interno. **Por quê:** RF-4 (seam puro: o `.ps1` coleta o
estado e shella `civmctl orchestrate decide`).

## ITEM-6 — Registro em `cmd/civmctl/main.go` (RF-4)

**O que muda:** adiciona `case "orchestrate": os.Exit(runOrchestrate(args))` no `switch` e uma linha em `printHelp`
(`orchestrate   Decisão pura do orchestrator scale-to-zero (decide|pr-slot|tick) [host]`) + exemplos. **Sync rule
#14:** o help é contrato → mesmo commit. **Por quê:** RF-4.

## ITEM-7 — Cutover da decisão: `.ps1` chamam `civmctl` com fallback (RF-8, Fase 1)

**O que muda:** `civm-orchestrator-decision.ps1`, `civm-pr-queue.ps1`, `civm-reclaim-gate.ps1` passam a, no caller,
**chamar** `civmctl orchestrate decide|pr-slot --json` e parsear a ação; se o binário faltar ou sair !=0, caem no
**dot-source** da função PS local (preservada nesta fase). **Como:** wrapper PS `Invoke-CivmDecision` que tenta o
binário (`& civmctl.exe orchestrate decide … | ConvertFrom-Json`) e no `catch`/exit!=0 chama
`Get-OrchestratorDecision`. **Por quê:** RF-8; single-source a decisão em Go sem perder o fail-safe. **Disciplina
#15** (ver mapa). **DT-5:** exceção Day-0 time-boxed — os `.ps1` de decisão ficam **marcados para deleção** após a
paridade verde + N ticks observe (Fase 2). **Evidência:** teste do wrapper (binário ausente → fallback; ação igual).

## ITEM-8 — Harness de paridade `pwsh ↔ civmctl` (RNF-1, DT-6)

**O que muda:** (a) fixtures compartilhadas `internal/orchestrator/testdata/decision_vectors.json` (a união dos
casos dos 3 `*.test.ps1`); (b) `decide_test.go`/`prqueue_test.go` consomem o golden (gate duro); (c) um alvo
executável (`scripts/parity-decision.sh` ou `go test` com build-tag `parity`) que, para cada vetor, roda
`pwsh -NoProfile -Command "... ; Get-OrchestratorDecision @args"` (dot-sourcing o `.ps1`) **e**
`civmctl orchestrate decide --json`, e compara a ação. **Como:** o alvo `pula` com mensagem clara se `pwsh`
ausente; em CI/host onde `pwsh` existe, **falha** em qualquer divergência. **Por quê:** RNF-1 + rule 16 (evidência
executável, não inspeção visual). **Diff de constantes:** o mesmo alvo extrai os defaults dos params dos `.ps1`
e os compara com as constantes do ITEM-1 (zero drift; evidência do ITEM-1 / #3).

---

## ITEM-9 — Fase 2 (contrato): `tick`, `queue.go`, `state.go` (RF-5, resto de RF-4, RF-8 atuação)

**Contrato (wiring interno refinado ao iniciar a Fase 2):**
- `internal/orchestrator/queue.go` — filtro *ghost-queued* **puro** sobre `[]Run` (conta só `run.status` real +
  `createdAt` < 12h), sobre um fetcher injetável `type Fetcher func(ctx) ([]Run, error)`. Guest: adaptador sobre
  `activeruns.Collect`. **Host: fetcher `net/http`+PAT novo (DT-1)** — `activeruns.Collect` NÃO serve no host.
- `internal/orchestrator/state.go` — read/write **byte-compatível** de `V:\civm-orchestrator-state.json` e
  `V:\civm-pr-queue.json` (campos: `admitReclaimAttempts`, `lastBusyUtc`, `lastPanicUtc`, `idleSinceUtc`,
  `prevRunning`, `contexts`, `currentPr`, `currentIdleSinceUtc`). Escrita atômica (tmp + `os.Rename`). **Golden
  test** fixa o shape (mapa Kahneman #15). `struct tags` JSON reproduzem os nomes exatos do `.ps1`.
- `cmd/civmctl/orchestrate.go` ganha `tick` com `--observe` (loga `would_*` sem atuar, espelha o `-Observe` do
  loop PS) e `--state-dir`/`--repos`.
- `civm-vm-orchestrator.ps1` encolhe para shim: lê token, `Get-VM`/`Start-VM`/`Stop-VM`, bundle Optimize, SSH; e
  chama `civmctl orchestrate tick`. Cutover de atuação **single-owner** (mapa Kahneman #5): task nova em observe
  paralelo → legada desabilitada (`Disable-ScheduledTask`) só após `would_*` baterem N ticks (log em `validation.md`).

## ITEM-10 — Fase 3 (contrato): produtor de host-metrics em Go (RF-6)

**Contrato:** o produtor do snapshot migra para `internal/hostdisk` (struct `Metrics` já existe e é **consumida**
pelo Go hoje). `civm-host-metrics.ps1` encolhe a só `Get-VHD`/`Get-Volume` (medição Hyper-V); guest-free via
`civmctl idle-check`/SSH; JSON idêntico ao schema atual. Assinaturas exatas fixadas ao iniciar a Fase 3.

## ITEM-11 — Fase 3 (contrato): `civmctl runner serialize` (RF-7)

**Contrato:** novo subcomando que reusa `internal/runreaper` (`listActiveRuns`/`cancelRun`) para remover runners
redundantes, substituindo `serialize-runner.ps1`. Validação de repos via `civm.ValidateRepo`. Assinaturas exatas
fixadas ao iniciar a Fase 3.

---

## Documentos a atualizar (sync rule #14)

- `cmd/civmctl/main.go` `printHelp` (ITEM-6) — mesmo commit dos subcomandos.
- `README.md`/`AGENTS.md`/`CODEX.md`/`rules/*` quando o contrato do host mudar (Fase 2: o orchestrator vira binário).
- `validation.md` — prova empírica de cada cutover (ITEM-7 decisão; ITEM-9 atuação).
- Os 3 `.ps1` de decisão **marcados para deleção** após paridade (DT-5/rule 20).

## Validação (gates)

`go build ./... && go vet ./... && golangci-lint run && go test -race -cover ./...`. **Cobertura 100% (linhas +
branches)** nas funções puras novas de `internal/orchestrator` e nos gates novos de `internal/civm/reclaim.go`
(DT-8); ≥80% permanece só como piso para glue com I/O — **não cobrir código inexistente/inalcançável**. ITEM-8
(paridade) verde; Fase 2: golden test do state + `tick --observe` batendo na box viva antes do cutover de atuação.

---

> **Próximo passo SSDV3:** **Passo 2.5 (Red-Team)** — obrigatório (muta VM/VHDX, novo subcomando, privilégio
> SYSTEM). Auditar os focos do PRD §9 + DT-1..DT-7. Veredito `go` → IMPL Fase 1; `no-go` → `SPECv2.md`.
