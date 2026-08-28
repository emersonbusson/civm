---
slug: civm-disk-gate-per-batch
title: SPECv3 — Disco limpo (≥51 GB) por batch (PR e re-run)
milestone: —
issues: []
---

# SPECv3 — Disco limpo (≥51 GB) por batch: PR e re-run

> ⚠️ **SUPERSEDED por [`SPECv5.md`](SPECv5.md)** (gate por-evento `running>0→0`; piso vivo `AdmitFloorGB`=55). Mantido como histórico de design.

> Versão melhorada após a **2ª** auditoria do Passo 2.5.
> Baselines preservados: `SPEC.md` e `SPECv2.md` (histórico; não alterados).
> **Auditado:** `SPECv2.md` → veredito **no-go** (1 blocker + 9 high + 6 med + 6 low).
> **V1→V2 já resolvidos** (mantidos): B1/B2/B3 (caller no escopo + contador), H1
> (host-only), H2 (panic precede warm), H5 (números Kahneman), M1/M3/M5/L1.
> **Motivo do v3:** a prova do anti-deadlock era **inexecutável** (incremento/reset
> inline no `switch` do caller, não em função) e a alegação "≤2 compacts/episódio"
> era **falsa** na faixa `V<18` (panic não conta a tentativa); além de evento fora
> do `-Observe`, doc viva incompleta, deploy não-atômico e limite de rollback vago.
> **Candidato ativo** para nova auditoria do Passo 2.5.

## Findings do Passo 2.5 (SPECv2) endereçados

