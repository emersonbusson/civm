# SPEC — orchestrator scale-to-zero da VM do runner

Issue: acme/civm#135 (base) + #18 (validation log) + fix `fix/orchestrator-disk-panic`
Status: implementado e ativo na box (2026-06-17)
Precedente: `docs/specs/host-volume-reclamation/SPEC.md` (gate de 2 fases, issue #106),
`docs/specs/host-volume-reclaim-liveness/SPEC.md`, `docs/specs/civm-disk-watchdog-busy-cleanup/SPEC.md`

> **Nota de reconciliação (implementação atual).** O piso `AdmitFloorGB` foi elevado de **51→55** no código vivo (`decision.ps1`/`reclaim-gate.ps1`/`vm-orchestrator.ps1`); onde este doc cita "51" como piso de admissão, o valor vivo é **55** (guest floor 40; host warn/panic 28/18; `MinFreeGB`=58 por job). O **RF-10** (disco limpo por batch) foi **fechado** por [`civm-disk-gate-per-batch/SPECv5.md`](../civm-disk-gate-per-batch/SPECv5.md) — gate POR-EVENTO (`running>0→0`).

> **Por que este SPEC existe.** Uma scheduled task SYSTEM (`civm-vm-orchestrator`,
> a cada 2 min) tem poder de **desligar a VM com `Stop-VM -Force` e compactar o
> VHDX offline MESMO com jobs de CI rodando** (caminho `panic_compact`). É uma
> operação destrutiva-por-design — mata workers que re-rodam. O audit de docs
> apontou como gap CRÍTICO: existia código sem spec nem runbook. Este documento é
> a fonte de verdade do contrato (quando age, quando se recusa, como reverter).

---

## 1. Contexto

A box do CI roda 7 runners self-hosted numa VM Hyper-V pesada (`gha-ubuntu-2404`)
no volume `V:`. O problema medido é footprint contínuo entre rajadas:

- Com a VM ligada e ociosa, o Hyper-V retém toda a RAM e o VHDX dinâmico só
  **cresce** — nunca encolhe sozinho. O VMRS (saved-state, ~8 GB) e o scratch do
  `Optimize-VHD` ocupam `V:` permanentemente.
- O `civm-vhdx-autoreclaim` legado tentava compactar no idle, mas o **idle
  predicate estava documentadamente quebrado** (o próprio header do script
  recomendava desabilitá-lo) e ele era enganado por **runs fantasmas** do índice
  stale do GitHub (ver §10, item ghost-queued). Resultado empírico: `V:` travado
  em 31 GB por 10 min com a box idle, sem compactar (`validation.md`,
  2026-06-17 06:15).

A decisão é mover o housekeeping para a camada certa: **scale-to-zero**. A VM só
existe quando há trabalho.

## 2. Decisão / Design

### Visão geral

Uma tarefa minúscula sempre-ligada no host (Scheduled Task, ~2 min,
`civm-vm-orchestrator.ps1:1-33`) decide a cada tick com base no estado observado
(power-state da VM + fila do GitHub + `V:` livre):

- **VM Off + existe run `queued` ou `in_progress`** num dos repos vigiados →
  `Start-VM`. Os runners sobem no boot e pegam os jobs
  (`civm-orchestrator-decision.ps1:25-27`).
- **VM Running + nada na fila + nada rodando + ocioso ≥ `IdleStopMinutes`** →
  limpeza total do guest, `Stop-VM`, `Optimize-VHD` (compacta). A VM fica Off
  (`civm-orchestrator-decision.ps1:41-47`, `Invoke-StopAndCompact`
  `civm-vm-orchestrator.ps1:206-255`).

**Ganho:** footprint zero entre rajadas (RAM devolvida ao Windows, VHDX
compactado). **Custo:** cold-start de ~1-2 min na primeira rajada (boot + runners
conectando) — aceitável para CI (`civm-vm-orchestrator.ps1:14-18`). Medido:
`V:` 31 → 51 GB no primeiro ciclo idle ativo (`validation.md`, 2026-06-17 09:59).

### Política definitiva — disco limpo (≥51 GB livre) por PR e por re-run

Regra canônica do fluxo de checks (decisão do usuário, 2026-06-21):

1. **Um PR só COMEÇA os checks com `V:` ≥ `AdmitFloorGB` (51 GB) livre.** Abaixo
   disso, antes de admitir o batch, o orchestrator faz o ciclo completo de
   recuperação: limpa TODOS os caches do guest (`docker system prune -af
   --volumes` + `docker builder prune -af` + `~/.cache/*`) e **compacta o VHDX no
   Windows** (`Stop-VM` + `Optimize-VHD -Mode Full`), repetindo até `V:` ≥ 51.
2. **Depois que o PR inteiro termina**, se for pedido **re-run** dos checks: ANTES
   de re-rodar, o orchestrator executa o **mesmo ciclo** (limpar todos os caches +
   compactar no Windows) e **só** quando `V:` ≥ 51 GB de novo o re-run inicia.
3. **Qualquer outro PR na fila** para rodar checks **respeita o mesmo fluxo**:
   nenhum batch novo (PR inicial ou re-run) inicia sem o ciclo limpa+compacta →
   ≥ 51 GB.

A garantia é **por-batch**, independente do power-state da VM (warm ou Off) — não
só no cold-start. É o `reclaim_before_admit` (§4.1) elevado a invariante de **todo**
início de batch. O build cache é regenerável e DEVE ser descartado para o
`Optimize-VHD` recuperar o `V:` (§4 — Redução na FONTE); preservar cache entre runs
já falhou (enche o disco; `--filter until=` apaga imagem vendor recém-puxada,
`7e9cc0d`). Implementação e gap: ver RF-10 e o PRD
`docs/specs/civm-disk-gate-per-batch/` (PRD→SPEC→IMPL).

### Decisão pura, separada do efeito (Kahneman #13)

> **Âncoras de linha (`decision.ps1:NN` / `orchestrator.ps1:NN`) neste doc** foram
> deslocadas pela slice `civm-disk-gate-per-batch` (funções puras `Update-`/
> `Resolve-AdmitTransition` + gate de admissão warm + remoção do param
> `BoundaryCompactFloorGB`). São **aproximadas**; a autoridade do "o que/onde" é
> `docs/specs/civm-disk-gate-per-batch/SPECv4.md`. Hygiene pendente: converter para
> âncoras simbólicas (SPECv4 §9 + checklist grep).

A lógica de decisão vive em `civm-orchestrator-decision.ps1`
(`Get-OrchestratorDecision` + as funções puras `Update-AdmitAttempts` /
`Resolve-AdmitTransition`): recebe o estado observado e devolve **uma** ação string;
**não toca a VM**. O orchestrator só **executa** a ação no `switch`
(`civm-vm-orchestrator.ps1`). Isso permite a decision-table
testar o **mesmo módulo deployado** sem Hyper-V (`civm-orchestrator-decision.ps1:1-5`).

A probe de job ativo é um **scriptblock lazy** (`HasActiveJobProbe`,
`civm-orchestrator-decision.ps1:14,46`): só é chamada quando se chega ao gate de
stop, evitando um SSH por tick.

### Ações possíveis (saídas de `Get-OrchestratorDecision`)

| Ação | Condição | Efeito no `switch` |
| --- | --- | --- |
| `start` | VM Off + `(queued+running)>0` | `Start-VM` (linha 286-292) |
| `noop_off` | VM Off + nada | nada (linha 285) |
| `panic_compact` | VM Running + `0<VFreeGB<PanicFloorGB` + `CanPanic` | `Invoke-StopAndCompact` MESMO ocupado (linha 305-318) |
| `warn_clean` | VM Running + `0<VFreeGB<WarnFloorGB` | poda online, sem stop (linha 299-304) |
| `boundary_compact` | VM Running + `Running==0` + `Queued>0` + `0<VFreeGB<AdmitFloorGB(51)` + `attempts<2` | `Invoke-StopAndCompact` (gate de admissão warm), **sem matar job** (Running==0); senão `mark_busy` admite |
| `disk_below_floor_admitted` (evento) | admite batch com `V<51` (após `attempts>=2`) | log de warning (warm/cold); gatilho de rollback/abort |
| `mark_busy` | VM Running + `(queued+running)>0` | grava `lastBusyUtc` (linha 293) |
| `idle_debounce` | VM Running + ocioso + `idle<IdleStopMinutes` | só loga (linha 294) |
| `stop_aborted_active_job` | VM Running + ocioso ≥ debounce + probe SSH vê worker | aborta stop, re-arma busy (linha 295-298) |
| `stop_and_compact` | VM Running + ocioso ≥ debounce + sem worker | `Invoke-StopAndCompact` (linha 319-324) |

**Ordem de precedência** dentro de `Get-OrchestratorDecision` (crítica; SPECv4 §0):
VM Off primeiro → `panic_compact` (`<18`, preserva o piso crítico + cooldown) →
**gate de admissão warm** (`Running==0` + `Queued>0` → `boundary_compact` se `V<51`,
senão `mark_busy`) → `warn_clean` (`<28`, **só com job rodando**, `Running>0`) → busy
→ debounce → gate de stop. O gate precede `warn` porque `warn` é online e **não**
recupera o `V:` do host. Ver `docs/specs/civm-disk-gate-per-batch/SPECv4.md`.

## 3. Requisitos

| ID | Requisito | Âncora no código |
| --- | --- | --- |
| RF-1 | VM Off + qualquer trabalho na fila → liga (lado seguro: `running>0` com VM off é estado stale, mas sobe mesmo assim) | `decision.ps1:25-27`, test casos 1-2 |
| RF-2 | VM Off + nada → permanece Off (a essência do scale-to-zero) | `decision.ps1:26-27`, test caso 3 |
| RF-3 | VM Running com trabalho → `mark_busy`, **nunca** desliga | `decision.ps1:41`, test casos 4-6 |
| RF-4 | Idle < `IdleStopMinutes` (default 10) → `idle_debounce` (não desliga no primeiro tick idle) | `decision.ps1:43`, test casos 7-8 |
| RF-5 | Idle ≥ debounce + **sem worker no guest** → `stop_and_compact` | `decision.ps1:47`, test casos 10-11 |
| RF-6 | Idle ≥ debounce mas a probe SSH vê `Runner.Worker` ativo (repo fora do escopo do token) → `stop_aborted_active_job` | `decision.ps1:44-46`, test caso 9 |
| RF-7 | Contagem da fila ignora runs fantasmas: conta só `run.status` real + `created_at < MaxAgeHours` (12h) | `Get-RunCount` `orchestrator.ps1:107-125` |
| RF-8 | Um PAT `actions:read` por resource owner; o stop-guard via SSH é a salvaguarda final independente de token | `orchestrator.ps1:38-54,166-175` |
| RF-9 | Ownership exclusivo: orchestrator é o único dono do power-state + stop/compact (ver §2 Ownership abaixo) | `orchestrator.ps1:30-32`, `activate-orchestrator.ps1:12` |
| RF-10 | **Disco limpo por batch (§2 Política definitiva):** todo início de batch (PR inicial OU re-run) só admite com `V:` ≥ `AdmitFloorGB` (51); abaixo disso, ciclo limpa-tudo + `Optimize-VHD` ANTES de iniciar, independente do power-state. Re-run após PR completo dispara o ciclo antes de re-rodar; PR na fila respeita o mesmo. **Estende** `reclaim_before_admit` (hoje só VM-Off+fila) e `boundary_compact` (hoje `<40` no gap) para disparar sempre que um batch novo vai iniciar e `V: < 51`. | `decision.ps1` (`reclaim_before_admit`/`boundary_compact`), `orchestrator.ps1:286` (AdmitFloorGB). **GAP: requer mudança na decisão + casos na decision-table (validar em pwsh na box)** |
| RF-11 | **Liveness pós-reboot/energia:** a task usa gatilhos periódico + `AtStartup`, `StartWhenAvailable`, permite iniciar e continuar em bateria e é substituída com `Register-ScheduledTask -Force`, sem janela criada por `Unregister-ScheduledTask`. A ativação recusa dual-owner quando `civm-host-orchestrator` aponta para `--active`/wrapper `active.cmd`. | `activate-orchestrator.ps1`, `register-orchestrator.ps1`, `orchestrator-task-registration.test.ps1` |

### Ownership (CRÍTICO — um dono só, fail-safe #15)

O orchestrator é o **único dono** do power-state da VM e do par
`Stop-VM`/`Optimize-VHD`. Ao ativá-lo (`activate-orchestrator.ps1`), os curadores
legados foram **DESABILITADOS na box** (todos `Disabled`, 2026-06-17):

- `civm-vhdx-autoreclaim` — `Disable-ScheduledTask` em `activate-orchestrator.ps1:12`.
- `civm-vhdx-optimize` — supervisionado, aposentado pelo orchestrator.
- `civm-vhdx-optimize-watchdog` — religava a VM se ela ficasse Off; conflitaria
  com o scale-to-zero (`orchestrator.ps1:30-32`).

**Por quê:** dois reclaimers disputando o mesmo `V:\civm-reclaim.lock` e o
power-state é exatamente "o curador matando o recurso que cura". Um dono só
elimina a corrida (fail-safe #15). O autoreclaim legado mantém sua cópia inline do
gate até a remoção, mas a **fonte ativa** do gate de 2 fases é
`civm-reclaim-gate.ps1` (`civm-reclaim-gate.ps1:1-5`).

## 4. Camada de disk-safety (panic / warn)

O scale-to-zero só compacta no **idle**. Sob CI **sustentado** (PRs back-to-back),
a VM nunca idla → o VHDX cresce monotonicamente → death-spiral. Medido:
`V:` 39 → 16 GB em ~1h (~22 GB/h), saturando o host até derrubar o interop
WSL↔Windows (`validation.md`, 2026-06-17 18:38). A camada de disco fecha esse gap.

| Camada | Gatilho | Ação | Mata job? |
| --- | --- | --- | --- |
| `warn_clean` | `V: < WarnFloorGB` (28) | `docker builder prune -af` + `fstrim` online (`Invoke-GuestWarnClean`, `orchestrator.ps1:188-195`) | **Não** — poda só cache de build regenerável; nunca imagens de run (sem o bug de eviction que o age-guard consertou) |
| `boundary_compact` (gate de admissão warm) | `Running==0` + `Queued>0` + `V: < AdmitFloorGB` (51) + `attempts<2` | `Stop-VM` + `Optimize-VHD` offline ANTES de admitir o batch (`Invoke-StopAndCompact -Reason 'boundary_compact'`); **precede `warn`** | **Não** — nenhum job `in_progress` (Running==0); recupera o `V:` a ≥51 por batch |
| `panic_compact` | `V: < PanicFloorGB` (18) + `CanPanic` | `Stop-VM -Force` + `Optimize-VHD` offline **mesmo ocupado** (`orchestrator.ps1:305-318`) | **Sim** — jobs ativos morrem e re-rodam |

### Gate de admissão warm — disco limpo (≥51) por batch (civm-disk-gate-per-batch)

> **Atualizado pela slice `civm-disk-gate-per-batch` (SPECv4).** O antigo
> `boundary_compact` (piso 40, **depois** de `warn`) virou o **gate de admissão
> warm** (piso `AdmitFloorGB=51`, **antes** de `warn`). A ação no `switch` continua
> `boundary_compact` (reuso); o **piso (40→51)** e a **precedência** mudaram. O
> contador `admitReclaimAttempts` é gerenciado pelo caller via `Resolve-AdmitTransition`.

Sob **fila contínua** (PRs back-to-back) a VM fica `Running` o tempo todo, nunca
chega ao idle → o `stop_and_compact` ocioso nunca roda. O gate pega a janela em que
a sequência de **um** PR terminou (`Running==0`, nada `in_progress`) e o próximo já
está na fila (`Queued>0`): se `V: < 51`, `Stop-VM` + `Optimize-VHD` recuperam o disco
**sem matar job** (nenhum rodando), e o próximo tick religa via `start` (cold). É o
mesmo `Invoke-StopAndCompact` (mesmo lock canônico, mesmo gate `Test-OptimizeSlack`).

**Piso = `AdmitFloorGB` (51), não 40 (superseded):** a política definitiva é **≥51
por batch** (o CI pago dá disco fresco por job; ver §2 "Política definitiva"). O
antigo 40 era uma otimização de cold-start (não compactar entre 40–51) que a política
≥51-por-batch **supera** — todo batch começa limpo, ao custo de ~1 compact/batch
(aceito e medido; SPECv4 §10).

**Precedência (gate ANTES de `warn`):** `warn_clean` é online (poda cache + `fstrim`)
e **não** recupera o `V:` do host — só `Optimize-VHD` recupera. Logo, no gap warm
(`Running==0`), o gate compacta até 51 em vez de podar online; `warn_clean` fica
reservado a **job rodando** (`Running>0`, não dá para parar). `panic` (`<18`)
permanece **antes** do gate (preserva o piso crítico + `lastPanicUtc`/cooldown); com
`Running==0` o panic compacta sem matar e **conta** a tentativa (DT-9). O gate cobre a
faixa **18..51** no gap.

**Anti-deadlock (`attempts<2`):** o contador `admitReclaimAttempts` (incrementado em
cada compact de admissão, zerado na admissão — tudo via a função pura
`Resolve-AdmitTransition`, testada) garante que disco irrecuperável admite após **≤2
compacts/episódio** emitindo `disk_below_floor_admitted` — nunca deadlock (fail-safe #15).

**Caso back-to-back SEM gap + invariante no-kill (SPECv4 §0.1):** se um job novo entra
`in_progress` antes de `Running` chegar a 0, o gate **nunca** dispara. Mas com
clean-start ≥51 por batch um único batch não cai do piso de admissão (51) ao piso de
kill (18) no meio → `panic_compact` (a única ação que mata job) **não deve disparar**
em operação normal; é mantido como backstop, e disparar com `running>0` é sinal de
**ABORT** (§7), não operação normal.

### Composição com a poda de imagens do #137

O `boundary_compact` e a poda de imagens taggeadas de compose-project no
`job-completed` (branch `fix/issue-137-source-reduction`, commit `da41d45`) se
**compõem**, não se duplicam: o **#137 libera os blocos** dentro do guest (remove
imagens de runs finalizadas → blocos viram TRIM-áveis), e o `boundary_compact`
**recupera o `V:` do host** via `Optimize-VHD` offline (encolhe o VHDX de fato —
podar imagem no guest sozinho NÃO encolhe o arquivo). Um prepara o espaço
recuperável; o outro o devolve ao host no gap. São camadas distintas (guest-side
free vs. host-side shrink) e não se sobrepõem.

**Tradeoff explícito do panic** (`orchestrator.ps1:305-318`,
`decision.ps1:29-38`): matar jobs que re-rodam é ruim, mas **disco encher é pior**
— satura o host e derruba até o interop. O panic usa `Optimize` **offline** (VM
desligada) → sem o bug de eviction de imagem que ocorre online. A VM volta sozinha
pela fila no próximo tick (cold start).

### Redução na FONTE — reap de imagens de run no job-completed (#137)

A camada acima é a **salvaguarda** (a jusante). A **fonte** do crescimento é o
guest acumular imagens de service mais rápido do que o prune libera: a E2E builda
~35 GB de imagens num único job (`validation.md`, 2026-06-18 19:17), e o
`job-completed` só removia imagens **dangling** — as imagens taggeadas de service
do run nunca saíam e empilhavam na rajada até o panic floor.

O `job-completed` agora reapa as imagens taggeadas do PRÓPRIO run que terminou
(`reapRunImages`, `internal/hook/hook.go`): um `docker image prune -a -f --filter
label=com.docker.compose.project=<scope>` por escopo, onde `<scope>` é o compose
project deste runner (`<slot>` e `<slot>-<run_id>`, lidos do `.env` via
`CIVM_RUNNER_SLOT`/`COMPOSE_PROJECT_NAME`). O `-a` é seguro **porque escopado**:

- o slot é **box-único por runner** (multi-project-isolation) — um sibling jamais
  carrega o mesmo project, então o reap nunca evicta a imagem de outro run;
- imagem de **vendor pull** (redis/minio/postgres/alpine/clamav) **não** tem label
  de compose → nunca é matched, então o "No such image" race que o PR #135 removeu
  do path online **não volta**;
- o docker recusa remover imagem com container vivo, e o reap roda **depois** do
  `killWorkRootContainers` + `container prune` — o run já terminou.

Sem project no env (degradado) → no-op (fail-safe; sem escopo seguro, não se reapa
nada). Falha do reap é Warning, nunca falha o job (higiene pós-job não vira job
vermelho). É a redução de TAXA que faz o `panic_compact` quase nunca precisar
disparar — o panic permanece como floor permanente (#15), não é substituído.

Floors são parametrizáveis (`orchestrator.ps1:59-60`, defaults
`WarnFloorGB=28`/`PanicFloorGB=18`). Os boundaries são `<` estrito: `V==18` →
`warn` (não panic); `V==28` → `mark_busy` (não warn) — provados nos test casos
17-18.

## 4.1 — Mapa dos gates de disco (DUAS escalas — não confundir)

> **Por que esta seção existe.** Uma validação adversarial Kahneman (#3 número
> antes de adjetivo, #4 anchoring de escala, #5 worst-case) achou um HIGH: os
> "floors" de disco do sistema estavam apresentados como se fossem o **mesmo**
> número, mas vivem em **duas escalas físicas diferentes**. `18`, `28`, `51` são
> **GB livres no volume V: do host**; `90`, `60` são **% de uso do filesystem do
> guest**. Comparar "54 − 35 = 19, só 1 acima do 18" mistura `V:`-livre-GB (o 54,
> o 18) com um dreno (o 35) que nunca foi medido na mesma escala — e nem o `90%`
> é a mesma régua que o `18`. O mapa abaixo fixa cada gate na sua escala.

Existem **dois planos de medição independentes**, cada um com seus gates:

| Plano | Unidade | O que mede | Direção do perigo |
| --- | --- | --- | --- |
| **Host V:** | GB **livres** no volume `V:` (Hyper-V) | espaço que sobra no disco que hospeda o VHDX | perigo = número **baixo** (acabando) |
| **Guest %** | **% usado** do filesystem `/` do guest | quão cheio está o disco DENTRO da VM | perigo = número **alto** (enchendo) |

São acoplados (o guest enchendo faz o VHDX crescer e o V: cair) mas **não são
conversíveis 1:1** — o VHDX é dinâmico e o VMRS/scratch ocupam V: fora do guest.
Um floor de um plano **nunca** deve ser comparado a um floor do outro.

### Gates do plano Host V: (GB livres) — perigo é baixar

| Gate | Valor | Onde vive (constante) | Onde é aplicado | Ação ao cruzar |
| --- | --- | --- | --- | --- |
| `AdmitFloorGB` | **55** | `orchestrator.ps1:286`, `decision.ps1:27` | `Get-OrchestratorDecision` (VM Off + fila) | `reclaim_before_admit`: compacta ANTES de admitir o batch |
| `WarnFloorGB` | **28** | `orchestrator.ps1:59`, `decision.ps1:16` | `Get-OrchestratorDecision` (Running + work) | `warn_clean`: poda online, **não** mata job |
| `PanicFloorGB` | **18** | `orchestrator.ps1:60`, `decision.ps1:17` | `Get-OrchestratorDecision` (Running + work) | `panic_compact`: compacta offline **mata job** |
| `CritFreeGB` | **10** | `civm.DefaultHostVolumeCritFreeGB` (`civm.go:105`) | `hostdisk.levelByFree` (`hostdisk.go:193-201`); gate host-aware do hook `job-started` (`hook.go:246-249`) | rejeita o job (`Blocks()`) se o snapshot for FRESCO |
| `WarnFreeGB` | **30** | `civm.DefaultHostVolumeWarnFreeGB` (`civm.go:104`) | `hostdisk.levelByFree` | `WantsCleanup()` → cleanup no `job-started` |
| `HeadroomGB` | **8** | `civm.DefaultHostVolumeHeadroomGB` (`civm.go:110`) | `hostdisk.Check` (`hostdisk.go:154`); gate pós-Off `Test-OptimizeSlack` | aborta o `Optimize-VHD` sem zero-fill |
| `HardFloorGB` | **1** | `civm.DefaultHostVolumeHardFloorGB` (`civm.go:125`) | gate pós-Off (`reclaim-gate.ps1:12,34`) | piso duro absoluto; nunca operar abaixo |

### Gates do plano Guest % (uso) — perigo é subir

| Gate | Valor | Onde vive (constante) | Onde é aplicado | Ação ao cruzar |
| --- | --- | --- | --- | --- |
| `DefaultHardFailPct` | **90** | `civm.go:38` | hook `job-started` (`hook.go:242-245`, `diskUsedPct` `hook.go:725`) | rejeita o job (exit 75) |
| `DefaultPreCleanupPct` | **60** | `civm.go:37` | hook `job-started` (`hook.go:220`); disk-watchdog (timer) | dispara cleanup de pressão (purga caches) |
| `DefaultEmergencyBypassPct` | **75** | `civm.go:47` | disk-watchdog: para de adiar reclaim SAFE ao tick idle | trim de emergência mid-job |

**Leitura única do anchoring desfeito:** o "floor 18" do orchestrator (`V:`-livre)
e o "fail 90%" do hook (guest-uso) **não são o mesmo floor** — são planos
diferentes. O autoreclaim/panic e o `CritFreeGB`/`HeadroomGB`/`HardFloorGB`
concordam **todos** na escala `V:`-livre-GB; é nela, e só nela, que o floor de
sobrevivência de job deve ser expresso (próxima seção).

## 4.2 — Floor de sobrevivência de job (definição MEDIDA, não estimada)

O floor de admissão (`AdmitFloorGB=51`) e o panic (`PanicFloorGB=18`) decidem se
um job E2E pesado (ex.: **Tenant Isolation**) sobrevive sem ser morto por
`panic_compact`. A pergunta real é: *quanto de `V:` um job consome até o pico?* —
o **dreno de V: por job** (high-water). Hoje esse número é **ESTIMADO**: o SPEC e
o `validation.md` falam "~35GB" inferido da taxa medida (~22 GB/h, §4) e da
recuperação dos `warn_clean`. Estimativa não é medição (Kahneman #3/#5).

**Definição correta do floor — tudo na escala `V:`-livre-GB:**

```
floor_de_admissão  >=  CritFreeGB(10)  +  p95(drain_por_job_MEDIDO)  +  safety
```

onde `drain_por_job` = `host_v_free_gb@job-started − host_v_free_gb@job-completed`
de um par de registros do mesmo `run_id` no `hooks.jsonl` (helper canônico
`hook.JobVDrainGB`, `internal/hook/hook.go`). Lê-se: para um job sobreviver, o V:
ao admiti-lo precisa cobrir o piso crítico do host (10GB, onde o gate host-aware
já rejeita) MAIS o pior dreno plausível de um job (p95) MAIS uma folga.

**NÃO** se define o floor como "floor − dreno típico estimado" (a forma que gerou
o "54 − 35 = 19, 1 acima do 18"): isso (a) ancora dois planos e (b) usa um dreno
que nunca foi medido. O `AdmitFloorGB=51` atual é uma escolha conservadora
empírica (o V: pós-compact limpo, §11 `validation.md`), **não** um floor derivado
de p95 de dreno — e permanece assim até o número de dreno ser capturado.

**Estado da medição (PENDENTE de captura):** o mecanismo já existe — o hook grava
`host_v_free_gb` no `hooks.jsonl` em **ambos** `job-started` e `job-completed`
(`hook.go` `EventJobStarted`/`EventJobCompleted`; emitido por `appendLog`). O
high-water (`JobVDrainGB`) é então um pós-processamento dos pares por `run_id`. O
**número de dreno p95 real será capturado no próximo E2E Tenant Isolation** com a
box ligada; até lá, `AdmitFloorGB`/`PanicFloorGB` **não devem ser mexidos** — não
há dado que justifique mover um threshold. Esta seção é a desconfusão de escala +
a instrumentação da medição, **não** uma recalibração de floor.

## 5. Gate de segurança de 2 fases (reusado do #106)

`Invoke-StopAndCompact` (`orchestrator.ps1:206-255`) tem **três** salvaguardas
para o curador não matar o `V:` que cura:

1. **Lock canônico** `V:\civm-reclaim.lock` (`FileShare::None`,
   `orchestrator.ps1:209-216`): exclusão mútua com qualquer outro reclaimer do
   mesmo VHDX. Falha ao abrir → `reclaim_skip_locked` e retorna sem agir.
2. **Gate pós-Off** (`Test-OptimizeSlack`, `civm-reclaim-gate.ps1:28-35`): após
   `Stop-VM`, **re-mede `V:`** — o VMRS (~8 GB) só é liberado com a VM **Off**;
   medir antes subestima (foi o que travou a espiral a 6.6 GB no #106,
   `civm-reclaim-gate.ps1:22-27`). O `Optimize-VHD` é **ininterruptível** e
   **não-monotônico** (pode crescer o `V:` no meio), consumindo scratch ~10 GB. Só
   compacta se `(VFreeAfterOff − HardFloor 1) ≥ ScratchBudget 11`
   (`civm-reclaim-gate.ps1:12-13,34`); senão `reclaim_skip_insufficient_slack` e
   pula — **não empurra o `V:` abaixo do piso** (`orchestrator.ps1:232-237`).
3. **Recover-detection** (`orchestrator.ps1:245-249`): se recuperou
   `< MinRecoverGB` (3, `civm-reclaim-gate.ps1:20`), loga `reclaim_no_progress`
   como ERROR — disco apertado que o compact não resolve (precisa de humano), **não
   finge sucesso** (#13: existe ≠ funciona).

**Cooldown do panic** (`Test-ReclaimCooldown`, `civm-reclaim-gate.ps1:42-55`):
15 min (`PanicCooldownMinutes`, linha 17). Fora da janela → pode panicar; dentro →
a decisão **rebaixa** para `warn_clean` (`decision.ps1:18-21,38`), não re-mata jobs
em loop. O `lastPanicUtc` é gravado **antes** do compact (`orchestrator.ps1:315`):
o cooldown conta do disparo, e se o compact pendurar o próximo tick não re-mata.
O `Optimize` recupera ~25 GB e o crescimento medido é ~22 GB/h → ~1h entre panics
naturais; 15 min só barra o **loop apertado** (panic a cada tick quando o guest
reenche rápido), sem atrapalhar o ritmo real (`civm-reclaim-gate.ps1:14-16`).

## 6. Fail-safe (Kahneman #15 — na dúvida, nunca desliga)

Todo caminho de incerteza resolve para o **lado seguro: manter a capacidade de
pegar job**. Só o caminho de `Start` é seguro por si.

| Condição de incerteza | Comportamento | Âncora |
| --- | --- | --- |
| `VFreeGB <= 0` (medida de `V:` falhou) | **não** entra em panic/warn (não age por medida ruim) | `decision.ps1:38-39`, `Get-VFreeGB` `orchestrator.ps1:179-182`, test caso 19 |
| Probe SSH de job ativo falhou | assume "há job" → **não** desliga | `Get-GuestHasActiveJob` `orchestrator.ps1:166-175` |
| Falha de API do GitHub | relança → cai no `catch` do main → **não** desliga | `Get-RunCount` `orchestrator.ps1:98`, `orchestrator.ps1:327-332` |
| `lastPanicUtc` ilegível no cooldown | **não** trava o reclaim (deixa proteger) | `Test-ReclaimCooldown` `civm-reclaim-gate.ps1:48-54`, test caso 24 |
| SSH de limpeza (full/warn) falhou | best-effort, **não** bloqueia o stop | `Invoke-GuestFullClean` `orchestrator.ps1:149-160` |
| Erro qualquer no main | `orchestrator_error` ERROR + `exit 1`, **deixa a VM como está** | `orchestrator.ps1:327-332` |
| VM não chegou a Off no deadline (180s) | `reclaim_abort_vm_not_off` ERROR, **não** monta VHDX em uso | `orchestrator.ps1:223-228` |

O lock de estado expira por tempo (cooldown), **nunca trava pra sempre**
(`orchestrator.ps1:22-23`).

## 7. Rollback trigger (numérico / observável)

Reverter (re-habilitar o `civm-vhdx-optimize` supervisionado e desabilitar o
orchestrator, ou desarmar **só** a camada de disco via floors=0) quando **qualquer
um** dos sinais objetivos no `V:\civm-orchestrator.log` ocorrer:

- **≥ 2 eventos `disk_panic` com `running>0`** em janela de **< 30 min** — o panic
  está matando jobs em flap em vez de só barrar o death-spiral (o cooldown deveria
  impedir; se ocorrer, a calibração de 15 min está errada para o perfil de carga).
- **`reclaim_no_progress` repetido** (≥ 2 ocorrências consecutivas) — o compact
  "passa" mas `recovered_gb < 3`: o `Optimize` não está liberando espaço; problema
  fora do alcance do orchestrator (precisa de humano).
- **`V:` não recupera pós-panic medido** — após um `disk_panic` + `reclaim_done`,
  o `v_free_gb` registrado continua `< PanicFloorGB`: a compactação não está
  recuperando o piso esperado (~25 GB), então o panic só está custando jobs sem
  ganho.
- **(civm-disk-gate-per-batch) `disk_boundary_compact/h > 4` por ≥ 2 h consecutivas**
  — a compactação de admissão virou o gargalo dominante (a box não sustenta a
  política ≥51-por-batch); escalar para o fix maior (VM-por-job / mais disco).
  *(valor inicial Slice 0, revisável após N batches medidos.)*
- **(civm-disk-gate-per-batch) ≥ 3 `disk_below_floor_admitted` com `v_free_gb < 51`
  em 1 h** — disco irrecuperável: admissões sujas recorrentes (anti-deadlock em
  série). Nota: com a invariante no-kill (§4 / SPECv4 §0.1), **qualquer** `disk_panic`
  com `running>0` já é alarme (esperado: zero).

Reversão imediata (não destrutiva): `Disable-ScheduledTask civm-vm-orchestrator` +
re-habilitar o caminho de optimize supervisionado. A VM volta a ser gerida pelo
curador legado conhecido.

## 8. Eventos de log (`V:\civm-orchestrator.log`)

Linha NDJSON por evento (`Write-OrcLog`, `orchestrator.ps1:75-82`): `ts` (UTC ISO),
`level`, `event` + campos.

| Evento | Quando | Campos principais | Âncora |
| --- | --- | --- | --- |
| `tick` | todo tick | `vm`, `queued`, `running`, `idle_min`, `v_free_gb`, `can_panic` | `orchestrator.ps1:277` |
| `vm_started` | ação `start` aplicada | `queued`, `running` | `orchestrator.ps1:290` |
| `idle_debounce` | ocioso antes do debounce | `idle_min`, `need` | `orchestrator.ps1:294` |
| `stop_aborted_active_job` | stop abortado: worker ativo via SSH | `note` | `orchestrator.ps1:296` |
| `disk_warn` | `warn_clean` disparado | `v_free_gb`, `floor` | `orchestrator.ps1:303` |
| `disk_boundary_compact` | `boundary_compact` (gate de admissão warm, `V<51`, **não** mata job) | `v_free_gb`, `floor` (=`AdmitFloorGB` 51), `queued`, `note` | switch `boundary_compact` |
| `disk_below_floor_admitted` | admite batch com `V<51` (warm/cold, após `attempts>=2`) — gatilho de rollback/abort | `v_free_gb`, `guest_free_gb`, `floor`, `attempts`, `path` | switch `start`/`mark_busy` |
| `disk_panic` | `panic_compact` disparado (mata jobs — **não deve ocorrer**, SPECv4 §0.1) | `v_free_gb`, `floor`, `note` | `orchestrator.ps1:312` |
| `would_start` / `would_stop_and_compact` / `would_warn_clean` / `would_boundary_compact` / `would_panic_compact` / `would_disk_below_floor_admitted` | modo `-Observe` (loga, não age) | conforme a ação | switch arms |
| `reclaim_start` | início de `Invoke-StopAndCompact` | `reason` | `orchestrator.ps1:218` |
| `reclaim_post_off_remeasure` | re-medida de `V:` pós-Off (gate fase 2) | `v_free_after_off_gb`, `scratch_budget_gb` | `orchestrator.ps1:231` |
| `reclaim_skip_insufficient_slack` | folga não cobre o scratch → pula compact | `v_free_after_off_gb`, `hard_floor_gb` | `orchestrator.ps1:235` |
| `reclaim_skip_locked` | lock canônico ocupado | `reason`, `lock` | `orchestrator.ps1:214` |
| `reclaim_abort_vm_not_off` | VM não parou no deadline | `reason` | `orchestrator.ps1:226` |
| `reclaim_done` | compact concluído | `vhdx_gb`, `v_free_gb`, `recovered_gb` | `orchestrator.ps1:244` |
| `reclaim_no_progress` | recuperou `< MinRecoverGB` (ERROR) | `recovered_gb`, `min_recover_gb` | `orchestrator.ps1:248` |
| `orchestrator_error` | erro no main (ERROR, exit 1) | `error` | `orchestrator.ps1:330` |

Auxiliares (WARN, best-effort, não bloqueiam): `guest_full_clean[_warn]`,
`disk_warn_clean[_warn]`, `guest_active_probe_failed`, `vfree_probe_failed`.

## 9. Âncoras Kahneman (CIVM = 16 disciplinas)

- **#13 — existência ≠ função; deployado == testado.** A decisão pura é
  dot-sourced pelo orchestrator E pelos testes (`orchestrator.ps1:257-261`,
  `civm-orchestrator-decision.test.ps1:4`): o código deployado é o **mesmo** que a
  decision-table exercita (59: 47 table + 10 unit + 2 stateful) e `civm-reclaim-gate.test.ps1` (10 casos).
  Cada recusa é pareada com seu positivo (ex.: `V<18` panicar **vs.** `V<18` em
  cooldown rebaixar para warn — casos 20/30). `reclaim_no_progress` materializa o
  princípio: o `Optimize` "passar" não prova que liberou espaço — mede-se
  `recovered_gb`.
- **#14 — retry calibrado, não cego.** O cooldown de 15 min (`Test-ReclaimCooldown`)
  é o retry calibrado: barra o `panic` re-disparando a cada tick quando o disco
  reenche rápido, mas não atrapalha o ritmo natural (~1h entre panics medido).
- **#15 — fail-safe = o gate + um dono só.** O gate de 2 fases (lock + slack
  pós-Off + recover-detection) impede o curador de matar o `V:` que cura; o
  ownership exclusivo (autoreclaim/optimize/watchdog desabilitados) elimina
  curadores em conflito disputando o lock/power. Na dúvida (§6), nunca desliga.

## 10. Notas de implementação / armadilhas conhecidas

- **Ghost-queued (RF-7).** O índice `?status=queued` do GitHub fica STALE e lista
  runs JÁ COMPLETED como queued — 2 runs de 3 semanas atrás travaram o
  scale-to-zero (`gh run cancel` respondia "Cannot cancel a run that is
  completed"). `Get-RunCount` ignora o `total_count` do filtro e conta só
  `run.status` real + `created_at < 12h` (`orchestrator.ps1:98-125`,
  `validation.md` 2026-06-17 09:27).
- **Principal SYSTEM obrigatório.** Deve rodar como SYSTEM (mesmo principal do
  autoreclaim) — como elevated-user a ssh key fica ilegível
  (`orchestrator.ps1:25-29`). A task registra com `New-ScheduledTaskPrincipal
  -UserId 'SYSTEM'` (`activate-orchestrator.ps1:8`).
- **Tokens.** Um PAT fine-grained `actions:read` por resource owner em
  `C:\ProgramData\civm\gh-token-{acme,other}.txt` (o host não tem `gh`,
  `orchestrator.ps1:25-27,38-43`).
- **`MultipleInstances IgnoreNew`** (`activate-orchestrator.ps1:9`): um
  `Optimize-VHD` de ~8 min bloqueia os ticks seguintes; um job que chega durante
  um compact espera ~10 min (cold start pior) — aceitável para CI
  (`validation.md` 2026-06-17 09:59).
- **Liveness da task (RF-11).** `AtStartup` + `StartWhenAvailable` recuperam o
  loop depois de reboot ou tick perdido. `AllowStartIfOnBatteries` e
  `DontStopIfGoingOnBatteries` impedem que uma troca de alimentação silencie o
  único publicador de `C:\ProgramData\civm\gate\current-context`. O registro usa `-Force` sobre a
  definição existente; falha de cópia/parse não remove a última task válida.
- **Modo `-Observe`** (`orchestrator.ps1:66-69`): loga `would_*` em vez de agir —
  valida a lógica contra a box real sem mexer na VM. Preferido a `-WhatIf` (que
  suprimiria até o `Add-Content` do log).

## 11. Validação (evidência empírica)

Ver `validation.md` na raiz do repo (log append-only). Marcos:

- **2026-06-17 09:59** — ✅ ciclo scale-to-zero COMPLETO medido: idle → compacta
  (`V:` 31→51) e fila → liga → 2 jobs reais rodando (sem flap).
- **2026-06-17 18:38** — 🟡 death-spiral REAL medido (`V:` 39→16, interop caiu);
  camada de disco coded + unit-validada (decision-table 20/20 à época, hoje 59 com o gate de admissão warm).
- **2026-06-17 19:45** — ✅ camada panic/warn DEPLOYADA: decision-table **20/20 no
  PowerShell 5.1 da box**, wiring `v_free_gb` vivo no tick real, sem erro. 🟡
  pendente: um `disk_panic` real (`V:<18` + VM Running) registrado num ciclo.
- **2026-06-19** — 🟡 dreno de V: por job INSTRUMENTADO (não mais estimado): o
  hook grava `host_v_free_gb` no `hooks.jsonl` em `job-started` E `job-completed`;
  `hook.JobVDrainGB` calcula o high-water (`vfree@started − vfree@completed`) por
  `run_id`. Unit-validado (`internal/hook` -race verde). 🟡 **PENDENTE**: o p95 de
  dreno real (e portanto a derivação do `AdmitFloorGB` por §4.2) será capturado no
  próximo E2E Tenant Isolation com a box ligada. Até lá, floors **não** mexidos.

Testes (rodar localmente, sem Hyper-V):

```powershell
pwsh deploy/windows/civm-orchestrator-decision.test.ps1   # 59 (47 decision-table + 10 unit + 2 stateful; gate de admissão warm)
pwsh deploy/windows/civm-reclaim-gate.test.ps1            # 10 casos
pwsh deploy/windows/orchestrator-task-registration.test.ps1 # contrato de liveness/registro/ownership
```

```bash
go test -race ./internal/hook/...   # inclui JobVDrainGB + emissão de host_v_free_gb
```
