---
slug: civm-disk-gate-per-batch
title: SPECv4 (FINAL) — Disco limpo (≥51 GB) por batch (PR e re-run)
milestone: —
issues: []
---

# SPECv4 (FINAL e DEFINITIVA) — Disco limpo (≥51 GB) por batch: PR e re-run

> ⚠️ **SUPERSEDED por [`SPECv5.md`](SPECv5.md)** — apesar do "FINAL" no título, a SPECv5 mudou o trigger para POR-EVENTO (`running>0→0`) e o piso vivo é `AdmitFloorGB`=55. Mantido como histórico de design.

> Versão **final** após a 3ª auditoria do Passo 2.5. Auto-contida: é **o contrato
> de implementação** (Passo 3). Substitui as anteriores como candidata ativa.
> Baselines preservados (histórico, não alterados): `SPEC.md`, `SPECv2.md`, `SPECv3.md`.
> **Auditado:** `SPECv3.md` → **no-go** (1 blocker + 4 high + 5 med + 8 low).
> **Já resolvidos e mantidos** (v1→v3): B1/B2/B3/B4, H1/H2/H3/H4/H5/H6, F2/F3/F9/F10/
> F13/F14/F16/F17, M1/M3/M4/M5, L1.
> **Corrigido neste v4:** (blocker) contradição de precedência PRD↔SPEC; (high)
> orquestração do contador sob teste real; deploy = mecanismo; doc viva completa
> (âncoras de linha + §7); (med/low) número do 3º abort, `ExecutionTimeLimit`,
> bordas de teste, redação do `start`, StrictMode, double-Save, `vAfter<=0`.

## 0. Decisão definitiva de precedência (fecha o blocker)

A cadeia da decisão é, em ordem: **`panic_compact` (`V<18`) → gate de admissão warm
(`Running==0`) → `warn_clean` (`V<28`) → `mark_busy`**.

- **Por quê o gate precede `warn_clean`:** a política é **≥51 antes de todo batch**.
  `warn_clean` é online (poda cache + `fstrim`) e **não recupera o `V:` do host** —
  só `Optimize-VHD` (compact) recupera. Entre batches (`Running==0`) podemos parar e
  compactar; logo o gate compacta até 51. `warn_clean` fica reservado a **job
  rodando** (`Running>0`), onde não dá para parar.
