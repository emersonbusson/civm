---
slug: civm-self-cleaning-runner
title: Runner auto-limpante — SPECv2 (reconciliado com o working tree, pós red-team Passo 2.5)
milestone: —
issues: [106, 113]
supersedes: SPEC.md
---

# SPECv2 — Runner auto-limpante (candidato ativo pós-NO-GO)

> **Regra de saída do Passo 2.5 — registro obrigatório.**
>
> **(a) SPEC.md foi auditado** (red-team adversarial, 2026-06-06) **sobre o código
> no working tree** (não sobre o esqueleto no papel): o gate de duas fases do
> autoreclaim e a constante `ScratchBudgetGB=11` já estão landados (não
> commitados). Passo 2.5 deu **NO-GO no SPEC.md** porque o SPEC.md está
> materialmente desatualizado vs o código e porque o caminho privilegiado tem
> modo de falha silencioso (fstrim fail-hard sem sudoers).
>
> **(b) Findings bloqueantes endereçados** (cada um com DT concreto, rastreável por
> RF, abort-trigger numérico + evidência mínima + link Kahneman):
>
> | # | Finding bloqueante (SPEC.md) | Endereçado por |
> | --- | --- | --- |
> | B1 | `fstrim` fail-hard no autoreclaim aborta ANTES do `Stop-VM` e torna o fix #106 inerte e silencioso (autoreclaim.ps1:301-303 vs optimize.ps1:344-346); **a sudoers versionada não concede NOPASSWD a `/usr/sbin/fstrim`**, então `sudo -n fstrim` falha EPERM no caminho SYSTEM | **DT-4** (best-effort EPERM-tolerante, reconcilia divergência) + **DT-6** (sudoers NOPASSWD fstrim como pré-req Day-0 go/no-go + validação `sudo -n fstrim -av`) |
> | B2 | `ScratchBudgetGB=11` habilitado **sem** a campanha de ≥5 medições que o abort-trigger Kahneman do próprio SPEC exige, **sem exceção registrada** (viola Day-0) | **DT-2** (exceção Day-0 explícita: razão + deadline + rollback numérico + evidência; Slice 1 reaberto; o `autoreclaim_post_off_remeasure` vira o coletor da campanha em produção) |
> | B3 | SPEC.md materialmente stale: afirma fix #106 INERTE/default 0 (falso — worker default=11), afirma lint falso-verde em token removido (falso — o teste já assere tokens reais), e adia a escolha de nomes de evento canônicos | **§Reconciliação SPEC↔código↔lint** (este doc) + **DT-1** (nomes canônicos fixados + lint endurecido) + **DT-11** (drift const↔ps1 = hygiene MEDIUM, não "gate inerte") |
>
> **(c) SPECv2 é o candidato ativo para nova auditoria.** SPEC.md é preservado como
> registro do estado auditado; **toda divergência SPEC.md ↔ código foi resolvida a
> favor do código real** (o gate está LIVE) salvo onde o código tem defeito (B1),
> caso em que SPECv2 manda corrigir o código. Promover a IMPL só após nova passada
> de Passo 2.5 sobre este SPECv2.

> Tradução do `docs/specs/civm-self-cleaning-runner/PRD.md` em mudanças exatas no
> repo. Rastreável por `RF-N`. Decisão central: **RF-2 (gate autoritativo
> pós-`Stop-VM`)** refina `host-volume-reclamation/SPECv3` DT-v3-1 — por mutar
> `Stop-VM`/`Optimize-VHD`, **exige Passo 2.5 (red-team) antes do IMPL**.

---

## Reconciliação SPEC↔código↔lint (substitui o callout stale do SPEC.md:10-54)

Estado **real** do working tree, verificado linha a linha (2026-06-06):

| Afirmação do SPEC.md (stale) | Realidade no código | Veredito |
| --- | --- | --- |
| "O fix do #106 está INERTE no host; worker roda com default 0" | `civm-vhdx-autoreclaim.ps1:53` → `[int]$ScratchBudgetGB = 11`. O `register-*.ps1` não passa o arg, então **o default do worker (11) é o valor efetivo**. A Fase 1 NÃO aborta por `emergency_disabled_no_budget`. | **FALSO.** O gate está **LIVE em 11**. |
| "`EmergencyAdmits` não tem caller de produção → RF-1/RF-2 são no-op" | `EmergencyAdmits` (reclaim.go:19) **não tem caller Go de produção** (só def + `reclaim_test.go`). Mas o gate **não vive em Go** — vive no PowerShell (`autoreclaim.ps1:338`, aritmética `liveFreeAfterOff - HardFloor >= ScratchBudget` inline). | **PARCIALMENTE FALSO.** Gate **PowerShell-autoritativo** (DT-11). Não é no-op. |
| "Threadar `-ScratchBudgetGB` do `register-*.ps1` é fix obrigatório, senão RF-1/RF-2 são no-op" | Não é obrigatório p/ o gate funcionar (o default do worker já é 11). É **hygiene anti-drift** (a const Go e o default do ps1 podem divergir). | **REBAIXADO** a MEDIUM (DT-11). |
| "Lint falso-verde: `specv3_reclaim_test.go` exige `autoreclaim_abort_insufficient_slack`, token só em comentário" | `TestAutoreclaimAdmissionGate` (specv3_reclaim_test.go:79-92) **já assere os tokens reais** (`autoreclaim_post_off_remeasure`, `autoreclaim_skip_insufficient_slack_post_off`, `vmrs_release_gb`, `emergency_reclaim_start`). | **FALSO** para o estado atual. |
| Esqueleto com `$StopMarginGB` / `autoreclaim_abort_pre_stop_unsafe` / `$skipOptimize` / `autoreclaim_post_stop_measure` | **Nenhum existe** no código. Fase 1 só checa `ScratchBudget>0` (`autoreclaim_abort_headroom`, exit 2); skip pós-Off é `exit 0` direto via `finally`. | Esqueleto **descartado**; design relaxado real é canônico (DT-1). |