| Finding | Severidade | Como o SPECv3 resolve |
| ------- | ---------- | --------------------- |
| **F1** prova anti-deadlock inexecutável (lógica inline) | blocker | **ITEM-1**: extrai `Update-AdmitAttempts` (função PURA em `decision.ps1`, dot-sourced pelo caller **e** pelo teste); **ITEM-2c**: teste stateful exercita as funções REAIS (Kahneman #13). |
| **F2** "≤2 compacts" falso na faixa `V<18` (panic não conta) | high | **DT-9**: o arm `panic_compact` chama `Update-AdmitAttempts` **quando `Running==0`** → episódio de admissão = ≤2 compacts mesmo iniciando por panic. Claim re-escopada. |
| **F3/F11** evento só em non-`Observe`; repro conflacionado | high/med | **ITEM-2**: emite `would_disk_below_floor_admitted` no `-Observe`; **§Plano** separa repro-de-estado (`-Observe`+state forjado) de prova-de-convergência (stateful). |
| **F4/F5/H7/L2** doc viva incompleta + evento fora da §8 | high | **ITEM-3**: lista TODAS as âncoras (prosa de precedência, subseção boundary, banda 28..40, event-table §8, contagem) + adiciona `disk_below_floor_admitted` à §8. |
| **F6/B4** limite de rollback por throughput vago | high | **§Throughput/§Abort**: número operável (`disk_boundary_compact/h > 4` por ≥2h **ou** ≥3 `disk_below_floor_admitted`<51 em 1h); PRD §9 alinhado. |
| **F7/M4** perda de ticks por `IgnoreNew` não modelada | high | **§Throughput**: quantifica os ~4 ticks engolidos; o `start` vem ~8–10 min após o início do compact; orçamento de 75 min decomposto. |
| **F8/L1** deploy não-atômico dos 2 `.ps1` | high | **§Deploy**: stage + cópia dos dois `.ps1` ANTES do `activate`; nunca mismatch decision/caller. |
| **F9** host-only vs PRD RF-1 (both-sides) | med | **DT-5+**: RF-1 é **host-only no warm** (guest só no Off-path pós-Stop); reconciliado no PRD. |
| **F10** "~1 compact/batch" vs até 2 irrecuperável | med | **§Throughput**: "1/batch saudável; até 2/batch irrecuperável". |
| **F12** `ExecutionTimeLimit` 30min vs Optimize | med | **§Deploy**: medir a duração do Optimize ao vivo; se >25min, subir o limite p/ `PT2H` (como o autoreclaim). |
| **F13** §Abort trocou (não somou) o 3º trigger do PRD §9 | med | **§Abort**: mantém os 3 do PRD §9 + adiciona `running>0` como 4º. |
| **F14/F16/F17** comentários órfãos; skip conta; lock vs IgnoreNew | low | **ITEM-1/2** removem comentários órfãos; **§Abort** documenta skip→attempts; **§Race** esclarece `IgnoreNew` como serializador. |
| **F15** faltam casos `V=0` warm e banda 28-40 | low | **ITEM-2b** adiciona `vfree=0→mark_busy` e `vfree=30→boundary_compact`. |

## Escopo fechado

**Entra agora:**

- `civm-orchestrator-decision.ps1`: (a) o gate de admissão warm (DT-6, host-only);
  (b) **nova função pura `Update-AdmitAttempts`** (F1); (c) remoção do param
  `BoundaryCompactFloorGB` + comentários órfãos.
- `civm-vm-orchestrator.ps1`: arms `boundary_compact`/`reclaim_before_admit`/
  `panic_compact`(quando `Running==0`) chamam `Update-AdmitAttempts`; `mark_busy`/
  `start` resetam + emitem `disk_below_floor_admitted` (e `would_*` no `-Observe`);
  remoção do param (decl/passagem/logs/comentários).
- `civm-orchestrator-decision.test.ps1`: decision-table (alterados + novos) **+**
  teste **stateful** de convergência usando as funções reais.
- `orchestrator-scale-to-zero/SPEC.md`: **todas** as âncoras do piso 40 + event-table §8.

**Fora agora:** o `switch` segue chamando `Invoke-StopAndCompact` (reuso); o
Off-path guest-side (inalterado); serialização docker-heavy/workflow acme; pré-build.

## Matriz PRD → SPECv3

| PRD | Implementação |
| --- | ------------- |
| RF-1 (≥51 antes de todo batch — **host-only no warm**, DT-5+) | ITEM-1 (gate) + ITEM-2 (`Update-AdmitAttempts` garante convergência) |
| RF-2 (nunca com job rodando) | ITEM-1 (`Running==0`); teste `r=2,vfree=45→mark_busy` |
| RF-3 (fail-safe anti-deadlock) | ITEM-1 (`Update-AdmitAttempts`) + ITEM-2c (prova stateful) |
| RF-4 (precedência) | DT-6 (warm precede só warn) + DT-9 (panic conta no warm) |
| RF-5 (cache descartado) | invariante |
| RF-6 (observabilidade) | ITEM-2 (`disk_below_floor_admitted` + `would_*`) + ITEM-3 (§8 doc viva) |

## Decisões técnicas

| #    | Decisão | Justificativa |
| ---- | ------- | ------------- |
| DT-1..DT-8 | (mantidas do SPECv2: caller no escopo; reset em mark_busy/start; remoção do param; sem histerese; host-only warm; panic precede warm; terminação por hand-off; `boundary_compact` retunada) | — |
| **DT-9** | O arm `panic_compact` chama `Update-AdmitAttempts` **somente quando `Running==0`** (admissão warm que começou por panic). Com `Running>0` (emergência mid-job) **não** conta. | Faz o episódio de admissão (`Running==0` onset) ter **≤2 compacts mesmo iniciando por panic** (F2); a emergência mid-job é outro fluxo (não admissão). |
| **DT-10** | A lógica do contador vira a função pura **`Update-AdmitAttempts`** em `decision.ps1` (dot-sourced pelo caller e pelo teste). Os 3 arms de compact a chamam; nenhum reimplementa inline. | F1/Kahneman #13: o teste exercita a função REAL deployada, não uma cópia. |
| **DT-11** | `disk_below_floor_admitted` é emitido em non-`Observe` **e** como `would_disk_below_floor_admitted` em `-Observe`. | F3: o evento é o objeto de prova de H3/RF-6; tem de existir no modo de repro. |
| **DT-12** | Deploy é **atômico nos 2 `.ps1`** (stage + cópia de `decision.ps1`+`caller` antes do `activate`); o `ExecutionTimeLimit` do compact-path é validado/ajustado a `PT2H` se Optimize >25min. | F8 (mismatch = `ParameterBindingException`/tick) + F12 (kill mid-Optimize). |

## Disciplina Kahneman (etapa crítica — risco operacional)

Fonte: `disciplines/KAHNEMAN-DISCIPLINES.md` — **#13** ilusão de validade
(existência≠função); **#14** retry calibrado; **#15** fail-safe + curador (proíbe
decidir por medição stale); **#16** idempotência.

