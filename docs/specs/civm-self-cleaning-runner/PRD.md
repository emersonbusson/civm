---
slug: civm-self-cleaning-runner
title: Runner auto-limpante — guest + host + admissão + reaper + OS hygiene, zero intervenção manual
milestone: —
issues: [106, 113]
---

# PRD — Runner auto-limpante: o disco do host nunca mais é apagado à mão

> **Convenção de proveniência** (além das 3 tags SSDV3): itens marcados
> `Observação operacional (auditoria)` são fatos vistos no box ao vivo durante a
> auditoria de 2026-06 mas **não** re-medidos por este documento; o **Slice 0**
> os converte em número medido e versionado (Kahneman #3). Tags oficiais:
> `Confirmado no codebase`, `Confirmado em docs`, `Inferência`.

> **⚠ Reconciliação com o working tree (auditoria 2026-06-06).** Há IMPL
> concorrente **não commitada** (`git status`: `M civm-vhdx-autoreclaim.ps1`,
> `M internal/civm/civm.go`, `M internal/civm/reclaim_test.go`) que já alterou o
> estado descrito abaixo como "problema atual":
> - O gate de emergência **já é de duas fases** em `autoreclaim.ps1` e
>   `DefaultHostVolumeScratchBudgetGB` **já é 11** (`civm.go:93`, era 0). A
>   "espiral de morte" do §Resumo e a citação do gate único
>   (`autoreclaim.ps1:252-260,262`) refletem o **HEAD pré-IMPL**, não o working
>   tree. O evento `autoreclaim_abort_insufficient_slack` (linha 262) foi removido.
> - Os nomes de evento reais do código são `autoreclaim_post_off_remeasure` e
>   `autoreclaim_skip_insufficient_slack_post_off` (**WARN, exit 0**), não os
>   `*_post_stop_*`/`*_abort_*` ERROR/exit-2 deste PRD.
> - RF-1: o valor 11 **não** veio da campanha de 5 ciclos com `vmrs_release_gb` em
>   `optimize.ps1` (não instrumentado); veio de log do host + um `vmrs_release`
>   único (8.02GB). O critério "5 medições anexadas" **não está cumprido**.
> - RF-3 (`HeavyMaxMB`, #113) **não** foi implementado (`civm.go:132` = 0).
> Ver SPEC §Reconciliação para a lista completa e a ação de Passo 2.5.

## Resumo

O runner self-hosted do `civm` (VM Linux `gha-ubuntu-2404` sob Hyper-V, host
Windows, VHDX dinâmico em `V:` de ~119 GB) **enche o disco do host e exige
exclusão manual**. A causa não é um bug isolado: é uma **espiral de morte por
headroom** mais mecanismos de auto-cura **escritos mas não
habilitados/instalados**.

- O VHDX cresce ~11 GB por run de CI; o `Optimize-VHD` offline recupera ~11 GB
  por evento — mas o gate de emergência que dispara o reclaim com `V:` baixo está
  **desabilitado** (`DefaultHostVolumeScratchBudgetGB = 0`, issue **#106**).
  Quando `V:` cruza 8 GB, **todo reclaimer aborta** com
  `autoreclaim_abort_headroom` / `emergency_disabled_no_budget` (exit 2) e o
  espaço fica preso em ~6.6 GB livres. (`Observação operacional (auditoria)`;
  `Confirmado no codebase`: `deploy/windows/civm-vhdx-autoreclaim.ps1:50,252-260`)
- Pior: o gate decide sobre `beforeFreeGB` **pré-`Stop-VM`**, que **não
  contabiliza** os ~8 GB de estado da VM (VMRS) liberados só **quando a VM vai a
  Off**. A folga real para o `Optimize` é sistematicamente subestimada — a
  emergência se recusa a rodar mesmo quando, pós-`Stop-VM`, haveria folga de
  sobra. (`Confirmado no codebase`: `civm-vhdx-autoreclaim.ps1:237,262`)
- A limpeza do guest roda, mas `fstrim -av` retorna `FITRIM ioctl: Operation not
  permitted` no controlador atual (UNMAP não propagado): o discard não flui e o
  VHDX não encolhe online. (`Observação operacional (auditoria)`)
- As 3 Scheduled Tasks do host (`civm-host-metrics`, `civm-vhdx-optimize`,
  `civm-vhdx-autoreclaim`) **existem em `deploy/windows/` mas nunca foram
  registradas** no host; `host-metrics.json` está ausente e `civmctl host-disk`
  reporta `crit/stale`. (`Observação operacional (auditoria)`; `Confirmado no
  codebase`: scripts presentes, sem automação de registro)

**Valor operacional:** o objetivo único e literal deste PRD é **"nunca mais
apagar nada à mão"**. Exige fechar a espiral (#106), calibrar a admissão de
memória que abre a janela de compactação (#113), garantir limpeza do guest
auto-sustentável, cancelar runs órfãs que travam a fila, e aplicar patches de OS
sem quebrar reprodutibilidade — tudo com gates fail-closed e auditável.

## Contexto técnico

Topologia (`Confirmado em docs`: `docs/SSDV3-PROMPTS.md` §topologia;
`runbooks/MULTI-PROJECT-RUNNER.md`): Windows host → Hyper-V → VM Linux guest; `V:`
NTFS hospeda o VHDX dinâmico. Guest Go (`cmd/civmctl/` + `internal/**`) coordena;
host PowerShell (`deploy/windows/*.ps1`) dirige `Stop-VM`/`Optimize-VHD`/`Start-VM`
via Scheduled Tasks SYSTEM; guest↔host por SSH read-only (métricas, idle-check,
fstrim).

**Componentes já existentes (reuso) — `Confirmado no codebase`:**

| Área | Arquivo / símbolo | Papel |
| --- | --- | --- |
| Reclaim host (emergência) | `deploy/windows/civm-vhdx-autoreclaim.ps1` | gate de admissão (252-272), lock canônico `V:\civm-reclaim.lock` (212-224), Stop-VM (319-325), poll de scratch (342-354), `finally` Start-VM 3× (372-402) |
| Reclaim host (supervisionado) | `deploy/windows/civm-vhdx-optimize.ps1` | drain→idle→fstrim→shutdown→`Convert-VhdxToScsi` (def 206, call 359)→Optimize; guarda headroom fase-1 (300-310) e fase-2 pós-drain (332-340) |
| Métricas host | `deploy/windows/civm-host-metrics.ps1` | emite `vhdx_block_size_bytes` (152,178), `V:` free/size, VHDX file/min/max |
| Observabilidade guest | `internal/hostdisk/hostdisk.go` | `Metrics.VHDXBlockSizeBytes` (51-53), render (219-220); `civmctl host-disk` |
| Constantes | `internal/civm/civm.go` | headroom (77), hard-floor (89), **scratch-budget=0 (90)**, pressure (91), min-interval (92), **admit-heavy-max=0 (129)**, auto-restart/hora=3 (46) |
| Cleanup guest (hook) | `internal/hook/hook.go` | job-completed `cleanup(...,false)` (188): buildx prune `until=24h`, image prune `-f` dangling-only (240-243), apt clean, journal vacuum, **fstrim -av (250, best-effort)** |
| Cleanup cron | `internal/cleanup/cleanup.go` | `dockerPruneSafe` host-busy (444-459), `system prune -af --volumes` idle-gated (425), `apt-get clean`+`autoremove -y` idle-gated (461-480), `ensureIdle` (482) |
| Escalada root-owned | `internal/safedelete/` + `deploy/bin/civm-safedelete` + `deploy/sudoers.d/civm-cleanup` | remove leftover root-owned do `_work` sem travar "Complete runner" |
| Watchdog runner | `internal/runner/watchdog.go` | auto-restart por sentinela com teto `AutoRestartPerHour=3` (150,458-459) |
| Admissão memória | `internal/admit/`, `internal/memwatchdog/`, `internal/idle/` | `civmctl admit` (2 slots heavy, cgroup `MemoryMax`), `idle-check` |
| Reaper de runs | `internal/runreaper/runreaper.go` + `deploy/systemd/civmctl-run-reaper.{service,timer}` | cancela runs de PR fechado; force-cancel→cancel (284-295); timer 5 min; `SuccessExitStatus=0 1` |

**Isolamento/concorrência — `Confirmado em docs` (host-volume-reclamation/SPECv3
DT-v3-3) + `Confirmado no codebase`:** os dois reclaimers do host se excluem pelo
lock canônico `V:\civm-reclaim.lock` (FileShare::None) antes de qualquer
`Stop-VM`. O cleanup do guest defere por `dockerlock` (build pesado) e por
`ensureIdle` (job ativo). `civmctl admit` serializa heavy por flock-slots.

**Confirmado em docs:** specs vizinhas reutilizadas — `host-volume-reclamation`
(PRD RF-1..7 + SPECv3 DT-v3-1..7), `civm-runner-reliability` (PRD RF-1..9 +
SPECv2), `civm-disk-watchdog-busy-cleanup` (SPEC issue #70),
`runner-memory-admission` (SPECv4 DT-v4-1..7 + "Pós-merge" HeavyMaxMB),
`multi-project-isolation`.

**Proposto (novo neste PRD):** (N1) campanha de medição que liga o gate de
emergência; (N2) **re-medição autoritativa pós-`Stop-VM` no autoreclaim** — o fix
do VMRS que quebra a espiral; (N3) calibração de `HeavyMaxMB`; (N4)
classificação de HTTP 409 no reaper; (N5) saúde do `fstrim` exposta na admissão;
(N6) gate Day-0 de registro das tasks + sudoers/tmpfiles no box; (N7) política de
OS patching security-only; (N8) runbook consolidado.

## Opção recomendada

**Solução escolhida:** pacote único "runner auto-limpante" em 5 camadas, com o
**fix do VMRS por re-medição direta** como decisão central, reusando ao máximo o
implementado e habilitando o dormente.

1. **Host reclaim self-healing (fecha #106).**
   - **N1 — medir primeiro (executa SPECv3 DT-v3-2).** Rodar ≥5 ciclos
     supervisionados de `Optimize-VHD -Mode Full` com o poll de 1 s já existente
     (`civm-vhdx-optimize.ps1:387-395`, loop `Wait-Job -Timeout 1`), gravando
     `scratch_high_water_gb` **e** `vmrs_release_gb = liveFreeAfterOff −
     liveFreeBeforeStop` por ciclo, **ambos lidos ao vivo via `Get-PSDrive V`**
     (NUNCA `Get-VFreeGB`, que em `optimize.ps1` lê o JSON de 10 min — DT-v3-2
     abort trigger / red-team Finding 3). Definir
     `DefaultHostVolumeScratchBudgetGB = ceil(p100 scratch) + 1` por commit com as
     5 medições anexadas.
   - **N2 — gate autoritativo pós-`Stop-VM` (NOVO; refina SPECv3 DT-v3-1).** Em
     `civm-vhdx-autoreclaim.ps1`, o gate de emergência passa a **duas fases**:
     - *Fase 1 (pré-`Stop-VM`, grosseira):* só recusa parar a VM se
       `beforeFreeGB ≤ HardFloorGB + margemDeStop` (margem pequena, suficiente
       para o `Stop-VM` gracioso, que **libera** espaço e quase não escreve).
     - *Fase 2 (pós-`Stop-VM`, autoritativa):* após `Wait-VMState Off`, re-medir
       `liveFreeAfterOff = Get-PSDrive V` (agora **com o VMRS já liberado**) e
       admitir o `Optimize` **somente** se
       `liveFreeAfterOff − HardFloorGB ≥ ScratchBudgetGB`. Senão → log
       `autoreclaim_abort_post_stop_insufficient_slack`, **pula o Optimize** e o
       `finally` religa a VM (sem dano: paramos e religamos, nada foi compactado).
   - **Por que N2 é o fix real:** a folga que importa para o `Optimize` é a que
     existe **no instante do `Optimize`**, depois do `Stop-VM` liberar o VMRS.
     Medir esse número diretamente (em vez do `beforeFreeGB` pré-stop, que
     subestima por ~8 GB) permite a emergência rodar a 6.6 GB e sair da espiral,
     **sem adivinhar o VMRS** e **sem abortar um Optimize ininterruptível no
     meio**.

2. **Cleanup guest auto-sustentável.** Reusa `hook.go`/`cleanup.go` (prune
   dangling-only, buildx `until=24h`, apt clean+autoremove idle-gated, journal
   vacuum, `system prune -af --volumes` idle-gated, `safedelete` para root-owned).
   **N5:** detectar `fstrim` inefetivo (FITRIM ioctl falha) e expô-lo em
   `civmctl host-disk`/`doctor` como `level=warn` (não erro): discard morto
   significa que o reclaim depende 100% do `Optimize` offline — operador e
   autoreclaim precisam saber. **Ressalva sobre imagens warm:** só o prune do
   *hook* job-completed é dangling-only (`hook.go:240-243`, `-f` sem `-a`,
   preserva todas as imagens tagueadas). O *cron* `civmctl cleanup --execute`
   (`DockerPrune` default `true`, `cleanup.go:119`) roda `docker system prune -af
   --volumes` (`cleanup.go:425`) no ramo idle — `-a` remove imagens **tagueadas
   não-usadas**, e no idle (gate do cron) as bases não têm container vivo → são
   apagadas, recriando o cold-build no próximo job. Trade-off deliberado do código
   atual (reclamar disco no idle), não "warm por construção"; ver §Fora de escopo
   para a decisão sobre proteção de bases.

3. **Admissão/idle coordenando a janela de compactação (relaciona #113).** Reusa
   `civmctl admit` + `memwatchdog` + `idle-check`. **N3:** calibrar
   `DefaultAdmitHeavyMaxMB` medindo pico RSS de jobs heavy reais (executa a nota
   "Pós-merge" de `runner-memory-admission/SPECv4`), fechando #113. Com a admissão
   calibrada, a pressão de memória recusa novos heavy → jobs correntes terminam →
   `idle-check` passa → autoreclaim consegue sua janela de `Stop-VM`+Optimize.

4. **Cancelamento de runs órfãs.** Reusa `runreaper` + timer de 5 min. **N4:**
   classificar HTTP 409 (run já saiu de `queued`/em transição) como
   `already-transitioned` (info, **não** `cancel-failed`), parando de poluir o
   JSON e de subir o exit por corrida benigna.

5. **OS patching sem quebrar reprodutibilidade.** **N7:** `unattended-upgrades`
   security-only (já ativo) + `Package-Blacklist` para toolchain/docker/kernel +
   `apt clean/autoremove` coordenado por idle (reusa `cleanup.aptClean`), de modo
   que um patch de segurança nunca troque gcc/go/docker no meio de um CI.

**Gate transversal Day-0 (N6):** registrar as 3 Scheduled Tasks
(`register-*.ps1`, SYSTEM/HIGHEST) e instalar `sudoers.d/civm-cleanup` +
provisionar `/run/civm` no box. Sem isso, **nenhuma** das camadas de host opera.

**Alternativas descartadas:**

- *"Opção conservadora: não mexer no gate do autoreclaim; só setar
  `ScratchBudget>0`" (proposta de uma perspectiva da auditoria).* Descartada
  porque **não quebra a espiral**: a 6.6 GB livres, `6.6 − 1 = 5.6 < 11` → o gate
  pré-stop ainda aborta. Setar o budget sem corrigir o predito de folga só troca
  `emergency_disabled_no_budget` por `abort_insufficient_slack` — mesmo deadlock,
  mensagem diferente. (`Confirmado no codebase`: `civm-vhdx-autoreclaim.ps1:262`)
- *Estimar "+8 GB de VMRS" e somar a `beforeFreeGB` pré-flight.* Descartada:
  estimar é adivinhar (Kahneman #3). Se num run o VMRS liberar menos, o predito
  superestima e o `Optimize` pode cruzar o `HardFloor` e zerar `V:` (o worst-case
  que o SPECv3 #5 proíbe). A re-medição direta (N2) observa o número real e é
  fail-closed.
- *Baixar o `HeadroomGB` de 8 para um valor menor.* Descartada (SPECv3 Finding 5):
  só **realoca** o deadlock para um piso menor.
- *SCSI+discard online como mecanismo primário.* Mantido como fallback
  "verificar-mas-não-confiar": neste box o UNMAP não é honrado no blocksize atual;
  `Convert-VhdxToScsi` já está no caminho supervisionado, mas o primário é
  `1 MB BlockSize + Optimize offline`. (`Confirmado em docs`:
  host-volume-reclamation PRD §descartadas; `Observação operacional (auditoria)`)
- *VHDX de tamanho fixo / expandir `V:`.* Mitigação estrutural (RF-6 da spec
  vizinha), não auto-cura.

**Trade-offs aceitos:** (a) Fase 1 do gate N2 pode, em casos raros, `Stop-VM` e
depois recusar o `Optimize` (Fase 2), gastando um ciclo de parada/religação — mas
só em janela idle e rate-limited, e a VM volta a Running sem dano. (b) A campanha
de medição (N1/N3) exige janela supervisionada antes de habilitar os gates.

## Requisitos funcionais

- **RF-1 — Campanha de medição do scratch (fecha #106, parte 1).** Coletar
  `scratch_high_water_gb` e `vmrs_release_gb` em ≥5 ciclos de `Optimize-VHD`
  supervisionado e definir `DefaultHostVolumeScratchBudgetGB = ceil(p100) + 1`.
  - *Critério:* commit altera `internal/civm/civm.go:90` de `0` para o valor
    medido, com as 5 linhas de log anexadas; `EmergencyAdmits` unit testa
    `budget>0` habilita / `budget=0` desabilita.
  - *Concorrência:* medição roda no caminho supervisionado sob lock canônico; não
    concorre com autoreclaim.
  - `Confirmado em docs` (SPECv3 DT-v3-2) · `Confirmado no codebase`
    (`civm-vhdx-optimize.ps1:387-395` poll ao vivo; `Get-PSDrive V` 374,388).

- **RF-2 — Gate autoritativo pós-`Stop-VM` no autoreclaim (fecha #106, parte 2;
  NOVO).** O caminho de emergência admite o `Optimize` por
  `liveFreeAfterOff − HardFloorGB ≥ ScratchBudgetGB`, re-medido **após** a VM
  chegar a Off; a Fase 1 pré-stop só impede parar a VM sob `HardFloor + margem`.
  - *Critério:* lint host (`internal/hostdisk/specv3_reclaim_test.go`, onde já
    vive `TestAutoreclaimAdmissionGate`) exige a presença dos eventos **reais do
    código** `autoreclaim_post_off_remeasure` e
    `autoreclaim_skip_insufficient_slack_post_off` (não os nomes `*_post_stop_*`)
    como **eventos emitidos** (não substring em comentário) e da re-leitura
    `Get-VFreeGB` (wrapper live `Get-PSDrive` do autoreclaim, 117-123) após
    `Wait-VMState Off`; janela supervisionada: com
    `V:`≈6.6 GB e `ScratchBudget=11`, a VM para, re-mede ~14.6 GB, admite e
    completa; com folga pós-Off insuficiente, pula o Optimize e religa a VM
    (nunca deixa Off).
  - *Concorrência/fail-closed:* sob incerteza (folga pós-Off não cobre, métrica
    ilegível) → **recusa o Optimize**, religa a VM.
  - `Confirmado no codebase` (lacuna em `civm-vhdx-autoreclaim.ps1:319-336`) ·
    refina `Confirmado em docs` (SPECv3 DT-v3-1).

- **RF-3 — Calibração de `HeavyMaxMB` (fecha #113).** Medir pico RSS de jobs heavy
  reais (≥5 execuções via `civmctl admit --weight heavy --exec`) e setar
  `DefaultAdmitHeavyMaxMB = ceil(p95 RSS) + margem`.
  - *Critério:* commit altera `internal/civm/civm.go:129` de `0` para o valor
    medido com a tabela de RSS anexada; `go test ./internal/admit -race` verde;
    cgroup `MemoryMax` passa a enforçar o cap fixo.
  - `Confirmado em docs` (runner-memory-admission/SPECv4 "Pós-merge", DT-v3-5) ·
    `Confirmado no codebase` (`civm.go:127-129`).

- **RF-4 — Cleanup guest auto-sustentável + saúde do fstrim.** Manter o conjunto
  seguro do hook job-completed e do cron, e expor a inefetividade do `fstrim`.
  - *Critério:* `civmctl host-disk --json`/`doctor` reporta `trim_effective`
    (ou `level=warn` com `fstrim_ineffective`) quando o FITRIM ioctl falha;
    nenhuma regressão no conjunto de prune (dangling-only, sem `-a`, sem `system
    prune --volumes` fora do idle gate).
  - *Concorrência:* prune seguro roda host-busy; delete/`-af --volumes` atrás de
    `ensureIdle`; defer por `dockerlock`.
  - `Confirmado no codebase` (`hook.go:240-250`, `cleanup.go:182-208,425,461-480`)
    · `Observação operacional (auditoria)` (FITRIM ioctl falha).

- **RF-5 — Cancelamento de runs órfãs robusto a 409.** O reaper classifica HTTP
  409/422 como `already-transitioned` (info), não conta como `cancelled` nem como
  `cancel-failed`, e não sobe o exit por isso.
  - *Critério:* unit test com `CancelFn` retornando erro contendo `HTTP 409` →
    `Report` traz evento `already-transitioned` info, `Cancelled=0`, `Exit=0`;
    erro genuíno (403/permissão) segue `cancel-failed` exit 1.
  - `Confirmado no codebase` (`runreaper.go:284-295,148-155`;
    `civmctl-run-reaper.service` `SuccessExitStatus=0 1`).

- **RF-6 — Gate Day-0 de instalação no host (bloqueador go/no-go).** Registrar as
  3 Scheduled Tasks (SYSTEM/HIGHEST) e instalar `sudoers.d/civm-cleanup` +
  `/run/civm`.
  - *Critério:* `Get-ScheduledTask civm-host-metrics|civm-vhdx-optimize|
    civm-vhdx-autoreclaim` retorna `Ready` em ≤10 min; `/var/lib/civm/
    host-metrics.json` aparece no guest; `civmctl host-disk` retorna `level=ok`
    (não `stale`); `sudo -n civm-safedelete` autorizado.
  - *Privilégio:* tasks SYSTEM só com direito Hyper-V; sudoers escopado ao wrapper
    validado.
  - `Confirmado no codebase` (`register-*.ps1`, `deploy/sudoers.d/civm-cleanup`) ·
    `Observação operacional (auditoria)` (não registradas no box).

- **RF-7 — Política de OS patching security-only sem quebrar reprodutibilidade.**
  `Package-Blacklist` para `gcc.*`, `clang.*`, `g++.*`, `linux-image.*`,
  `linux-headers.*`, `golang.*`, `docker.*`; `apt clean/autoremove` coordenado por
  idle (reusa `cleanup.aptClean`); `civmctl doctor` flag de `/var/run/
  reboot-required`.
  - *Critério:* `/etc/apt/apt.conf.d/50unattended-upgrades` contém o blacklist;
    `civmctl doctor` reporta `OS_REBOOT_REQUIRED` quando o arquivo existe; nenhum
    `dist-upgrade`/toolchain no histórico durante CI.
  - `Inferência` (política nova) · `Observação operacional (auditoria)`
    (unattended-upgrades ativo, security-only).

- **RF-8 — Observabilidade do BlockSize como bloqueador de reclamação.**
  `vhdx_block_size_bytes > 1 MiB` vira sinal `warn` em `hostdisk.Check`/`doctor`.
  - *Critério:* `internal/hostdisk` testa que `VHDXBlockSizeBytes > 1048576`
    eleva o `level` para `warn` (campo dedicado), pareado com o positivo
    (`== 1048576` → ok).
  - `Confirmado no codebase` (campo + render existem: `hostdisk.go:51-53,219-220`;
    falta cruzar para `level`).

- **RF-9 — Runbook consolidado + contrato Day-0.** Criar
  `runbooks/RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md` cobrindo as 5 camadas + o mapa
  Kahneman + os procedimentos Day-0, e reconciliar o status de #106/#113.
  - *Critério:* `validate-templates` (links) verde; sync rule respeitada
    (README/AGENTS/CODEX/rules se contrato mudar).
  - `Confirmado em docs` (RF-9 de civm-runner-reliability exige runbook análogo,
    inexistente) · `Confirmado no codebase` (`runbooks/` sem este arquivo).

- **RF-10 — Registry pull-through cache local (anti rate-limit na CI).** Subir um
  `registry:2` como pull-through cache de `docker.io` em `127.0.0.1:5000`, volume
  nomeado `registry-cache-data`, `--restart always`; `daemon.json`
  `registry-mirrors=["http://127.0.0.1:5000"]` substituindo `mirror.gcr.io` — que só
  espelha `library/` e deixa `minio/`, `clamav/`, `evoapicloud/` baterem direto no
  Docker Hub → rate limit anônimo (100/6h/IP) → `compose up --build` falha com
  "No such image" de largada (sintoma no `tenant-isolation-smoke` de #1092).
  O cache serve QUALQUER namespace localmente após a 1ª pull; um warm set (tags
  exatas do `image:` do compose + bases dos `FROM` dos Dockerfiles) pré-popula. Auth
  upstream opcional (`DOCKERHUB_USER`/`DOCKERHUB_TOKEN`) levanta o limite no warm.
  - *Critério:* `docker info` lista o mirror local; após o warm, `compose up --build`
    no runner não puxa do Docker Hub (0 hits) e não falha por "No such image"; o cache
    sobrevive a `docker prune` (volume nomeado + container running + imagem tagged em
    uso — consistente com `hook.go cleanup`, que só poda DANGLING pelo mesmo motivo).
  - `Confirmado no codebase` (`internal/hook/hook.go:228-243` evita `image prune -a`
    exatamente para não apagar `redis/minio/alpine/clamav/postgres` sob um job;
    `deploy/bin/setup-registry-cache.sh` implementa cache+mirror+warm) ·
    `Observação operacional` (auditoria: `daemon.json` só com `mirror.gcr.io`, sem
    `~/.docker/config.json` ⇒ pulls anônimos; runner tinha `redis:8-alpine` enquanto o
    compose pina `redis:8.6.1-alpine3.23`).

## Requisitos não-funcionais

- **Performance.** Caminho online (fstrim/discard) = sem downtime, alvo `V:` ≥ 30
  GB sustentado (`DefaultHostVolumeWarnFreeGB`, `civm.go:71`). Caminho offline
  (`Optimize-VHD`) = minutos com a VM Off, rate-limited a ≥30 min entre eventos
  (`DefaultReclaimMinIntervalMin`, `civm.go:92`). (`Confirmado no codebase`)
- **Segurança / privilégio.** Tasks SYSTEM só com direito Hyper-V mínimo, sem
  rede, sem segredo em `deploy/windows/`. Sudoers escopado a `civm-safedelete`
  (`deploy/sudoers.d/civm-cleanup`). `civmctl` usa `exec.CommandContext` sem
  shell; `.ps1` sem `Invoke-Expression` de input externo. (`Confirmado no
  codebase`; `Confirmado em docs`: security checklist do Passo 2)
- **Observabilidade.** Host: `V:\civm-hyperv-maintenance.log` (JSON por evento) +
  `host-metrics.json`; guest: `slog`/JSON, `hooks.jsonl` (decisão por
  job-started/job-completed), `civmctl host-disk`/`doctor`/`capacity`. Alarmes nos
  pisos 30/10 GB (`civm.go:71-72`). (`Confirmado no codebase`)
- **Escalabilidade.** Por-host/por-VM. O **gap guest×host** é o sinal de saúde da
  reclamação; `vhdx_block_size_bytes` é o sinal de efetividade do discard.
- **Resiliência (worst-case — Kahneman #5).** Host a 6.6 GB → N2 sai da espiral;
  `Optimize` pendura/erra → `finally` Start-VM 3× (`autoreclaim.ps1:372-402`); VM
  ocupada → idle-check N=2 + rate-limit; controlador sem UNMAP → fallback offline
  primário; `fstrim` morto → exposto, não mascarado; reaper 409 → benigno; patch
  de OS → blacklist de toolchain. (`Confirmado no codebase` / `Confirmado em docs`)

## Fluxos

**Happy path — host reclaim self-healing (N2):**

1. Task `civm-vhdx-autoreclaim` (SYSTEM) dispara por agenda/pressão. (host `.ps1`)
2. Adquire `civm-autoreclaim.lock` e o canônico `civm-reclaim.lock`
   (FileShare::None); rate-limit ≥30 min. (host)
3. `beforeFreeGB = Get-VFreeGB`; se `≥ ThresholdGB` → `skip_threshold` exit 0.
4. **Fase 1:** se `beforeFreeGB ≤ HardFloor + margemDeStop` → `abort_pre_stop`
   exit 2 (não para a VM). Senão segue.
5. Checa gap reclamável, `idle-check` (SSH guest), `fstrim`. (guest via SSH)
6. `Stop-VM` → `Wait-VMState Off` (≤180 s) → **VMRS liberado**. (host/Hyper-V)
7. **Fase 2 (já no working tree):** `liveFreeAfterOff = Get-VFreeGB` (live);
   loga `autoreclaim_post_off_remeasure`; se `liveFreeAfterOff − HardFloor <
   ScratchBudget` → `autoreclaim_skip_insufficient_slack_post_off` **WARN exit 0**
   (nome/severidade reais; **pula Optimize**, o `finally` religa a VM via
   `operationStarted`). Senão, `Optimize-VHD -Mode Full` (ininterruptível, poll de
   1 s só telemetria). (host/Hyper-V)
8. `finally`: Dismount, `Start-VM` (3× retry), libera locks, grava
   `civm-reclaim-last.txt`, loga `autoreclaim_done`. (host)

**Fluxo alternativo — auto-shrink online (após RF-2 da spec vizinha, se UNMAP
honrado):** `fstrim` periódico do guest faz o VHDX encolher online; `host-metrics`
registra `v_free_gb` subindo; sem `Stop-VM`. Hoje é fallback (UNMAP não honrado no
blocksize atual). (`Confirmado em docs`: host-volume-reclamation PRD §Happy A)

**Fluxo — janela de compactação aberta pela admissão (#113):** memória sob
pressão → `admit` recusa novos heavy → jobs correntes terminam → `idle-check`
passa → autoreclaim consegue a janela. (`Confirmado no codebase`: admit/idle)

**Fluxos de erro:**

| Condição | Resultado | Log / level | Consistência |
| --- | --- | --- | --- |
| `ScratchBudget=0` e `V:<Headroom` | abort, sem parar VM | `autoreclaim_abort_headroom` ERROR exit 2 | host intacto |
| Folga pós-Off não cobre budget (N2) | pula Optimize, religa VM | `autoreclaim_skip_insufficient_slack_post_off` **WARN exit 0** (nome/severidade reais do código; este PRD dizia `*_abort_*` ERROR exit 2) | VM Running, nada compactado |
| Outro reclaimer ativo | skip | `reclaim_skip_other_active` WARN exit 0 | sem concorrência |
| `Start-VM` falha 3× | VM Off | `autoreclaim_vm_left_off` CRITICAL exit 1 | **pior caso**; alarme |
| `fstrim` exit **0** mas inefetivo (FileSize não cai) | segue (offline primário) | `host-disk level=warn fstrim_ineffective` | discard morto sinalizado |
| `fstrim` exit **≠0** (ex.: EPERM "Operation not permitted") | **best-effort (corrigido no working tree)**: registra `autoreclaim_fstrim_warn` e segue para `Stop-VM`/`Optimize` (`autoreclaim.ps1:302-310`); antes dava `throw` antes do `Stop-VM` e bloqueava o #106 mesmo com o gate pós-Off | `autoreclaim_fstrim_warn` WARN, segue | N2 roda; discard é oportunístico, `Optimize -Mode Full` compacta offline independente do trim |
| Reaper HTTP 409 | benigno | `already-transitioned` INFO, exit 0 | run já saiu de queued |

## Modelo de dados

> **N/A — sem banco.** Estado = arquivos efêmeros. Backfill = **N/A — Day-0**.

**Estado/constantes alteradas (`internal/civm/civm.go`):**

```go
// Antes (linha 90)  -> Depois (valor MEDIDO no Slice 0, exemplo p100=10):
DefaultHostVolumeScratchBudgetGB = 0   // -> 11  (RF-1; 5 medições anexadas)
// Antes (linha 129) -> Depois (valor MEDIDO no Slice 0, exemplo p95 RSS=1850):
DefaultAdmitHeavyMaxMB = 0             // -> 2048 (RF-3; tabela RSS anexada)
```

- *Quem lê:* `ScratchBudgetGB` → **hoje ninguém** (gap): nenhum Go o lê,
  `EmergencyAdmits` não tem caller, e o `register-*.ps1` não passa `-ScratchBudgetGB`
  ao worker (default 0). É preciso threadar o valor para o fix valer. `HeavyMaxMB`
  → `cmd/civmctl/admit.go` (cgroup cap) — wired de fato.
- *Invariante:* `HardFloor(1) < Headroom(8) < Pressure(25)`; `ScratchBudget ≥ 0`;
  emergência só habilita com `ScratchBudget > 0`. (`Confirmado em docs`: SPECv3)
- *Migração:* **N/A — Day-0** (constantes, sem estado persistido).

**Estado novo (host, telemetria de medição) — efêmero:** linhas
`autoreclaim_optimized`/`optimize_end` em `V:\civm-hyperv-maintenance.log` ganham
`vmrs_release_gb` ao lado de `scratch_high_water_gb` (campo aditivo, sem migração).

**Estado lido (box, RF-6/RF-7):** `/etc/civm/run-reaper.env`
(`CIVM_REAPER_REPOS`, `GH_TOKEN`); `/etc/apt/apt.conf.d/50unattended-upgrades`
(blacklist); `/var/run/reboot-required` (flag do doctor).

## API / Interfaces

> **Sem HTTP/OpenAPI.** Interfaces = CLI `civmctl` + componente host + arquivos.

**Subcomandos `civmctl` (todos já existentes — `Confirmado no codebase`
`cmd/civmctl/main.go`):** `host-disk` (78, read-only), `doctor` (54, read-only),
`idle-check` (70, read-only), `cleanup` (56, muta com `--execute`), `disk-watchdog`
(66), `maintenance` (80, muta), `reap-runs` (102, muta com `--execute`), `admit`
(106, muta). **Nenhum subcomando novo** — RF-4/5/7/8 estendem os existentes.

| Campo | host-disk / doctor | reap-runs | admit |
| --- | --- | --- | --- |
| Read-only? | sim | não (`--execute`) | não |
| Exit | 0 ok / 1 crit / 2 stale-ish | 0 / 1 transitório (`SuccessExitStatus=0 1`) | 0 / 78 wait-timeout |
| Privilégio | runner / SSH→host | runner + `GH_TOKEN` | `sudo systemd-run` |

**Componente host (`deploy/windows/`):** `civm-host-metrics.ps1`,
`civm-vhdx-optimize.ps1`, `civm-vhdx-autoreclaim.ps1` + `register-*.ps1`
(`schtasks /RU SYSTEM /RL HIGHEST`). RF-2 modifica o autoreclaim; RF-1 usa o poll
do optimize; RF-6 roda os `register-*`.

**Impacto em contrato/docs:** `internal/civm/civm.go` (constantes RF-1/RF-3,
gateiam comportamento → sync rule); `deploy/windows/*` versionado + lint host;
`runbooks/RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md` (RF-9);
`runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md` + `MULTI-PROJECT-RUNNER.md` §Disk.

## Dependências e riscos

**Pré-requisitos:** RF-6 (registro das tasks + sudoers no box) é **bloqueador
go/no-go** de todas as camadas de host; sem `host-metrics.json` nada do reclaim
observa o estado. (`Observação operacional (auditoria)`)

**Riscos e mitigação:**

| Risco | Mitigação |
| --- | --- |
| N2: `Stop-VM` a baixo headroom não consegue completar | Fase 1 exige `HardFloor + margemDeStop`; `Stop-VM` gracioso quase não escreve; se falhar, `finally` Start-VM 3× |
| N2: folga pós-Off ainda insuficiente | abort fail-closed + religa VM; nada compactado; alarme |
| `Optimize-VHD` ininterruptível pendura | sem `Stop-Job` (SPECv3); poll só telemetria; timeout do schtasks + alarme `vm_left_off` |
| Medição (N1/N3) sob carga atípica enviesa o budget | ≥5 ciclos; `ScratchBudget=ceil(p100)+1`, `HeavyMaxMB=ceil(p95)+margem`; commit com evidência |
| Patch de OS troca toolchain mid-CI | `Package-Blacklist` (RF-7) |
| Reaper sem `GH_TOKEN`/permissão | RF-5 inclui evento de skip claro; runbook documenta env |

**Impacto em componentes existentes:** RF-2 altera o contrato de admissão do
autoreclaim (refina SPECv3 DT-v3-1) → **exige Passo 2.5 (red-team)** antes do
IMPL, por mutar `Stop-VM`/`Optimize`. RF-4/RF-8 estendem `hostdisk`/`cleanup` sem
quebrar assinatura. RF-1/RF-3 só mudam constantes.

**Breaking changes:** nenhum de contrato externo; RF-2 muda o comportamento
interno do gate de emergência (forward-only no host, reversível por
`schtasks /change`).

**Rollout (slices):** ver §Estratégia. **Rollback:** app = `civmctl self-upgrade`
anterior; host = `schtasks /change /disable` do autoreclaim (volta ao
supervisionado); estado = N/A (efêmero). **Proibido:** zero-fill sob baixo
headroom; deixar a VM Off ao fim de qualquer caminho.

**Hipóteses para disciplina explícita no SPEC:** N2 (Kahneman #2 counterfactual +
#5 worst-case), N1/N3 (#3 número não adjetivo), RF-6 (#5 — nada opera sem o
registro), RF-7 (#3 — versão de toolchain antes/depois).

## Estratégia de implementação

Ordem recomendada (cada fatia valida antes da próxima):

0. **Slice 0 — baseline + instalação Day-0 (RF-6).** SSH read-only mede o estado
   atual (`df`, `docker system df`, `civmctl host-disk`, `hooks.jsonl`); registrar
   as 3 tasks + sudoers + `/run/civm`. **Sem isso o resto não observa nada.**
1. **Slice 1 — medição do scratch + VMRS (RF-1).** ≥5 ciclos supervisionados;
   define `ScratchBudgetGB`. (depende de Slice 0)
2. **Slice 2 — gate N2 no autoreclaim (RF-2).** Passo 2.5 obrigatório; lint host +
   janela supervisionada. (depende de Slice 1)
3. **Slice 3 — calibração `HeavyMaxMB` (RF-3, #113).** ≥5 jobs heavy medidos.
4. **Slice 4 — fstrim health + BlockSize warn (RF-4, RF-8).**
5. **Slice 5 — reaper 409 (RF-5).** Unit test + observação no journal.
6. **Slice 6 — OS hygiene (RF-7).** Blacklist + doctor reboot-required.
7. **Slice 7 — runbook + sync (RF-9).**
8. **Slice 8 — validação live 3 dias + fechamento #106/#113 (Kahneman #2).**

Validável cedo: Slice 0 (baseline numérico, sem código). Exige janela
supervisionada: Slices 1, 2, 3. Reversível por task disable: Slice 2.

## Documentos a atualizar (mesmo commit — sync rule)

- `internal/civm/civm.go` (RF-1/RF-3 — constantes que gateiam).
- `deploy/windows/civm-vhdx-autoreclaim.ps1` (RF-2) + lint host
  `internal/hostdisk/specv3_reclaim_test.go` (tokens do gate); `ps1_safety_test.go`
  permanece só o guard de clamp Int32 (#17).
- `internal/hostdisk/hostdisk.go` (RF-4/RF-8).
- `internal/runreaper/runreaper.go` (RF-5).
- `runbooks/RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md` (RF-9, novo) +
  `RUNBOOK-HOST-VHDX-MAINTENANCE.md` + `MULTI-PROJECT-RUNNER.md` §Disk.
- `docs/specs/host-volume-reclamation/SPECv3` (nota de reconciliação: gate N2
  refina DT-v3-1) — **ver decisão no SPEC: SPECv4 vizinha vs nota**.
- `docs/specs/civm-self-cleaning-runner/{SPEC,IMPL}.md`.
- `disciplines/KAHNEMAN-DISCIPLINES.md` / `INVARIANTS.md` (se novo gate/invariante).
- README/AGENTS/CODEX/rules (se contrato/convenção mudar).

## Fora de escopo

- **Allowlist explícita de imagens warm (`warm-images.json`) / proteção de bases
  no cron.** Fica **fora deste PRD**, mas **não** porque "o prune preserva todas
  as bases": o prune do *hook* (`hook.go:240-243`) é dangling-only e preserva as
  tagueadas, porém o *cron* idle `docker system prune -af --volumes`
  (`cleanup.go:425`, `DockerPrune` default `true`) **apaga** bases tagueadas
  não-usadas. Tratar o cold-build resultante (allowlist de bases, ou trocar o
  ramo idle para dangling-only + builder prune como o ramo host-busy
  `dockerPruneSafe`, `cleanup.go:444-459`) é auto-cura real, deixada como
  follow-up explícito — não "otimização marginal". Day-0: aceitamos o cold-build
  ocasional como custo do reclaim agressivo no idle. (`Confirmado no codebase`)
- **`civmctl cache-gc` (SPECv3 DT-v3-7).** Slice própria; depende de DT-v3-1..6.
- **SCSI re-attach como primário / `Convert-VHD -BlockSizeBytes 1MB` one-time.**
  Pertence a `host-volume-reclamation` (RF-2/RF-6); aqui só consumimos o resultado
  (BlockSize observável).
- **Right-sizing estrutural do VHDX / expansão de `V:`.** Mitigação de capex.
- **Mudar os passos de CI dos repos consumidores** (ex.: não rodar como root) —
  não controlável; o fix é por construção no runner (`safedelete`).
- **`pull_request_target`/código de PR no host.** Proibido por segurança.

## Critérios de aceitação

1. `internal/civm/civm.go` tem `ScratchBudgetGB > 0` (RF-1) e `HeavyMaxMB > 0`
   (RF-3), ambos com evidência numérica anexada ao commit.
2. Janela supervisionada: com `V:`≈6.6 GB, o autoreclaim **sai da espiral** —
   `Stop-VM`, re-mede pós-Off, admite e completa o `Optimize` (RF-2); ou recusa e
   religa a VM (nunca Off).
3. `Get-ScheduledTask` das 3 tasks = `Ready`; `civmctl host-disk` = `level=ok`
   (não `stale`) (RF-6).
4. `civmctl host-disk` expõe `fstrim`/`BlockSize` warn quando aplicável (RF-4/8).
5. Reaper trata 409 como `already-transitioned` (RF-5).
6. `50unattended-upgrades` tem o blacklist; `doctor` reporta reboot-required
   (RF-7).
7. `RUNBOOK-CIVM-SELF-CLEANING-RUNNER.md` existe e linka (RF-9).
8. #106 e #113 fechadas com referência ao commit que setou os budgets.

## Validação

- **Guest (Go):** `gofmt`, `golangci-lint run -c .golangci.yml ./...` (0 issues),
  `go vet`, `go test ./... -race -count=1`, cobertura ≥80% em
  `internal/{civm,hostdisk,runreaper,admit}`, `govulncheck`,
  `go build -ldflags='-s -w' ./cmd/civmctl` (<10 MB).
- **Host (PowerShell):** lint `specv3_reclaim_test.go` (sem `Stop-Job` no
  Optimize; lock canônico; eventos reais `autoreclaim_post_off_remeasure` +
  `autoreclaim_skip_insufficient_slack_post_off`;
  `scratch_high_water_gb`+`vmrs_release_gb` ao vivo via `Get-PSDrive V`) +
  `ps1_safety_test.go` (sem `[math]::Max(0,…)` Int32, #17); janela supervisionada
  com as evidências numéricas.
- **Live (3 dias — Kahneman #2):** `hooks.jsonl` → `job-completed decision=error`
  por filesystem = 0; `systemctl --failed` sem `civmctl-*`; `civmctl host-disk`
  oscila mas nunca cruza `HardFloor + ScratchBudget`; autoreclaim respeita
  rate-limit ≥30 min; **nenhuma exclusão manual de arquivo do host**.
  **Abort-trigger:** se qualquer métrica não convergir em 3 dias → reverter a
  fatia + reabrir diagnóstico.