- **Por quê `panic` precede o gate:** mantém o piso crítico `<18` único e preserva
  `lastPanicUtc`/cooldown (Kahneman #13: não re-derivar `V<18` em dois caminhos).
  Com `Running==0`, o panic compacta sem matar job; sua tentativa **conta** (DT-9).
- O **PRD RF-4 + §4 + a doc viva §4** foram reconciliados para esta ordem (eram a
  fonte da contradição do SPECv3).

## 0.1 Invariante: nenhum job é morto em operação normal (Kahneman #15)

A política **≥51 GB por batch** (RF-1) + serialização docker-heavy (lock, slice à
parte) implicam: **cada batch começa com ≥51 GB limpos e roda um de cada vez**.
Logo um único batch **não** consome do piso de admissão (51) até o piso de panic
(18) no meio da execução → **nenhum job precisa ser morto**. A compactação ocorre
**só entre batches** (`Running==0`): quando o **último job termina**, limpa-tudo +
`Optimize-VHD` libera ≥51 GB no `V:` do Windows e **só então** o próximo PR/batch
inicia (= o gate de admissão warm, §0/§5).

- **`panic_compact` (a única ação que mata job) NÃO deve disparar.** É mantido só
  como **rede de última instância** contra saturação do host: a box é
  share-everything; sem rede, um fill imprevisto derrubaria o interop e **todos** os
  repos — pior que 1 job re-rodar. **Não removo o backstop neste slice** (decisão de
  segurança maior, separada); mas o design garante que ele não é exercido.
- **Se `panic_compact` disparar com `running>0`, é violação desta invariante → sinal
  de ABORT/rollback** (a premissa de clean-start falhou: batch consumiu >33 GB ou
  houve concorrência não serializada) — investigar, não normalizar (§10 Abort #3).
- `warn_clean` (online, sem stop/kill) idem: não deve ser necessário com clean-start;
  é inofensivo se disparar. `boundary_compact` (entre batches, `Running==0`) **nunca**
  mata job — é o único caminho de compactação esperado em operação normal.
- **Disciplina #15 (fail-safe + curador) / #5 (worst-case) + qualificação (L-α):**
  "operação normal" pressupõe a **serialização docker-heavy efetiva** (lock — §1, fora
  deste slice). O `panic` é o modo-de-falha-seguro que **não deve ser exercido**;
  **pergunta de quebra:** o que prova a invariante violada? = **≥1 `disk_panic` com
  `running>0`** no log → exatamente o sinal de abort do §10 #3 (investigar, não
  normalizar — ex.: concorrência não-serializada ou batch consumindo >33 GB).

## 1. Escopo fechado

**Entra agora** (4 arquivos de código + 1 doc viva):

- `deploy/windows/civm-orchestrator-decision.ps1`: o gate de admissão warm + as
  **funções puras `Update-AdmitAttempts` e `Resolve-AdmitTransition`** + remoção do
  param `BoundaryCompactFloorGB` e comentários órfãos.
- `deploy/windows/civm-vm-orchestrator.ps1` (caller): arms emitem efeito/evento; a
  **transição do contador é delegada a `Resolve-AdmitTransition`** (uma chamada pós-
  switch); remoção do param (decl/passagem/logs/comentários).
- `deploy/windows/civm-orchestrator-decision.test.ps1`: decision-table (alterados +
  novos + bordas) + unitário das funções puras + **teste stateful** que roda as
  funções reais.
- `deploy/windows/activate-orchestrator.ps1`: **mecanismo de deploy** (`Unregister` →
  copiar os 2 `.ps1` → validar por **AST** → `Register`) + `ExecutionTimeLimit`.
- `docs/specs/orchestrator-scale-to-zero/SPEC.md` (doc viva): **todas** as âncoras.

**Fora:** o `switch` segue chamando `Invoke-StopAndCompact` (reuso); Off-path
guest-side (inalterado); serialização docker-heavy/workflow acme; pré-build/pull.

## 2. Matriz PRD → SPECv4

| PRD | Implementação |
| --- | ------------- |
| RF-1 (≥51 antes de todo batch — **host-only no warm**) | ITEM-1 (gate) + ITEM-2 (`Resolve-AdmitTransition` converge) |
| RF-2 (nunca com job rodando) | ITEM-1 (`Running==0`); testes `r=2,*→mark_busy` |
| RF-3 (fail-safe anti-deadlock) | ITEM-1 (funções puras) + ITEM-2c (prova stateful) |
| RF-4 (precedência: panic→gate→warn) | §0 + ITEM-1 (ordem) + decision-table |
| RF-5 (cache descartado) | invariante |
| RF-6 (observabilidade) | ITEM-2 (`disk_below_floor_admitted` + `would_*`) + ITEM-3 (§8 doc viva) |

## 3. Decisões técnicas

| #    | Decisão | Justificativa |
| ---- | ------- | ------------- |
| DT-1 | Caller no escopo (não "inalterado"). | RF-3/param exigem editá-lo. |
| DT-2 | `start` e `mark_busy` (admissão warm) resetam o contador. | Sem reset, admite sujo p/ sempre. |
| DT-3 | Remover `BoundaryCompactFloorGB` da função **e** do caller (decl/passagem/logs/comentários). | Day-0; senão `ParameterBindingException`. |
| DT-4 | Sem histerese; compacta sempre que batch vai iniciar e `V<51`. | Política ≥51-por-batch. |
| DT-5 | Gate warm **host-only**; Off-path mantém both-sides. | Snapshot guest = 10 min, stale p/ decidir compact (#15). |
| DT-6 | **panic → gate → warn** (§0); ação `boundary_compact` retunada (piso 51), não ação nova. | Política + superfície mínima; reconciliado no PRD. |
| DT-7 | Terminação por hand-off (boundary faz Stop-VM → próximo tick é Off) + cap `attempts<2`; provada por **teste stateful** das funções reais. | H6/M2/F1. |
| DT-8 | **`Resolve-AdmitTransition`** (pura) encapsula *qual decisão* mexe no contador, incl. DT-9; caller **e** teste a CHAMAM. | Fecha o furo #13 (a orquestração, não só a aritmética, é testada — H-1 do v3). |
| DT-9 | `panic_compact` conta tentativa **somente quando `Running==0`** (admissão que começou por panic). | Faz "≤2 compacts/episódio de admissão" valer na faixa `V<18` (F2). |
| DT-10 | `Update-AdmitAttempts`: `vAfter<=0` (não medi) **preserva** o contador (não zera nem incrementa). | Não perde progresso anti-deadlock sob medição flaky (#15) — melhora o inline original que zerava. |
| DT-11 | Evento `disk_below_floor_admitted` em non-`Observe` e `would_*` no `-Observe`. | F3: é o objeto de prova/abort; tem de existir no repro. |
| DT-12 | Deploy = **mecanismo** no `activate`: `Unregister` → copiar 2 `.ps1` → validar por **AST** (`ParseFile` + dot-source só de `decision.ps1`, **sem** rodar o caller) → `Register`; `ExecutionTimeLimit=PT2H` (ambos os registers). | Janela fechada pela **ausência de task entre `Unregister` e `Register`**; PT2H cobre o Optimize (**~8 min medido**; ~13 min é comentário pior-caso do autoreclaim — re-medir no Slice 6). |

## 4. Disciplina Kahneman (etapa crítica — risco operacional)

Fonte: `disciplines/KAHNEMAN-DISCIPLINES.md` — **#13** existência≠função; **#14**
retry calibrado; **#15** fail-safe + curador (não decidir por medição stale);
**#16** idempotência.

- **#13:** `Get-OrchestratorDecision`, `Update-AdmitAttempts` **e**
  `Resolve-AdmitTransition` são as MESMAS funções no deploy e no teste; o caller só
  as **chama** (não duplica a orquestração). O stateful test exercita a cadeia real.
- **#15:** `V<=0`/guest stale → não bloqueia/host-only/preserva contador;
  `attempts>=2` → admite (+ evento), não deadlock.
- **#16:** ciclo multi-tick compacta→admite re-executável (lock + gate de slack).
- **#14:** `attempts` cap 2 por episódio de admissão.
- **#3:** todos os pisos/limites são números medidos (51/28/18; rollback abaixo).
- **Pergunta obrigatória / Evidência / Abort:** ver §10 (Plano) e §11 (Abort).

## 5. `civm-orchestrator-decision.ps1` — ITEM-1

1. **Remover** o param `[int]$BoundaryCompactFloorGB = 40` e o(s) bloco(s)-comentário
   que o explicam (o raciocínio do piso 40 — hoje superseded).
2. **Gate de admissão warm** — inserir **após** `panic_compact` e **antes** de
   `warn_clean`; remover o gate boundary antigo:
   ```powershell
   # ADMISSAO DE BATCH WARM (precede warn; panic <18 ja ganhou acima). VM Running,
   # nenhum job ativo (Running==0) e batch na fila (Queued>0): o batch (PR/re-run) so
   # inicia com V: >= AdmitFloorGB (51). Sujo -> boundary_compact (Stop+Optimize; NAO
   # mata job, Running==0); o proximo tick cai no ramo Off e religa via 'start'. warn
   # NAO recupera o V: do host (so online) -> NAO serve p/ admitir; por isso o gate
   # precede warn (warn fica p/ Running>0). HOST-ONLY (guest snapshot e stale, #15).
   # V<=0 = "nao medi" -> admite (fail-safe). attempts>=2 -> admite mesmo <51
   # (anti-deadlock; Resolve-AdmitTransition conta no caller).
   if ($Running -eq 0 -and $Queued -gt 0) {
       if ($VFreeGB -gt 0 -and $VFreeGB -lt $AdmitFloorGB -and $AdmitReclaimAttempts -lt 2) { return 'boundary_compact' }
       return 'mark_busy'
   }
   ```
3. **Funções puras** (antes de `Get-OrchestratorDecision`):
   ```powershell
   # Aritmetica do contador da barreira. vAfter = V: livre medido APOS o compact.
   # vAfter<=0 (nao medi) -> PRESERVA (nao perde progresso anti-deadlock nem da false
   # give-up; #15). <Floor -> +1. >=Floor (limpo, incl. ==Floor) -> 0.
   function Update-AdmitAttempts {
       param([Parameter(Mandatory)]$State, [int]$VAfter, [int]$Floor = 51)
       if (-not ($State.PSObject.Properties.Name -contains 'admitReclaimAttempts')) {
           $State | Add-Member -NotePropertyName admitReclaimAttempts -NotePropertyValue 0 -Force
       }
       if ($VAfter -le 0) { return $State }
       if ($VAfter -lt $Floor) { $State.admitReclaimAttempts = [int]$State.admitReclaimAttempts + 1 }
       else { $State.admitReclaimAttempts = 0 }
       return $State
   }
   # ORQUESTRACAO PURA decisao -> efeito-no-contador (Kahneman #13: o caller CHAMA
   # esta; o teste exercita a MESMA). A regra DT-9 ("panic conta so com Running==0")
   # vive AQUI, nao no switch. Retorna o $State mutado.
   function Resolve-AdmitTransition {
       param([Parameter(Mandatory)]$State, [Parameter(Mandatory)][string]$Decision,
             [int]$Running, [int]$Queued, [int]$VAfter, [int]$Floor = 51)
       switch ($Decision) {
           'reclaim_before_admit' { return (Update-AdmitAttempts -State $State -VAfter $VAfter -Floor $Floor) }
           'boundary_compact'     { return (Update-AdmitAttempts -State $State -VAfter $VAfter -Floor $Floor) }
           'panic_compact'        { if ($Running -eq 0) { return (Update-AdmitAttempts -State $State -VAfter $VAfter -Floor $Floor) }; return $State }
           'start'                { $State.admitReclaimAttempts = 0; return $State }
           'mark_busy'            { if ($Running -eq 0 -and $Queued -gt 0) { $State.admitReclaimAttempts = 0 }; return $State }
           default                { return $State }
       }
   }
   ```

## 6. `civm-vm-orchestrator.ps1` (caller) — ITEM-2

1. **Remover** o param `BoundaryCompactFloorGB` (decl + comentário) e a passagem
   `-BoundaryCompactFloorGB ...`; trocar os logs do arm `boundary_compact`
   (`floor=$BoundaryCompactFloorGB`) por `$AdmitFloorGB`.
2. **Efeito nos arms** (inalterado o efeito; **remover** a aritmética inline de
   contador do `reclaim_before_admit`, pois agora é delegada): cada arm de compact
   (`reclaim_before_admit`, `boundary_compact`, `panic_compact`) faz só o
   `Invoke-StopAndCompact` (+ logs); o `panic_compact` mantém o `lastPanicUtc`
   pré-compact (double-Save **intencional** — cooldown persistido antes do compact
   longo; não "otimizar").
3. **Evento de admissão suja** nos arms `start` e `mark_busy` — emitir **antes** do
   split `if ($Observe)/else` (espelhando-se), quando `$running -eq 0 -and $queued -gt
   0 -and $vfree -gt 0 -and $vfree -lt $AdmitFloorGB`:
   ```powershell
   $evt = if ($Observe) { 'would_disk_below_floor_admitted' } else { 'disk_below_floor_admitted' }
   Write-OrcLog $evt @{ v_free_gb = $vfree; guest_free_gb = $guestFree; floor = $AdmitFloorGB; attempts = [int]$state.admitReclaimAttempts; path = $(if ($vm.State -eq 'Off') { 'cold' } else { 'warm' }) }
   ```
   (`start` mantém `Start-VM`+`lastBusyUtc` no `mark_busy`; **remover** o reset inline
   `admitReclaimAttempts=0` dos arms — o reset passa a ser do `Resolve-AdmitTransition`.)
4. **Transição do contador (uma chamada, pós-switch)** — após o `switch`, medir o
   `V:` e delegar à função pura, então persistir:
   ```powershell
   $vAfter = Get-VFreeGB
   $state = Resolve-AdmitTransition -State $state -Decision $decision -Running $running -Queued $queued -VAfter $vAfter -Floor $AdmitFloorGB
   if (-not $Observe) { Save-State $state }
   ```
   (Para decisões sem compact/admissão, `Resolve-AdmitTransition` é no-op no contador.)

## 7. `civm-orchestrator-decision.test.ps1` — ITEM-2b/2c

- **Decision-table (gate-antes-de-warn; ordem §0):**
  - **Alterados (3):** `q=3,r=0,vfree=45→boundary_compact`; `q=1,r=0,vfree=40→boundary_compact`;
    `q=1,r=0,vfree=25→boundary_compact`. Atualizar comentários L28-41 p/ a nova ordem.
  - **Inalterado crítico:** `q=1,r=0,vfree=15,cp=T→panic_compact` (panic precede o gate).
  - **Novos (6):** `vfree=55→mark_busy`; `vfree=45,ra=2→mark_busy`;
    `vfree=45,ra=1→boundary_compact`; `r=2,vfree=45→mark_busy` (RF-2);
    `vfree=15,cp=F→boundary_compact`; `vfree=0→mark_busy` (#15). _(o `vfree=51→mark_busy`
    está nas Bordas — não duplicar.)_
  - **Bordas (5):** `r=0,q=1,vfree=50→boundary_compact` e `vfree=51→mark_busy`
    (borda de admissão); `r=2,vfree=27→warn_clean` e `r=2,vfree=28→mark_busy` (borda
    warn só com `Running>0`; o `vfree=28` confirma o existente); `r=0,q=1,vfree=28→
    boundary_compact` (no gap, 28 já é gate, não warn).
  - **Contagem = a que o `*.test.ps1` AUTO-REPORTA** (`RESULTADO: N PASS`, ~49 após o
    dedup) — reconciliar §9/§13/§14 e a doc viva a esse `N`, não hard-codar um número.
- **Unitário das funções puras:** `Update-AdmitAttempts VAfter=48→+1`; `=55→0`;
  `=51→0`; `=0→preserva` (DT-10). `Resolve-AdmitTransition`: `panic_compact,Running=0,
  VAfter=15→+1`; `panic_compact,Running=2→inalterado`; `mark_busy,Running=0,Queued=1→0`;
  `mark_busy,Running=2→inalterado`; `boundary_compact→+1`.
- **Stateful (ITEM-2c) — funções REAIS (DT-7/DT-8):** dot-source `decision.ps1`; loop
  usando `Get-OrchestratorDecision` + `Resolve-AdmitTransition` (mock só do I/O:
  `vAfter` injetado `<51`, sem chamar `Invoke-StopAndCompact`; alterna `VmState`):
  - **[18,51):** warm `V=45` → `boundary_compact`(→1,Off) → `reclaim_before_admit`(→2)
    → `start`. **Assert: 2 compacts; reset no `start`.**
  - **`V<18`,cp=T (DT-9):** warm `V=15` → `panic_compact`(Running==0→1,Off) →
    `reclaim_before_admit`(→2) → `start`. **Assert: 2 compacts.**
  - Constrói o estado via `[pscustomobject]@{ admitReclaimAttempts = 0; lastBusyUtc=...;
    lastPanicUtc=... }` (StrictMode-safe; L-3).

## 8. `activate-orchestrator.ps1` — ITEM-2d (deploy = mecanismo; DT-12/H-2)

**Estado real (verificar):** `grep -c Copy-Item activate-orchestrator.ps1` = **0** hoje
— o `activate` só faz `Unregister-ScheduledTask` (L3) → `Register-ScheduledTask` (L14)
+ desabilita legados (L19-20); a cópia dos `.ps1` para `C:\civm-deploy` é
externa/manual. Este ITEM **adiciona** (não "substitui") a cópia + validação ao
`activate`, **entre o Unregister e o Register** — sem `Disable/Enable`.

A janela é fechada pela **ausência de task entre `Unregister` e `Register`**: nenhum
tick novo inicia; um tick em voo que já passou do dot-source (caller L280) termina com o
par antigo. **Resíduo aceito (1 tick, sem efeito):** um tick lançado microssegundos
antes do `Unregister` e ainda **antes** do L280 pode emparelhar caller-antigo +
`decision.ps1`-novo → `ParameterBindingException` no L310 — mas isso ocorre **antes** de
qualquer efeito de power/compact (fail-safe #15: VM intacta) e o próximo tick (par novo)
roda limpo; janela de sub-segundo, 1 tick perdido. (O `reclaim.lock` abaixo cobre só
compact-em-curso, não esta janela de versão-mista.) Sequência determinística:
```powershell
if (Test-Path 'V:\civm-reclaim.lock') { throw 'reclaim em curso; abortar deploy' }
Unregister-ScheduledTask -TaskName 'civm-vm-orchestrator' -Confirm:$false   # (L3 atual) para o agendamento
Copy-Item $src\civm-orchestrator-decision.ps1 C:\civm-deploy\ -Force        # ADICAO
Copy-Item $src\civm-vm-orchestrator.ps1       C:\civm-deploy\ -Force        # ADICAO
# VALIDACAO POR PARSE (AST) — NAO executar o caller (ele nao tem guarda; rodar -Observe
# faria um TICK ao vivo: Get-VM/Get-RunCount+tokens/host-metrics, L285-298, e abortaria
# o deploy em falha transitoria).
$e=$null; [System.Management.Automation.Language.Parser]::ParseFile('C:\civm-deploy\civm-vm-orchestrator.ps1',[ref]$null,[ref]$e); if($e){ throw 'parse error no caller' }
. C:\civm-deploy\civm-orchestrator-decision.ps1   # dot-source so as FUNCOES (valida-as; este arquivo nao tem bloco principal)
Register-ScheduledTask ... -Settings $st          # (L14 atual) com ExecutionTimeLimit=PT2H
```
- **Validação = parse, não execução (H-A):** só AST do caller + dot-source das funções
  de `decision.ps1`; **não** rodar `civm-vm-orchestrator.ps1 -Observe` no deploy.
- **`ExecutionTimeLimit=PT2H`** em `activate-orchestrator.ps1` (L13, hoje 30 min) **e**
  `register-orchestrator.ps1` (L11, hoje 30 min), espelhando
  `register-civm-vhdx-autoreclaim.ps1` **L94-103** (`$st.Settings.ExecutionTimeLimit='PT2H'`).
  Trade-off (documentado lá): o Optimize é o passo longo; PT2H dá margem; se um tick
  pendurar, `IgnoreNew` engole ticks (sem backstop de 30 min — aceito como o
  autoreclaim) e o `CompactVirtualDisk` segue nativo (VHDX não corrompe).
- **Duração do Optimize — número a MEDIR no Slice 6 (M-α):** a medição ao vivo
  (`validation.md`/doc viva §10) registra **~8 min** (só Optimize); o "~13 min" do
  autoreclaim é **comentário não medido** (pior-caso `Stop-VM`+full-clean+Optimize).
  Usar o **medido** no Slice 6; não citar "13 bate com autoreclaim" como evidência.

## 9. `orchestrator-scale-to-zero/SPEC.md` (doc viva) — ITEM-3 (mesmo commit)

Reconciliar **TODAS** as âncoras (prosa + **número de linha** que deslocam ao inserir
as 2 funções + o gate):

- **Precedência:** §2 prosa "Ordem de precedência" → **panic → gate de admissão warm
  → warn**; §4 "Banda efetiva" `28..40 → 18..51`; §4 "Escolha do piso 40" → superseded;
  §4 subseção "boundary_compact — o gap" + "Caso back-to-back" + "Composição #137".
- **Ações/RF/decisão (âncoras de linha — `decision.ps1:NN` E `orchestrator.ps1:NN`):**
  §2 "Decisão pura" (`linhas 6-48`, `switch ...:284-325`); §2 tabela "Ações possíveis";
  §3 RF-table (`decision.ps1:NN`); §4.2 (casos do teste); §6/§8 (`decision.ps1:NN`).
  **Atenção (H-C): a doc viva tem ~52 refs `orchestrator.ps1:NN`** (do caller) que
  **deslocam** ao remover o param/inline e inserir a chamada pós-switch — re-derivar
  TODAS, não só as de `decision.ps1`. (Robusto: migrar refs de linha → âncoras
  simbólicas de função/arm para eliminar a fragilidade na raiz.)
- **Eventos §8:** **adicionar** `disk_below_floor_admitted | admite batch com V<51
  (warm/cold) | v_free_gb, guest_free_gb, floor, attempts, path | start/mark_busy`;
  atualizar `disk_boundary_compact` (`floor→AdmitFloorGB`).
- **Rollback §7 (H-4):** **somar** os 2 novos sinais (`disk_boundary_compact/h`,
  `disk_below_floor_admitted`) ao §7 (não criar tabela divergente).
- **Contagem (M-3):** o total é o que o `*.test.ps1` **auto-reporta** após a edição
  (`RESULTADO: N PASS`; ~49 decision-table = 38 atuais — 3 reescritos in-place — + 6 novos
  + 5 bordas, dedup do `vfree=51`) **+ 9 unitários** (`Update-AdmitAttempts` 4 +
  `Resolve-AdmitTransition` 5) **+ 2 stateful**. Reconciliar **§9 (≈L430 "23"), ≈L473
  ("hoje 23") e §11 (≈L487 "38")** a esse `N` — não hard-codar.
- **Checklist de varredura:** `grep -nE "BoundaryCompactFloorGB|28\.\.40|piso 40|reclaim_before_batch|decision\.ps1:[0-9]|orchestrator\.ps1:[0-9]|linhas 6-48|284-325"`
  na doc viva → cada hit deve apontar para a linha **correta** pós-edição (re-derivada),
  **0** stale.

## 10. Throughput, abort e rollback (números)

- **Custo:** ~11 GB/PR; Optimize **~8 min medido** (`validation.md`/doc viva §10; o
  "~13 min" do autoreclaim é comentário/pior-caso `Stop`+clean+Optimize — **re-medir no
  Slice 6**). `V` cai `<51` após ~todo PR → **~1 compact/batch saudável; até 2/batch no
  irrecuperável** (2 reclaims + admite-sujo).
- **Perda de ticks:** o compact é síncrono (~8–13 min); com repetição 2 min +
  `MultipleInstances=IgnoreNew`, ~4–6 ticks são engolidos; o tick `start` vem ~8–13 min
  após o **início** do compact (não 2 min).
- **Histerese rejeitada** (violaria ≥51-por-batch).
- **Rollback (valores iniciais — Slice 0; revisáveis após N batches medidos):**
  reverter se **`disk_boundary_compact/h > 4` por ≥2 h** (compactação dominante)
  **ou** **≥3 `disk_below_floor_admitted` com `V<51` em 1 h** (irrecuperável).
- **Abort triggers (todos observáveis no log):**
  1. **Deadlock:** batch não admitido por **>75 min** (pior caso anti-deadlock = 2
     compacts ~16–26 min + 2 cold-starts + esperas — folga ampla).
  2. **Admissão suja:** `disk_below_floor_admitted` com `V<51` recorrente (≥3/1 h).
  3. **Panic (job morto) — viola a invariante §0.1:** **qualquer** `disk_panic` com
     `running>0` é alarme (esperado: **zero**); **`≥2` em <30 min** (número da doc viva
     §8, M-1) é **rollback duro** + investigar a premissa de clean-start.
  4. **Inválido:** qualquer `disk_boundary_compact` com `running>0` (nunca deve ocorrer).
- **Skip conta (F16):** um skip do `Invoke-StopAndCompact` (lock/slack) deixa
  `vAfter<51` → conta como tentativa (por design; lock persistente → admite em ~2 ticks).

## 11. Fronteira de atomicidade, rollback de app e race

- **Atômico:** decisão + `Update/Resolve-AdmitTransition` sem efeito; cada
  `Invoke-StopAndCompact` atômico (lock). Ciclo compacta→admite **idempotente** (#16).
- **Rollback de app (unidade de 4 arquivos):** reverter `decision.ps1` (gate@40 +
  param + remover as 2 funções), `caller` (restaurar inline + param + logs),
  `*.test.ps1` e `activate` (deploy/ExecutionTimeLimit) **juntos**. Revert parcial =
  `ParameterBindingException`. Aplicar sem reclaim em curso. **migration/dados:** N/A.
  **forward-only:** N/A.
- **Race:** `MultipleInstances=IgnoreNew` serializa os ticks em **runtime**; o lock
  `V:\civm-reclaim.lock` é defesa-em-profundidade. **Ressalva (L-6):** `IgnoreNew`
  **não** protege a janela de **deploy** (cópia dos 2 `.ps1`) — essa é fechada pela
  **ausência de task entre `Unregister` e `Register`** (§8): nenhum tick novo inicia e
  um tick em voo já leu o par antigo. Não por `IgnoreNew`/lock.

## 12. Ordem de implementação

1. ITEM-1 (`decision.ps1`: param/comentários, gate, `Update-/Resolve-AdmitTransition`).
2. ITEM-2 (caller: param/comentários, efeito+evento+`would_*`, chamada única de
   `Resolve-AdmitTransition` pós-switch).
3. ITEM-2b/2c (`*.test.ps1`: decision-table + bordas + unitário + stateful).
4. `pwsh *.test.ps1` na box → `0 FAIL`; unitário + stateful (incl. `V<18`) PASS.
5. ITEM-3 (doc viva — todas as âncoras + §7 + §8 + contagem) no mesmo commit.
6. ITEM-2d (deploy mecanismo + `ExecutionTimeLimit`); aplicar sem reclaim em curso;
   medir o Optimge ao vivo; capturar os 3 cenários; `civm/validation.md`.

## 13. Plano de testes

- **Decision-table (pwsh, na box):** `0 FAIL` — a contagem que o test **auto-reporta**
  (~49: 38, dos quais 3 reescritos in-place, + 6 novos + 5 bordas).
- **Unitário (pwsh):** `Update-AdmitAttempts` (incl. `vAfter=0` preserva) +
  `Resolve-AdmitTransition` (incl. DT-9 `panic` só `Running==0`).
- **Stateful (pwsh, funções reais):** convergência ≤2 compacts/episódio nos 2
  cenários ([18,51) e `V<18`), reset na admissão.
- **Ao vivo (separar repro de prova):** `-Observe`+`state.json`/`host-metrics.json`
  forjados (1 tick/cenário) → `would_boundary_compact`/`would_disk_below_floor_admitted`/
  `mark_busy`; convergência pelo stateful; **1** captura non-`Observe` controlada do
  `disk_below_floor_admitted` real — **gate de aceitação obrigatório** (o rollback §10 e
  o abort #2 dependem do evento; sem a captura ele é inverificável). Abort se algum
  cenário não reproduzir.

## 14. Checklist de validação

- [ ] `pwsh civm-orchestrator-decision.test.ps1` → `0 FAIL` (PS 5.1); unitário +
      stateful (incl. `V<18`) PASS.
- [ ] `grep -n "BoundaryCompactFloorGB"` = **0** em **ambos** os `.ps1` (código+comentário).
- [ ] `grep -nE "28\.\.40|piso 40|reclaim_before_batch|decision\.ps1:[0-9]|orchestrator\.ps1:[0-9]|linhas 6-48|284-325"`
      na doc viva → só refs corretas (re-derivadas), 0 stale; contagem consistente em
      §9/§11/§13 e na doc viva (= a contagem auto-reportada do test + 9 unitários + 2 stateful).
- [ ] `Running>0` nunca produz `boundary_compact` nem incrementa o contador (testes `r=2`).
- [ ] `disk_below_floor_admitted` na §8 (event-table) da doc viva; §7 Rollback reconciliado
      (somou os 2 sinais; sem tabela divergente).
- [ ] `grep -c Copy-Item activate-orchestrator.ps1` = **2** (antes: 0); deploy =
      `Unregister`→copiar 2→**validar por AST** (não rodar `-Observe`)→`Register`, sem
      reclaim em curso; `ExecutionTimeLimit=PT2H` em `activate` (L13) e
      `register-orchestrator` (L11).
- [ ] **Gate obrigatório:** captura non-`Observe` do `disk_below_floor_admitted` real +
      os 3 cenários; `civm/validation.md` (`orchestrator-decision`+`disk-reclaim`, dados
      medidos: **Optimize real**, V antes/depois).
- [ ] Números Kahneman conferidos contra `disciplines/KAHNEMAN-DISCIPLINES.md`.
