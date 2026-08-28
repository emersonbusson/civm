---
slug: civm-disk-gate-per-batch
title: SPEC — Disco limpo (≥51 GB) por batch (PR e re-run)
milestone: —
issues: []
---

# SPEC — Disco limpo (≥51 GB) por batch: PR e re-run

> Gerado a partir de `docs/specs/civm-disk-gate-per-batch/PRD.md` (Passo 2).
> Traduz os RFs do PRD em mudanças exatas na **decisão pura** do orchestrator
> (`deploy/windows/civm-orchestrator-decision.ps1`) + casos da decision-table.
> Disciplinas: `disciplines/` (Kahneman). Validação: `pwsh` (decision-table) na box.

## Escopo fechado desta implementação

**Entra agora:**

- Elevar o gate de **admissão de batch warm** (VM Running, `Running==0` + `Queued>0`)
  para o `AdmitFloorGB` (51), checando **ambos os lados** (host `V:` + guest), com
  o anti-deadlock `AdmitReclaimAttempts`, espelhando o `reclaim_before_admit` do
  caso VM-Off — em `Get-OrchestratorDecision`.
- Dar a esse gate **precedência** sobre `panic_compact`/`warn_clean` **quando
  `Running==0`** (nenhum job a proteger), para que a admissão sempre compacte
  até 51 em vez de só podar online.