- **#13:** `Update-AdmitAttempts` + `Get-OrchestratorDecision` são as MESMAS funções
  no deploy e no teste (decision-table + stateful). O arm do caller só **chama** as
  funções puras (não duplica lógica) — senão o teste seria uma cópia.
- **#15:** `V<=0`/guest stale → não bloqueia/host-only; `attempts>=2` → admite (não
  deadlock) + evento; o curador (gate) não decide por snapshot guest de 10 min.
- **#16:** ciclo multi-tick compacta→admite re-executável (lock + gate de slack).
- **#14:** `attempts` limita o reclaim a 2 por episódio de admissão.
- **Pergunta obrigatória:** existe caminho que (a) admite `V<51` sem
  `disk_below_floor_admitted`, (b) chama `Update-AdmitAttempts` com `Running>0`
  fora do panic-emergency, (c) não converge em ≤2 compacts no episódio de admissão
  (incl. faixa `V<18`)? **Evidência:** decision-table `0 FAIL` + stateful PASS
  (incl. cenário `V<18`) + captura ao vivo (3 cenários) + grep doc viva = 0 stale.
  **Abort:** ver §Abort.

## Arquivos a MODIFICAR

### `deploy/windows/civm-orchestrator-decision.ps1` — ITEM-1

1. **Remover** o param `[int]$BoundaryCompactFloorGB = 40` **e** o bloco-comentário
   que o explica (decision.ps1 L18-26 **e** L71-84 — o raciocínio do piso 40, hoje
   superseded; F14).
2. **Inserir** o gate warm (host-only, precede só warn) após `panic_compact` e antes
   de `warn_clean`; remover o gate boundary antigo (L85):
   ```powershell
   if ($Running -eq 0 -and $Queued -gt 0) {
       if ($VFreeGB -gt 0 -and $VFreeGB -lt $AdmitFloorGB -and $AdmitReclaimAttempts -lt 2) { return 'boundary_compact' }
       return 'mark_busy'
   }
   ```
3. **Adicionar** a função pura (DT-10), no mesmo arquivo, antes de `Get-OrchestratorDecision`:
   ```powershell
   # Conta tentativas da BARREIRA DE ADMISSAO — PURA e testavel (Kahneman #13: o
   # caller CHAMA esta funcao; o teste exercita a MESMA). vAfter = V: livre medido
   # APOS o compact. Incrementa se ainda sujo (0<vAfter<Floor); zera se limpo
   # (>=Floor) OU se nao mediu (<=0 -> #15 fail-safe: nao da false give-up). NOTA: um
   # skip do Invoke-StopAndCompact (lock/slack) deixa vAfter<Floor -> conta como
   # tentativa (por design: lock persistente -> admite em ~2 ticks, sem deadlock).
   function Update-AdmitAttempts {
       param([Parameter(Mandatory)]$State, [int]$VAfter, [int]$Floor = 51)
       if ($VAfter -gt 0 -and $VAfter -lt $Floor) { $State.admitReclaimAttempts = [int]$State.admitReclaimAttempts + 1 }
       else { $State.admitReclaimAttempts = 0 }
       return $State
   }
   ```

### `deploy/windows/civm-vm-orchestrator.ps1` — ITEM-2 (caller)

1. **Remover** o param `[int]$BoundaryCompactFloorGB = 40` (L65) **e** seu comentário
   (L61-64; F14), e a passagem `-BoundaryCompactFloorGB $BoundaryCompactFloorGB` (L310).
2. **`reclaim_before_admit`** (L331-335): trocar o inline L332-334 por
   `$state = Update-AdmitAttempts -State $state -VAfter (Get-VFreeGB) -Floor $AdmitFloorGB; Save-State $state`.
