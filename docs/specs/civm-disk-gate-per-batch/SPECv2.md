---
slug: civm-disk-gate-per-batch
title: SPECv2 — Disco limpo (≥51 GB) por batch (PR e re-run)
milestone: —
issues: []
---

# SPECv2 — Disco limpo (≥51 GB) por batch: PR e re-run

> Versão melhorada após auditoria do Passo 2.5.
> Baseline preservado: `SPEC.md` (não alterado; é o histórico).
> **Auditado:** `SPEC.md` → veredito **no-go** (7 blockers + 9 high).
> **Motivo:** o `SPEC.md` declarava o caller `civm-vm-orchestrator.ps1` "fora de
> escopo", mas RF-3 (anti-deadlock), o reset do contador, a remoção do param e o
> evento observável **todos exigem editá-lo** — sem isso o anti-deadlock é morto
> (loop infinito de compactação) e a remoção do param quebra o caller em runtime.
> **Candidato ativo** para nova auditoria do Passo 2.5.

## Blockers do Passo 2.5 endereçados (rastreio)

| Finding | Como o SPECv2 resolve |
| ------- | --------------------- |
| **B1** anti-deadlock inimplementável (contador nunca incrementa no warm) | DT-1 + ITEM-2: o caller incrementa `admitReclaimAttempts` no arm `boundary_compact` (espelho do `reclaim_before_admit`). |
| **B2** contador nunca reseta no warm (`mark_busy`) | DT-2 + ITEM-2: `mark_busy`/`start` resetam o contador na admissão warm (`Running==0`). |
| **B3** remover o param quebra o caller (Day-0) | DT-3 + ITEM-2: o param `BoundaryCompactFloorGB` é removido da função **e** do caller (decl/passagem/logs). |
| **B4** trade-off 40→51 não quantificado | DT-4 + §Throughput: cálculo explícito + métrica-guarda ligada ao rollback; histerese rejeitada (violaria ≥51). |
| **H1** `GuestFreeGB` stale no warm | DT-5: o gate warm é **host-only** (o Off mantém both-sides); guest stale não decide compactação warm. |
| **H2** precedência sobre `panic` | DT-6: o gate warm precede **só `warn_clean`**; `panic` (`<18`) ganha antes (preserva piso crítico + `lastPanicUtc`). |
| **H3** evento `disk_below_floor_admitted` ausente | ITEM-2: `start`/`mark_busy` emitem `disk_below_floor_admitted` ao admitir com `V<51`. |
| **H4** evidência ao vivo irreproduzível | §Plano de testes: 3 cenários reais + repro determinístico via `-Observe` com snapshot/state forjados. |
| **H5** números Kahneman errados | §Disciplina: fail-safe=#15, retry=#14, idempotência=#16 (corrigido aqui e no PRD §8). |
| **H6** sem cooldown → loop tick-a-tick | DT-7 + §Plano: a terminação é provada por teste stateful multi-tick (≤2 compacts/episódio); boundary para a VM (hand-off ao Off). |
| **H7** nomenclatura PRD↔doc viva | ITEM-3: doc viva reconciliada; nota de rastreio `reclaim_before_batch`(PRD)↔`boundary_compact`(impl). |
| **M1–M5, L1–L2** | endereçados em §Rollback (3 arquivos), §Plano (caso RF-2 banda nova + stateful), §Abort (limiares do PRD §9), §Deploy. |

## Escopo fechado desta implementação

**Entra agora:**

- `Get-OrchestratorDecision` (`civm-orchestrator-decision.ps1`): gate de admissão
  de batch **warm** (`Running==0` + `Queued>0`), **host-only**, piso `AdmitFloorGB`
  (51), com precedência **só sobre `warn_clean`** (panic mantém precedência), e
  o caso "admite" (V≥51 ou `attempts>=2`) caindo em `mark_busy`. Remoção do param
  `BoundaryCompactFloorGB`.
- `civm-vm-orchestrator.ps1` (**caller/switch — agora no escopo**): incremento/reset
  de `admitReclaimAttempts` no caminho warm; emissão de `disk_below_floor_admitted`;
  remoção da declaração/passagem/logs de `BoundaryCompactFloorGB`.
- `civm-orchestrator-decision.test.ps1`: ajustar os 3 casos alterados + adicionar
  os casos novos; **+ teste stateful multi-tick** de terminação (anti-deadlock).
- Doc viva `orchestrator-scale-to-zero/SPEC.md` (todas as âncoras do piso 40).