- Atualizar a **decision-table** (`civm-orchestrator-decision.test.ps1`): ajustar
  os casos cuja expectativa muda e adicionar os casos novos do par +/- (Kahneman #13).
- Atualizar a **doc viva** do orchestrator (`orchestrator-scale-to-zero/SPEC.md`:
  tabela de ações §2, pisos §4.1, seção boundary §4) para o novo piso/precedência.

**Fora agora:**

- O `switch` executor (`civm-vm-orchestrator.ps1`) — **não muda**: a ação
  `boundary_compact` já mapeia para `Invoke-StopAndCompact` (reuso total).
- Medição guest-side (`GuestFreeGB` do host-metrics) — já existe e é consumida.
- Serialização transparente de docker-heavy (lock) e workflow acme — outro item.
- Pré-build/pull; cache durável.

**Dependências já prontas (reuso):** `Invoke-StopAndCompact` (limpa-tudo +
`Optimize-VHD`), `civm-reclaim-gate.ps1` (gate de 2 fases + `Test-ReclaimCooldown`),
o snapshot `GuestFreeGB`/`VFreeGB` do host-metrics, o contador `AdmitReclaimAttempts`.

## Matriz de rastreabilidade PRD → SPEC

| PRD                                            | Implementação no SPEC |
| ---------------------------------------------- | --------------------- |
| RF-1 (≥51 antes de todo batch, warm ou Off)    | ITEM-1 (gate warm @51 both-sides) + ITEM-2 (precedência) |
| RF-2 (nunca compacta com job rodando)          | ITEM-1 (gate exige `Running==0`) + ITEM-2 (panic/warn seguem para `Running>0`) |
| RF-3 (fail-safe anti-deadlock)                 | ITEM-1 (`AdmitReclaimAttempts<2`; ≥2 admite) + casos da decision-table |
| RF-4 (precedência panic/warn antes do gate)    | ITEM-2 (gate só toma precedência com `Running==0`; com `Running>0` panic/warn mandam) |
| RF-5 (cache descartado; sem `--filter until=`) | invariante — nada neste SPEC reintroduz |
| RF-6 (observabilidade)                         | ITEM-3 (ação logada com `v_free_gb` antes/depois — já no switch) |

## Decisões técnicas (fecham ambiguidade do PRD)

| #    | Decisão | Justificativa |
| ---- | ------- | ------------- |
| DT-1 | **Reusar a ação `boundary_compact`** (retunada), em vez de criar a ação nova `reclaim_before_batch` que o PRD cogitou. | Mesmo efeito (`Invoke-StopAndCompact` no gap, `Running==0`, sem matar job); string nova exigiria mexer no `switch` e no parsing de log sem ganho. Day-0: superfície mínima. |
| DT-2 | **Remover o parâmetro `BoundaryCompactFloorGB` (40)**; o gate warm passa a usar `$AdmitFloorGB` (51). | Unifica "admitir batch" num piso só (51), warm e Off. O 40 era uma otimização de cold-start (não compactar em 40–51) que a **política do usuário supera** (garantir ≥51 por batch). |
| DT-3 | O gate warm checa **host OR guest** `< AdmitFloorGB` (como `reclaim_before_admit`), não só host. | Paridade com a barreira de admissão VM-Off; o batch precisa de disco limpo nos dois lados. |
| DT-4 | O gate warm **precede** `panic_compact`/`warn_clean` **somente quando `Running==0`**. Com `Running>0`, panic/warn seguem mandando. | Sem job rodando, a admissão deve compactar até 51 (não só podar online). Com job rodando, panic/warn (intra-job) continuam o contrato atual — `warn_clean` online em 18–28, `panic_compact` <18 (mata). |
| DT-5 | Anti-deadlock `AdmitReclaimAttempts>=2` → admite mesmo `<51` (cai no `mark_busy`). | Fail-safe (Kahneman #16): disco irrecuperável não trava a fila eternamente. Reusa o contador já existente do `reclaim_before_admit`. |

## Fronteira de atomicidade e política de rollback

- **Atômico:** a decisão pura é sem efeito (string); cada `Invoke-StopAndCompact`
  (stop→`Optimize-VHD`→start) é atômico por chamada (reuso, inalterado).
- **Fora da atomicidade:** o ciclo "compacta (1 tick) → admite (próximo tick)" é
  multi-tick e idempotente — cada tick re-avalia o estado; seguro por construção.
- **Rollback de aplicação:** reverter `civm-orchestrator-decision.ps1` (restaurar o
  gate `boundary_compact` em `BoundaryCompactFloorGB=40`, host-only, depois de
  panic/warn) + os casos da decision-table. Single-commit reversível.
- **Rollback de migration/dados:** N/A (sem schema, sem dados).
- **forward-only:** N/A.

## Disciplina Kahneman (etapa crítica — risco operacional)

- **Disciplina:** #13 (existência ≠ função), #16 (fail-safe é default), #3 (número
  ancorado em dado). `disciplines/KAHNEMAN-DISCIPLINES.md`.
- **Pergunta obrigatória:** com o gate warm @51 e a precedência sobre warn/panic,
  algum caminho admite um batch com `V<51` (fora do anti-deadlock), OU compacta com
  job rodando (`Running>0`), OU entra em loop sem admitir?
- **Evidência mínima:** `pwsh civm-orchestrator-decision.test.ps1` na box —
  0 regressão nos casos existentes + os casos novos do §"Plano de testes" PASS;
  e 1 captura ao vivo no `V:\civm-orchestrator.log` de um `boundary_compact` em
  gap warm com `40 ≤ V < 51` seguido de `start`/`mark_busy` com `V≥51`.
- **Abort trigger:** qualquer caso de teste mostrando admissão com `V<51` (sem
  attempts≥2) OU `boundary_compact` com `Running>0` OU deadlock (nunca admite).

## Arquivos a MODIFICAR

### `deploy/windows/civm-orchestrator-decision.ps1` — ITEM-1 / ITEM-2

- **O que muda:**
  1. **Remover** o parâmetro `[int]$BoundaryCompactFloorGB = 40` e seu comentário
     (DT-2). O gate warm passa a usar `$AdmitFloorGB`.
  2. **Mover** o gate de batch warm para **antes** de `panic_compact`/`warn_clean`,
     gateado por `Running -eq 0` (DT-4), com a forma espelhada do `reclaim_before_admit`:
     ```powershell
     # ADMISSÃO DE BATCH WARM: VM Running, nenhum job ativo (Running==0) e batch na
     # fila (Queued>0). Espelha a barreira de admissão do caso VM-Off: o batch
     # (PR ou re-run) só inicia com o disco limpo em AdmitFloorGB (51) nos DOIS
     # lados. Disco sujo -> boundary_compact (stop+Optimize offline; nao mata job
     # pois Running==0), e o proximo tick admite via 'start'/'mark_busy'. Precede
     # panic/warn porque, sem job a proteger, a admissao deve COMPACTAR ate 51, nao
     # so podar online. VFreeGB<=0 / GuestFreeGB<=0 = "nao medi" -> nao bloqueia
     # (fail-safe #15). AdmitReclaimAttempts>=2 -> admite mesmo <51 (anti-deadlock).
     if ($Running -eq 0 -and $Queued -gt 0) {
         $hostBelow  = ($VFreeGB -gt 0 -and $VFreeGB -lt $AdmitFloorGB)
         $guestBelow = ($GuestFreeGB -gt 0 -and $GuestFreeGB -lt $AdmitFloorGB)
         if (($hostBelow -or $guestBelow) -and $AdmitReclaimAttempts -lt 2) { return 'boundary_compact' }
     }
     ```
     Inserir este bloco **após** o ramo `VmState -eq 'Off'` e **antes** de
     `$hasWork`/`panic_compact`. O antigo `if ($Running -eq 0 -and $Queued -gt 0
     -and ... -lt $BoundaryCompactFloorGB)` (linha ~85) é **removido** (substituído
     por este, com piso 51 + both-sides + anti-deadlock + precedência).
- **Por que muda:** RF-1/RF-2/RF-3/RF-4 — garante ≥51 por batch (warm ou Off),
  nunca compacta com job rodando, e não deadlocka.
- **Impacto:** com `Running>0` o novo gate não dispara → panic/warn/mark_busy
  intactos (contrato job-running preservado). Com `Running==0`+`Queued>0` o gate
  decide compactar (V<51) ou admitir (V≥51 ou attempts≥2).

### `deploy/windows/civm-orchestrator-decision.test.ps1` — ITEM-3

- **Casos com expectativa ALTERADA** (do floor 40→51 + precedência):
  - `q=3,r=0,vfree=45` → **`boundary_compact`** (antes `mark_busy`; 45<51).
  - `q=1,r=0,vfree=40` → **`boundary_compact`** (antes `mark_busy`; 40<51).
  - `q=1,r=0,vfree=15,cp=T` (gap, V<18) → **`boundary_compact`** (antes `panic_compact`;
    Running==0 → o gate de batch precede; compacta sem matar, mais limpo).
  - `q=1,r=0,vfree=25` (gap, V<28) → **`boundary_compact`** (antes `warn_clean`;
    a admissão compacta até 51, não poda online).
- **Casos INALTERADOS (devem continuar PASS):**
  - `q=1,r=2,vfree=35` → `mark_busy` (Running=2 → gate não dispara; job protegido).
  - `q=0,r=0,vfree=35` → `idle_debounce` (Queued=0 → sem batch).
  - `q=1,r=0,vfree=0` → `mark_busy` (V=0 "não medi" → fail-safe).
  - `q=9,r=2,vfree=15/25` → `panic_compact`/`warn_clean` (Running>0 → contrato job).
- **Casos NOVOS a adicionar:**
  - `q=2,r=0,vfree=55` → `mark_busy` (gap, V≥51 → admite, poupa compact).
  - `q=2,r=0,vfree=51` → `mark_busy` (==floor, `<` estrito).
  - `q=2,r=0,vfree=45,gf=40` → `boundary_compact` (host OK, **guest**<51).
  - `q=2,r=0,vfree=45,ra=2` → `mark_busy` (anti-deadlock: 2 tentativas → admite).
  - `q=2,r=0,vfree=0,gf=0` → `mark_busy` (ambos "não medi" → fail-safe).

### `docs/specs/orchestrator-scale-to-zero/SPEC.md` — ITEM-4 (doc viva, mesmo commit)

- §2 tabela de ações: `boundary_compact` passa a "VM Running + `Running==0` +
  `Queued>0` + (host|guest)`<AdmitFloorGB(51)`", **antes** de panic/warn.
- §2 precedência: registrar que o gate de batch (`Running==0`) precede panic/warn.
- §4.1 gates: remover `BoundaryCompactFloorGB=40`; o gate warm usa `AdmitFloorGB=51`.
- §4 seção boundary + RF-10: marcar implementado por este slug; superseder a
  justificativa do piso 40 (cold-start) pela política ≥51-por-batch.

## Ordem de implementação

1. ITEM-1/2 — `civm-orchestrator-decision.ps1` (remover param, inserir gate com
   precedência).
2. ITEM-3 — `civm-orchestrator-decision.test.ps1` (ajustar + adicionar casos).
3. Rodar `pwsh ...test.ps1` na box → todos PASS, 0 regressão.
4. ITEM-4 — docs vivas (orchestrator SPEC) no mesmo commit.
5. Deploy via `activate-orchestrator.ps1` / `register-orchestrator.ps1`; capturar o
   `boundary_compact` warm ao vivo no log; registrar em `civm/validation.md`.

## Plano de testes

- **Decision-table (pwsh, na box) — fonte única, código real:**
  `pwsh deploy/windows/civm-orchestrator-decision.test.ps1` → `N PASS / 0 FAIL`,
  incluindo os 4 casos alterados + 5 novos acima. Cada recusa pareada com o
  positivo (Kahneman #13).
- **Ao vivo (box):** forçar/observar um gap warm com `40 ≤ V < 51` → ver
  `boundary_compact` no `V:\civm-orchestrator.log`, seguido de admissão com `V≥51`;
  e um re-run que só inicia após o ciclo.
- **Não-regressão:** os casos de job-running (`Running>0`) e idle seguem idênticos.

## Checklist de validação

- [ ] `pwsh civm-orchestrator-decision.test.ps1` → `0 FAIL` na box (PS 5.1).
- [ ] Balance de braces/parens nos `.ps1` (lint estático local).
- [ ] `Get-OrchestratorDecision` sem o param `BoundaryCompactFloorGB` (grep = 0 refs).
- [ ] orchestrator SPEC atualizado (§2/§4.1/§4/RF-10) no mesmo commit.
- [ ] Captura ao vivo do `boundary_compact` warm + admissão `≥51` em
      `V:\civm-orchestrator.log`; entrada em `civm/validation.md`
      (`orchestrator-decision` + `disk-reclaim`, dados medidos).
- [ ] Gate cognitivo: pergunta/evidência/abort do §"Disciplina Kahneman" respondidos.