3. **`boundary_compact`** (L362-366): trocar logs `floor=$BoundaryCompactFloorGB`
   (L362/L364) por `$AdmitFloorGB`; após `Invoke-StopAndCompact` (L365), idem:
   `$state = Update-AdmitAttempts -State $state -VAfter (Get-VFreeGB) -Floor $AdmitFloorGB; Save-State $state`.
4. **`panic_compact`** (L368-381): após `Invoke-StopAndCompact` (L379), **quando
   `$running -eq 0`** (DT-9): `$state = Update-AdmitAttempts -State $state -VAfter (Get-VFreeGB) -Floor $AdmitFloorGB; Save-State $state`.
5. **`mark_busy`** (L338): na admissão warm (`$running -eq 0 -and $queued -gt 0`),
   resetar `admitReclaimAttempts=0` e emitir o evento se sujo — com ramo `-Observe`:
   ```powershell
   'mark_busy' {
       if ($running -eq 0 -and $queued -gt 0 -and $vfree -gt 0 -and $vfree -lt $AdmitFloorGB) {
           $evt = if ($Observe) { 'would_disk_below_floor_admitted' } else { 'disk_below_floor_admitted' }
           Write-OrcLog $evt @{ v_free_gb = $vfree; guest_free_gb = $guestFree; floor = $AdmitFloorGB; attempts = [int]$state.admitReclaimAttempts; path = 'warm' }
       }
       if (-not $Observe) {
           if ($running -eq 0 -and $queued -gt 0) { $state.admitReclaimAttempts = 0 }
           $state.lastBusyUtc = $nowUtc; Save-State $state
       }
   }
   ```
6. **`start`** (L314-321): antes do reset L320, emitir o evento (com `would_*` no
   `-Observe`) quando `$vfree -gt 0 -and $vfree -lt $AdmitFloorGB` (path='cold').

### `deploy/windows/civm-orchestrator-decision.test.ps1` — ITEM-2b

- **Alterados (3):** `q=3,r=0,vfree=45→boundary_compact`; `q=1,r=0,vfree=40→boundary_compact`;
  `q=1,r=0,vfree=25→boundary_compact` (gate precede warn).
- **Inalterado crítico:** `q=1,r=0,vfree=15,cp=T→panic_compact` (DT-6). Atualizar o
  comentário L28-30 ("V<40") e L37-41 p/ refletir DT-6/DT-9 (F14).