**Nomes de evento canônicos (estabilidade declarada para observadores — DT-1):**
`autoreclaim_post_off_remeasure`, `autoreclaim_skip_insufficient_slack_post_off`
(WARN, exit 0), `emergency_reclaim_start`, `autoreclaim_optimized`,
`autoreclaim_vm_left_off` (CRITICAL), `autoreclaim_done`. Qualquer renomeação
futura é breaking change de contrato de log e exige sync do lint + runbook.

**Resíduo de lint (hygiene, DT-1):** os checks de token em
`TestAutoreclaimAdmissionGate:79-92` usam `strings.Contains` (substring) — passam
mesmo se o token só aparecer em **comentário**. O check de `Stop-Job` (linhas
98-107) já faz certo (varre linha a linha, pula `#`). Endurecer os checks de token
para assertar **evento emitido** (mesmo padrão do `Stop-Job`) fecha a porta a
falsos-verdes **futuros** (Kahneman #13: existência ≠ função). Não urgente
(hoje os tokens são reais), mas é parte do IMPL de RF-2.

---

## Escopo fechado desta implementação

**Entra agora:**

- RF-1 medição `scratch_high_water_gb`+`vmrs_release_gb` → `ScratchBudgetGB` (#106).
- RF-2 gate de duas fases no `civm-vhdx-autoreclaim.ps1` (re-medição pós-Off).
- RF-3 calibração `HeavyMaxMB` (#113).
- RF-4 saúde do `fstrim` em `hooks.jsonl`+`civmctl doctor`/`host-disk` **+ reconciliar o `fstrim` do autoreclaim para best-effort (DT-4, B1)**.
- RF-5 classificação HTTP 409/422 no `runreaper`.
- RF-6 gate Day-0: registrar 3 tasks + sudoers (**incluindo NOPASSWD fstrim, DT-6/B1**) + `/run/civm` + chave SSH 0600 SYSTEM-only.
- RF-7 OS hygiene: drop-in `Package-Blacklist` + `doctor` reboot-required.
- RF-8 `VHDXBlockSizeBytes > 1 MiB` → `level=warn` em `hostdisk.Check`.
- RF-9 `runbooks/RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md` + reconciliação.
- RF-10 registry pull-through cache local (já landado, `deploy/bin/setup-registry-cache.sh` — fechou a CI #1092; ver IMPL.md §2).

**Fica fora agora:** `warm-images.json`; `civmctl cache-gc` (DT-v3-7); SCSI
re-attach primário e `Convert-VHD -BlockSizeBytes 1MB` one-time (pertencem a
`host-volume-reclamation`); right-sizing do VHDX; mudança de CI dos repos
consumidores.

**Dependências assumidas prontas (`Confirmado no codebase`):** lock canônico
`V:\civm-reclaim.lock` (autoreclaim.ps1:212-227, optimize.ps1:277-292); poll de
scratch ao vivo (optimize.ps1:387-395 e autoreclaim.ps1:370-373); `finally`
Start-VM 3× (autoreclaim.ps1:389-442); `internal/admit` + `memwatchdog` + `idle`;
`runreaper` + timer 5 min; `internal/runner/watchdog.go` auto-restart cap=3;
`hostdisk.Metrics.VHDXBlockSizeBytes`; `deploy/bin/civm-safedelete` +
`deploy/sudoers.d/civm-cleanup`.

## Matriz de rastreabilidade PRD → SPECv2

| PRD | Implementação no SPECv2 |
| --- | --- |
| RF-1 | DT-2 · MOD `civm-vhdx-optimize.ps1` (vmrs_release) · MOD `civm.go:93` · Slice 1 (REABERTO) |
| RF-2 | **DT-1** · `civm-vhdx-autoreclaim.ps1` (Fase 1+2, JÁ LANDADO) · MOD `specv3_reclaim_test.go` (lint endurecido) · Slice 2 |
| RF-3 | DT-3 · MOD `civm.go:132` · Slice 3 |
| RF-4 | DT-4 (**fstrim autoreclaim → best-effort, B1**) · MOD `internal/hook/hook.go` · MOD `internal/doctor` · MOD `autoreclaim.ps1:301-303` · Slice 4 |
| RF-5 | DT-5 · MOD `internal/runreaper/runreaper.go` · Slice 5 |
| RF-6 | DT-6 (**+ NOPASSWD fstrim + chave SSH 0600 + rollout cancel-safe, B1**) · Slice 0 |
| RF-7 | DT-7 · CREATE `deploy/apt/51civm-reproducibility` · MOD `internal/doctor` · Slice 6 |
| RF-8 | DT-8 · MOD `internal/hostdisk/hostdisk.go` · Slice 4 |
| RF-9 | CREATE `runbooks/RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md` · Slice 7 |
| — | DT-10 (anti-leak Optimize-VHD hard-kill) · DT-11 (drift const↔ps1) · DT-12 (cleanup.go:425) · DT-13 (semântica exit-code) |

## Decisões técnicas

| # | Decisão | Justificativa |
| --- | --- | --- |
| **DT-1** | **Gate de duas fases no autoreclaim; Fase 2 re-mede `Get-VFreeGB` (= `Get-PSDrive V` LIVE, def autoreclaim.ps1:120-126) após `Wait-VMState Off`.** Admissão usa `liveFreeAfterOff − HardFloor ≥ ScratchBudget` (autoreclaim.ps1:338), não `beforeFreeGB` pré-stop. **Fase 1 é deliberadamente relaxada** (só `ScratchBudget>0`; sem `$StopMarginGB`), porque `Stop-VM` é empiricamente space-positivo em `V:` (+8.02 GB observado, 06/06/2026). Nomes de evento canônicos fixados; lint endurecido para assertar evento emitido, não substring. | O VMRS (~8 GB) só libera no Off; medir pós-stop é o número real que o Optimize terá. Resolve a espiral a 6.6 GB **sem adivinhar** o VMRS, **sem realocar o deadlock** a um piso menor e **sem abortar** o Optimize ininterruptível. Refina SPECv3 DT-v3-1. |
| **DT-2** | **`ScratchBudget=11` aceito no Day-0 como EXCEÇÃO EXPLÍCITA registrada** (não como campanha cumprida). Slice 1 reaberto: instrumentar `optimize.ps1` com `vmrs_release_gb` e rodar ≥5 ciclos; **enquanto isso, o `autoreclaim_post_off_remeasure` (que JÁ loga `vmrs_release_gb` a cada emergência) é o coletor da campanha em produção.** Rollback-trigger numérico abaixo. | Número, não adjetivo. O gate é **fail-closed** e re-mede ao vivo a cada run, então um 11 errado **não causa dano** (over-budget → skip+restart; o número autoritativo é o pós-Off). A exceção satisfaz a política Day-0 (razão+deadline+rollback+evidência). |
| **DT-3** | **`HeavyMaxMB = ceil(p95 RSS)+margem`** medido em ≥5 jobs heavy reais. | Fecha #113; admissão deixa de ser cap generoso e passa a enforçar. |
| **DT-4** | **`fstrim` do autoreclaim vira best-effort EPERM/EOPNOTSUPP-tolerante (B1).** Trocar `if ($trim.ExitCode -ne 0) { throw }` (autoreclaim.ps1:302-304) por `Write-ReclaimLog -Event 'autoreclaim_fstrim_warn' -Level 'WARN'` e **seguir para o Stop-VM/Fase 2**; corrigir a doc-header linha 18 (remover "sudo -n fstrim must succeed before Stop-VM"). Saúde do fstrim continua sinal estruturado (`fstrim_ineffective` no hook + check `doctor`). | O `fstrim` é **otimização de yield**, não pré-condição de segurança. A pré-condição de segurança é o gate pós-Off (Fase 2) + o `finally` Start-VM. Um fstrim que falha (EPERM por sudoers ausente) **não pode abortar o reclaim e re-armar a espiral silenciosamente** (Kahneman #13). Reconcilia a divergência autoreclaim(fail-hard)↔optimize(best-effort): **ambos best-effort**. |
| **DT-5** | **Classificar HTTP 409/422 como `already-transitioned` (info).** `cancelRun` detecta via `*exec.ExitError.Stderr`; `reapRepo` não conta como `cancelled` nem sobe exit. | 409 = run já saiu de `queued`/em transição; é benigno, não falha. Para de poluir o JSON do journal. |
| **DT-6** | **Registro Day-0 das 3 tasks + sudoers + `/run/civm` é gate go/no-go, AMPLIADO (B1):** a sudoers deve conter **`emdev ALL=(root) NOPASSWD: /usr/sbin/fstrim`** além do `civm-safedelete`; a chave SSH `C:\ProgramData\civm\ssh\id_ed25519` deve ser **0600 SYSTEM-only** e provisionada no guest (`authorized_keys`); validar `sudo -n fstrim -av` exit 0 **antes** de registrar a task autoreclaim. Rollout cancel-safe por task (copy+Test-Path antes de `schtasks /create`). | Sem `host-metrics.json` nada do reclaim observa o estado; é o bloqueador raiz operacional. **Com DT-4 (best-effort) o fstrim ausente não trava o reclaim, mas reduz yield** — por isso a sudoers fstrim entra como pré-req de **yield**, não de segurança. Defesa em profundidade: as duas pontas (best-effort + sudoers). |
| **DT-7** | **OS patching security-only com `Package-Blacklist` versionado em `deploy/apt/`.** | Patch de segurança não pode trocar gcc/go/docker/kernel mid-CI (reprodutibilidade). |
| **DT-8** | **`VHDXBlockSizeBytes > 1 MiB` eleva `level` para `warn`** (não só render). | BlockSize alto = UNMAP não honrado = reclaim offline obrigatório; é bloqueador, deve gateiar o nível. |
| **DT-9** | **Reconciliação por nota, não SPEC vizinha duplicada.** O IMPL adiciona adendo curto a `host-volume-reclamation/SPECv3.md` apontando que DT-1 (pós-Off) refina DT-v3-1, no mesmo commit do RF-2. | Evita duplicar a árvore de decisão; mantém a fonte única do contrato de reclaim com cross-reference. |
| **DT-10** | **Anti-leak do `Optimize-VHD` em hard-kill.** Hoistar `$optJob` para escopo externo ao `try` (ambos os scripts) e `Remove-Job -Force` no `finally`; **documentar que o guard anti-corrupção real é o Hyper-V recusar `Start-VM` enquanto `CompactVirtualDisk` mantém o `.vhdx` bloqueado** (0x80070020), já honrado pelo watchdog (`register-civm-vhdx-optimize.ps1:218-225`). | `Task Scheduler ExecutionTimeLimit` pode hard-kill o processo no meio do `while` (autoreclaim.ps1:370-373 / optimize.ps1:387-395); `finally` não roda em hard-kill, vazando o background job. A correção (escopo externo) é defensiva; a **integridade** do VHDX é garantida pelo lock do Hyper-V, não pelo cleanup do job. |
| **DT-11** | **Gate é PowerShell-autoritativo; `EmergencyAdmits` (Go) é espelho testável, sem caller de produção — por design.** O drift entre `civm.go:93` (11) e `autoreclaim.ps1:53` (11) é **hygiene anti-drift MEDIUM**: ou threadar `-ScratchBudgetGB`/`-HardFloorGB` do `register-civm-vhdx-autoreclaim.ps1` a partir da const Go, **ou** declarar o default do worker como fonte de verdade e o `civm.go:93` como documentação espelhada (sync rule). **Escolha Day-0:** worker-default é a fonte de verdade; `civm.go:93` espelha; `reclaim_test.go:57` trava ambos em 11. | Remove a alegação enganosa de "gate inerte". O número vive onde o gate executa (PowerShell). O Go documenta e o teste trava — anti-noise. |
| **DT-12** | **Alinhar `cleanup.go:425` ao `hook.go:240-243`.** Trocar `docker system prune -af --volumes` (caminho cron idle) por `docker buildx prune --force --filter until=24h` + `docker image prune -f` (dangling). | `ensureIdle` (cleanup.go:482) é **idle do runner local**, não box-wide; com 8 repos sibling há TOCTOU entre o check e o prune, e `--volumes` pode apagar o volume anônimo do registry-cache se o container for recriado. O `hook.go:225-227` já documenta que `system prune --volumes` corrompe `docker pull` concorrente. |
| **DT-13** | **Semântica de exit-code documentada no header do autoreclaim.** `exit 0` = sucesso OU skip benigno; `exit 1` = erro/`vm_left_off`; `exit 2` = `abort_headroom` (sem budget). **Supervisores leem o JSON `autoreclaim_done` (`vm_started`, `v_final_gb`, `exit_code`), não o código numérico isolado.** | `exit 0` é overloaded (skip vs sucesso); o sinal observável correto é o registro estruturado, não o número. |

## Fronteira de atomicidade e política de rollback

- **Atômico nesta issue:** cada `os.WriteFile`/edição de constante; cada
  `Optimize-VHD` é uma operação Hyper-V única; cada linha de log.
- **Fora da atomicidade (estados parciais aceitos):** o ciclo
  `gate→Stop-VM→re-medir→Optimize→Start-VM` **não** é atômico — o estado
  intermediário "VM Off, Optimize pulado por Fase 2" é aceito e resolvido pelo
  `finally` (Start-VM 3×). **Fronteira explícita (resolve a ambiguidade B3):** a
  ÚNICA seção que não tolera estado parcial é `Stop-VM ... Optimize-VHD`: enquanto
  `CompactVirtualDisk` roda, o Hyper-V mantém o `.vhdx` bloqueado, então nem o
  `finally` nem o watchdog conseguem `Start-VM` (0x80070020) — o **guard
  anti-corrupção** (DT-10), não um bug. Tudo antes do `Stop-VM` é abortável sem
  efeito; tudo depois cai no `finally`. A entrega SSH de métricas é best-effort.
- **Rollback de app:** `civmctl self-upgrade` para o binário anterior; RF-4/5/8
  viram no-op de campo.
- **Rollback de host:** `schtasks /change /tn civm-vhdx-autoreclaim /disable`
  (volta ao caminho supervisionado `civm-vhdx-optimize`); reverter o `.ps1` por
  `git revert` + re-deploy. **Forward-only/janela:** mudanças no `.ps1` só entram
  via janela supervisionada (mutam Stop-VM/Optimize).
- **Rollback de estado:** **N/A — Day-0** (constantes + arquivos efêmeros).
- **Proibido:** zero-fill sob baixo headroom; deixar a VM Off ao fim de qualquer
  caminho; **re-introduzir `throw` no fstrim do autoreclaim** (regressão de B1);
  habilitar `HeavyMaxMB>0` sem as ≥5 medições anexadas.

## Mapa Kahneman por etapa crítica

| Etapa / DT | Disciplina | Link | Pergunta obrigatória | Evidência mínima | Abort trigger (numérico) |
| --- | --- | --- | --- | --- | --- |
| **DT-1 (Fase 2)** | #2 Counterfactual + #5 Availability | `disciplines/KAHNEMAN-DISCIPLINES.md` | "A espiral quebra a 6.6 GB sem re-medir pós-Off?" (não: `5.6 < 11`) | janela: `V:`≈6.6→para→re-mede ~14.6→admite→completa (validado 06/06: 6.59→14.61→31.52) | folga pós-Off `< ScratchBudget` → `autoreclaim_skip_insufficient_slack_post_off`, religa VM |
| **DT-1 (Fase 1 relaxada)** | #5 Availability + #1 WYSIATI | idem | "Parar a VM acima do HardFloor é seguro sem `$StopMargin`?" | `Stop-VM` empiricamente space-positivo (+8.02 GB, 06/06); cadência 30 min mantém `beforeFreeGB` em 6-8 GB | **qualquer run com `beforeFreeGB < HardFloor+2` (=3 GB)** sai do envelope validado → investigar (Stop-VM nunca testado <6 GB) |
| **DT-2 (budget=11 exceção)** | #3 Número não adjetivo + #13 Ilusão de validade | idem | "11 cobre o pior scratch REAL, ou é um palpite de log?" | 1 `vmrs_release`=8.02 GB (06/06) + p100 high-water=10 (logs); **campanha de 5 PENDENTE** | **safety:** qualquer `scratch_high_water_gb ≥ 9` (margem <2 GB) → subir budget + reabrir Slice 1 hard. **premissa VMRS:** p95 `vmrs_release_gb` dos 5 primeiros runs reais `< 3 GB` → baixar efeito / reavaliar Fase 2 |
| **DT-4 (fstrim best-effort)** | #13 Validar propósito | idem | "fstrim que falha deve abortar o reclaim?" (não) | `optimize.ps1:344-346` já é best-effort; reconciliar autoreclaim | fstrim `throw` antes do Stop-VM em qualquer run = regressão de B1 → bloquear merge |
| **DT-6 (Day-0)** | #5 Availability + #13 | idem | "O caminho SYSTEM tem o privilégio que o caminho manual tinha?" | `sudo -n fstrim -av` exit 0 no host; `Get-ScheduledTask`=Ready; `host-disk`=ok | `sudo -n fstrim` ≠ 0 OU task ausente → host-disk `stale` → **bloqueia go** |

## Checklist de segurança (pré-implementação)

- [ ] Exclusão mútua: RF-2 mantém aquisição de `V:\civm-reclaim.lock`
      (FileShare::None) antes de qualquer `Stop-VM`; quem não obtém →
      `reclaim_skip_other_active` exit 0 (autoreclaim.ps1:215-227, inalterado).
- [ ] **TOCTOU idle→Stop-VM (ressalva):** adicionar 2º `civmctl idle-check`
      imediatamente antes do `Stop-VM` (autoreclaim.ps1:~315), pois há janela entre
      o `Wait-GuestIdle` (293) e o `Stop-VM` (315) onde um job pode iniciar.
      Documentar que, sob pressão real de disco, **interromper um job é aceitável**
      (rollback-trigger já no header do `.ps1`).
- [ ] Exec safety: `runreaper`/`doctor` usam `exec.CommandContext` sem shell; o
      `.ps1` não introduz `Invoke-Expression` de input externo.
- [ ] Privilégio: tasks SYSTEM só com direito Hyper-V; sudoers escopado a
      `civm-safedelete` **+ `/usr/sbin/fstrim` (DT-6)** — sem ampliação além disso;
      chave SSH 0600 SYSTEM-only.
- [ ] Fail-closed: Fase 2 sob folga insuficiente **recusa o Optimize** e religa;
      RF-5 só rebaixa 409 (nunca esconde 403/permissão).
- [ ] Secrets: `GH_TOKEN` só via `/etc/civm/run-reaper.env`; nada em
      `deploy/windows/`; chave SSH é host-state, não repo-state.
- [ ] Logs: `slog`/JSON no guest; `V:\civm-hyperv-maintenance.log` no host; sem
      PII; nunca deixa a VM Off em silêncio.
- [ ] Int32 clamp: nenhum `[math]::Max(0,…)` literal novo no `.ps1`
      (invariante #17; o existente em autoreclaim.ps1:281-282 usa `[int64]0`).

## Mudanças de estado / constantes

**Arquivo:** `internal/civm/civm.go` (bloco `const (...)`)

```go
// Linha 93 — RF-1 / DT-2. JÁ EM 11 (era 0). Espelha o default do worker
// (autoreclaim.ps1:53), que é a FONTE DE VERDADE do gate (DT-11). EXCEÇÃO Day-0:
// 11 = p100 scratch (10, logs) + 1, validado por 1 vmrs_release=8.02GB (06/06/2026);
// campanha de 5 medições REABERTA (Slice 1). Rollback: scratch_high_water_gb>=9.
DefaultHostVolumeScratchBudgetGB = 11
// Linha 132 — RF-3 / DT-3. Trocar 0 pelo p95 RSS medido no Slice 3:
DefaultAdmitHeavyMaxMB = 0 // -> ceil(p95 RSS)+margem; tabela RSS anexada <DATA>
```

- **Quem lê `ScratchBudgetGB`:** **o gate vive no PowerShell** (autoreclaim.ps1:338),
  que usa o **default do próprio worker** (linha 53 = 11). O `civm.go:93` é o espelho
  documentado + travado por `reclaim_test.go:57`. `EmergencyAdmits` (reclaim.go) é o
  modelo testável do gate, **sem caller de produção por design** (DT-11) — não é um
  bug, é a separação host(PowerShell)/contrato(Go). `HeavyMaxMB` → **wired de fato**:
  `cmd/civmctl/admit.go` (`effectiveMemMB`, cgroup `MemoryMax`).
- **Invariante:** `HardFloor(1) < Headroom(8) < Pressure(25)`; `ScratchBudget=11`;
  emergência só habilita com `ScratchBudget > 0`; `HeavyMaxMB ≥ 0`
  (`reclaim_test.go:48-68` trava a ordenação e o 11).
- **Política Day-0:** `ScratchBudget=11` é **exceção registrada** (DT-2), não
  campanha cumprida; `HeavyMaxMB` exige as ≥5 medições antes de sair de 0.
- **Disciplina Kahneman:** #3 + #13 · Pergunta: "11 cobre o pior scratch real?" ·
  Evidência: 1 medição + log histórico (campanha pendente) · Abort:
  `scratch_high_water_gb ≥ 9`.

## Arquivos a CRIAR

**`runbooks/RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md`** (RF-9)

- **Propósito:** runbook único das camadas de auto-limpeza + Day-0 + Kahneman.
- **Conteúdo vinculante (seções):** (a) espiral #106 e o fix DT-1 (Fase 1+2,
  diagrama do VMRS, evidência 6.59→14.61→31.52); (b) campanha DT-2/DT-3 (comando +
  onde lê o log; **e que o `autoreclaim_post_off_remeasure` coleta em produção**);
  (c) registro Day-0 das 3 tasks + sudoers (**incl. NOPASSWD fstrim**) + chave SSH
  0600 + `/run/civm` (DT-6, comandos exatos + validação `sudo -n fstrim`);
  (d) cleanup guest (prune dangling-only, fstrim health DT-4); (e) reaper 409
  (DT-5) + env `GH_TOKEN`; (f) OS hygiene (DT-7); (g) mapa Kahneman por etapa;
  (h) **semântica exit-code (DT-13)**; (i) rollback por camada.
- **Padrão de referência:** `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md`.

**`deploy/apt/51civm-reproducibility`** (RF-7 / DT-7)

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
- **Instalação:** copiado para `/etc/apt/apt.conf.d/51civm-reproducibility` (Day-0);
  idempotente. **Testes:** `apt-config dump` mostra o blacklist.

## Arquivos a MODIFICAR

### `deploy/windows/civm-vhdx-autoreclaim.ps1` (RF-2/DT-1 — JÁ LANDADO; RF-4/DT-4 — A FAZER)

- **Estado atual (canônico, NÃO reescrever):** Fase 1 relaxada (257-268,
  `autoreclaim_abort_headroom` exit 2 quando `ScratchBudget≤0`; senão
  `$emergency=true`); `Stop-VM` (315-321, Wait Off 180 s); Fase 2 pós-Off
  (329-347): `autoreclaim_post_off_remeasure` (331, com `vmrs_release_gb`),
  admite via `($liveFreeAfterOff - $HardFloorGB) -ge $ScratchBudgetGB` (338),
  senão `autoreclaim_skip_insufficient_slack_post_off` (339, WARN exit 0);
  Optimize com poll ao vivo sem `Stop-Job` (358-377); `finally` Start-VM 3×
  (389-442) + `autoreclaim_done` (437-441).
- **O que MUDA (DT-4/B1):** linhas 302-304 — trocar
  `if ($trim.ExitCode -ne 0) { throw "fstrim failed..." }` por
  `if ($trim.ExitCode -ne 0) { Write-ReclaimLog -Event 'autoreclaim_fstrim_warn' -Level 'WARN' -Data @{ exit_code = $trim.ExitCode; output = $trim.Output } }`
  e **prosseguir** para o `Stop-VM`. Corrigir doc-header linha 18.
- **O que MUDA (DT-10):** hoistar `$optJob` para `$null` antes do `try` e
  `if ($null -ne $optJob) { Remove-Job -Job $optJob -Force -ErrorAction SilentlyContinue }`
  no `finally`.
- **O que MUDA (ressalva TOCTOU):** 2º `Wait-GuestIdle`/`idle-check` imediatamente
  antes do `Stop-VM` (315).
- **O que MUDA (DT-13):** bloco `.SYNOPSIS`/`.NOTES` documenta exit 0/1/2 e que o
  sinal autoritativo é o JSON `autoreclaim_done`.
- **Testes:** `specv3_reclaim_test.go` — endurecer `TestAutoreclaimAdmissionGate`
  para assertar os tokens como **evento emitido** (linha não-comentário, padrão do
  check de `Stop-Job`), e adicionar assert de que o autoreclaim **não contém
  `throw` no caminho do fstrim** (B1 regressão-guard).
- **Disciplina Kahneman:** #2 + #5 + #13 (tabela acima).

### `deploy/windows/civm-vhdx-optimize.ps1` (RF-1 / DT-2)

- **O que muda:** medir o VMRS liberado com leituras **ao vivo `Get-PSDrive V`**:
  (a) `$liveFreeBeforeStop = (Get-PSDrive V).Free/1GB` imediatamente antes do
  `sudo shutdown -h now` (linha 351); (b) após `vm_off` (356) reusar
  `$liveFreeBeforeGB` (374) como pós-Off; (c) gravar
  `vmrs_release_gb = $liveFreeAfterOff - $liveFreeBeforeStop` no `optimize_end`
  (402-407).
- **Por quê:** dá o número que valida a premissa do DT-1 e calibra `ScratchBudget`.
  **Crítico:** NUNCA usar a `Get-VFreeGB` deste script (176-185) que lê o JSON de
  10 min — usar `Get-PSDrive V` ao vivo (red-team Finding 3), igual ao
  `scratch_high_water_gb`.
- **Testes:** atualizar `TestOptimizeScriptMeasuresScratchHighWater` (hoje só assere
  `scratch_high_water_gb` + `Get-PSDrive V`) para **também** exigir `vmrs_release_gb`
  e **duas** amostras `Get-PSDrive V` ao redor do shutdown/Off (Kahneman #13: campo
  presente ≠ medição correta).

### `internal/hook/hook.go` (RF-4 / DT-4)

- **O que muda:** a action `fstrim` (linha 250) captura exit/stderr e, quando o
  FITRIM ioctl falha (`Operation not permitted` / `not supported`), grava
  `fstrim_ineffective: true` no record de `hooks.jsonl` (best-effort, fail-open).
  **Reforça o par do DT-4/B1:** no host (autoreclaim) o fstrim é best-effort; no
  guest (hook) a inefetividade vira sinal estruturado.
- **Testes:** `RunFn` simula fstrim com stderr `Operation not permitted` → record
  traz `fstrim_ineffective:true`; pareado com exit 0 → `false`.

### `internal/cleanup/cleanup.go` (RF-4 / DT-12)

- **O que muda:** `dockerPrune` (411-433, caminho cron idle) troca
  `docker system prune -af --volumes` (425) por
  `docker buildx prune --force --filter until=24h` + `docker image prune -f`,
  alinhando ao `hook.go:240-243` e ao comentário `hook.go:225-227`.
- **Por quê:** `ensureIdle` é idle do runner local, não box-wide (8 repos); o
  `--volumes` pode apagar o volume anônimo do registry-cache (IMPL.md §2).
- **Testes:** `cleanup_test.go:455-485` — assertar o novo comando, não
  `system prune --volumes`; pareado com o caminho host-busy (`dockerPruneSafe`).

### `internal/hostdisk/hostdisk.go` (RF-8 / DT-8)

- `VHDXBlockSizeBytes > 1048576` → `level=warn` motivo `vhdx_block_size_above_1mib`.
- **Testes:** `=2097152` → `warn` (negativo) pareado com `=1048576` → ok (positivo).

### `internal/doctor` (RF-4/RF-7) e `internal/runreaper/runreaper.go` (RF-5/DT-5)

- `doctor`: checks `TRIM_EFFECTIVE` (`lsblk -D` DISC-MAX>0) e `OS_REBOOT_REQUIRED`.
- `runreaper`: `ErrAlreadyTransitioned` quando stderr contém `HTTP 409`/`HTTP 422`/
  `Conflict`/`already`; `reapRepo` trata como `already-transitioned` (info), sem
  `Cancelled++`, sem subir exit. **Testes:** 409 → info/exit 0; 403 →
  `cancel-failed`/exit 1 (pareado, Kahneman #13).

## Observabilidade (campos/eventos REAIS do código)

**Host — `V:\civm-hyperv-maintenance.log` (JSON por evento):**

| Evento | Level | Campos |
| --- | --- | --- |
| `emergency_reclaim_start` / `autoreclaim_start` | Info | `v_free_gb_before`, `gap_gb`, `emergency`, `scratch_budget_gb` |
| `autoreclaim_post_off_remeasure` | Info | `v_free_gb_before_stop`, `live_free_after_off_gb`, `vmrs_release_gb`, `hard_floor_gb`, `scratch_budget_gb` |
| `autoreclaim_skip_insufficient_slack_post_off` | **Warn (exit 0)** | `live_free_after_off_gb`, `hard_floor_gb`, `scratch_budget_gb` |
| `autoreclaim_fstrim_warn` (NOVO, DT-4) | Warn | `exit_code`, `output` |
| `autoreclaim_optimized` | Info | `file_size_gb_before/after`, `reclaimed_gb`, `v_free_gb_after`, `scratch_high_water_gb` |
| `optimize_end` (optimize.ps1, RF-1) | Info | + `vmrs_release_gb` ao vivo (aditivo, DT-2) |
| `autoreclaim_vm_left_off` | Critical | `attempts` (pior caso) |
| `autoreclaim_done` | Info | `vm_started`, `v_final_gb`, `exit_code` (sinal autoritativo — DT-13) |

**Guest — `hooks.jsonl` / `civmctl`:** `fstrim_ineffective:bool`; `host-disk` →
`level` (ok/warn/crit), `stale`, `vhdx_block_size_bytes`; `doctor` →
`TRIM_EFFECTIVE`, `OS_REBOOT_REQUIRED`; `runreaper` → `already-transitioned`.

## Ordem de implementação

1. **Slice 0 — baseline + Day-0 (RF-6/DT-6).** SSH read-only; instalar
   `sudoers.d/civm-cleanup` (**+ NOPASSWD fstrim**) + chave SSH 0600 + `/run/civm`;
   **validar `sudo -n fstrim -av` exit 0**; registrar as 3 tasks (cancel-safe,
   copy+Test-Path antes do `schtasks /create`). **Go/no-go:** `Get-ScheduledTask`=
   Ready + `host-disk`=ok + `sudo -n fstrim`=0 em ≤10 min.
2. **Slice 1 — medição (RF-1/DT-2) — REABERTO.** `optimize.ps1` (vmrs_release) +
   ≥5 ciclos supervisionados; reconciliar `civm.go:93` (já em 11) com a tabela.
3. **Slice 2 — gate de duas fases (RF-2/DT-1) — JÁ LANDADO; falta DT-4/DT-10/lint.**
   MOD `autoreclaim.ps1` (fstrim best-effort + anti-leak + 2º idle-check) +
   `specv3_reclaim_test.go` (lint endurecido). **Passo 2.5 sobre SPECv2.**
4. **Slice 3 — `HeavyMaxMB` (RF-3/DT-3).** ≥5 jobs heavy medidos; `civm.go:132`.
5. **Slice 4 — fstrim health + BlockSize warn + cleanup.go:425 (RF-4/RF-8/DT-12).**
6. **Slice 5 — reaper 409 (RF-5/DT-5).**
7. **Slice 6 — OS hygiene (RF-7/DT-7).**
8. **Slice 7 — runbook + reconciliação (RF-9/DT-9).**
9. **Slice 8 — validação live 3 dias + fechar #106/#113.**

> **Exceção de ordem (registrar no commit/PR — B2/R12):** o Slice 2 (gate) foi
> entregue ANTES do Slice 1 (medição) estar fechado, por intervenção de emergência
> (V: a 6.59 GB, CI #1092 quebrada — ver IMPL.md). Racional: quebrar a espiral
> agora > ordem formal; o gate é fail-closed e re-mede ao vivo, então não depende da
> campanha para ser seguro. Slice 1 segue REABERTO; a auditoria futura acha aqui o
> racional do número 11.

## Plano de testes

**Guest (Go):**

- `internal/civm`: invariantes + `EmergencyAdmits` (budget=0 desabilita; folga
  exata; insuficiente) — `reclaim_test.go` (já verde).
- `internal/hostdisk`: BlockSize>1 MiB → warn pareado com ==1 MiB → ok.
- `internal/hook`: fstrim `Operation not permitted` → `fstrim_ineffective:true`;
  exit 0 → `false`.
- `internal/cleanup`: `dockerPrune` usa buildx+image prune, não
  `system prune --volumes` (DT-12).
- `internal/runreaper`: 409 → info/exit 0; 403 → `cancel-failed`/exit 1.
- `internal/doctor`: `TRIM_EFFECTIVE`/`OS_REBOOT_REQUIRED` com fns injetadas.

**Host (PowerShell — lint + janela):**

- `specv3_reclaim_test.go`: tokens reais como **evento emitido** (não substring) —
  `autoreclaim_post_off_remeasure`, `autoreclaim_skip_insufficient_slack_post_off`,
  `vmrs_release_gb`, `emergency_reclaim_start`; re-leitura pós-Off; **sem `Stop-Job`**
  (já coberto); **sem `throw` no fstrim** (DT-4/B1 guard); lock canônico; optimize
  com `vmrs_release_gb` + 2 amostras `Get-PSDrive V`.
- `ps1_safety_test.go`: sem clamp Int32 (invariante #17).
- Janela (Slice 1): 5 ciclos `scratch_high_water_gb` + `vmrs_release_gb`.
- Janela (Slice 2): `V:`≈6.6 + `ScratchBudget=11` → para, re-mede ~14.6, admite,
  completa; folga pós-Off insuficiente → pula Optimize, religa; dois reclaimers →
  2º `reclaim_skip_other_active`; `Start-VM` falha simulada → 3 tentativas →
  CRITICAL; **fstrim EPERM simulado → `autoreclaim_fstrim_warn`, segue para
  Stop-VM** (DT-4); VM nunca fica Off.

## Checklist de validação

**Guest (Go):** `gofmt`; `golangci-lint` (0); `go vet`; `go test -race -count=1`;
cobertura ≥80% em civm/hostdisk/runreaper/admit/cleanup; `govulncheck`;
`go build -ldflags='-s -w'` (<10 MB).

**Host (PowerShell):** lint host (tokens-como-evento + sem `Stop-Job` + sem `throw`
no fstrim + lock + `vmrs_release_gb`); `ps1_safety_test.go`; janela (aborta sem
budget; admite pós-Off; fstrim EPERM não aborta; nunca deixa VM Off);
**`sudo -n fstrim -av` exit 0 no host antes de registrar a task** (DT-6).

**Docs:** `validate-templates`; sync rule; adendo de reconciliação no
`host-volume-reclamation/SPECv3.md` (DT-9).

**Gates cognitivos:** cada etapa crítica aponta `KAHNEMAN-DISCIPLINES.md`, com
pergunta + evidência mínima + abort-trigger **numérico**; sem linguagem vaga.

## Veredito

**SPEC.md → NO-GO** (Passo 2.5). Motivos bloqueantes: (B1) o caminho privilegiado
SYSTEM tem modo de falha silencioso — `fstrim` fail-hard (autoreclaim.ps1:302-304)
+ sudoers versionada sem NOPASSWD fstrim → `sudo -n fstrim` falha EPERM → o fix
#106 (Fase 2) nunca roda, mas o operador vê o gate "existir" (Kahneman #13); (B2)
`ScratchBudget=11` habilitado sem campanha nem exceção registrada (viola Day-0);
(B3) SPEC.md materialmente stale vs working tree.

**SPECv2 → candidato ativo.** Endereça B1 (DT-4 best-effort + DT-6 sudoers/validação),
B2 (DT-2 exceção Day-0 com rollback numérico + Slice 1 reaberto + coletor em
produção) e B3 (reconciliação SPEC↔código↔lint + DT-1 nomes canônicos + DT-11
drift rebaixado). O mecanismo `Stop→Optimize→Start` foi validado end-to-end
**uma vez** (manual, 6.59→14.61→31.52, 06/06/2026); falta para o GO: rodar o
caminho **automatizado** (SYSTEM task) com a sudoers correta, fechar a campanha
de 5 medições (ou consumir a exceção até os triggers), e aplicar DT-4/DT-6/DT-10/
DT-12 + lint endurecido. **Re-auditar este SPECv2 no próximo Passo 2.5.**