**Fora agora:** o `switch` continua chamando `Invoke-StopAndCompact` (reuso); a
medição guest-side do Off-path (inalterada); serialização do docker-heavy/workflow
acme; pré-build/pull.

**Dependências prontas (reuso):** `Invoke-StopAndCompact`, `civm-reclaim-gate.ps1`
(`Test-OptimizeSlack`, lock `V:\civm-reclaim.lock`), `Get-VFreeGB`, `Get-State`/
`Save-State`, `Write-OrcLog`.

## Matriz de rastreabilidade PRD → SPECv2

| PRD  | Implementação |
| ---- | ------------- |
| RF-1 (≥51 antes de todo batch) | ITEM-1 (gate warm @51) + ITEM-2 (incremento/reset garantem convergência) |
| RF-2 (nunca compacta com job rodando) | ITEM-1 (gate exige `Running==0`); caso de teste `r=2,vfree=45 → mark_busy` |
| RF-3 (fail-safe anti-deadlock) | ITEM-2 (contador incrementa no boundary, reseta na admissão) + teste stateful |
| RF-4 (precedência) | DT-6 (warm precede só warn; panic antes) |
| RF-5 (cache descartado) | invariante |
| RF-6 (observabilidade) | ITEM-2 (`disk_below_floor_admitted` + logs já existentes) |

## Decisões técnicas (fecham ambiguidade + corrigem o no-go)

