---
slug: civm-self-cleaning-runner
title: Runner auto-limpante — guest + host + admissão + reaper + OS hygiene, zero intervenção manual
milestone: —
issues: [106, 113]
---

# SPEC — Runner auto-limpante

> **SUPERSEDED-BY (2026-06-17): orchestrator scale-to-zero.** O stop+compact da VM
> e o reclaim do VHDX agora pertencem ao `civm-vm-orchestrator.ps1` (único dono do
> power-state; tasks `autoreclaim`/`optimize`/`optimize-watchdog` `Disabled`).
> Fonte de verdade viva: `docs/specs/orchestrator-scale-to-zero/`. O conteúdo
> abaixo é preservado como histórico — não o reimplemente.

> **⚠ Reconciliação SPEC↔código (auditoria 2026-06-06) — LEIA ANTES DO IMPL.**
> O working tree do `civm` (alterações **não commitadas**: `git status` →
> `M deploy/windows/civm-vhdx-autoreclaim.ps1`, `M internal/civm/civm.go`,
> `M internal/civm/reclaim_test.go`) **já implementa RF-1 e RF-2**, com decisões
> que **divergem deste SPEC**. O Veredito "pronto para Passo 2.5 antes do IMPL"
> está **desatualizado**. Antes de prosseguir, reconcilie:
> - **🔴 O FIX DO #106 ESTÁ INERTE NO HOST (mais grave).** `civm.go:93 = 11` é
>   lido por **nada** em Go (`grep DefaultHostVolumeScratchBudgetGB` só acha a
>   própria definição) e `EmergencyAdmits` **não tem caller de produção** (só def +
>   teste). O worker `autoreclaim.ps1` recebe `$ScratchBudgetGB` só por parâmetro
>   CLI, default `0` (linha 50); `register-civm-vhdx-autoreclaim.ps1` monta `/tr`
>   como `powershell -File <script>` **sem** `-ScratchBudgetGB` (o comentário do
>   register, linhas 76-78, diz que valores custom exigem "editing the installed
>   script"). Logo, em runtime `$ScratchBudgetGB = 0` → a Fase 1 **ainda aborta**
>   `emergency_disabled_no_budget` (linha 255). A constante, o gate de duas fases e
>   o teste verde de `EmergencyAdmits` **não quebram a espiral** porque o número
>   nunca chega ao worker (Kahneman #13: teste verde de helper não-chamado ≠ host
>   funcionando). **Fix obrigatório:** threadar `-ScratchBudgetGB` (e `-HardFloorGB`)
>   do `register-*.ps1` a partir da constante, ou um config que o worker leia —
>   senão RF-1/RF-2 são no-op.
> - **RF-1 já aplicado:** `civm.go:93` `DefaultHostVolumeScratchBudgetGB = 11`
>   (era 0). Mas o critério "5 medições via `optimize.ps1` `vmrs_release_gb`"
>   **não foi cumprido**: `optimize.ps1` **não foi instrumentado** (não está em
>   `git status`); o 11 vem de "p100 scratch observado (logs do host)=10 +1" e de
>   um único "vmrs_release medido = 8.02GB" no comentário, não da campanha de 5
>   ciclos. Kahneman #3/#13: constante habilitada sem a evidência que o RF-1 exige.
> - **RF-2 já aplicado** em `autoreclaim.ps1`, mas com **nomes de evento e fluxo
>   diferentes** do esqueleto abaixo: código usa `autoreclaim_post_off_remeasure`
>   e `autoreclaim_skip_insufficient_slack_post_off` (**WARN, exit 0**); **não**
>   existe `$StopMarginGB`, `autoreclaim_abort_pre_stop_unsafe`,
>   `autoreclaim_post_stop_measure`, `autoreclaim_abort_post_stop_insufficient_slack`
>   nem `$skipOptimize` (o código faz `exit 0` direto e confia no `finally`).
> - **Lint falso-verde:** `specv3_reclaim_test.go` exige `autoreclaim_abort_insufficient_slack`;
>   o **evento** foi removido, mas a string sobrevive num **comentário**
>   (`autoreclaim.ps1:250`), então o `strings.Contains` passa sem o evento existir
>   (existência ≠ função, Kahneman #13). O lint precisa assertar o token real.
> - **RF-3 NÃO aplicado:** `civm.go:132` `DefaultAdmitHeavyMaxMB = 0` (#113 aberto).
> - **Drift de linhas:** as citações `autoreclaim.ps1:251-272 / 319-325 / 336 /
>   285-286 / 325-328` referem o HEAD pré-IMPL; no working tree os âncoras são
>   Fase 1 245-265, Stop-VM 312-318, Fase 2 já em 320-344, Optimize 355,
>   `[int64]0` 278-279. As constantes drifaram 90→93 e 129→132.
> - **Ação:** decidir (Passo 2.5) qual conjunto de nomes/fluxo é canônico e
>   alinhar SPEC **e** código + lint num só commit; reabrir RF-1 para a campanha
>   real ou registrar a evidência usada como exceção explícita.

> Tradução do `docs/specs/civm-self-cleaning-runner/PRD.md` em mudanças exatas no
> repo. Rastreável por `RF-N`. Decisão central: **RF-2 (gate autoritativo
> pós-`Stop-VM`)** refina `host-volume-reclamation/SPECv3` DT-v3-1 — por mutar
> `Stop-VM`/`Optimize-VHD`, **exige Passo 2.5 (red-team) antes do IMPL**.
> Proveniência operacional (`Observação operacional (auditoria)`) é re-medida no
> Slice 0; nada é habilitado por número adivinhado (Kahneman #3).

## Escopo fechado desta implementação

**Entra agora:**

- RF-1 medição `scratch_high_water_gb`+`vmrs_release_gb` → `ScratchBudgetGB` (#106).
- RF-2 gate de duas fases no `civm-vhdx-autoreclaim.ps1` (re-medição pós-Off).
- RF-3 calibração `HeavyMaxMB` (#113).
- RF-4 saúde do `fstrim` em `hooks.jsonl`+`civmctl doctor`/`host-disk`.
- RF-5 classificação HTTP 409/422 no `runreaper`.
- RF-6 gate Day-0: registrar 3 tasks + sudoers + `/run/civm`.
- RF-7 OS hygiene: drop-in `Package-Blacklist` + `doctor` reboot-required.
- RF-8 `VHDXBlockSizeBytes > 1 MiB` → `level=warn` em `hostdisk.Check`.
- RF-9 `runbooks/RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md` + reconciliação.

**Fica fora agora:** `warm-images.json`; `civmctl cache-gc` (DT-v3-7); SCSI
re-attach primário e `Convert-VHD -BlockSizeBytes 1MB` one-time (pertencem a
`host-volume-reclamation`); right-sizing do VHDX; mudança de CI dos repos
consumidores.

**Dependências assumidas prontas (`Confirmado no codebase`):** lock canônico
`V:\civm-reclaim.lock` (autoreclaim.ps1:212-224); poll de scratch ao vivo
(optimize.ps1:387-395 e autoreclaim.ps1:342-354); `finally` Start-VM 3×
(autoreclaim.ps1:372-402); `internal/admit` + `memwatchdog` + `idle`;
`runreaper` + timer 5 min; `internal/runner/watchdog.go` auto-restart cap=3;
`hostdisk.Metrics.VHDXBlockSizeBytes` (hostdisk.go:51-53,219-220);
`deploy/bin/civm-safedelete` + `deploy/sudoers.d/civm-cleanup`.

## Matriz de rastreabilidade PRD → SPEC

| PRD | Implementação no SPEC |
| --- | --- |
| RF-1 | DT-2 · MOD `civm-vhdx-optimize.ps1` (vmrs_release) · MOD `civm.go:90` · Slice 1 |
| RF-2 | **DT-1** · MOD `civm-vhdx-autoreclaim.ps1` (Fase 1+2) · MOD `specv3_reclaim_test.go` (lint do gate) · Slice 2 |
| RF-3 | DT-3 · MOD `civm.go:129` · Slice 3 |
| RF-4 | DT-4 · MOD `internal/hook/hook.go` · MOD `internal/doctor` · Slice 4 |
| RF-5 | DT-5 · MOD `internal/runreaper/runreaper.go` · Slice 5 |
| RF-6 | DT-6 · procedimento (register-*.ps1 + sudoers + /run/civm) · Slice 0 |
| RF-7 | DT-7 · CREATE `deploy/apt/51civm-reproducibility` · MOD `internal/doctor` · Slice 6 |
| RF-8 | DT-8 · MOD `internal/hostdisk/hostdisk.go` · Slice 4 |
| RF-9 | CREATE `runbooks/RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md` · Slice 7 |

## Decisões técnicas

| # | Decisão | Justificativa |
| --- | --- | --- |
| **DT-1** | **Gate de duas fases no autoreclaim; Fase 2 re-mede `Get-PSDrive V` após `Wait-VMState Off`.** A admissão do `Optimize` de emergência usa `liveFreeAfterOff − HardFloor ≥ ScratchBudget`, não o `beforeFreeGB` pré-stop. | O VMRS (~8 GB) só libera no Off; medir pós-stop é o número real que o Optimize terá. Resolve a espiral a 6.6 GB **sem adivinhar** o VMRS e **sem abortar** o Optimize ininterruptível. Refina SPECv3 DT-v3-1. |
| **DT-2** | **Medir antes de habilitar (executa SPECv3 DT-v3-2) + registrar `vmrs_release_gb`.** `ScratchBudget = ceil(p100 scratch)+1`. | Número, não adjetivo. `vmrs_release_gb` valida empiricamente a premissa do DT-1. |
| **DT-3** | **`HeavyMaxMB = ceil(p95 RSS)+margem`** medido em ≥5 jobs heavy reais. | Fecha #113; admissão deixa de ser cap generoso e passa a enforçar. |
| **DT-4** | **`fstrim` inefetivo é sinal, não erro silencioso.** Hook grava `fstrim_ineffective` em `hooks.jsonl`; `doctor` adiciona check `TRIM_EFFECTIVE` (`lsblk -D` DISC-MAX>0). | Discard morto = reclaim 100% dependente do Optimize offline; precisa ser visível (Kahneman #13: existência ≠ função). |
| **DT-5** | **Classificar HTTP 409/422 como `already-transitioned` (info).** `cancelRun` detecta via `*exec.ExitError.Stderr`; `reapRepo` não conta como `cancelled` nem sobe exit. | 409 = run já saiu de `queued`/em transição; benigno, não falha. Para de poluir o JSON do journal. |
| **DT-6** | **Registro Day-0 das 3 tasks + sudoers + `/run/civm` é gate go/no-go.** | Sem `host-metrics.json` nada do reclaim observa o estado; é o bloqueador raiz operacional. |
| **DT-7** | **OS patching security-only com `Package-Blacklist` versionado em `deploy/apt/`.** | Patch de segurança não pode trocar gcc/go/docker/kernel mid-CI (reprodutibilidade). |
| **DT-8** | **`VHDXBlockSizeBytes > 1 MiB` eleva `level` para `warn`** (não só render). | BlockSize alto = UNMAP não honrado = reclaim offline obrigatório; bloqueador, deve gateiar o nível. |
| **DT-9** | **Reconciliação por nota, não SPECv4 vizinha.** O IMPL adiciona um adendo curto a `host-volume-reclamation/SPECv3.md` apontando que DT-1 (pós-Off) refina DT-v3-1, no mesmo commit do RF-2. | Evita duplicar a árvore de decisão; mantém a fonte única do contrato de reclaim com cross-reference. |

## Fronteira de atomicidade e política de rollback

- **Atômico nesta issue:** cada `os.WriteFile`/edição de constante; cada
  `Optimize-VHD` é uma operação Hyper-V única; cada linha de `hooks.jsonl`.
- **Fora da atomicidade (estados parciais aceitos):** o ciclo
  `gate→Stop-VM→re-medir→Optimize→Start-VM` **não** é atômico — o estado
  intermediário "VM Off, Optimize pulado por Fase 2" é aceito e resolvido pelo
  `finally` (Start-VM 3×). A entrega SSH de métricas é best-effort (degrada para
  guest-only/stale).
- **Rollback de app:** `civmctl self-upgrade` para o binário anterior; RF-4/5/8
  viram no-op de campo.
- **Rollback de host:** `schtasks /change /tn civm-vhdx-autoreclaim /disable`
  (volta ao caminho supervisionado `civm-vhdx-optimize`); reverter o `.ps1` por
  `git revert` + re-deploy. **Forward-only/janela:** mudanças no `.ps1` só entram
  via janela supervisionada (mutam Stop-VM/Optimize).
- **Rollback de estado:** **N/A — Day-0** (constantes + arquivos efêmeros).
- **Proibido:** zero-fill sob baixo headroom; deixar a VM Off ao fim de qualquer
  caminho; habilitar `ScratchBudget>0`/`HeavyMaxMB>0` sem as ≥5 medições
  anexadas.

## Mapa Kahneman por etapa crítica

| Etapa / DT | Disciplina | Link | Pergunta obrigatória | Evidência mínima | Abort trigger |
| --- | --- | --- | --- | --- | --- |
| **DT-1 (Fase 2)** | #2 Counterfactual + #5 Availability | `disciplines/KAHNEMAN-DISCIPLINES.md` | "A espiral quebra a 6.6 GB sem re-medir pós-Off?" (não: `5.6 < 11`) | janela: `V:`≈6.6→para→re-mede ~14.6→admite→completa | folga pós-Off não cobrir `ScratchBudget` → pula Optimize, religa VM |
| **DT-1 (Fase 1)** | #5 Availability | idem | "Parar a VM a HardFloor+margem é seguro?" | `Stop-VM` gracioso completa; `finally` Start-VM 3× | `Stop-VM` não chega a Off em 180 s → erro, religa |
| **DT-2 / DT-3** | #3 Número não adjetivo | idem | "Quanto scratch/RSS de verdade?" | ≥5 medições logadas, commit com tabela | habilitar gate sem 5 medições; usar JSON de 10 min em vez de live |
| **DT-4** | #13 Validar propósito | idem | "O `fstrim` realmente recupera, ou só não deu erro?" | `lsblk -D` DISC-MAX + delta FileSize antes/depois | `fstrim` exit 0 mas FileSize não cai → marca `ineffective` |
| **DT-6** | #5 Availability | idem | "Algo do host opera sem as tasks registradas?" (não) | `Get-ScheduledTask`=Ready; `host-disk`=ok | task ausente → host-disk `stale` → bloqueia go |

## Checklist de segurança (pré-implementação)

- [ ] Exclusão mútua: RF-2 mantém aquisição de `V:\civm-reclaim.lock`
      (FileShare::None) antes de qualquer `Stop-VM`; quem não obtém →
      `reclaim_skip_other_active` exit 0 (autoreclaim.ps1:212-224, inalterado).
- [ ] Exec safety: `runreaper`/`doctor` usam `exec.CommandContext` sem shell; o
      `.ps1` não introduz `Invoke-Expression` de input externo.
- [ ] Privilégio: tasks SYSTEM só com direito Hyper-V; sudoers escopado a
      `civm-safedelete` (sem ampliação).
- [ ] Input validation: novo `$StopMarginGB` com `ValidateRange(0,4096)`; números
      de headroom/budget validados antes de agir.
- [ ] Fail-closed: Fase 2 sob folga insuficiente **recusa o Optimize** e religa;
      RF-5 só rebaixa 409 (nunca esconde 403/permissão).
- [ ] Secrets: `GH_TOKEN` só via `/etc/civm/run-reaper.env`; nada em
      `deploy/windows/`.
- [ ] Logs: `slog`/JSON no guest; `V:\civm-hyperv-maintenance.log` no host; sem
      PII; nunca deixa a VM Off em silêncio.
- [ ] Int32 clamp: nenhum `[math]::Max(0,…)` literal novo no `.ps1`
      (invariante #17; o existente em autoreclaim.ps1:285-286 usa `[int64]0`).

## Mudanças de estado / constantes

**Arquivo:** `internal/civm/civm.go` (bloco `const (...)`)

```go
// Linha 90 — RF-1 / DT-2. Trocar 0 pelo valor MEDIDO no Slice 1 (exemplo p100=10):
DefaultHostVolumeScratchBudgetGB = 11 // pior scratch high-water medido (10) + 1; 5 medições anexadas <DATA>
// Linha 129 — RF-3 / DT-3. Trocar 0 pelo valor MEDIDO no Slice 3 (exemplo p95 RSS=1850):
DefaultAdmitHeavyMaxMB = 2048 // p95 RSS heavy medido (1850) + ~200 margem; tabela RSS anexada <DATA>
```

- **Quem lê:** `ScratchBudgetGB` → **hoje: ninguém** (gap). Nenhum Go o lê;
  `EmergencyAdmits` (que o usaria) não tem caller; e o `autoreclaim.ps1`/
  `optimize.ps1` recebem `$ScratchBudgetGB` por parâmetro CLI que o `register-*.ps1`
  **não passa** → worker roda com default 0. Para RF-1/RF-2 valerem, threadar
  `-ScratchBudgetGB` no `register-*.ps1` (ou config lida pelo worker).
  `HeavyMaxMB` → **wired de fato**: `cmd/civmctl/admit.go:97,145` (`effectiveMemMB`,
  cgroup `MemoryMax`).
- **Invariante:** `HardFloor(1) < Headroom(8) < Pressure(25)`; `ScratchBudget ≥ 0`;
  emergência só habilita com `ScratchBudget > 0`; `HeavyMaxMB ≥ 0`.
- **Política Day-0:** ambas as constantes são **commit explícito com as ≥5
  medições anexadas** (Número, não adjetivo). Migração de estado: **N/A — Day-0**.
- **Disciplina Kahneman:** #3 Número não adjetivo · `disciplines/KAHNEMAN-DISCIPLINES.md` ·
  Pergunta: "qual o p100/p95 medido?" · Evidência: 5 linhas de log/RSS no commit ·
  Abort: editar a constante sem as 5 medições.

## Arquivos a CRIAR

**`runbooks/RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md`** (RF-9)

- **Propósito:** runbook único das 5 camadas de auto-limpeza + Day-0 + Kahneman.
- **Requisitos cobertos:** RF-9 (consolida RF-1..8).
- **Conteúdo vinculante (seções):** (a) espiral #106 e o fix DT-1 (Fase 1+2,
  diagrama do VMRS); (b) campanha de medição DT-2/DT-3 (comando + onde lê o log);
  (c) registro Day-0 das 3 tasks + sudoers + `/run/civm` (DT-6, comandos exatos);
  (d) cleanup guest (prune dangling-only, fstrim health DT-4); (e) reaper 409
  (DT-5) + env `GH_TOKEN`; (f) OS hygiene (DT-7) + como mascarar
  `apt-daily-upgrade.timer`; (g) mapa Kahneman por etapa; (h) rollback por camada.
- **Padrão de referência:** `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md`.
- **Testes:** `validate-templates` (links locais resolvem).

**`deploy/apt/51civm-reproducibility`** (RF-7 / DT-7)

- **Propósito:** drop-in `apt.conf.d` aditivo com `Package-Blacklist` de toolchain.
- **Requisitos cobertos:** RF-7.
- **Esqueleto vinculante:**
  ```
  // Reprodutibilidade do runner civm: security updates NUNCA trocam toolchain
  // mid-CI (docs/specs/civm-self-cleaning-runner, RF-7).
  Unattended-Upgrade::Package-Blacklist {
      "gcc.*"; "g\+\+.*"; "clang.*";
      "linux-image.*"; "linux-headers.*"; "linux-modules.*";
      "golang.*"; "docker.*"; "containerd.*";
  };
  ```
- **Dependências externas:** `unattended-upgrades` (já ativo no box).
- **Instalação:** copiado para `/etc/apt/apt.conf.d/51civm-reproducibility` (Day-0,
  procedimento no runbook); idempotente.
- **Testes:** `apt-config dump` no box mostra o blacklist; nenhum lint Go.
- **Disciplina Kahneman:** #3 · Pergunta "qual versão de gcc/docker antes/depois
  de um patch?" · Evidência: `gcc --version`/`docker --version` no runbook ·
  Abort: aparecer `golang`/`docker`/`linux-image` no histórico do apt durante CI.

## Arquivos a MODIFICAR

### `deploy/windows/civm-vhdx-autoreclaim.ps1` (RF-2 / DT-1)

> **⚠ Esqueleto abaixo SUPERADO pelo working tree** (ver callout de reconciliação
> no topo): o código já implementa Fase 1+2, mas com `autoreclaim_post_off_remeasure`
> / `autoreclaim_skip_insufficient_slack_post_off` (WARN, exit 0), **sem**
> `$StopMarginGB`/`autoreclaim_abort_pre_stop_unsafe` e **sem** `$skipOptimize`
> (faz `exit 0` direto). Tratar este esqueleto como o desenho proposto, não o
> estado do código; reconciliar nomes/fluxo no Passo 2.5.

- **O que muda:** (1) novo `param` `$StopMarginGB` (default 2); (2) Fase 1
  relaxada nas linhas 251-272; (3) Fase 2 NOVA após a linha 325 (Off atingido),
  antes do `Mount-VHD` da linha 328; (4) guard `-not $skipOptimize` no `if` do
  Optimize (linha 336).
- **Requisitos cobertos:** RF-2, DT-1.
- **Antes (251-272):** gate único — `ScratchBudget≤0`→`abort_headroom`;
  `(beforeFreeGB-HardFloor)<ScratchBudget`→`abort_insufficient_slack`.
- **Depois (esqueleto vinculante):**
  ```powershell
  # param adicional (junto aos demais, ~linha 47):
  #   [Parameter()][ValidateRange(0,4096)][int]$StopMarginGB = 2,

  # Fase 1 (substitui 251-272):
  $emergency   = $false
  $skipOptimize = $false
  if ($beforeFreeGB -lt $MinHeadroomGB) {
      if ($ScratchBudgetGB -le 0) {
          # INALTERADO: sem medicao -> sem emergencia (zero regressao).
          Write-ReclaimLog -Event 'autoreclaim_abort_headroom' -Level 'ERROR' -Data @{
              v_free_gb = $beforeFreeGB; headroom_gb = $MinHeadroomGB
              reason = 'emergency_disabled_no_budget' }
          $exitCode = 2; exit 2
      }
      # NOVO (DT-1): Fase 1 e GROSSEIRA. So recusa PARAR a VM se nem ha folga
      # para o Stop-VM gracioso. O gate autoritativo e pos-Off (Fase 2). Parar
      # a VM LIBERA o VMRS e quase nao escreve V:, entao parar a
      # HardFloor+StopMargin e seguro; o predito pre-stop subestima por ~VMRS.
      if (($beforeFreeGB - $HardFloorGB) -lt $StopMarginGB) {
          Write-ReclaimLog -Event 'autoreclaim_abort_pre_stop_unsafe' -Level 'ERROR' -Data @{
              v_free_gb = $beforeFreeGB; hard_floor_gb = $HardFloorGB
              stop_margin_gb = $StopMarginGB }
          $exitCode = 2; exit 2
      }
      $emergency = $true
  }

  # ... gap/idle/fstrim/Stop-VM/Wait Off inalterados (288-325) ...

  # Fase 2 NOVA (inserir apos a linha 325, ANTES do Mount-VHD da 328):
  if ($emergency) {
      # DT-1: VMRS ja liberado (VM Off). Medir a folga REAL que o Optimize tera.
      $liveFreeAfterOff = Get-VFreeGB
      Write-ReclaimLog -Event 'autoreclaim_post_stop_measure' -Data @{
          v_free_before_gb    = $beforeFreeGB
          v_free_after_off_gb = $liveFreeAfterOff
          vmrs_release_gb     = [math]::Round($liveFreeAfterOff - $beforeFreeGB, 2) }
      if (($liveFreeAfterOff - $HardFloorGB) -lt $ScratchBudgetGB) {
          Write-ReclaimLog -Event 'autoreclaim_abort_post_stop_insufficient_slack' -Level 'ERROR' -Data @{
              v_free_after_off_gb = $liveFreeAfterOff
              hard_floor_gb       = $HardFloorGB
              scratch_budget_gb   = $ScratchBudgetGB }
          $exitCode = 2
          $skipOptimize = $true   # NAO exit: cai no finally que faz Start-VM (3x)
      }
  }

  # Guard do Optimize (linha 336):
  #   if (-not $skipOptimize -and $PSCmdlet.ShouldProcess($VhdxPath, 'Optimize-VHD -Mode Full')) {
  ```
- **Por quê:** PRD RF-2 — quebra a espiral medindo a folga no instante que importa.
- **Impacto:** muda o contrato de admissão de emergência (refina SPECv3 DT-v3-1 —
  ver DT-9, adendo no SPECv3). Não quebra assinatura Go. Afeta o lint host.
- **Testes:** estender `specv3_reclaim_test.go` (lint do gate, ver §Plano de
  testes) + janela supervisionada (Slice 2). `ps1_safety_test.go` cobre só o
  clamp Int32.
- **Disciplina Kahneman:** #2 + #5 (tabela acima).

### `deploy/windows/civm-vhdx-optimize.ps1` (RF-1 / DT-2)

- **O que muda:** medir o VMRS liberado com leituras **ao vivo `Get-PSDrive V`**
  (NUNCA `Get-VFreeGB`, que aqui lê o JSON de 10 min — ver "Por quê"): (a)
  capturar `$liveFreeBeforeStop = (Get-PSDrive V).Free/1GB` imediatamente antes do
  `shutdown -h now` (~linha 349-351); (b) após `vm_off` (~linha 356) capturar
  `$liveFreeAfterOff = (Get-PSDrive V).Free/1GB`; (c) gravar
  `vmrs_release_gb = $liveFreeAfterOff - $liveFreeBeforeStop` no `optimize_end`
  (~linha 402). O baseline live pós-Off já existe como `$liveFreeBeforeGB`
  (linha 374) e pode ser reutilizado.
- **Requisitos cobertos:** RF-1, DT-2.
- **Antes:** `optimize_end` (linha 402-407) loga `scratch_high_water_gb`; o poll
  de 1 s ao vivo já existe em `civm-vhdx-optimize.ps1:387-395` (loop
  `Wait-Job -Timeout 1`), baseline live em 374.
- **Depois:** `optimize_end` loga `scratch_high_water_gb` **e** `vmrs_release_gb`,
  ambos derivados de `Get-PSDrive V` ao vivo.
- **Por quê:** dá o número que valida a premissa do DT-1 (o VMRS libera ~o
  esperado) e calibra `ScratchBudget`. **Crítico:** em `civm-vhdx-optimize.ps1` a
  função `Get-VFreeGB` (linhas 176-185) lê o snapshot `V:\civm-host-metrics.json`
  de ~10 min — `$vFreeAfterDrain` (linha 332) é exatamente essa leitura stale.
  Usá-la para `vmrs_release` daria a diferença de dois snapshots de 10 min (ruído,
  tipicamente 0), violando DT-v3-2 ("usar JSON de 10 min em vez de live" é abort
  trigger) e o lint `specv3_reclaim_test.go` `TestOptimizeScriptMeasuresScratchHighWater`
  ("Get-PSDrive, never the stale 10-min metrics JSON — red-team Finding 3"). A
  medição que valida o VMRS deve ser ao vivo, igual ao `scratch_high_water_gb`.
- **Impacto:** aditivo (campo de log) + 2 leituras `Get-PSDrive V` ao vivo; sem
  mudança de fluxo.
- **Testes:** lint host em `specv3_reclaim_test.go` (presença de `vmrs_release_gb`
  **e** de duas amostras `Get-PSDrive V` ao redor do shutdown/Off — não só o
  nome do campo, Kahneman #13: campo presente ≠ medição correta); janela (Slice 1,
  5 ciclos com o número live).

### `internal/hostdisk/hostdisk.go` (RF-8 / DT-8)

- **O que muda:** em `Check` (ou função de classificação de `level`), quando
  `r.VHDXBlockSizeBytes > 1048576` → elevar `level` para `warn` com motivo
  `vhdx_block_size_above_1mib` (o campo + render já existem 51-53,219-220).
- **Requisitos cobertos:** RF-8, DT-8.
- **Por quê:** BlockSize alto = UNMAP não honrado = reclaim offline obrigatório.
- **Impacto:** muda o `level` reportado por `civmctl host-disk`/`doctor`; nenhum
  caller quebra (level é enum existente).
- **Testes:** `internal/hostdisk/hostdisk_test.go` — `VHDXBlockSizeBytes=2097152`
  → `level=warn` (negativo) pareado com `=1048576` → não eleva (positivo); reusa o
  padrão de `hostdisk_test.go:49-61,249-263`.

### `internal/hook/hook.go` (RF-4 / DT-4)

- **O que muda:** a action `fstrim` (linha 250) deixa de só rebaixar erro a
  warning silencioso: captura exit/stderr e, quando o FITRIM ioctl falha
  (`Operation not permitted` / `not supported`), grava `fstrim_ineffective: true`
  no record de `hooks.jsonl` do job-completed.
- **Requisitos cobertos:** RF-4, DT-4.
- **Antes:** `commandActionWarn(opts, ctx, "fstrim", "sudo", "fstrim", "-av")`
  (erro → warning, sem sinal estruturado).
- **Depois:** mesma chamada best-effort, mas o resultado alimenta um campo
  estruturado no log de decisão do hook (existência ≠ função — Kahneman #13).
- **Impacto:** aditivo no `hooks.jsonl`; não falha o job (fail-open mantido).
- **Gap correlato (fora do hook, mas crítico para #106):** o `fstrim` do
  `autoreclaim.ps1:298-300` é **fail-hard** — `if ($trim.ExitCode -ne 0) { throw }`
  ANTES do `Stop-VM` (a doc-header linha 18 declara "fstrim must succeed before
  Stop-VM"). Se no box o `sudo -n fstrim -av` sai com código ≠0 (EPERM observado),
  o autoreclaim aborta em `autoreclaim_error` e o gate pós-Off (RF-2) **nunca
  roda** — #106 continua bloqueado mesmo com `ScratchBudget=11`. RF-4 deve incluir
  tornar esse `fstrim` best-effort (como `optimize.ps1:345-347`, que só loga
  `fstrim_warn`) ou tratar EPERM/EOPNOTSUPP como não-fatal. Sem isso, a saúde do
  fstrim é só observada, não desbloqueada.
- **Testes:** `internal/hook/hook_test.go` — `RunFn` simula fstrim com stderr
  `Operation not permitted` → record traz `fstrim_ineffective:true`; pareado com
  fstrim exit 0 → `false`.

### `internal/doctor` (RF-4/RF-7 — checks read-only)

- **O que muda:** dois checks novos no `civmctl doctor`: (a) `TRIM_EFFECTIVE`
  (`lsblk -D` → DISC-MAX>0 e/ou delta FileSize; warn se discard morto); (b)
  `OS_REBOOT_REQUIRED` (existência de `/var/run/reboot-required` → warn).
- **Requisitos cobertos:** RF-4, RF-7.
- **Por quê:** expõe discard morto e reboot pendente sem mascarar.
- **Impacto:** novos labels no relatório do `doctor` (read-only).
- **Testes:** unit com `RunFn`/`StatFn` injetados (padrão do pacote).

### `internal/runreaper/runreaper.go` (RF-5 / DT-5)

- **O que muda:** `cancelRun` (284-295) classifica falha como benigna quando
  `*exec.ExitError.Stderr` contém `HTTP 409`/`HTTP 422`/`Conflict`/`already`,
  retornando um sentinel `ErrAlreadyTransitioned`. `reapRepo` (148-155) trata
  `errors.Is(err, ErrAlreadyTransitioned)` como evento `already-transitioned`
  (info), **sem** `report.Cancelled++`, **sem** `report.Exit = maxInt(...,1)`.
- **Requisitos cobertos:** RF-5, DT-5.
- **Antes:** ambos os POSTs falham → `cancel-failed` warning + `Exit=1`.
- **Depois (esqueleto vinculante):**
  ```go
  // pacote-level:
  var ErrAlreadyTransitioned = errors.New("run already left queued (HTTP 409/422)")

  // em cancelRun, quando ambos os POSTs falham, inspeciona stderr:
  //   var ee *exec.ExitError
  //   if errors.As(err, &ee) && isBenignConflict(string(ee.Stderr)) {
  //       return ErrAlreadyTransitioned
  //   }
  // isBenignConflict: strings.Contains de "HTTP 409"/"HTTP 422"/"Conflict"/"already"

  // em reapRepo (no ramo err != nil da linha 148):
  //   if errors.Is(err, ErrAlreadyTransitioned) {
  //       ev.Reason = "already-transitioned"; ev.Severity = "info"
  //       report.add(ev); continue   // nao conta cancelled, nao sobe exit
  //   }
  ```
- **Por quê:** 409 = run já saiu de `queued`; benigno (o `SuccessExitStatus=0 1`
  já evitava flap, mas o JSON marcava `cancel-failed` enganoso).
- **Impacto:** muda só a classificação/observabilidade; o efeito (run cancelada
  ou já em transição) é o mesmo.
- **Testes:** `runreaper_test.go` — `CancelFn` retornando `*exec.ExitError` com
  `Stderr="HTTP 409 ..."` → evento `already-transitioned` info, `Cancelled=0`,
  `Exit=0`; pareado com erro `HTTP 403` → `cancel-failed` warning, `Exit=1`.

## Arquivos a DELETAR

| Arquivo | Motivo |
| --- | --- |
| — | Nenhum (Day-0; nada de shim/compat para remover). |

## Observabilidade

**Host — `V:\civm-hyperv-maintenance.log` (JSON por evento):**

| Evento | Level | Campos |
| --- | --- | --- |
| `autoreclaim_post_off_remeasure` (nome real no código; SPEC dizia `autoreclaim_post_stop_measure`) | Info | `v_free_gb_before_stop`, `live_free_after_off_gb`, `vmrs_release_gb`, `hard_floor_gb`, `scratch_budget_gb` |
| `autoreclaim_skip_insufficient_slack_post_off` (nome real; **WARN, exit 0** — SPEC dizia `autoreclaim_abort_post_stop_insufficient_slack` ERROR exit 2) | Warn | `live_free_after_off_gb`, `hard_floor_gb`, `scratch_budget_gb` |
| ~~`autoreclaim_abort_pre_stop_unsafe`~~ | — | **não existe no código** (sem `$StopMarginGB`) |
| `autoreclaim_optimized` | Info | `scratch_high_water_gb` (o `vmrs_release_gb` fica no `post_off_remeasure`, não aqui) |
| `optimize_end` (RF-1, optimize.ps1) | Info | + `vmrs_release_gb` ao vivo (aditivo) |
| `autoreclaim_vm_left_off` | Critical | `attempts` (inalterado, pior caso) |

**Guest — `hooks.jsonl` / `civmctl`:** `fstrim_ineffective:bool` no record de
job-completed; `civmctl host-disk` → `level` (ok/warn/crit), `stale`,
`vhdx_block_size_bytes`; `civmctl doctor` → `TRIM_EFFECTIVE`,
`OS_REBOOT_REQUIRED`; `runreaper` → evento `already-transitioned`.

## Contratos e documentação viva

| Documento | Atualização | Motivo |
| --- | --- | --- |
| `internal/civm/civm.go` | Alterar (90, 129) | constantes que gateiam (RF-1/RF-3) — sync rule |
| `deploy/windows/civm-vhdx-autoreclaim.ps1` + `specv3_reclaim_test.go` | Alterar | gate de duas fases (RF-2); lint do gate vive no specv3_reclaim_test, não no ps1_safety_test |
| `deploy/windows/civm-vhdx-optimize.ps1` | Alterar | `vmrs_release_gb` (RF-1) |
| `internal/hostdisk/hostdisk.go` | Alterar | BlockSize warn (RF-8) |
| `internal/hook/hook.go` | Alterar | fstrim health (RF-4) |
| `internal/doctor` + `internal/runreaper/runreaper.go` | Alterar | RF-4/RF-7/RF-5 |
| `deploy/apt/51civm-reproducibility` | Criar | RF-7 |
| `runbooks/RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md` | Criar | RF-9 |
| `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md` + `MULTI-PROJECT-RUNNER.md` §Disk | Alterar | gate de duas fases + OS hygiene |
| `docs/specs/host-volume-reclamation/SPECv3.md` | Alterar (adendo) | DT-9: DT-1 refina DT-v3-1 |
| `docs/specs/civm-self-cleaning-runner/IMPL.md` | Criar | registro do que foi feito |
| `disciplines/INVARIANTS.md` | Alterar / N/A | se Fase 2 virar invariante |
| `README.md`/`AGENTS.md`/`CODEX.md`/`rules/*.md` | Alterar / N/A | sync rule se contrato mudar |

## Ordem de implementação

1. **Slice 0 — baseline + Day-0 (RF-6/DT-6).** SSH read-only (`df -h`,
   `docker system df`, `civmctl host-disk --json`, tail `hooks.jsonl`); registrar
   as 3 tasks (`register-civm-host-metrics.ps1`, `register-civm-vhdx-optimize.ps1`,
   `register-civm-vhdx-autoreclaim.ps1`, elevado, `/RU SYSTEM /RL HIGHEST`);
   instalar `sudoers.d/civm-cleanup` + `/run/civm`. **Go/no-go:**
   `Get-ScheduledTask`=Ready + `host-disk`=ok em ≤10 min.
2. **Slice 1 — medição (RF-1/DT-2).** `civm-vhdx-optimize.ps1` (vmrs_release) +
   ≥5 ciclos supervisionados; setar `civm.go:90`.
3. **Slice 2 — gate de duas fases (RF-2/DT-1).** **Passo 2.5 obrigatório.** MOD
   `autoreclaim.ps1` + `specv3_reclaim_test.go` (lint do gate); janela supervisionada.
4. **Slice 3 — `HeavyMaxMB` (RF-3/DT-3).** ≥5 jobs heavy medidos; setar
   `civm.go:129`.
5. **Slice 4 — fstrim health + BlockSize warn (RF-4/RF-8).** MOD `hook.go`,
   `doctor`, `hostdisk.go` + testes.
6. **Slice 5 — reaper 409 (RF-5).** MOD `runreaper.go` + teste.
7. **Slice 6 — OS hygiene (RF-7).** CREATE `deploy/apt/51civm-reproducibility` +
   `doctor` reboot-required; instalar no box.
8. **Slice 7 — runbook + reconciliação (RF-9/DT-9).** CREATE runbook + adendo no
   SPECv3 + sync rule.
9. **Slice 8 — validação live 3 dias + fechar #106/#113.**

## Plano de testes

**Guest (Go):**

- `internal/civm`: invariantes das constantes; `EmergencyAdmits(liveFree,
  hardFloor, budget)` (budget=0 desabilita; folga exata; folga insuficiente).
- `internal/hostdisk`: BlockSize>1 MiB → warn (negativo) + ==1 MiB → ok (positivo).
- `internal/hook`: fstrim `Operation not permitted` → `fstrim_ineffective:true`;
  exit 0 → `false`.
- `internal/runreaper`: 409 → `already-transitioned` info/exit 0; 403 →
  `cancel-failed`/exit 1.
- `internal/doctor`: `TRIM_EFFECTIVE`/`OS_REBOOT_REQUIRED` com fns injetadas.

**Host (PowerShell — lint + janela):**

- `internal/hostdisk/specv3_reclaim_test.go` (onde já vivem
  `TestAutoreclaimAdmissionGate`, `TestOptimizeScriptMeasuresScratchHighWater`,
  `TestReclaimersShareCanonicalLock`): estender para exigir que o autoreclaim
  contenha os tokens **reais do código** `autoreclaim_post_off_remeasure` e
  `autoreclaim_skip_insufficient_slack_post_off` (NÃO os nomes do esqueleto antigo
  `autoreclaim_post_stop_measure`/`autoreclaim_abort_post_stop_insufficient_slack`,
  que não existem) e a re-leitura `Get-VFreeGB` após `Wait-VMState Off`; assertar
  o token de skip como **evento emitido**, não substring em comentário (o
  `autoreclaim_abort_insufficient_slack` antigo só sobrevive em comentário —
  falso-verde); que **não** contenha `Stop-Job` no caminho do Optimize (já coberto
  por `TestAutoreclaimAdmissionGate`); que contenha o lock canônico (já coberto por
  `TestReclaimersShareCanonicalLock`); e que o optimize contenha `vmrs_release_gb`
  + duas amostras `Get-PSDrive V` ao redor do shutdown/Off.
- `internal/hostdisk/ps1_safety_test.go` (escopo: APENAS o clamp Int32,
  invariante #17): permanece garantindo que nenhum `[math]::Max(0,…)` literal novo
  entre nos `.ps1`. Não é o arquivo dos tokens de gate/lock/scratch acima.
- Janela supervisionada (Slice 1): 5 ciclos com `scratch_high_water_gb` +
  `vmrs_release_gb` logados; `ScratchBudgetGB` definido por commit.
- Janela supervisionada (Slice 2): `V:`≈6.6 GB + `ScratchBudget=11` → para,
  re-mede ~14.6 GB, admite e completa; folga pós-Off insuficiente → pula Optimize,
  religa VM; dois reclaimers → 2º `reclaim_skip_other_active`; `Start-VM` falha
  simulada → 3 tentativas → CRITICAL; VM nunca fica Off.

**Manuais (evidência das etapas críticas):**

- 5 linhas `optimize_end` com `scratch_high_water_gb`+`vmrs_release_gb` (Slice 1).
- log `autoreclaim_post_off_remeasure` (nome real) mostrando o VMRS liberado (Slice 2).
- tabela de pico RSS dos 5 jobs heavy (Slice 3).
- `civmctl host-disk --json` com `level=ok` pós-registro (Slice 0).

## Checklist de validação

**Guest (Go):**

- [ ] `gofmt -w ./...`
- [ ] `golangci-lint run -c .golangci.yml ./...` (0 issues)
- [ ] `go vet ./...`
- [ ] `go test ./... -race -count=1`
- [ ] `go test -count=1 -cover ./internal/...` (≥80% em civm/hostdisk/runreaper/admit)
- [ ] `govulncheck ./...`
- [ ] `go build -ldflags='-s -w' -o /tmp/civmctl ./cmd/civmctl` (<10 MB)

**Host (PowerShell):**

- [ ] lint host (`specv3_reclaim_test.go`): Fase 2 (tokens reais
      `autoreclaim_post_off_remeasure` + `autoreclaim_skip_insufficient_slack_post_off`
      + re-leitura pós-Off) + sem `Stop-Job` + lock canônico + `vmrs_release_gb` ao
      vivo; `ps1_safety_test.go`: sem clamp Int32 (invariante #17)
- [ ] PSScriptAnalyzer (se disponível)
- [ ] janela: aborta sob baixo headroom sem budget; com budget+Fase 2 admite pós-Off
      e completa; nunca deixa VM Off
- [ ] ≥5 medições coletadas; `ScratchBudgetGB` + `HeavyMaxMB` por commit com evidência

**Docs:**

- [ ] `validate-templates` (links locais resolvem)
- [ ] Sync rule: README ≡ AGENTS ≡ CODEX ≡ rules se contrato/convenção mudou
- [ ] Adendo de reconciliação no `host-volume-reclamation/SPECv3.md` (DT-9)

**Gates cognitivos:**

- [ ] Cada etapa crítica aponta `disciplines/KAHNEMAN-DISCIPLINES.md`
- [ ] Cada etapa crítica tem pergunta obrigatória + evidência mínima + abort trigger
- [ ] Sem linguagem vaga em pontos críticos sem critério observável

## Veredito

**Estado real (auditoria 2026-06-06): RF-1 e RF-2 já estão no working tree
(não commitados), antecipando este SPEC e divergindo dele** — ver a callout de
reconciliação no topo. O Passo 2.5 (red-team), obrigatório por mutar
`Stop-VM`/`Optimize-VHD`, deve ser feito **sobre o código que já existe** (não
sobre o esqueleto): validar nomes de evento, ausência de `$StopMarginGB`, o `exit
0` em vez de `exit 2`+`$skipOptimize`, e o lint que assertou um token só presente
em comentário. **Contradição a resolver:** o gate de emergência **já está
habilitado** (`ScratchBudgetGB = 11`) **sem** a campanha de 5 ciclos do Slice 1
(optimize.ps1 não foi instrumentado) — ou reabrir o Slice 1 para a medição real,
ou registrar a evidência usada (logs do host + `vmrs_release` único) como exceção
explícita assinada. Os demais RFs (RF-3 `HeavyMaxMB` ainda 0/#113 aberto; 409,
fstrim health, BlockSize warn, OS hygiene, runbook) **não foram implementados** e
seguem de baixo risco. Ordem de fatias **obrigatória**: Slice 0 (Day-0) → Slice 1
(medição) → Slice 2 (gate) — hoje o Slice 2 foi entregue antes do Slice 1 estar
fechado, o que precisa ser reconciliado.
