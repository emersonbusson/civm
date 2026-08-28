---
slug: host-volume-reclamation
title: Reclamação de volume do host (VHDX) — guest-free vira host-free com segurança
milestone: —
issues: []
---

# SPECv3 — Resiliência do reclaim: quebrar o deadlock de headroom com admissão por folga provada

> **SUPERSEDED-BY (2026-06-17): orchestrator scale-to-zero.** O reclaim do VHDX
> agora pertence ao `civm-vm-orchestrator.ps1` (único dono do stop/compact/
> power-state; tasks `autoreclaim`/`optimize`/`optimize-watchdog` `Disabled`). O
> gate de 2 fases provado aqui foi portado para `civm-reclaim-gate.ps1`, reusado
> pelo orchestrator. Fonte de verdade viva:
> `docs/specs/orchestrator-scale-to-zero/`. Preservado como histórico — não o
> reimplemente.

> Versão melhorada após DUAS rodadas do Passo 2.5 sobre `SPECv2.md`.
> Baseline preservado: `SPEC.md`; camada anterior: `SPECv2.md`.
> A 1ª versão deste v3 (admitir Optimize abaixo de 8 GB e abortar via `Stop-Job`
> ao cruzar 1 GB) levou **no-go** no red-team com 2 CRÍTICOS — esta versão
> incorpora as correções in-place. Onde houver conflito, **o v3 prevalece**.
>
> Por que o `no-go` da rodada anterior (registrado para não reincidir):
> 1. **CRÍTICO — evidência N=1 não mediu scratch.** Nenhum script faz poll de
>    `V:` durante o `Optimize-VHD` hoje; "não cruzou 5,69 GB" era inferência de
>    "não deu erro", não medição. E `Optimize-VHD -Mode Full` NÃO é provadamente
>    monotônico — pode crescer temporariamente além da folga e zerar o `V:`.
> 2. **CRÍTICO — `Stop-Job` não aborta `Optimize-VHD`.** A compactação nativa
>    (`virtdisk` `CompactVirtualDisk`) roda fora do processo PowerShell; `Stop-Job`
>    abandona o job mas a operação segue escrevendo `V:` e deixa o VHDX locked/
>    inconsistente. Logo: **o Optimize é ininterruptível** — não há abort seguro
>    no meio.
> 3. HIGH — poll de 5 s é instrumento errado (com a VM Off não há outro
>    escritor; e 5 s não pega um spike de scratch antes de cruzar 1 GB).
> 4. HIGH — os dois reclaimers usam locks SEPARADOS e não se excluem; o fix do
>    watchdog (#98) não os torna mutuamente exclusivos.
> 5. HIGH — `exit 2` rígido a 3 GB só REALOCA o deadlock para um piso menor.

## Princípio central (a virada do design)

`Optimize-VHD -Mode Full` é **ininterruptível e de consumo de scratch
desconhecido a priori**. A única alavanca segura não é abortar no meio — é
**recusar começar** quando a folga não cobre, comprovadamente, o pior scratch já
medido. Trocamos "abortar tarde" por "admitir só com folga provada".

---

## Resolução dos blockers do Passo 2.5 (decisões fechadas)

| # | Blocker (severidade) | Decisão vinculante |
| --- | --- | --- |
| DT-v3-1 | **CRÍTICO — admission gate em vez de piso fixo** (fecha Findings 1+2) | O caminho de emergência (`V: < HeadroomGB`) é admitido SOMENTE quando `liveVFree − HardFloorGB >= ScratchBudgetGB`, onde `ScratchBudgetGB` é o **pior scratch high-water medido** (DT-v3-2) + margem. O Optimize roda como hoje (síncrono/`Start-Job`), mas **nunca é abortado no meio** (é ininterruptível). Sem `Stop-Job`. Se a folga não cobre o budget → `abort_insufficient_slack` exit 2 (não tenta). `liveVFree` é SEMPRE `Get-PSDrive` ao vivo, nunca o JSON de 10 min. |
| DT-v3-2 | **CRÍTICO — campanha de medição ANTES de baixar o piso** (fecha Finding 1; inverte o antigo DT-v3-5) | Instrumentar o caminho NORMAL/supervisionado (`civm-vhdx-optimize.ps1`) com poll de `V:` ao vivo **a cada 1 s** durante o Optimize, gravando `scratch_high_water_gb = max(VFreeBefore − VFreeDuring)`. Rodar `>= 5` ciclos supervisionados. `ScratchBudgetGB = ceil(p100 das 5 medições) + 1 GB` de margem. **Até existir essa medição, o gate de emergência fica DESABILITrADO e o headroom de 8 GB vale (sem regressão, sem realocar o deadlock).** A constante de emergência é commit explícito com as 5 medições anexadas (Número, não adjetivo). |
| DT-v3-3 | **HIGH — exclusão mútua entre os dois reclaimers** (fecha Finding 5) | Lock canônico ÚNICO `V:\civm-reclaim.lock` adquirido por AMBOS `civm-vhdx-optimize.ps1` e `civm-vhdx-autoreclaim.ps1` (FileShare::None) ANTES de qualquer `Stop-VM`; quem não obtém → log `reclaim_skip_other_active` exit 0. Os locks por-script atuais (`civm-optimize.lock`, `civm-autoreclaim.lock`) permanecem para anti-reentrância da própria task; o watchdog (#98) passa a checar os TRÊS. Nenhum `Stop-VM`/`Optimize` concorrente. |
| DT-v3-4 | **HIGH — cadência por pressão não pode multiplicar Stop-VM** (fecha Finding 4) | A cadência curta (10 min sob `V: < 25 GB`) é APENAS detecção/refresh de métrica. O ato disruptivo (`Stop-VM`+Optimize) é rate-limited a **>= 30 min entre eventos reais** (marcador `V:\civm-reclaim-last.txt`) e exige `idle-check` passar N=2 vezes consecutivas. Pré-condição: corrigir o preditor de idle que o rollback trigger das tasks já nomeia. |
| DT-v3-5 | **HIGH — poll é telemetria, nunca controle de segurança** (fecha Finding 3) | O poll de 1 s de DT-v3-2 NÃO aborta nada; só registra `scratch_high_water_gb` para recalibrar `ScratchBudgetGB`. A segurança é 100% o gate de admissão pré-flight (DT-v3-1). |
| DT-v3-6 | **HIGH — override explícito de DT-v2-19/DT-v2-24** (fecha Finding 6) | (a) DT-v2-19 (rollback se low-water `<=8 GB`) é **explicitamente substituído**: o low-water deixa de ser gatilho de rollback e passa a ser a ENTRADA do `ScratchBudgetGB`; rollback agora dispara se um run cruzar o piso DURO (`HardFloorGB`) — o que o gate de admissão deve tornar impossível. (b) DT-v2-24: o comentário "folga p/ crescimento temporário do VHDX" é CORRIGIDO para reconhecer que esse crescimento EXISTE e agora é **medido** (`ScratchBudgetGB`), não assumido em 8 GB. |
| DT-v3-7 | **HIGH — footprint do guest infla o high-water-mark** (era DT-v3-3; slice própria) | Hook `civmctl cache-gc` (guest): orçamento LRU por categoria (`yarn`, `go-build`) com teto; evict além do teto preservando o cache mais recente e checando lock de job ativo antes do evict. Reduz o high-water do VHDX → host mantém margem em pico. Depende de DT-v3-1..6 entregues. |

---

## Constantes (override de DT-v2-24)

```go
// DefaultHostVolumeHeadroomGB: minimo de V: livre ANTES do Optimize no caminho
// NORMAL/agendado. Abaixo disso so o caminho de EMERGENCIA (DT-v3-1) pode rodar,
// e SOMENTE se a folga cobrir o ScratchBudget medido.
DefaultHostVolumeHeadroomGB = 8
// DefaultHostVolumeHardFloorGB: piso DURO absoluto. O gate de admissao garante
// que o Optimize so comeca se liveVFree - HardFloor >= ScratchBudget. NUNCA
// operar abaixo disso. (Optimize-VHD e ininterruptivel: nao ha abort no meio.)
DefaultHostVolumeHardFloorGB = 1
// DefaultHostVolumeScratchBudgetGB: pior scratch high-water MEDIDO (DT-v3-2) +1.
// ZERO ate a campanha de >=5 medicoes existir; com 0, o gate de emergencia fica
// desabilitado e o headroom de 8 GB vale (sem regressao).
DefaultHostVolumeScratchBudgetGB = 0
// DefaultAutoreclaimPressureGB: abaixo disso o registrar adiciona o gatilho de
// 10 min de DETECCAO (nao de acao; acao e rate-limited por DT-v3-4).
DefaultAutoreclaimPressureGB = 25
// DefaultReclaimMinIntervalMin: minimo entre eventos reais de Stop-VM+Optimize.
DefaultReclaimMinIntervalMin = 30
```

Invariante: `HardFloor(1) < Headroom(8) < Pressure(25)`; `ScratchBudget >= 0`;
e o gate de emergência só habilita quando `ScratchBudget > 0`.

## Override do `civm-vhdx-autoreclaim.ps1` (DT-v3-1/3/4) — esqueleto vinculante

```powershell
# Lock canonico compartilhado ANTES de tudo (DT-v3-3):
#   abrir V:\civm-reclaim.lock FileShare::None; se falhar -> reclaim_skip_other_active; exit 0
# Rate-limit (DT-v3-4):
#   if (now - lastReclaim < MinIntervalMin) { reclaim_skip_ratelimited; exit 0 }
# Guards de threshold/gap/idle inalterados; idle-check exigido N=2x.
#   $live = Get-VFreeGB                                  # SEMPRE ao vivo (Get-PSDrive)
#   if ($live -ge ThresholdGB) { skip_threshold; exit 0 }
#   if ($live -lt HeadroomGB) {
#       if (ScratchBudgetGB -le 0) { abort_headroom (gate disabled); exit 2 }   # sem medicao -> sem emergencia
#       if ($live - HardFloorGB -lt ScratchBudgetGB) { abort_insufficient_slack; exit 2 }
#       $emergency = $true
#   }
#   ... fstrim; Stop-VM; wait Off; Mount-VHD -ReadOnly ...
#   if ($emergency) { emergency_reclaim_start } else { autoreclaim_start }
#   Optimize-VHD -Path $VhdxPath -Mode Full -ErrorAction Stop   # ININTERRUPTIVEL: sem Stop-Job
#   ... dismount; finally Start-VM (3x); liberar locks; gravar civm-reclaim-last.txt ...
```

## Instrumentação de medição (DT-v3-2) — em `civm-vhdx-optimize.ps1`

```powershell
# Optimize ja roda em Start-Job (linhas 350-353). Adicionar, no Wait-Job loop,
# um sample de Get-PSDrive a cada 1s registrando o minimo de V: livre:
#   $lowWater = $vFreeBefore
#   while (-not (Wait-Job $optJob -Timeout 1)) {
#       $now = (Get-PSDrive V).Free / 1GB
#       if ($now -lt $lowWater) { $lowWater = $now }
#   }
#   $scratchHighWater = $vFreeBefore - $lowWater
#   Write-CivmLog optimize_end (... scratch_high_water_gb = $scratchHighWater)
# Coletar 5 runs supervisionados; ScratchBudget = ceil(max) + 1.
```

---

## Mapa Kahneman v3

| Etapa / DT | Disciplina | Pergunta obrigatória | Evidência mínima | Abort trigger |
| --- | --- | --- | --- | --- |
| **DT-v3-1 (admission gate)** | #5 Availability | O Optimize pode estourar `V:`? | gate pré-flight: `liveVFree − HardFloor >= ScratchBudget`; Optimize ininterruptível | folga não cobrir o ScratchBudget medido → recusa começar |
| **DT-v3-2 (medição)** | #3 Número não adjetivo | Quanto scratch o Full realmente usa? | poll de 1 s, 5 runs, `scratch_high_water_gb` logado | baixar `ScratchBudget` sem 5 medições; usar JSON de 10 min em vez de live |
| **DT-v3-3 (exclusão)** | #5 Availability | Dois reclaimers podem colidir no VHDX? | lock canônico único antes de `Stop-VM`; watchdog checa os 3 locks | qualquer `Stop-VM`/`Optimize` concorrente |
| **DT-v3-4 (cadência)** | #2 Counterfactual | A cadência curta multiplica `Stop-VM`? | detecção 10 min, ação >= 30 min, idle N=2x | evento real de `Stop-VM` < 30 min do anterior |
| **DT-v3-6 (override)** | #2 Counterfactual | O low-water ainda é gatilho de rollback? | DT-v2-19 substituído: low-water vira entrada do budget; rollback = cruzar HardFloor | run cruzar `HardFloorGB` (deve ser impossível pelo gate) |

---

## Plano de testes v3

- **`internal/civm` (unit):** as constantes existem; `HardFloor < Headroom < Pressure`; `ScratchBudget >= 0`; helper de admissão `EmergencyAdmits(liveFree, hardFloor, budget) bool` testado (folga insuficiente, budget=0 desabilita, folga exata).
- **`internal/hostdisk` (lint host, padrão do repo):** `civm-vhdx-autoreclaim.ps1` NÃO contém `Stop-Job` no caminho do Optimize; contém o lock canônico `civm-reclaim.lock` e o gate `abort_insufficient_slack`; `civm-vhdx-optimize.ps1` contém o sample `scratch_high_water_gb`. (Mesmo padrão do lint `[math]::Max` e do watchdog.)
- **Host (janela supervisionada):** rodar os 5 ciclos de medição; verificar que `V: < 8` com `ScratchBudget=0` ainda aborta (sem regressão); depois de setado o budget, `V:` entre `HardFloor+Budget` e `8` admite e completa; abaixo disso recusa; dois reclaimers simultâneos → o segundo `reclaim_skip_other_active`.

## Checklist de validação v3

- [ ] `go test ./... -race -count=1`
- [ ] Ordem das constantes + `EmergencyAdmits` unit
- [ ] Lint host: ausência de `Stop-Job` no Optimize; lock canônico; sample de scratch
- [ ] Janela: 5 medições coletadas e `ScratchBudgetGB` definido por commit com evidência
- [ ] `npm run docs:index` (SPECv3 no índice)
- [ ] Sync rule: README/AGENTS/CODEX/rules no mesmo commit se a constante/contrato mudar

## Veredito

1ª rodada do v3: **no-go** (2 CRÍTICOS: evidência não-medida + `Stop-Job` não
aborta). Esta revisão fecha com **`go` condicional**, com ordem de fatias
OBRIGATÓRIA:

1. **DT-v3-2 (medição) PRIMEIRO** — instrumentar o caminho normal, coletar 5
   runs, definir `ScratchBudgetGB`. Sem isso o gate de emergência fica
   desabilitado (8 GB vale; zero regressão).
2. **DT-v3-3 (lock canônico)** — exclusão mútua dos dois reclaimers.
3. **DT-v3-1 (admission gate)** — habilita a emergência só com budget medido.
4. **DT-v3-4 (cadência rate-limited)** e **DT-v3-7 (cache-gc)** — redução de
   frequência/footprint, slices próprias.

A emergência só opera abaixo de 8 GB quando a folga ao vivo cobre,
comprovadamente, o pior scratch medido — nunca por um piso adivinhado, nunca
abortando um Optimize ininterruptível no meio.