- **Novos:** `vfree=55→mark_busy`; `vfree=51→mark_busy`; `vfree=45,ra=2→mark_busy`;
  `vfree=45,ra=1→boundary_compact`; `r=2,vfree=45→mark_busy` (RF-2); `vfree=15,cp=F→
  boundary_compact`; **`vfree=0→mark_busy`** (fail-safe #15, F15); **`vfree=30→
  boundary_compact`** (banda 28-40, F15).
- **Unitário de `Update-AdmitAttempts`:** `VAfter=48,Floor=51→attempts+1`;
  `VAfter=55→0`; `VAfter=0→0`.

### Teste stateful de terminação — ITEM-2c (F1/F2/H6/M2)

Novo bloco no `*.test.ps1` que **dot-source `decision.ps1`** e roda um loop usando as
funções REAIS `Get-OrchestratorDecision` + `Update-AdmitAttempts` (sem o `switch` do
caller; mock só do I/O: `vAfter` injetado, `Invoke-StopAndCompact` não é chamado).
Simula a sequência por episódio:
- **Cenário [18,51):** start warm `V=45` → `boundary_compact` (Update→1, VmState=Off)
  → `reclaim_before_admit` (Update→2) → decisão `start`. **Assert: 2 compacts.**
- **Cenário `V<18` (DT-9):** start warm `V=15,cp=T` → `panic_compact` (Running==0 →
  Update→1, Off) → `reclaim_before_admit` (Update→2) → `start`. **Assert: 2 compacts.**
- **Assert geral:** todo episódio admite em **≤2 compacts**; no ponto de admissão
  com `V<51`, a condição de `disk_below_floor_admitted` é verdadeira.

### `docs/specs/orchestrator-scale-to-zero/SPEC.md` — ITEM-3 (doc viva, mesmo commit)

Reconciliar **TODAS** as âncoras (F4/F5/L2; grep load-bearing):
- §2 prosa "Política definitiva" (L56-79) + RF-10 (implementado; `boundary_compact`,
  **não** `reclaim_before_batch`; nota de rastreio).
- §2 prosa "Ordem de precedência" (L106-109): atualizar a cadeia para
  **panic → gate de admissão warm → warn** (warm agora precede warn).
- §4 tabela "Camada de disk-safety"; §4 subseção "boundary_compact — o gap"
  (L156-201) incl. banda **28..40 → 18..51** e a nota de janela-vazia (L191);
  §4 "Escolha do piso 40" → **superseded**; §4 "Banda efetiva"; §4 "Caso back-to-back
  sem gap" (L189-194); §4 "Composição com #137" (L196-205).
- §4.1 gates: remover `BoundaryCompactFloorGB`.
- §8 event-table: **adicionar** `disk_below_floor_admitted | admite batch com V<51
  (warm/cold) | v_free_gb, guest_free_gb, floor, attempts, path | arms start/mark_busy`;
  atualizar `disk_boundary_compact` (campo `floor`→`AdmitFloorGB`).
- §11 contagem de casos (38 → novo total).

## Throughput — quantificação, perda de ticks e guarda (B4/F6/F7/F10/M4)

- ~11 GB/PR (`decision.ps1`); V parte de 51 → ~40 após o PR → batch seguinte
  `40<51` → 1 `boundary_compact`. **Custo: ~1 compact/batch saudável; até 2/batch no
  irrecuperável** (2 reclaims + admite-sujo, reset).
- **Compact síncrono ~8 min** (caller chama `Invoke-StopAndCompact` bloqueante);
  com repetição 2 min + `MultipleInstances=IgnoreNew` (`activate` L13), ~3–4 ticks
  (+2/+4/+6/+8 min) são **descartados** → o tick `start`/admite vem ~8–10 min após o
  **início** do compact, **não** 2 min depois. (Corrige a prosa "próximo tick religa".)
- **Histerese rejeitada** (violaria ≥51-por-batch).
- **Métrica-guarda + rollback numérico (Kahneman #3):** **rollback se
  `disk_boundary_compact/h > 4` por ≥2 h consecutivas** (compactação virou dominante)
  **ou** **≥3 `disk_below_floor_admitted` com `V<51` em 1 h** (disco irrecuperável).
  Calibrar contra o baseline medido (Slice 0); alinhar PRD §9 ao mesmo número.

## Abort triggers (numéricos — F6/F13/M5)

Mantém os **3 do PRD §9** + **adiciona** o 4º:
1. **Deadlock:** batch não admitido por **> 75 min** no log. Decomposição do pior
   caso anti-deadlock: 2 compacts (~16 min) + 2 cold-starts + esperas de fila — folga
   ampla sob 75 min.
2. **Admissão suja:** `disk_below_floor_admitted` com `v_free_gb < 51` recorrente
   (≥3 em 1 h → escalar).
3. **Frequência de panic:** `disk_panic`/h subindo após o merge (sinal de que o gate
   entre-batches não alivia o disco) — mantido do PRD §9.
4. **(novo)** Qualquer `disk_boundary_compact` com `running>0` no log (nunca deve ocorrer).
- **Skip conta como tentativa (F16):** um skip do `Invoke-StopAndCompact`
  (`reclaim_skip_locked`/`_insufficient_slack`/`abort_vm_not_off`) deixa `vAfter<51`
  → `Update-AdmitAttempts` incrementa — **por design**; `disk_below_floor_admitted`
  recorrente com lock ativo é o sinal de abort.

## Deploy (F8/F12/L1/DT-12)

- **Atômico:** copiar `civm-orchestrator-decision.ps1` **e** `civm-vm-orchestrator.ps1`
  para `C:\civm-deploy` (staging) e renomear/substituir **juntos**, ANTES do
  `Unregister`/`Register` do `activate-orchestrator.ps1` — nunca deixar mismatch
  decision/caller entre ticks (senão `ParameterBindingException` a cada 2 min).
- **Sem reclaim em curso:** checar ausência de `V:\civm-reclaim.lock` antes do deploy.
- **`ExecutionTimeLimit` (F12):** medir a duração de um `Optimize-VHD` ao vivo no
  boundary-path; se **>25 min**, subir o limite da task do orchestrator para `PT2H`
  (como `register-civm-vhdx-autoreclaim.ps1`) para não matar mid-Optimize.

## Fronteira de atomicidade e rollback

- Atômico: decisão/`Update-AdmitAttempts` sem efeito; cada `Invoke-StopAndCompact`
  atômico (lock). Ciclo compacta→admite multi-tick **idempotente** (#16).
- **Rollback de app (unidade de 3 arquivos):** reverter `decision.ps1` (gate@40 +
  param + remover `Update-AdmitAttempts`), `caller` (restaurar inline + param + logs)
  e `*.test.ps1` **juntos**. Revert parcial = `ParameterBindingException`. Aplicar
  sem reclaim em curso. **migration/dados:** N/A. **forward-only:** N/A.

## §Race (F17)

O serializador tick-a-tick da própria task é **`MultipleInstances=IgnoreNew`**
(`activate` L13); o lock `V:\civm-reclaim.lock` é defesa-em-profundidade contra
reclaimers externos/legados (já desabilitados no `activate`).

## Ordem de implementação

1. ITEM-1 (`decision.ps1`: param/comentários, gate warm, `Update-AdmitAttempts`).
2. ITEM-2 (caller: param/comentários, 3 arms chamam `Update-AdmitAttempts`,
   reset+evento+`would_*`).
3. ITEM-2b/2c (`*.test.ps1`: decision-table + unitário + stateful).
4. `pwsh *.test.ps1` na box → `0 FAIL`; stateful PASS (incl. `V<18`).
5. ITEM-3 (doc viva — todas as âncoras + §8) no mesmo commit.
6. Deploy atômico (DT-12) sem reclaim em curso; medir Optimize (F12); capturar os 3
   cenários ao vivo; registrar em `civm/validation.md`.

## Plano de testes

- **Decision-table (pwsh, na box):** `0 FAIL` (3 alterados + 8 novos + unitário).
- **Stateful (pwsh, funções reais):** convergência ≤2 compacts/episódio nos 2
  cenários ([18,51) e `V<18`) (ITEM-2c).
- **Ao vivo (separar repro de prova — F3/F11):**
  - **Estados (`-Observe` + `state.json`/`host-metrics.json` forjados, 1 tick/cenário):**
    `would_boundary_compact` (gap `18≤V<51`), `would_disk_below_floor_admitted`
    (`attempts=2,V<51`), `mark_busy` (`V≥51`). `-Observe` **não** muta o contador.
  - **Convergência:** provada pelo stateful (não pelo `-Observe`).
  - **Evento real:** 1 captura em execução non-`Observe` controlada de
    `disk_below_floor_admitted`.
  - **Abort** se algum cenário não reproduzir.

## Checklist de validação

- [ ] `pwsh civm-orchestrator-decision.test.ps1` → `0 FAIL` (PS 5.1); unitário +
      stateful (incl. `V<18`) PASS.
- [ ] `grep -n "BoundaryCompactFloorGB"` = **0** em **ambos** os `.ps1` (código **e**
      comentários).
- [ ] `grep -n "28\.\.40\|piso 40\|=40\|reclaim_before_batch"` na doc viva = **0** stale.
- [ ] `Running>0` nunca produz `boundary_compact`/`Update-AdmitAttempts` (teste `r=2`).
- [ ] `disk_below_floor_admitted` na §8 da doc viva e emitido (warm+cold, `would_*` no Observe).
- [ ] Deploy copiou os 2 `.ps1` juntos; Optimize medido (≤25min ou `PT2H`).
- [ ] Captura ao vivo dos 3 cenários + evento; `civm/validation.md`
      (`orchestrator-decision`+`disk-reclaim`, dados medidos).
- [ ] Números Kahneman conferidos contra `disciplines/KAHNEMAN-DISCIPLINES.md`.
