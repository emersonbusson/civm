# SPECv3 — Port do orchestrator scale-to-zero de PowerShell para Go

> Versão melhorada após a **2ª rodada** do Passo 2.5 (**2 lentes em paralelo**: correção/paridade + viabilidade CI/operacional).
> Baseline preservado: `SPEC.md` e `SPECv2.md` (candidato auditado).
> SPEC auditado nesta rodada: **`SPECv2.md`** → veredito **no-go** (1 CRÍTICO + 4 ALTO + 3 MÉDIO/BAIXO da lente de correção).
> Onde houver conflito, **esta versão prevalece**.
> Findings bloqueantes endereçados: (C) DT-14 `ActionBoundaryCompact` é arm MORTO — o oráculo o asserta **0×**, não "2×",
> e `Get-OrchestratorDecision` **nunca** o retorna → o alvo 100% quebra ou força fabricação proibida pela DT-8;
> (A1) ITEM-8b instala `pwsh` no `build-civmctl`, que roda **no guest disk-constrained** (o gargalo que o orchestrator existe
> para gerenciar) + "`go install` do PowerShell" é impossível; (A2) golden "gerado mecanicamente" sem mecanismo, sobre **3
> oráculos heterogêneos**; (A3) regra de CI de "100.0% statement" sem mecanismo e confundindo cobertura por-package com
> por-símbolo; (A4) DT-17 N=48 ticks não-ancorado na justificativa ("≥1 ciclo" ≈ 5 ticks) e satisfazível vacuamente numa box ociosa.

## Blockers do Passo 2.5 (2ª rodada) endereçados

