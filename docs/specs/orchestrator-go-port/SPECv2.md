# SPECv2 — Port do orchestrator scale-to-zero de PowerShell para Go

> Versão melhorada após auditoria do Passo 2.5.
> Baseline preservado: `SPEC.md`.
> SPEC auditado: `SPEC.md`. Candidato ativo para nova auditoria: **este `SPECv2.md`**.
> Motivo: 3 red-teams independentes (paridade, segurança-host, evidência/metodologia) deram **no-go**.
> Onde houver conflito, **esta versão prevalece**.

## Blockers do Passo 2.5 endereçados

| ID (lente) | Finding | Correção | Onde |
| --- | --- | --- | --- |
| C-1 evidência | Paridade PS↔Go sem caminho executável no CI (`pwsh` ausente) → Go testa Go | Instalar PowerShell 7 no CI; **golden gerado mecanicamente rodando os `.ps1` via pwsh** | DT-9, ITEM-8, ITEM-8b |
| C-1 host | `Set-Content -Encoding UTF8` (PS 5.1) grava **com BOM** → `json.Unmarshal` falha → cooldown de panic anulado | State files **sem BOM** (`WriteAllText`+`UTF8Encoding($false)`) + reader Go `TrimPrefix` BOM | DT-12, ITEM-9 |
| HIGH-1 paridade | Default `--can-panic` Go `false` rebaixa `panic_compact` silenciosamente | Defaults **por-flag verbatim** (`--can-panic=true`, `--v-free-gb/--guest-free-gb=999`) | DT-10, ITEM-5 |
| HIGH-2 paridade | Timestamp deployado `.ToString('o')` (7 frações) ≠ golden RFC3339 limpo | Timestamps **passados verbatim** (nunca reparse-reformat); vetor `'o'`; fixture de estado `'o'` | DT-11, ITEM-2/3 |
| MED-1/A-2 | `boundary_compact` no oráculo + `Resolve-AdmitTransition`, mas fora do enum `Action` | Adicionar `ActionBoundaryCompact`; `ResolveAdmitTransition(dec Action)`; 100% via casos diretos do oráculo | DT-14, ITEM-2 |
| A-1 evidência | "100% linhas+branches" inverificável (Go não mede branch; gate ≥80%) | **100.0% statement** enforçado por regra de CI dedicada p/ `internal/orchestrator` | DT-8(rev), ITEM-8b |
| A-1 host | Observe paralelo: dois escritores no `V:` corrompem a FIFO | Observe escreve só em **paths-sombra** `*.observe.json`; civmctl vira **único escritor** pós-cutover | DT-16, ITEM-9 |
| A-2/A-3 host | Lock `V:` Go não-especificado; processo-pai ambíguo | **Adiado p/ Fase 2** (ITEM-9 DIRECIONAL): A/B + ownership do lock decididos no ciclo próprio da Fase 2; Fase 1 model-agnostic | DT-15, ITEM-9 |
| M-1 evidência | Fase 2/3 em nível de contrato viola Passo 2 rule 10 | ITEM-9/10/11 = **DIRECIONAL**, fora do escopo implementável; `go` cerca só a Fase 1 | §Escopo |
| M-2 todas | "N ticks" sem número (viola #3) | **N = 48 ticks** (~96 min, ≥1 ciclo start→compact→admit) + marcador in-code + data-teto | DT-17, ITEM-7 |
| DT-7 (host+evid) | `powershell.exe` "a validar" | **Resolvido:** `validation.md` prova PS 5.1 via `powershell.exe` rodando Hyper-V cmdlets como SYSTEM | DT-7(rev) |

## Escopo fechado desta implementação — **CERCADO À FASE 1**

**Entra agora (implementável sob este SPEC):** ITEM-1..8 + ITEM-8b — decisão pura, gates de reclaim, constantes,
`civmctl orchestrate {decide,pr-slot}`, harness de paridade executável, gate de CI.

**DIRECIONAL — NÃO implementável sob este SPEC (M-1):** ITEM-9 (tick/queue/state/cutover atuação), ITEM-10
(host-metrics), ITEM-11 (`runner serialize`). As **decisões** de design (DT-12..DT-16) ficam fechadas aqui para
não reabrir, mas cada ITEM exige seu **próprio PRD→SPEC→Passo 2.5** antes de qualquer código. O veredito `go`
desta auditoria autoriza **apenas a Fase 1**.

**Fora:** reimplementar `Optimize-VHD`/`Mount-VHD`; recalibrar threshold; `civmctl.exe` como Scheduled Task
(Model B — DT-15); eliminar `register-*.ps1`.

## Matriz de rastreabilidade PRD → SPECv2

| PRD | SPECv2 |
| --- | --- |
| RF-1 | ITEM-2 (decide + `ActionBoundaryCompact`), ITEM-3 (prqueue) |
| RF-2 | ITEM-4 |
| RF-3 | ITEM-1 |
| RF-4 | ITEM-5 (`decide`,`pr-slot`); `tick` → ITEM-9 (DIRECIONAL) |
| RF-5/6/7 | ITEM-9/10/11 (DIRECIONAL) |
| RF-8 | ITEM-7 (decisão, Fase 1); atuação → ITEM-9 (DIRECIONAL) |
| RNF-1 | ITEM-8 + ITEM-8b (paridade executável) |
| RNF-2 | DT-8(rev) + ITEM-8b (100.0% statement enforçado) |
| RNF-4 | ITEM-1 (diff mecânico, teste Go puro) |
| RNF-5 | DT-7(rev) |

## Decisões técnicas

DT-1..DT-6 do `SPEC.md` permanecem (DT-1 fetcher host net/http é da Fase 2 DIRECIONAL). Revisadas/novas:

| # | Decisão | Justificativa |
| - | ------- | ------------- |
| DT-7 (rev) | **`powershell.exe` (PS 5.1) — RESOLVIDO, não-blocker.** | `validation.md` (datas 2026-06: "PS 5.1 via powershell.exe") prova que `Mount-VHD`/`Optimize-VHD`/`Get-VHD` já rodam como Scheduled Task SYSTEM nesse runtime. Citar essa evidência viva; remover o "a validar". |
| DT-8 (rev) | **Cobertura 100.0% statement (não "linhas+branches").** | Go **não mede branch nativo** (sem ferramenta no repo) — "100% branches" é número que nenhuma tool produz (viola #3). Alvo verificável = **100.0% statement** em `internal/orchestrator` e nos gates novos de `reclaim.go`, **enforçado** por regra de CI dedicada (ITEM-8b), separada do loop ≥80%. Intent inalterado (cobrir todo código real; não fabricar p/ inexistente/inalcançável). |
| DT-9 (novo) | **Paridade executável: PowerShell 7 no CI + golden gerado mecanicamente dos `.ps1`.** | `validation.md` já prova que os `.test.ps1` rodam em **pwsh 7.4.6 Linux** ("o pwsh Linux roda o código real"). O golden `decision_vectors.json` tem as ações esperadas **capturadas executando os `.ps1` via pwsh**, não transcritas à mão — o oráculo passa a ser a lógica PS (Kahneman #13 de verdade). |
| DT-10 (novo) | **Defaults por-flag VERBATIM do `.ps1`** (não "= constantes do ITEM-1"). | `--can-panic` default **`true`** (`decision.ps1:54`); `--v-free-gb`/`--guest-free-gb` default **`999`** (sentinela "não medi"); `--prev-running`/`--admit-attempts` default `0`. Um `flag.Bool("can-panic", false…)` ingênuo desabilitaria o `panic_compact` (a ação anti-death-spiral). |
| DT-11 (novo) | **Timestamps passados VERBATIM; invariante `.ToString('o')`.** | O loop grava `(...).ToUniversalTime().ToString('o')` (7 frações; `civm-vm-orchestrator.ps1:175,409,508`). `Resolve-PrSlot` devolve `idleSinceUtc`/`nowUtc` **byte-a-byte** — o Go **nunca** faz `Parse→Format` (dropa fração, trunca grace, quebra round-trip). Parse usa `time.Parse(time.RFC3339Nano, …)`. Vetores golden incluem o formato `'o'`. |
| DT-12 (novo) | **State files sem BOM.** | PS 5.1 `Set-Content -Encoding UTF8` grava **UTF-8 com BOM** → `json.Unmarshal` falha (sem skip de BOM no Go). O port flipa os `Set-Content` dos state files para `[System.IO.File]::WriteAllText(path, json, (New-Object System.Text.UTF8Encoding($false)))` (igual ao `civm-host-metrics.ps1:139` que o Go já consome), **e** o reader Go faz `bytes.TrimPrefix(data, []byte{0xEF,0xBB,0xBF})` por defesa. (Decisão fechada aqui; aplicação na Fase 2.) |
| DT-13 (novo) | **"Round-trip semântico", não "byte-compatível".** | Mesmo Go→PS o byte nunca bate (BOM; ordem de chaves; `json.Marshal` HTML-escapa `&<>` e os ids vêm de `head_branch`). Nada disso quebra a decodificação. Alvo = round-trip **semântico** tolerante a BOM/ordem/escape/campo-ausente; `Encoder.SetEscapeHTML(false)`; **sem `omitempty`** nos campos de estado; golden com fixture **BOM-ful real da 5.1** nos dois sentidos. |
| DT-14 (novo) | **`ActionBoundaryCompact` é constante do enum; `ResolveAdmitTransition(dec Action)` a recebe.** | `boundary_compact` é ação real do switch do loop host (`civm-vm-orchestrator.ps1:474`) e arm de `Resolve-AdmitTransition` (`decision.ps1:31`), e o oráculo o asserta 2×. Listá-la torna o port **fiel** e o branch **alcançável pela API tipada** (o oráculo chama `ResolveAdmitTransition` direto com ela) → 100% statement **sem fabricar** (não é alcançada por `Decide`, mas é por `ResolveAdmitTransition`; mapear cada caso do oráculo p/ a função Go correspondente). |
| DT-15 (rev) | **Processo-pai (Model A vs B) ADIADO para a Fase 2.** | Decisão do usuário (2026-06-27): a Fase 1 é model-agnostic; A/B (e o ownership do lock) é decidido no ciclo PRD→SPEC→2.5 próprio da Fase 2 (ITEM-9 DIRECIONAL). Análise capturada p/ a Fase 2: **Model A** (`.ps1` continua a task/lock em PS; civmctl único escritor de estado) = menor risco; **Model B** (`civmctl.exe` é a task; lock em Go via `syscall.CreateFile`) = mais programático. Sob qualquer um: observe em paths-sombra (DT-16), sem dois escritores. |
| DT-16 (novo) | **Observe escreve só em paths-sombra.** | `civmctl orchestrate tick --observe` persiste só `*.observe.json`; **nunca** toca arquivo que o legado grava — elimina os dois escritores no `V:` durante o observe paralelo (A-1). Pós-cutover, civmctl é o único escritor (DT-15). |
| DT-17 (novo) | **N = 48 ticks (~96 min).** | Cobre ≥1 ciclo completo `start→compact→admit` medido (reference class dos logs do host). Gate do cutover (ITEM-7/ITEM-9) e prazo da exceção DT-5: marcador in-code `// DELETE-AFTER: <issue> — paridade verde + 48 ticks observe` + data-teto explícita no commit. |

## Fronteira de atomicidade e política de rollback (revisada)

**Atômico:** cada `os.WriteFile` de state (tmp+`os.Rename`); `Decide` é avaliação pura. **Fora:** o ciclo do tick;
`Optimize-VHD` ininterruptível. **Rollback de estado: NÃO é N/A** (M-3) — perder `lastPanicUtc` re-arma o panic
(mata jobs), perder `prevRunning` pula um boundary; **mitigado por DT-12** (sem BOM → o estado legado é lido, não
descartado). **Rollback app:** subcomandos aditivos; `self-upgrade` anterior → fallback dot-source (DT-5).
**Rollback host:** tasks legadas só **desabilitadas** no cutover, nunca deletadas até a Fase 2 validar.
**Proibido:** dois escritores de estado no `V:` (DT-16); dois atuadores (DT-15); VM Off ao fim.

## Mapa Kahneman por etapa crítica (revisado)

| ITEM | Disciplina | Pergunta | Evidência mínima (executável) | Abort trigger |
| ---- | ---------- | -------- | ----------------------------- | ------------- |
| ITEM-1 | #3 · #2 | Algum threshold mudou? | Teste Go **puro** (ITEM-8b) lê os `.ps1` como texto, resolve `$script:PanicCooldownMinutes`, compara com as constantes — **sem pwsh** | Qualquer número divergir |
| ITEM-2 | #13 (Ilusão de validade) | A decisão Go == PS deployado? | Golden **gerado via pwsh** dos `.ps1` (DT-9) + `go test` consumindo-o; 100.0% statement | Qualquer ação divergir; golden não gerado por pwsh |
| ITEM-7 | #15 · #3 | E se `civmctl` faltar? Quando remover o `.ps1`? | Teste do wrapper (binário ausente→dot-source; ação igual); N=48 (DT-17) com marcador in-code | Fallback não acionar; N sem número |
| ITEM-9 (DIR.) | #15 · #5 | Host meio-migrado corrompe estado? | Golden com fixture **BOM-ful real da 5.1** nos dois sentidos (DT-12/13) | BOM falha o parse; campo perdido; dois escritores |

## Checklist de segurança (delta vs SPEC)

- [x] **BOM:** state files gravados **sem BOM**; reader Go `TrimPrefix` (DT-12).
- [x] **Escape:** `Encoder.SetEscapeHTML(false)`; sem `omitempty` em campo de estado (DT-13).
- [x] **Dois escritores:** observe só em paths-sombra (DT-16); civmctl único escritor pós-cutover (DT-15).
- [x] **Defaults:** `--can-panic=true` e sentinelas `999` verbatim (DT-10); fail-closed preservado.
- [x] (herdados do SPEC) exec sem shell + timeout; sem segredo em `deploy/windows/`; `slog`/JSONL; sem clamp Int32.

## Mudanças de estado / constantes

Bloco const do `SPEC.md` **inalterado** (os 8 valores 55/40/28/18/10/3/15/3 foram **confirmados verbatim** pelo
Passo 2.5 contra os `.ps1` vivos). Único acréscimo: a constante de ação `ActionBoundaryCompact` vive em
`internal/orchestrator/decide.go` (não em `civm.go`).

---

## ITEM-1 — Constantes (RF-3) — *revisado*

Igual ao `SPEC.md`, **mais**: o "diff mecânico" que prova verbatim é um **teste Go puro** em
`internal/orchestrator` (lê `deploy/windows/*.ps1` como texto, resolve defaults de param **e** `$script:` vars,
compara com as constantes), **sempre executável** (não depende de pwsh). Resolve M-3.

## ITEM-2 — `decide.go` + `decide_test.go` (RF-1) — *revisado*

Igual ao `SPEC.md`, **mais**:
- Enum inclui **`ActionBoundaryCompact`** (DT-14); `ResolveAdmitTransition(attempts int, dec Action, …)` cobre o
  arm `boundary_compact` (alcançável via API tipada; os 2 casos do oráculo mapeiam para `ResolveAdmitTransition`,
  não `Decide`).
- `DecisionInput` com **defaults verbatim** quando vindo do CLI (DT-10): `CanPanic=true`, `VFreeGB=GuestFreeGB=999`.
- Timestamps (se houver no caminho) **verbatim** (DT-11).
- **Evidência:** golden `decision_vectors.json` **gerado via pwsh** (DT-9/ITEM-8); `go test -race`; **100.0%
  statement** (DT-8); `PrevRunning` é input morto (B-2) → exceção DT-8 (não fabricar teste).

## ITEM-3 — `prqueue.go` + `prqueue_test.go` (RF-1) — *revisado*

Igual ao `SPEC.md`, **mais**: `ResolvePrSlot` retorna **`(PrSlotResult, error)`**; em parse de data malformada →
`error` não-nil e o caller **segura o slot (`hold`) fail-safe** (espelha o `catch`/WARN do PS), nunca avança/perde
FIFO. Timestamps de saída **verbatim** (DT-11); vetor no formato `'o'`. `Reason` é **log-only** (fora do contrato
de paridade; `ITEM-8` compara `Action`/`CurrentPr`/`IdleSinceUTC`, não `Reason` — evita o banker's-rounding LOW-2).

## ITEM-4 — Gates de reclaim (RF-2) — *inalterado*

Igual ao `SPEC.md` (`ReclaimStuck`, `ReclaimCooldownOK`; fórmulas confirmadas pelo 2.5). `100.0%` statement.

## ITEM-5 — `orchestrate.go` `decide`/`pr-slot` (RF-4) — *revisado*

Igual ao `SPEC.md`, **mais**: defaults de flag **verbatim** (DT-10) — `fs.Bool("can-panic", true, …)`,
`fs.Int("v-free-gb", 999, …)`, `fs.Int("guest-free-gb", 999, …)`. O harness (ITEM-8) **omite** a flag para expor o
default (nunca normaliza para `--can-panic=true`, que mascararia o bug).

## ITEM-6 — `main.go` (RF-4) — *inalterado*

## ITEM-7 — Cutover da decisão (RF-8, Fase 1) — *revisado*

Igual ao `SPEC.md`, **mais**: a probe `--has-active-job` é avaliada **eager** pelo wrapper PS nesta janela (o loop
PS continua pai na Fase 1 — DT-15 — e já tem a probe); o SSH-por-tick extra é **regressão não-funcional aceita**
na janela time-boxed (a laziness real volta no `tick` Go da Fase 2 com closure). N=48 (DT-17) com marcador in-code.

## ITEM-8 — Harness de paridade (RNF-1) — *revisado*

Igual ao `SPEC.md`, **mais**: o golden é **gerado mecanicamente** rodando os `.ps1` via **pwsh** (DT-9), não
transcrito. O gate de CI **não pula** (ITEM-8b instala pwsh); o `skip-if-absent` fica só para o dev local.

## ITEM-8b — CI: PowerShell 7 + gate de cobertura 100% (RNF-1, RNF-2) — *novo*

`.github/workflows/ci.yml` (job `build-civmctl`): (a) passo de instalação do **PowerShell 7** (padrão `go install`
dos tools, fora do `go.mod` — preserva RNF-3); (b) gera `decision_vectors.json` via pwsh e roda o harness como
**gate duro**; (c) regra **dedicada** que assere **100.0%** de statement coverage em `internal/orchestrator` e nos
símbolos novos de `internal/civm/reclaim.go` (separada do loop genérico ≥80%). **Sync rule:** `ci.yml` no mesmo commit.

---

## ITEM-9 / ITEM-10 / ITEM-11 — Fase 2/3 — **DIRECIONAL (não implementável sob este SPEC)**

As decisões de design ficam **fechadas** (DT-12 BOM, DT-13 round-trip semântico, DT-16 observe-sombra, DT-17 N) para
quando virarem SPEC próprio; **Model A vs B (DT-15) fica ABERTO** para o ciclo da Fase 2. **Nenhum código** sai daqui. Cada um abre um `docs/specs/{slug}/`
com PRD→SPEC→Passo 2.5. **Dependência externa (open question):** se `docs/specs/civm-disk-gate-per-batch` re-ligar
`boundary_compact`, o port já tem `ActionBoundaryCompact` (DT-14) e congela contra os valores **vivos** (55/40),
nunca o `51` histórico — reconciliar via constante, não literal.

## Documentos a atualizar (sync rule #14)

`cmd/civmctl/main.go` (help, ITEM-6); `.github/workflows/ci.yml` (ITEM-8b); `validation.md` (evidência DT-7 +
paridade gerada por pwsh + cutover); os 3 `.ps1` de decisão **marcados para deleção** (DT-5/DT-17). README/AGENTS/
CODEX/rules só quando o contrato do host mudar (Fase 2, DIRECIONAL).

## Validação (revisada)

`go build ./... && go vet ./... && golangci-lint run && go test -race ./...`; **gate de CI**: pwsh instalado →
golden gerado dos `.ps1` → harness verde; **100.0% statement** em `internal/orchestrator` + gates novos de
`reclaim.go` (regra dedicada, ITEM-8b); diff de constantes (teste Go puro, ITEM-1). Não cobrir
inexistente/inalcançável (`PrevRunning` morto).

---

> **Próximo passo SSDV3:** re-auditar **este `SPECv2.md`** (Passo 2.5). `go` → IMPL **Fase 1** (ITEM-1..8b).
> `no-go` → atualizar `SPECv2.md` in-place (ou `SPECv3.md`).
