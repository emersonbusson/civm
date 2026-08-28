# IMPL — Runner auto-limpante (civm-self-cleaning-runner)

> **SUPERSEDED-BY (2026-06-17): orchestrator scale-to-zero.** O stop+compact da VM
> e o reclaim do VHDX agora pertencem ao `civm-vm-orchestrator.ps1` (único dono do
> power-state; tasks `autoreclaim`/`optimize`/`optimize-watchdog` `Disabled`).
> Fonte de verdade viva: `docs/specs/orchestrator-scale-to-zero/`. O conteúdo
> abaixo é preservado como histórico — não o reimplemente.

> **Natureza desta IMPL.** Intervenção de emergência em 2026-06-06: o `V:` do host
> chegou a **6.59 GB livres** (espiral de morte ativa) **e** a CI do acme (#1092
> `tenant-isolation-smoke`) falhava com `No such image` de largada. Esta IMPL
> executou o subconjunto do PRD/SPEC necessário para **quebrar a espiral agora** e
> **destravar a CI de forma durável**, deixando o restante do plano de slices
> formal (campanha de 5 medições, #113, runbook, registro das tasks no host)
> explicitamente pendente. Honestidade Day-0 > "tudo implementado".

## Executado e validado

### 1. Quebra imediata da espiral (compactação manual supervisionada)

`Stop-VM gha-ubuntu-2404 → Optimize-VHD -Mode Full → Start-VM`, precedido de
limpeza do guest + `fstrim`. **Validação empírica (medições vivas `Get-PSDrive V`):**

| Marcador | Valor |
| --- | --- |
| `V:` livre antes | 6.59 GB |
| `V:` livre após `Stop-VM` (VMRS liberado) | 14.61 GB → **`vmrs_release` = 8.02 GB** |
| `V:` livre após `Optimize-VHD -Mode Full` + `Start-VM` | **31.52 GB** |
| Recuperado | **+24.9 GB** |
| VHDX físico | 104.55 GB → ~80 GB |

Isto **valida empiricamente a premissa central do DT-1/DT-2** (o VMRS só libera no
`Off`; ~8 GB) que antes era `Observação operacional`. Limpeza do guest que liberou
o espaço: `~/.cache` (28 GB go-build/yarn/playwright), volumes docker (2.4 GB),
`_diag`, journald, apt; `fstrim` marcou 33.8 GiB descartáveis (estavam presos por
falta de trim automático).

### 2. RF-10 — Registry pull-through cache local (correção da CI)

**Causa raiz da falha `No such image`:** pulls anônimos (sem `~/.docker/config.json`)
+ mirror só `mirror.gcr.io` (cobre só `library/`, não `minio/`/`clamav/`/`evoapicloud/`)
→ rate limit do Docker Hub derruba o `compose up --build`. Tags exatas do compose
(`redis:8.6.1-alpine3.23` etc.) não estavam no cache.

- **`deploy/bin/setup-registry-cache.sh`** (novo): sobe `registry:2` como
  pull-through cache de `docker.io` em `127.0.0.1:5000` (volume nomeado
  `registry-cache-data`, `--restart always`), reconfigura `daemon.json`
  `registry-mirrors → http://127.0.0.1:5000` (substitui o gcr.io), reinicia o
  docker, e aquece o warm set.
- **Deploy validado no runner:** cache `running (restart=always)`, mirror ativo,
  **warm set 18/18 imagens OK, 0 falhas** (incl. as que falhavam: minio, clamav,
  redis:8.6.1-alpine3.23, alpine:3.23, postgres:18.3-alpine3.23). Cache: 1.2 GB de
  blobs persistidos.
- **Durabilidade (crítica):** o cache **sobrevive** ao cron idle
  `cleanup.go:425 docker system prune -af --volumes` — volume + container
  (`restart=always`) + imagem ficam *em uso*, logo referenciados e não removíveis.
  As bases warm do daemon podem ser podadas, mas a próxima CI as re-puxa do **cache
  local** (hit rápido, zero Docker Hub) — não é "cold build do Hub". O cache é a
  camada durável **exatamente porque** sobrevive ao prune que destruiria um warm de
  daemon (alinha com a razão de `hook.go:228-243` só podar dangling).

### 3. RF-2 / DT-1 — Gate de emergência de duas fases (fecha #106, código)

- **`deploy/windows/civm-vhdx-autoreclaim.ps1`:**
  - Fase 1 (pré-`Stop-VM`): removido o gate de folga sobre `beforeFreeGB` (linha
    262 antiga) — era ele que travava a espiral a 6.6 GB (`5.6 < 11` ⇒ abort
    eterno). Mantém só "emergência habilitada?" (`ScratchBudget > 0`).
  - Fase 2 (NOVA, pós-`Wait-VMState Off`): re-mede `Get-VFreeGB` (live `Get-PSDrive
    V`, **não** JSON stale), registra `autoreclaim_post_off_remeasure` com
    `vmrs_release_gb`, e admite o Optimize só se `liveFreeAfterOff − HardFloor ≥
    ScratchBudget`; senão `autoreclaim_skip_insufficient_slack_post_off` (WARN, exit
    0) e o `finally` religa a VM. Fail-closed.
  - Default `ScratchBudgetGB = 11` (era 0) — corrige o "#106 inerte" (o
    `register-*.ps1` não passa o arg; o default do worker é o valor efetivo).
  - **`fstrim` best-effort:** um `fstrim` que falha (EPERM/controlador sem UNMAP)
    agora registra `autoreclaim_fstrim_warn` (WARN) e segue para `Stop-VM`/`Optimize`
    (linhas 302-310), em vez de `throw` antes do `Stop-VM` — senão a Fase 2 (o próprio
    fix #106) nunca rodaria. Discard é oportunístico; `Optimize-VHD -Mode Full`
    compacta os blocos livres offline independente do trim online.
- **`internal/civm/civm.go`:** `DefaultHostVolumeScratchBudgetGB = 0 → 11` (p100
  scratch high-water observado 10 + 1).
- **Testes (verde):** `internal/civm/reclaim_test.go` (guard travando o valor
  medido + invariante `budget > hardfloor`, exige re-medição para mudar);
  `internal/hostdisk/specv3_reclaim_test.go` (`TestAutoreclaimAdmissionGate` agora
  exige os tokens do gate de duas fases — `autoreclaim_post_off_remeasure`,
  `autoreclaim_skip_insufficient_slack_post_off`, `vmrs_release_gb` — e mantém a
  proibição de `Stop-Job`). `go test ./internal/civm/... ./internal/hostdisk/...` ok.

### 4. Deploy ao host `C:\civm-deploy` + self-heal autônomo (stale-on-host — o elo que fechou #106)

**Achado "stale-on-host" (código existe ≠ proteção ativa — disciplina Kahneman #16
no acme / #15 no civm).** O fix #106 estava **correto no repo** mas a scheduled task
do host rodava o `.ps1` **antigo** de `C:\civm-deploy`: o artefato deployado ≠ o do
repo. Logo o gate de duas fases, o `fstrim` best-effort e o `ScratchBudget=11` **não
tinham efeito real** — "código existe" não é "proteção ativa". Este foi o elo que
mantinha o #106 aberto mesmo com o código mergeado, e o exemplo concreto que ancora a
disciplina.

**Ação (2026-06-06):** o `civm-vhdx-autoreclaim.ps1` corrigido (two-phase gate +
fstrim best-effort + budget 11) foi **deployado em `C:\civm-deploy`** e a scheduled
task do autoreclaim **registrada** (RF-6 / Slice 0 — antes pendente). **Validação
empírica:** a task disparou **autonomamente** e fez self-heal de `V:`
**6.14 GB → 28 GB** sem nenhuma intervenção manual, confirmando o gate de duas fases
end-to-end (pré-stop não-bloqueante → re-medição pós-Off → Optimize admitido). É a
primeira evidência de "nunca mais à mão" cumprida pela automação no host, não pela
compactação manual da §1.

## Pendente (NÃO implementado nesta intervenção)

- **Slice 1 — campanha de 5 medições (RF-1).** `ScratchBudget=11` veio de **1**
  `vmrs_release` (8.02 GB) + log histórico (high-water máx 10), **não** dos 5
  ciclos exigidos. O gate de duas fases é fail-closed, então o valor é pré-filtro
  grosseiro e a folga pós-Off é autoritativa — mas o critério "5 medições anexadas"
  do RF-1 segue **aberto**.
- **RF-6 / Slice 0 parcial.** O `civm-vhdx-autoreclaim` já está **deployado e
  registrado** e fez self-heal autônomo (§4). Restam confirmar/registrar
  `civm-host-metrics` e `civm-vhdx-optimize`, e fechar a evidência completa do RF-6:
  `host-metrics.json` presente no guest e `civmctl host-disk` = `level=ok` (não
  `stale`). Ação Day-0 supervisionada no host Windows.
- **RF-3 (#113) `HeavyMaxMB`**, **RF-5** (classificação 409 do reaper), **RF-9**
  (runbook), **DT-9** (adendo no `host-volume-reclamation/SPECv3.md`): pendentes.
- **Passo 2.5 (red-team) formal** do RF-2/DT-1 antes de promover a SPEC a
  implementada: o mecanismo foi validado end-to-end pela compactação manual, mas a
  revisão adversarial formal segue requisito do SPEC.

## Validação

- `go test ./internal/civm/... ./internal/hostdisk/...` → **ok**.
- `bash -n deploy/bin/setup-registry-cache.sh` → ok; deploy real no runner ok.
- Compactação: +24.9 GB no `V:` (6.59 → 31.52), VM `Running`.
- Warm set: 18/18 imagens, 0 falhas.
- CI #1092 re-run (Web CI attempt 3 + Go CI attempt 2) em validação no runner
  saudável + cache quente.

## Causa-raiz durável do footprint (2026-06-06, segunda iteração)

A primeira intervenção quebrou a espiral, mas um run full-stack fresco voltou a
zerar o `V:` (0.02 GB → Hyper-V `PausedCritical`, guest inalcançável). A
investigação por dentro do guest achou a causa-raiz **real** do footprint, que
não era o docker:

| Consumidor (guest 66 GB usados) | Tamanho | Natureza |
| --- | --- | --- |
| `~/.cache/go-build-acme-services` | 13 GB | cache de build regenerável |
| `~/.cache/yarn-acme-{web,gates,tenant-isolation-smoke}` | 4.3 GB cada | cache de deps |
| `~/.cache/go-build-acme{,-devctl}` | 3.6 GB | cache de build |
| `~/.cache/golangci-lint` | 1.1 GB | cache de lint |
| docker | **0 B** | (não era o vilão) |

Os workflows do acme apontam `GOCACHE`/yarn cache-folder para dirs **nomeados
por workflow** (isolamento de cache-key). O `cacheCaps()` do hook só casava os
paths **default** (`~/.cache/go-build`, `~/.yarn/cache`), então os dirs nomeados
**nunca casavam nenhum cap e cresciam sem limite** (.cache somou 31 GB; VHDX 105
GB num volume V: de 119 GB ≈ 14 GB de headroom).

### Fixes (commits nesta branch)

1. **`fix(hook): cap named CI cache dirs` (`c05fbf2`).** `cacheCaps()` agora faz
   glob das famílias (`go-build*`, `yarn*`, `golangci-lint*`) e **divide o budget
   por família** entre os dirs achados — limite por família independente de
   quantos dirs nomeados existam. Acrescenta cap para `golangci-lint`.
   `cachePaths()` (modo wipe / disk-pressure) deriva de `cacheCaps()`, então
   passa a varrer os nomeados também. **Deployado + provado no binário do guest:**
   `civmctl hook job-started --json --pre-cleanup-pct 1` → `cache_trim` com
   `path=~/.cache/go-build-acme-probe found=83886080` (antes: 0 ações).
2. **`feat(hook): host-aware job-started gate` (`5252efb`).** O gate só via a % do
   filesystem do **guest**; o guest podia estar em 69% (confortável) enquanto o
   `V:` do host já estava em 88% (crítico). Agora o hook lê o snapshot
   `host-metrics` entregue ao guest (`hostdisk.Check`): host `warn/crit` dispara
   cleanup mesmo com guest confortável; host `crit` **fresco** rejeita o job
   (exit 75) antes de cair em `PausedCritical`; snapshot `stale`/ausente força
   cleanup mas **não** bloqueia (telemetria ausente ≠ disco cheio — não
   auto-sabotar a CI). Semântica no domínio (`Report.WantsCleanup()/Blocks()`),
   não em strings mágicas no hook.

### Evidência empírica do reclaim durável

| Marcador | Valor |
| --- | --- |
| guest usado, antes do prune dos caches | 66 GB |
| guest usado, depois (prune dos `.cache` nomeados) | 36 GB |
| `fstrim` liberou | 68.3 GiB |
| VHDX após `Optimize-VHD -Mode Full` (457s) | 105 → **44.8 GB** |
| `V:` livre após Start | ~5.7 → **66 GB** |
| `gap_gb` (VHDX − guest usado) | 2 |

### Lacuna conhecida (follow-up)

O cache-trim vive **só** no `internal/hook` (dispara em `job-started`). O
`civmctl cleanup` e o `civmctl disk-watchdog` (`internal/cleanup`) **não** podam
caches — só `_work`/`tmp`/docker/apt. Isso cobre o incidente (jobs rodando, disco
> `PreCleanupPct`), mas **não** o caso "runner ocioso com disco cheio de cache".
Fechar a lacuna exige extrair `cacheCaps()`/`trimCacheByAge` para um pacote
compartilhado e chamá-los também do `internal/cleanup` (disk-watchdog). Não feito
nesta iteração — registrado como follow-up Day-0 honesto.