| ID (lente) | Finding na SPECv2 | Correção na SPECv3 | Onde |
| --- | --- | --- | --- |
| **C-2 evidência** | DT-14: "boundary_compact é arm de Resolve-AdmitTransition e o oráculo o asserta 2×" → **FALSO**. `Get-OrchestratorDecision` (decision.ps1:39-132) retorna 9 ações, **nenhuma** é `boundary_compact`; o oráculo (decision.test.ps1:58) diz literalmente "transicao >0->0 NAO compacta mais (era boundary_compact) -> mark_busy"; as 6 chamadas a `Resolve-AdmitTransition` (test:89-94) usam `reclaim_before_admit`/`panic_compact`/`start`/`mark_busy`, **nunca** `boundary_compact`. Logo o arm é MORTO e cobri-lo exige fabricar (proibido por DT-8). | `ActionBoundaryCompact` vira **const compartilhada** com `ActionReclaimBeforeAdmit` no `switch` de `ResolveAdmitTransition` (comportamento idêntico no PS) → fidelidade preservada **sem statement novo descoberto**; 100% medido só no alcançável; claim "asserta 2×" removido. | DT-14(rev), DT-18, ITEM-2 |
| **A1-CI/disco** | ITEM-8b instala pwsh no job `build-civmctl`, que (ci.yml:107) roda em `["self-hosted","civm"]` = **o guest VM** cujo VHDX o orchestrator compacta; pwsh-7.4 Linux ocupa **~210 MB** desempacotado → cresce o VHDX → derruba `V:` → pode disparar `panic_compact` (mata o próprio job de CI). "Padrão `go install`" é **impossível** (pwsh não é módulo Go). | Paridade pwsh **só num job novo `parity-decision` pinado a `ubuntu-latest`** (off-box, efêmero, disco irrelevante). O guest `build-civmctl` roda **só `go test` puro contra o golden COMMITADO** (zero pwsh). | DT-19, ITEM-8, ITEM-8b |
| **A2-evidência** | DT-9: golden "gerado mecanicamente rodando os `.ps1` via pwsh" — sem mecanismo, e os **3 `.test.ps1` têm schemas incompatíveis** (decision: array `$cases` com `exp=`; pr-queue: chamadas inline `Test-Slot`; reclaim: chamadas inline `Check`; `exp=`-count = 45/0/0). "União dos 3" não é extraível de forma uniforme. | **Fonte única de vetores** `civm-parity-vectors.ps1` (data file) que os `.test.ps1` E o gerador dot-sourceiam; `scripts/gen-parity-vectors.ps1` (rodado **off-box**) emite o golden tipado `{fn,in,out}`; CI faz `git diff --exit-code` no golden commitado (prova "deployado==testado" sem transcrição). | DT-9(rev), DT-20, ITEM-8 |
| **A3-evidência** | "regra dedicada que assere 100.0% statement" — sem mecanismo; `internal/orchestrator` é package (cobertura por-package), mas `reclaim.go` vive no package `civm` (com outro código) → 100% por-package não isola "símbolos novos". | Mecanismo explícito: `go tool cover -func` filtrado — `internal/orchestrator` no total `== 100.0%`; `reclaim.go:ReclaimStuck`/`ReclaimCooldownOK` **por-símbolo** `== 100.0%`. Snippet no ITEM-8b. | DT-8(rev2), ITEM-8b |
| **A4-método** | DT-17: "N=48 ticks (~96 min) cobre ≥1 ciclo start→compact→admit" — tick=2 min (register-orchestrator.ps1:8) ✓, mas 1 ciclo medido = **~10 min ≈ 5 ticks** (validation 2026-06-21 10:09-10:19), não 48; e ciclo só dispara com pressão+gap → numa box ociosa 48 ticks = 48 `noop_off`/`mark_busy` e **0 compact** → trigger satisfeito sem exercer o caminho crítico. | Trigger de cutover/deleção vira **cobertura comportamental**: observar ≥1 de **cada** transição viva crítica (`start`, `compact`, `admit-pós-compact`, `idle_stop`; `panic` se ocorrer) batendo com o PS, com **piso de wall-clock ≥48 ticks** como mínimo, não como suficiência. Tudo **Fase 2** (depende de `tick --observe`). | DT-17(rev), ITEM-7, ITEM-9 |
| **M-coerência** | Checklist de segurança da Fase 1 marca `[x]` mitigações que **só existem na Fase 2** (BOM DT-12, observe-sombra DT-16); a tabela de blockers confunde "decisão registrada p/ Fase 2" com "fix entregue na Fase 1". | Checklists **separam** "entregue na Fase 1" de "decisão fechada p/ Fase 2 (não entregue aqui)". | §Checklist |
| **M-encoding** | DT-12/13 fecham BOM/round-trip dos 2 JSON, mas **omitem** `V:\civm-current-context` (PRD §7: ascii, **sem newline**, sem BOM; lido pelo gate runner) — contrato mais estrito e consumidor distinto. | DT-21 adiciona o contrato de `civm-current-context` (Fase 2). | DT-21 |
| **M-Kahneman** | Mapa Kahneman só cobre ITEM-1/2/7/9; ITEM-3 (parse→hold fail-safe), ITEM-4 (data ilegível→pode reclamar) e **ITEM-5 (default `--can-panic` que pode silenciar `panic_compact`)** são críticos e ficam sem pergunta/evidência/abort (viola Passo 2 rule 14). | Linhas adicionadas p/ ITEM-3/4/5. | §Mapa Kahneman |
| **B-sync** | AGENTS.md (linhas 98-112) e README.md (§Comandos civmctl) **enumeram** subcomandos `civmctl`; adicionar `orchestrate` é mudança de convenção na Fase 1, mas a SPECv2 adia README/AGENTS p/ Fase 2. | `orchestrate` entra em AGENTS.md + README.md **no mesmo commit** do ITEM-6 (sync rule #14). | §Documentos |
| **B-idleSinceUtc** | SPEC.md ITEM-9 lista `idleSinceUtc` como campo de `civm-orchestrator-state.json`, mas PRD §7 diz que **não existe** lá (ociosidade derivada de `lastBusyUtc`); orchestrator.ps1:175-187 confirma (só `lastBusyUtc`/`lastPanicUtc`/`admitReclaimAttempts`/`prevRunning`). | DT-22: remover `idleSinceUtc` do shape de `state.go` (Fase 2). | DT-22 |
| **MED-1 correção** | PRD R8: `Test-OptimizeSlack` recebe `[double]`; `EmergencyAdmits` recebe `int64` + guarda `scratchBudget<=0 → false` que o PS não tem → divergem em `V:` fracionário/budget≤0. Sem vetor de paridade (reclaim-gate.test.ps1:12-16 só usa V: inteiro + budget positivo). | `EmergencyAdmits`/`Test-OptimizeSlack` **não são exercitados na Fase 1** (gate de atuação, não da decisão pura) → a paridade fracionária + budget≤0 + a regra de truncamento double→int64 é **deferida à Fase 2** (quando substitui `Test-OptimizeSlack` na atuação), marcada aqui p/ não se perder. | DT-23, ITEM-8 |
| **MED-2 correção** | ITEM-3 (herdado) diz "espelha o catch/WARN do PS" — **mal-atribuído**: a função pura `Resolve-PrSlot` **não tem try/catch** (pr-queue.ps1:66 `[datetime]::Parse` **lança**); o `catch` vive no loop (orchestrator.ps1:591). O `(result, error)` Go é fail-safe **Go-only**, não espelho. | ITEM-3 reescrito: o ramo `error` é **Go-only** (fail-safe; a pura PS lança), testado por unit Go, **fora do contrato de paridade**. | ITEM-3 |
| **BAIXO correção** | (a) DT-11 manda `RFC3339Nano` mas ITEM-4 herda `RFC3339` — inconsistência; (b) `--queued`/`--running` são `Mandatory` no PS (sem default), mas `fs.Int(…,0)` ingênuo transforma ausência em `0` silencioso. | (a) `RFC3339Nano` em **todos** os parses (prqueue + reclaim); (b) ITEM-5 **exige** `--queued`/`--running` (erro se ausentes), espelhando o `Mandatory` do PS. | ITEM-3, ITEM-5 |

## Escopo fechado — **CERCADO À FASE 1** (inalterado vs SPECv2)

**Entra agora (implementável sob este SPEC):** ITEM-1..8b — decisão pura, gates de reclaim, constantes,
`civmctl orchestrate {decide,pr-slot}`, harness de paridade executável (golden commitado + regeneração off-box), gate de CI.

**DIRECIONAL — NÃO implementável sob este SPEC (M-1):** ITEM-9 (tick/queue/state/cutover atuação), ITEM-10 (host-metrics),
ITEM-11 (`runner serialize`). Decisões de design (DT-12..DT-22) ficam fechadas aqui para não reabrir; cada ITEM exige seu
**próprio PRD→SPEC→Passo 2.5**. O `go` desta auditoria autoriza **apenas a Fase 1**.

**Fora:** reimplementar `Optimize-VHD`/`Mount-VHD`; recalibrar threshold; `civmctl.exe` como Scheduled Task (Model B — DT-15);
eliminar `register-*.ps1`.

## Matriz de rastreabilidade PRD → SPECv3

| PRD | SPECv3 |
| --- | --- |
| RF-1 | ITEM-2 (`Decide` + `ResolveAdmitTransition`/`ActionBoundaryCompact` shared-case), ITEM-3 (prqueue) |
| RF-2 | ITEM-4 |
| RF-3 | ITEM-1 |
| RF-4 | ITEM-5 (`decide`,`pr-slot`); `tick` → ITEM-9 (DIRECIONAL) |
| RF-5/6/7 | ITEM-9/10/11 (DIRECIONAL) |
| RF-8 | ITEM-7 (decisão, Fase 1); atuação → ITEM-9 (DIRECIONAL) |
| RNF-1 | ITEM-8 + ITEM-8b (paridade executável off-box; golden commitado) |
| RNF-2 | DT-8(rev2) + ITEM-8b (100.0% statement por-package + por-símbolo) |
| RNF-4 | ITEM-1 (diff mecânico, teste Go puro) |
| RNF-5 | DT-7(rev) |

## Decisões técnicas

DT-1..DT-6 do `SPEC.md` permanecem. DT-7(rev), DT-10, DT-11, DT-13, DT-15, DT-16 da SPECv2 permanecem **inalterados**.
Revisadas/novas nesta rodada:

| # | Decisão | Justificativa |
| - | ------- | ------------- |
| DT-8 (rev2) | **100.0% statement — mecanismo de verificação explícito.** | `go test -coverprofile` + `go tool cover -func`: (a) total de `internal/orchestrator` `== 100.0%`; (b) **por-símbolo** `ReclaimStuck`/`ReclaimCooldownOK` em `reclaim.go` `== 100.0%` (porque package `civm` tem outro código — cobertura por-package não isola os símbolos novos). `PrevRunning` (input morto — declarado em decision.ps1:37, usado só em comentário linha 32, nunca num branch) e o arm `ActionBoundaryCompact` (morto, ver DT-14(rev)) **não são statements descobertos** porque (resp.) é campo não-lido e case compartilhado → 100% real sem fabricar. |
| DT-9 (rev2) | **Golden COMMITADO + regenerado/diffado off-box; fonte única de vetores.** | A SPECv2 mandava gerar o golden "mecanicamente via pwsh" mas (i) não dava gerador e (ii) os 3 `.test.ps1` têm schemas incompatíveis (array `exp=` vs `Test-Slot` inline vs `Check` inline). Solução: extrair os casos para `deploy/windows/civm-parity-vectors.ps1` (data puro), dot-sourceado pelos 3 `.test.ps1` **e** pelo gerador. O gerador (`scripts/gen-parity-vectors.ps1`) roda **off-box** (ubuntu-latest), importa as 3 `.ps1` reais, replaia cada vetor e emite `internal/orchestrator/testdata/decision_vectors.json` tipado. CI **off-box** regenera e faz `git diff --exit-code` → prova "deployado==testado" sem transcrição manual (Kahneman #13). O guest nunca roda pwsh. |
| DT-14 (rev) | **`ActionBoundaryCompact` é const de fidelidade em case COMPARTILHADO com `ActionReclaimBeforeAdmit`; o oráculo o asserta 0×.** | Verificado: `Resolve-AdmitTransition` (decision.ps1:31) trata `'boundary_compact'` **idêntico** a `'reclaim_before_admit'` (ambos `Update-AdmitAttempts`), mas nada vivo produz `boundary_compact` (Get-OrchestratorDecision não o retorna; o loop passa `$decision` pós-switch) e o oráculo o exercita **0×**. Portanto o Go usa `case ActionReclaimBeforeAdmit, ActionBoundaryCompact:` (fall-through) — a const existe (fidelidade ao enum do switch PS), o branch é alcançável pelos vetores de `reclaim_before_admit`, e **não há statement órfão**. Proibido: case próprio para `ActionBoundaryCompact` + teste fabricado (DT-8). |
| DT-17 (rev) | **Cutover/deleção por COBERTURA COMPORTAMENTAL, não contagem de ticks.** | Tick=2 min (register-orchestrator.ps1:8); 1 ciclo `start→compact→admit` medido ≈ 8-10 min ≈ 5 ticks (validation 2026-06-21 10:09→10:19; Optimize ~8 min, scale-to-zero SPEC linhas 482-483). 48 ticks ≈ 9-10 ciclos OU, numa box ociosa, **0 compact**. O trigger de deletar os `.ps1` de decisão (Fase 2) exige observar **cada** transição viva crítica ≥1× batendo com o PS — `start`, um `*_compact` (boundary/panic/stop_and_compact conforme a carga), `admit pós-compact`, `idle_stop` — **com** piso de ≥48 ticks de wall-clock (não como suficiência). Marcador in-code: `// DELETE-AFTER: #<issue-fase2> — golden verde + cobertura viva das transições + data-teto AAAA-MM-DD` (data-teto = data do commit de cutover + 30 dias; `#<issue-fase2>` é a issue da Fase 2, obrigatório, sem placeholder). |
| DT-18 (novo) | **Tratamento simétrico de código morto (DT-8 vs DT-14).** | A SPECv2 tratava `PrevRunning` (input morto) com exceção DT-8 ("não fabricar") mas `ActionBoundaryCompact` (arm morto) como "coberto" — assimetria inconsistente. SPECv3 unifica: ambos são **não-alcançáveis** pelo oráculo vivo → nenhum recebe teste fabricado; `PrevRunning` é campo não-lido, `ActionBoundaryCompact` é case compartilhado. |
| DT-19 (novo) | **Invariante de disco do guest preservado no CI: zero pwsh no runner `civm`.** | O job `build-civmctl` roda em `["self-hosted","civm"]` (ci.yml:107) = o guest Ubuntu cujo VHDX o orchestrator compacta. Instalar pwsh (~210 MB) lá cresce o VHDX, derruba `V:`, e sob `V:<18` o orchestrator faz `panic_compact` (mata o job). A paridade pwsh é **exclusiva** do job `parity-decision` (ubuntu-latest, GitHub-hosted, off-box). RNF-3 (zero-deps Go) intacto: pwsh nunca entra no `go.mod` nem no runner do produto. |
| DT-20 (novo) | **Schema do golden tipado.** | `decision_vectors.json` = `[{ "fn": "Decide" \| "ResolveAdmitTransition" \| "ResolvePrSlot" \| "ReclaimStuck" \| "ReclaimCooldownOK", "in": {<args tipados verbatim, timestamps formato 'o'>}, "out": {<saída>} }]`. O discriminador `fn` resolve "qual função Go chamar" por vetor (necessário porque o golden une 5 entrypoints de 3 `.ps1`). |
| DT-21 (novo, Fase 2) | **`V:\civm-current-context` no contrato de encoding.** | PRD §7: linha única **ascii, sem newline, sem BOM**, lida pelo gate runner (`wait-for-slot`). Hoje orchestrator.ps1:567/571/578/582 grava `-Encoding ascii -NoNewline` (já sem BOM). O writer Go da Fase 2 **deve** replicar: `os.WriteFile` de bytes ascii **sem `\n` final** e sem BOM. Um `\n` extra ou UTF-8/BOM quebra o match exato do gate. (Distinto do contrato JSON dos DT-12/13.) |
| DT-22 (novo, Fase 2) | **`state.go` sem campo `idleSinceUtc`.** | SPEC.md ITEM-9 listava `idleSinceUtc` em `civm-orchestrator-state.json`, mas PRD §7 e orchestrator.ps1:175-187 confirmam que **não existe** (ociosidade derivada de `lastBusyUtc`). O shape Go da Fase 2 = `{admitReclaimAttempts, lastBusyUtc, lastPanicUtc, prevRunning}`. `idleSinceUtc`/`currentIdleSinceUtc` pertencem só a `civm-pr-queue.json` (ITEM-3/Resolve-PrSlot). |
| DT-23 (novo, Fase 2) | **Paridade do gate de slack (`EmergencyAdmits` vs `Test-OptimizeSlack`) deferida à Fase 2.** | PRD R8: o Go recebe `int64` + guarda `scratchBudget<=0 → false`; o PS recebe `[double]` sem a guarda → divergem em `V:` fracionário/budget≤0. Mas `EmergencyAdmits` **não é exercitado na Fase 1** (é gate de **atuação**, não da decisão pura) e é **reuso, não port** (RF-2). Quando entrar no caminho de atuação (Fase 2, substituindo `Test-OptimizeSlack`), o SPEC da Fase 2 **deve**: (a) fixar a regra de truncamento `double→int64` do produtor de métricas; (b) adicionar ao golden vetores de `V:` fracionário na fronteira e `scratchBudget<=0`. Registrado aqui p/ não se perder. |

## Fronteira de atomicidade e política de rollback

Inalterada vs SPECv2 (§ "Fronteira … revisada"). Reforço: **na Fase 1 não há escrita de state file** — DT-12 (BOM) e DT-21
(current-context) são writers de Fase 2. Logo o "fix de BOM" **não é entregue na Fase 1**; é decisão fechada para a Fase 2.

## Mapa Kahneman por etapa crítica (revisado)

| ITEM | Disciplina | Pergunta | Evidência mínima (executável) | Abort trigger |
| ---- | ---------- | -------- | ----------------------------- | ------------- |
| ITEM-1 | #3 · #2 | Algum threshold mudou? | Teste Go **puro** lê os `.ps1` como texto, resolve defaults de param + `$script:`, compara com as constantes — **sem pwsh** | Qualquer número divergir |
| ITEM-2 | #13 | `Decide` Go == PS deployado? | Golden commitado (DT-9 rev2) + regeneração off-box `git diff --exit-code`; `go test`; 100.0% statement (DT-8 rev2); `ActionBoundaryCompact` em case compartilhado (DT-14 rev) | Qualquer ação divergir; golden divergir da regeneração off-box; arm órfão exigindo teste fabricado |
| ITEM-3 | #15 (fail-safe) | E se a data do slot for ilegível? | `prqueue_test.go`: parse malformado → `error` não-nil + caller **segura o slot (`hold`)**, nunca avança/perde FIFO | Parse ruim avança a fila ou perde contexto |
| ITEM-4 | #15 (fail-safe) | E se `lastReclaimUTC` for ilegível? | `reclaim_test.go`: data ilegível → `ReclaimCooldownOK==true` (pode reclamar); vazio → `true` | Data ruim bloqueia o reclaim (fail-closed errado p/ disco) |
| ITEM-5 | #15 · #3 | O default da flag pode silenciar `panic_compact`? | Harness **omite** `--can-panic` p/ expor o default; `fs.Bool("can-panic", true, …)` verbatim (decision.ps1:54); idem sentinelas `999` | Default `false`/`0` rebaixa `panic_compact` ou bloqueia a fila |
| ITEM-7 | #15 | E se `civmctl` faltar? Quando remover o `.ps1`? | Teste do wrapper (binário ausente→dot-source; ação igual). **Deleção dos `.ps1` é Fase 2** (DT-17 rev: cobertura viva das transições, não contagem de ticks) | Fallback não acionar |
| ITEM-9 (DIR.) | #15 · #5 | Host meio-migrado corrompe estado? | Golden com fixture **BOM-ful real da 5.1** nos dois sentidos (DT-12/13); `state.go` sem `idleSinceUtc` (DT-22); current-context ascii/sem-newline (DT-21) | BOM falha o parse; campo perdido; dois escritores |

## Checklist de segurança — **Fase 1 (entregue aqui)**

- [x] **Defaults:** `--can-panic=true` e sentinelas `999` verbatim (DT-10); harness omite a flag p/ expor o default (ITEM-5).
- [x] **Exec safety:** `exec.CommandContext` sem shell + timeout; sem segredo em `deploy/windows/`; `slog`/JSONL; sem clamp Int32 (herdado).
- [x] **CI/disco:** paridade pwsh **só** no job `parity-decision` (ubuntu-latest, off-box); guest `build-civmctl` sem pwsh (DT-19).
- [x] **Evidência:** golden commitado + regeneração off-box com `git diff --exit-code` (DT-9 rev2); 100.0% por-package + por-símbolo (DT-8 rev2).

### Decisões fechadas para a **Fase 2 (NÃO entregues aqui)**

- [ ] (Fase 2) **BOM:** state files gravados sem BOM + reader Go `TrimPrefix` (DT-12).
- [ ] (Fase 2) **Escape/round-trip:** `SetEscapeHTML(false)`; sem `omitempty`; fixture BOM-ful 5.1 (DT-13).
- [ ] (Fase 2) **current-context:** ascii, sem newline, sem BOM (DT-21).
- [ ] (Fase 2) **Dois escritores:** observe só em paths-sombra `*.observe.json` (DT-16); civmctl único escritor pós-cutover (DT-15).

## Mudanças de estado / constantes

Bloco const do `SPEC.md` **inalterado** (8 valores 55/40/28/18/10/3/15/3 confirmados verbatim). A const de ação
`ActionBoundaryCompact` vive em `internal/orchestrator/decide.go` e é tratada em case compartilhado (DT-14 rev).

---

## ITEM-1 — Constantes (RF-3) — *inalterado vs SPECv2*

Teste Go **puro** em `internal/orchestrator` (lê `deploy/windows/*.ps1` como texto, resolve defaults de param + `$script:` vars,
compara com as constantes), sempre executável (não depende de pwsh).

## ITEM-2 — `decide.go` + `decide_test.go` (RF-1) — *revisado (DT-14 rev / DT-18)*

Igual ao `SPEC.md`, **mais**:
- Enum inclui `ActionBoundaryCompact` (const de fidelidade). `ResolveAdmitTransition(attempts int, dec Action, running, queued,
  vAfter, floor int) int` (assinatura completa do SPEC.md) trata `ActionBoundaryCompact` **no mesmo `case` de
  `ActionReclaimBeforeAdmit`** (`case ActionReclaimBeforeAdmit, ActionBoundaryCompact:`) — idêntico ao PS (decision.ps1:30-31),
  **sem statement órfão**. `Decide` porta os **9** retornos vivos de `Get-OrchestratorDecision` (decision.ps1: reclaim_before_admit,
  start, noop_off, panic_compact, mark_busy, warn_clean, idle_debounce, stop_aborted_active_job, stop_and_compact) — **não** emite
  `ActionBoundaryCompact`.
- `DecisionInput` com defaults verbatim do CLI (DT-10): `CanPanic=true`, `VFreeGB=GuestFreeGB=999`. `PrevRunning` é campo presente
  (fidelidade) mas **não-lido** por `Decide` (input morto, decision.ps1:37 declara / linha 32 só comenta) → exceção DT-8 (não fabricar).
- Timestamps verbatim (DT-11).
- **Evidência:** golden `decision_vectors.json` **commitado** e regenerado off-box (DT-9 rev2); `go test -race`; **100.0% statement**
  (DT-8 rev2, mecanismo no ITEM-8b).

## ITEM-3 — `prqueue.go` + `prqueue_test.go` (RF-1) — *revisado (lente de correção)*

`ResolvePrSlot` retorna `(PrSlotResult, error)`; parse malformado → `error` não-nil e o caller **segura o slot (`hold`)** fail-safe.
**Atenção (MED-2):** isso é fail-safe **Go-only** — a função pura `Resolve-PrSlot` **não tem try/catch** (pr-queue.ps1:66 `[datetime]::Parse`
**lança**; o `catch` que segura o slot vive no **loop**, orchestrator.ps1:591). Logo o ramo `error` é testado por **unit Go** e fica
**fora do contrato de paridade** (o golden não tem "PS lança" para comparar). Parse com **`RFC3339Nano`** (igual ao DT-11; padroniza
com ITEM-4). Timestamps verbatim (DT-11); `Reason` é log-only. Mapa Kahneman ITEM-3 (acima).

## ITEM-4 — Gates de reclaim (RF-2) — *revisado (lente de correção)*

`ReclaimStuck`, `ReclaimCooldownOK` (fórmulas confirmadas); `ReclaimCooldownOK` parseia com **`RFC3339Nano`** (padroniza com DT-11/ITEM-3).
Mapa Kahneman ITEM-4 (acima). 100.0% por-símbolo (DT-8 rev2). **`EmergencyAdmits` NÃO é tocado** (reuso; RF-2) e seu gap de paridade
fracionária/budget≤0 (DT-23) é **Fase 2** — não entra na Fase 1.

## ITEM-5 — `orchestrate.go` `decide`/`pr-slot` (RF-4) — *inalterado vs SPECv2 + Kahneman*

Defaults de flag verbatim (DT-10): `fs.Bool("can-panic", true, …)`, `fs.Int("v-free-gb", 999, …)`, `fs.Int("guest-free-gb", 999, …)`.
**`--queued` e `--running` são OBRIGATÓRIAS** (erro `exitUsage` se ausentes) — espelham o `[Parameter(Mandatory)]` do PS
(decision.ps1); um `fs.Int(…,0)` ingênuo transformaria ausência em `0` silencioso (`noop_off`). O harness **omite** `--can-panic`
para expor o default. Mapa Kahneman ITEM-5 (acima).

## ITEM-6 — `main.go` (RF-4) — *revisado (sync)*

`case "orchestrate"` no `switch` + linha em `printHelp`. **Sync rule #14:** no **mesmo commit**, adicionar `orchestrate` à
tabela de subcomandos do **README.md** (§Comandos civmctl) e ao bloco de comandos do **AGENTS.md** (linhas 98-112) — ambos
enumeram a superfície `civmctl`.

## ITEM-7 — Cutover da decisão (RF-8, Fase 1) — *revisado (DT-17 rev)*

`.ps1` chamam `civmctl orchestrate …` com fallback dot-source. A probe `--has-active-job` é eager nesta janela (loop PS continua
pai — DT-15). **A deleção dos `.ps1` de decisão é Fase 2** e usa cobertura comportamental viva (DT-17 rev), não contagem de ticks.
Marcador in-code conforme DT-17 rev.

## ITEM-8 — Harness de paridade (RNF-1) — *revisado (DT-9 rev2 / DT-19 / DT-20)*

- **Fonte única de vetores:** `deploy/windows/civm-parity-vectors.ps1` (data puro: `$Vectors = @( @{ fn=…; in=@{…}; out=… }, … )`),
  dot-sourceado pelos 3 `*.test.ps1` (que substituem suas tabelas inline) **e** pelo gerador.
- **Gerador (off-box):** `scripts/gen-parity-vectors.ps1` importa `civm-orchestrator-decision.ps1`/`civm-pr-queue.ps1`/
  `civm-reclaim-gate.ps1`, replaia cada vetor pela função real e emite `internal/orchestrator/testdata/decision_vectors.json`
  (schema DT-20).
- **Consumo Go:** `decide_test.go`/`prqueue_test.go`/`reclaim_test.go` carregam o golden e despacham por `fn` (gate duro).
- `skip-if-absent` de pwsh fica **só** para o dev local; no CI o job `parity-decision` (ubuntu-latest) nunca pula.

## ITEM-8b — CI: job `parity-decision` off-box + gate de cobertura 100% (RNF-1, RNF-2) — *revisado (DT-19 / DT-8 rev2)*

`.github/workflows/ci.yml`:

**(a) Novo job `parity-decision`** — `runs-on: ubuntu-latest` **fixo** (off-box; nunca o runner `civm`):
- instala PowerShell 7 via repo apt da Microsoft **ou** tarball de release (NÃO `go install` — pwsh não é módulo Go);
- roda `scripts/gen-parity-vectors.ps1` → `git diff --exit-code internal/orchestrator/testdata/decision_vectors.json`
  (prova que o golden commitado == regenerado dos `.ps1` vivos);
- roda os `*.test.ps1` via `pwsh` (gate duro: divergência falha).

**(b) `build-civmctl` (runner `civm`/guest) — SEM pwsh:** roda só `go test` puro contra o golden **commitado** (DT-19).

**(c) Gate de 100.0% statement** (no `build-civmctl`, dedicado, separado do loop ≥80%):
```bash
go test -race -coverprofile=cov.orch.out ./internal/orchestrator/...
orch=$(go tool cover -func=cov.orch.out | awk '/^total:/{gsub(/%/,"",$3);print $3}')
[ "$orch" = "100.0" ] || { echo "internal/orchestrator $orch% != 100.0%"; exit 1; }
go test -race -coverprofile=cov.civm.out ./internal/civm/...
go tool cover -func=cov.civm.out \
  | grep -E 'reclaim\.go:[0-9]+:[[:space:]]+(ReclaimStuck|ReclaimCooldownOK)' \
  | awk '{gsub(/%/,"",$3); if ($3!="100.0"){print "UNCOVERED "$0; rc=1}} END{exit rc+0}'
```
**Sync rule:** `ci.yml`, README.md, AGENTS.md no mesmo commit.

---

## ITEM-9 / ITEM-10 / ITEM-11 — Fase 2/3 — **DIRECIONAL (não implementável sob este SPEC)**

Decisões fechadas: DT-12 (BOM), DT-13 (round-trip semântico), DT-16 (observe-sombra), DT-17 rev (cobertura viva), DT-21
(current-context), DT-22 (`state.go` sem `idleSinceUtc`). **Model A vs B (DT-15) fica ABERTO** para o ciclo da Fase 2.
**Nenhum código** sai daqui. Cada um abre `docs/specs/{slug}/` com PRD→SPEC→Passo 2.5. **Dependência externa (open question):**
se `docs/specs/civm-disk-gate-per-batch` re-ligar `boundary_compact` **como saída de uma função de decisão** (hoje é arm morto),
o port já tem `ActionBoundaryCompact` (DT-14 rev) e o golden capturará o novo vetor; congelar contra os valores vivos (55/40),
nunca o `51` histórico (revertido — validation 2026-06-21).

## Documentos a atualizar (sync rule #14)

`cmd/civmctl/main.go` (help, ITEM-6); **README.md (§Comandos civmctl)** + **AGENTS.md (bloco de comandos)** com `orchestrate`
(mesmo commit); `.github/workflows/ci.yml` (ITEM-8b: job `parity-decision` + gate 100%); `validation.md` (evidência DT-7 +
paridade off-box + golden commitado); os 3 `.ps1` de decisão **marcados para deleção** com marcador DT-17 rev. CODEX/rules e
runbooks só quando o contrato do host mudar (Fase 2, DIRECIONAL).

## Validação (revisada)

`go build ./... && go vet ./... && golangci-lint run && go test -race ./...`; **gate de CI**: job `parity-decision` (ubuntu-latest)
regenera o golden e faz `git diff --exit-code` + roda os `*.test.ps1` em pwsh; `build-civmctl` (guest, sem pwsh) consome o golden
commitado e aplica o gate **100.0% statement** (por-package em `internal/orchestrator` + por-símbolo em `reclaim.go`, ITEM-8b);
diff de constantes (teste Go puro, ITEM-1). Não cobrir inexistente/inalcançável (`PrevRunning` morto; arm `ActionBoundaryCompact`
compartilhado).

---

> **Próximo passo SSDV3:** re-auditar **este `SPECv3.md`** (Passo 2.5). `go` → IMPL **Fase 1** (ITEM-1..8b).
> `no-go` → atualizar `SPECv3.md` in-place (ou `SPECv4.md`).