| #    | Decisão | Justificativa |
| ---- | ------- | ------------- |
| DT-1 | O **caller entra no escopo**; o arm `boundary_compact` passa a medir `vAfter` pós-compact e **incrementar** `admitReclaimAttempts` se `vAfter<51` (zerar se `>=51`), espelhando `reclaim_before_admit` (L332-334). | Sem isso o contador fica 0 no warm → `attempts<2` sempre true → loop infinito (B1). |
| DT-2 | `start` (L320, já reseta) **e** `mark_busy` (na admissão warm, `Running==0`) **resetam** `admitReclaimAttempts=0`. | A admissão warm é via `mark_busy` (VM já Running); sem reset o contador trava em 2 e admite sujo para sempre (B2). |
| DT-3 | Remover `BoundaryCompactFloorGB` da função **e** do caller (decl L65, passagem L310, logs L362/L364). | Remover só da função = `ParameterBindingException` em runtime; manter órfão = código morto (Day-0) (B3). |
| DT-4 | **Sem histerese**: compacta sempre que um batch vai iniciar e `V<51`. O custo (≈1 compact/batch) é aceito e **medido** (métrica-guarda + rollback). | A política do usuário é ≥51 **por batch**; histerese (gatilho<40, admite a 51) deixaria batch iniciar a 40–51, violando-a (B4). |
| DT-5 | O gate warm é **host-only** (`VFreeGB`); o Off-path mantém both-sides. | O snapshot guest é de 10 min (tick=2 min) — stale demais para decidir um Stop+Optimize de ~8 min no warm (Kahneman #15 proíbe decidir por medição stale) (H1). |
| DT-6 | O gate warm precede **só `warn_clean`**; `panic_compact` (`<18`) avalia **antes**. | Mantém o piso crítico `<18` único (não re-deriva entre dois caminhos) e preserva `lastPanicUtc`/cooldown — o teste existente `V<18→panic` fica inalterado (H2). |
| DT-7 | A terminação (não-loop) é garantida por: (a) `boundary_compact` faz Stop-VM → próximo tick é o ramo **Off** (hand-off, não re-avalia warm) + (b) `attempts>=2` admite. Provado por teste **stateful multi-tick**, não só decision-table de tick único. | `boundary_compact` não tem cooldown próprio; a terminação vem do hand-off + anti-deadlock, que precisa de prova de sequência (H6/M2). |
| DT-8 | A ação implementada é **`boundary_compact` retunada** (não a `reclaim_before_batch` que o PRD cogitou). | Day-0/superfície mínima; a doc viva e o rastreio são reconciliados (H7). |

## Fronteira de atomicidade e rollback

- **Atômico:** a decisão pura é sem efeito; cada `Invoke-StopAndCompact` é atômico
  por chamada (lock `V:\civm-reclaim.lock`). O **ciclo** `compacta (tick N) → admite
  (tick N+1)` é multi-tick e **idempotente** (Kahneman #16) — cada tick re-avalia
  o estado; `admitReclaimAttempts` no `state.json` é o único estado persistido.
- **Rollback de aplicação (unidade atômica de 3 arquivos):** reverter juntos
  `civm-orchestrator-decision.ps1`, `civm-vm-orchestrator.ps1` e
  `civm-orchestrator-decision.test.ps1` — restaurar o param `BoundaryCompactFloorGB`
  (decl+passagem+gate@40 host-only) e remover o incremento/reset/evento do warm. Um
  revert parcial reintroduz o `ParameterBindingException` (DT-3 ao contrário).
  Aplicar **sem reclaim em curso** (checar ausência de `V:\civm-reclaim.lock`).
- **Rollback de migration/dados:** N/A (sem schema, sem dados). **forward-only:** N/A.

## Disciplina Kahneman (etapa crítica — risco operacional)

Fonte: `disciplines/KAHNEMAN-DISCIPLINES.md` (16 disciplinas). **#13** = ilusão de
validade (existência≠função); **#14** = retry calibrado; **#15** = fail-safe default
+ curador (proíbe decidir por medição stale); **#16** = idempotência de efeitos
replayáveis.

- **#13:** provar por EFEITO — decision-table (string) **+** teste stateful
  (contador converge ≤2 compacts/episódio) **+** captura ao vivo do evento.
- **#15 (fail-safe):** `V<=0`/`guest<=0` = "não medi" → não bloqueia/não compacta;
  `attempts>=2` → admite (não deadlock); guest stale → host-only (DT-5).
- **#16 (idempotência):** o ciclo multi-tick compacta→admite é re-executável sem
  efeito duplicado (o lock + o gate de slack do `Invoke-StopAndCompact` garantem).
- **#14 (retry calibrado):** `attempts` limita o reclaim a 2 por episódio.
- **Pergunta obrigatória:** existe caminho que (a) admite batch `V<51` sem emitir
  `disk_below_floor_admitted`, (b) compacta com `Running>0`, ou (c) não converge
  para admissão? **Evidência mínima:** decision-table `0 FAIL` + teste stateful
  PASS + 1 captura ao vivo dos 3 cenários no log. **Abort trigger:** ver §Abort.

## Arquivos a MODIFICAR

### `deploy/windows/civm-orchestrator-decision.ps1` — ITEM-1

- **Remover** o parâmetro `[int]$BoundaryCompactFloorGB = 40` (L18-26) e seu comentário.
- **Inserir** o gate warm **após** `panic_compact` (L69) e **antes** de `warn_clean`
  (L70); **remover** o antigo gate boundary (L85). Forma final (host-only, precede
  só warn, admite quando V≥51 ou attempts maxed):
  ```powershell
  # ADMISSAO DE BATCH WARM: VM Running, nenhum job ativo (Running==0) e batch na fila
  # (Queued>0). Espelha a barreira de admissao do caso VM-Off: o batch (PR ou re-run)
  # so inicia com o disco LIMPO em AdmitFloorGB (51). Disco sujo -> boundary_compact
  # (Stop+Optimize offline; NAO mata job pois Running==0); o proximo tick cai no ramo
  # Off e religa via 'start'. Precede WARN (queremos compactar ate 51, nao so podar
  # online ao admitir um batch); NAO precede panic (<18 mantem o piso critico unico e
  # preserva o cooldown/lastPanicUtc). HOST-ONLY: o guest snapshot e de 10min, stale
  # demais p/ decidir um compact de ~8min (Kahneman #15). V<=0 = "nao medi" -> admite
  # (fail-safe). attempts>=2 -> admite mesmo <51 (anti-deadlock; o caller conta).
  if ($Running -eq 0 -and $Queued -gt 0) {
      if ($VFreeGB -gt 0 -and $VFreeGB -lt $AdmitFloorGB -and $AdmitReclaimAttempts -lt 2) { return 'boundary_compact' }
      return 'mark_busy'
  }
  ```
- **Por que:** RF-1/RF-2/RF-4. Com `Running>0` o gate não dispara → panic/warn/
  mark_busy intactos (contrato job-running). `mark_busy` aqui é a admissão warm.

### `deploy/windows/civm-vm-orchestrator.ps1` — ITEM-2 (caller, agora no escopo)

1. **Remover** `[int]$BoundaryCompactFloorGB = 40` (L60-65) e a passagem
   `-BoundaryCompactFloorGB $BoundaryCompactFloorGB` (L310).
2. **Arm `boundary_compact`** (L352-367): trocar os logs `floor = $BoundaryCompactFloorGB`
   (L362, L364) por `floor = $AdmitFloorGB`; e **após** `Invoke-StopAndCompact`
   (L365), adicionar o incremento (espelho de L332-334):
   ```powershell
   $vAfter = Get-VFreeGB
   if ($vAfter -gt 0 -and $vAfter -lt $AdmitFloorGB) { $state.admitReclaimAttempts = [int]$state.admitReclaimAttempts + 1 }
   else { $state.admitReclaimAttempts = 0 }
   Save-State $state
   ```
3. **Arm `mark_busy`** (L338): na admissão warm (`$running -eq 0 -and $queued -gt 0`),
   resetar o contador e emitir o evento se admitir sujo:
   ```powershell
   'mark_busy' {
       if (-not $Observe) {
           if ($running -eq 0 -and $queued -gt 0) {
               if ($vfree -gt 0 -and $vfree -lt $AdmitFloorGB) {
                   Write-OrcLog 'disk_below_floor_admitted' @{ v_free_gb = $vfree; guest_free_gb = $guestFree; floor = $AdmitFloorGB; attempts = [int]$state.admitReclaimAttempts; path = 'warm' }
               }
               $state.admitReclaimAttempts = 0
           }
           $state.lastBusyUtc = $nowUtc; Save-State $state
       }
   }
   ```
4. **Arm `start`** (L314-321): emitir o mesmo evento quando admite sujo (Off-path,
   `attempts>=2` + `V<51`), antes do reset existente (L320):
   ```powershell
   if ($vfree -gt 0 -and $vfree -lt $AdmitFloorGB) {
       Write-OrcLog 'disk_below_floor_admitted' @{ v_free_gb = $vfree; guest_free_gb = $guestFree; floor = $AdmitFloorGB; attempts = [int]$state.admitReclaimAttempts; path = 'cold' }
   }
   ```
- **Por que:** B1 (incremento warm), B2 (reset warm), B3 (param), H3 (evento).

### `deploy/windows/civm-orchestrator-decision.test.ps1` — ITEM-2b

- **Casos ALTERADOS** (3): `q=3,r=0,vfree=45 → boundary_compact` (era mark_busy);
  `q=1,r=0,vfree=40 → boundary_compact` (era mark_busy); `q=1,r=0,vfree=25 →
  boundary_compact` (era warn_clean — agora o gate precede warn).
- **Caso INALTERADO crítico:** `q=1,r=0,vfree=15,cp=T → panic_compact` (DT-6: panic
  precede o gate warm). Atualizar o **comentário** L37-41 para refletir DT-6 (panic
  ganha `<18`; boundary cobre `18..51`) — sem mentir (Kahneman #13).
- **Casos NOVOS:**
  - `q=2,r=0,vfree=55 → mark_busy` (warm, V≥51 → admite limpo).
  - `q=2,r=0,vfree=51 → mark_busy` (==floor, `<` estrito).
  - `q=2,r=0,vfree=45,ra=2 → mark_busy` (attempts maxed → admite, anti-deadlock).
  - `q=2,r=0,vfree=45,ra=1 → boundary_compact` (attempts<2 → ainda tenta).
  - `q=2,r=2,vfree=45 → mark_busy` (RF-2: `Running>0` → gate NÃO dispara, job protegido).
  - `q=1,r=0,vfree=15,cp=F → boundary_compact` (gap `<18` + cooldown → panic pulado → gate warm compacta sem matar).
- **Remover** o caso `gf=40 → boundary_compact` do SPEC.md (DT-5: warm é host-only;
  guest não decide no warm).

### Teste stateful de terminação — ITEM-2c (DT-7 / H6 / M2)

Novo bloco no `*.test.ps1` (ou arquivo `civm-orchestrator-decision.stateful.test.ps1`)
que prova a convergência do anti-deadlock **sem** depender da box: mockar
`Get-VFreeGB` para retornar sempre `<51`, `Invoke-StopAndCompact` como no-op, um
`$state` em memória, e rodar a sequência warm→Off por ≥3 ticks afirmando:
`admitReclaimAttempts` chega a 2 e então o tick admite (`start`/`mark_busy`) +
emite `disk_below_floor_admitted` — **no máximo 2 compacts por episódio**.

### `docs/specs/orchestrator-scale-to-zero/SPEC.md` — ITEM-3 (doc viva, mesmo commit)

Reconciliar **todas** as âncoras do piso 40 / banda efetiva (H7/L2): §2 tabela de
ações (`boundary_compact` → `Running==0`+`Queued>0`+`V<AdmitFloorGB(51)` host-only,
antes de warn, depois de panic); §4 tabela "Camada de disk-safety"; §4 "Escolha do
piso `BoundaryCompactFloorGB=40`" (marcar **superseded** pela política ≥51-por-batch);
§4 "Banda efetiva" (28..40 → 18..51); §4.1 gates (remover `BoundaryCompactFloorGB`);
§11 contagem de casos; RF-10 (implementado; usar `boundary_compact`, **não**
`reclaim_before_batch`). Nota de rastreio: "PRD usou `reclaim_before_batch`; impl é
`boundary_compact` retunada".

## Throughput — quantificação e guarda (DT-4 / B4)

- Dado: ~11 GB/PR (comentário `decision.ps1:24`), compact ~8 min (PRD §5), tick 2 min.
  Com piso 51, o disco cai `<51` após ~todo PR → **≈1 compact por batch** sob CI
  sustentado (~8 min de VM Off/batch). É o **custo aceito** da garantia de paridade.
- **Histerese rejeitada** (violaria ≥51-por-batch — DT-4).
- **Métrica-guarda:** contar `disk_boundary_compact`/h no `civm-orchestrator.log`.
- **Ligação ao rollback (PRD §9):** se `disk_boundary_compact`/h indicar compactação
  dominante (gargalo) por janela sustentada, é sinal de que a box não sustenta a
  política → escalar para o fix maior (VM-per-job/mais disco), não remendar o gate.

## Abort triggers (numéricos, do PRD §9 — M5)

- **Deadlock:** um batch não admitido por **> 75 min** no `civm-orchestrator.log`.
- **Admissão suja:** evento `disk_below_floor_admitted` com `v_free_gb < 51`
  (esperado só no caso irrecuperável; se recorrente, abortar).
- **Compacta com job:** qualquer `disk_boundary_compact` com `running>0` no log.

## Ordem de implementação

1. ITEM-1 (`decision.ps1`: remover param, inserir gate warm DT-6).
2. ITEM-2 (`civm-vm-orchestrator.ps1`: param, incremento boundary, reset+evento).
3. ITEM-2b/2c (`*.test.ps1`: casos + teste stateful).
4. `pwsh *.test.ps1` na box → `0 FAIL`; teste stateful PASS.
5. ITEM-3 (doc viva) no mesmo commit.
6. Deploy via `activate-orchestrator.ps1` **sem** `V:\civm-reclaim.lock` em curso (L1);
   capturar os 3 cenários ao vivo; registrar em `civm/validation.md`.

## Plano de testes

- **Decision-table (pwsh, na box):** `0 FAIL` com os 3 alterados + 6 novos.
- **Stateful (pwsh):** convergência ≤2 compacts/episódio + evento (ITEM-2c).
- **Ao vivo (3 cenários reais, repro determinístico via `-Observe` + snapshot/state
  forjados, sem esperar death-spiral):**
  1. **Reclaim:** warm gap `18≤V<51` → `disk_boundary_compact` → tick seguinte
     `VmState=Off` → `start` com `V≥51`.
  2. **Limpo:** warm gap `V≥51` → `mark_busy` (sem compactar).
  3. **Irrecuperável:** `attempts>=2` + `V<51` → admite + `disk_below_floor_admitted`.
  - Repro: injetar `V:\civm-host-metrics.json` + `state.json` forjados e rodar
    `civm-vm-orchestrator.ps1 -Observe` 1 tick por cenário; abort se não reproduzir.

## Checklist de validação

- [ ] `pwsh civm-orchestrator-decision.test.ps1` → `0 FAIL` (PS 5.1).
- [ ] Teste stateful de terminação PASS.
- [ ] `grep BoundaryCompactFloorGB` = **0 refs** em **ambos** os `.ps1`.
- [ ] `Running>0` nunca produz `boundary_compact` (caso `r=2,vfree=45`).
- [ ] doc viva (`orchestrator-scale-to-zero/SPEC.md`) reconciliada — todas as âncoras.
- [ ] Captura ao vivo dos 3 cenários + evento `disk_below_floor_admitted`;
      entrada em `civm/validation.md` (`orchestrator-decision`+`disk-reclaim`).
- [ ] Números Kahneman conferidos contra `disciplines/KAHNEMAN-DISCIPLINES.md`.
