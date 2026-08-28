---
slug: civm-runner-architecture
title: Arquitetura unificada do runner box civm — robusto, rápido e justo num único $HOME/7G RAM
milestone: —
issues: []
---

# PRD — Arquitetura unificada do runner box: uma raiz, uma solução

> **Supersedido por [`SPECv4.md`](./SPECv4.md)** (3ª rodada / re-fundação): o
> veredito grounded é GO no delta mínimo Day-0 (D1 + per-runner cache slot + D3).
> Os deep-dives ephemeral-clean-slate-ci / vm-disk-budget / guest-access-resilience
> e os DATA-REPORTs citados aqui **não existem** — provenância fantasma. Use o
> mapa real shipped/gap do SPECv4. Este doc fica como trilha de auditoria.

> SSDV3 PASSO 1. Slug: `civm-runner-architecture`. Repo: `civm`.
> Este é o documento **guarda-chuva** que reconcilia a saga distribuída do box
> (cachetrim, multi-project-isolation, host-volume-reclaim-liveness,
> vm-disk-budget, runner-memory-admission, guest-access-resilience,
> ephemeral-clean-slate-ci) numa **única arquitetura**. Os specs por-componente
> viram DEEP-DIVES referenciados — este PRD os linka, não duplica o detalhe.
> A regra é: aqui mora a TESE e a ORDEM; lá mora a implementação fina de cada peça.

## Resumo

> **Confirmado nos dados** (`#13`, 2026-06-15, guest `gha-ubuntu-2404`).

O box civm é UMA VM Hyper-V (12 vCPU, **7 GB RAM** + 4 GB swap, guest `/` 108 GB
em VHDX dinâmico, host `V:` 120 GB NTFS) que hospeda **8 self-hosted runners de 7
projetos** (acme ×2, civm, service-a, service-b, service-c,
service-d, peer) — **todos no mesmo `$HOME` do user `emdev`**, persistente
e compartilhado. Esse box sofreu uma saga de incidentes (corrupção de cache que
custou 4 camadas de remediação, wedge de `sshd` sob carga, death-spiral de disco)
que foram, cada um, tratados por um spec separado.

Este PRD afirma a **tese central única**: os incidentes têm **uma raiz dupla** —
(1) **estado persistente compartilhado** (1 `$HOME` + filesystem mutável escrito
e trimado concorrentemente por 8 runners) e (2) **RAM de 7 GB** insuficiente para
a concorrência que 8 runners geram. Tudo o mais (corrupção `ENOENT` de yarn,
`go vet` "can't import facts", `sshd` wedge, VHDX death-spiral) é **sintoma**
dessas duas causas.

A **solução unificada** é uma arquitetura de runner box compartilhado
**robusto / rápido / justo** com seis eixos que atacam a raiz, não o sintoma:

1. **Isolamento efêmero per-runner** (`$HOME`/`_work`/cache disjuntos por runner +
   wipe por-job) — **mata a corrupção por construção** (zero estado mutável
   compartilhado). Deep-dive: `docs/specs/ephemeral-clean-slate-ci/`.
2. **Managed cache content-addressed com backend LOCAL** (blob por hash do
   lockfile, restaurado/salvo por job) — **mata a corrupção** sem perder o
   benefício de cache; substitui o filesystem mutável por blob imutável.
   Deep-dive: `docs/specs/ephemeral-clean-slate-ci/`.
3. **Admissão por RAM** (`internal/admit`, `MaxHeavy=2`, `MemoryMax` por slot,
   gated por `internal/memwatchdog`) — **mata a contenção** (jobs concorrentes
   estouravam 7 G → `sshd` wedge). Deep-dive: `docs/specs/runner-memory-admission/`.
4. **Disk budget com Docker prune endurecido** (teto agregado + `docker image/volume
   prune` seguro-durante-job) — **mata a pressão de disco** que tornava o trim
   agressivo perigoso. Deep-dive: `docs/specs/vm-disk-budget/`.
5. **Guest-access serial OOB** (console serial via named pipe Hyper-V, atravessa
   o hypervisor quando o `sshd` morre) — **mata o wedge** como ponto cego de
   acesso. Deep-dive: `docs/specs/guest-access-resilience/`.
6. **`cpus:1` no build** (`NEXT_BUILD_CPUS=1` na VM) — **mata o OOM de build**
   (paralelismo do next estourava a RAM). Confirmado no estado atual.

O **JÁ SHIPADO** — cachetrim backstop/atômico, gate self-heal, `cpus:1`,
`internal/admit`, autoreclaim host-side — é a **FUNDAÇÃO / estado-atual**, que
provê robustez parcial tratando o sintoma. O **futuro** (efêmero + managed cache +
demais peças não-implementadas) é a **migração UNIFICADA incremental** com ordem
de dependência dura, tendo o per-PR `dockerlock` como **kill-switch** durante a
transição.

## Contexto técnico

> Cada item marcado **Confirmado no codebase** / **Confirmado em docs** /
> **Inferência** (hard-rule SSDV3 #2/#3).

### Topologia e recursos (medidos)

- **VM única** Hyper-V, 12 vCPU, **7 GB RAM** (host 32 GB físico, só ~1 GB livre;
  Memory Compression 3.6 GB; `vmmemWSL` 8.1 GB cap 16; VM dynamic 8–12, em ~9 GB),
  4 GB swap, guest `/` 108 GB (73 usado / 30 livre / 72%), host `V:` 120 GB NTFS
  (91 usado / 29 livre / 76%). **Confirmado nos dados** (`#13`, 2026-06-15;
  `docs/specs/ephemeral-clean-slate-ci/DATA-REPORT.md`, `vm-disk-budget/DATA-REPORT.md`).
- **8 runners de 7 projetos**, todos `user emdev`, **mesmo `$HOME` persistente**.
  **Confirmado nos dados** (`#13`).
- Guest `/` e host `V:` são **ACOPLADOS**: encher o guest infla o VHDX dinâmico,
  que só encolhe via `Optimize-VHD` offline (autoreclaim host-side), drenando o
  `V:`. **Confirmado em docs** (`vm-disk-budget/DATA-REPORT.md`).

### Consumidores de disco (medidos)

- **Docker ~27 GB** (~18 GB reclamável: imagens 14.4/8.3 + volumes 4.0/4.0 +
  build-cache 8.9/5.6) — **o maior lever reclamável e o único de 2 dígitos SEM
  teto**. **Confirmado nos dados** (`#13`).
- **Cache CI ~10 GB** natural (yarn-tenant 3.2 + yarn-audit 3.2 + go-build 2.4 +
  playwright 0.6 + yarn-gates 0.6), sob caps backstop somando 34 GB → 24 GB de
  teto morto inerte (o cache nunca morde sob uso normal). **Confirmado nos dados**.
- **OS + go/pkg/mod + `_work` ~36 GB** — número MENOS medido (subtração
  73−27−10, não `du` direto). **Inferência** (declarada como tal).
- **VMRS ~12 GB** vive no `V:`. **Confirmado nos dados** (`vm-disk-budget`).

### Raiz da corrupção (medida)

- A corrupção que custou 4 camadas **não veio do Docker** (content-addressed por
  digest SHA + refcount — nunca corrompeu). Veio de **caches de filesystem
  MUTÁVEL**: yarn v1 (pacote = diretório multi-arquivo lido por `--frozen-lockfile`
  → `ENOENT` e não re-fetch) e go-build (par `-a`/`-d` ref-cruzado → `go vet`
  "can't import facts ... no such file"), no MESMO `$HOME` compartilhado por 8
  runners, **trimados/escritos concorrentemente** sob o disk-watchdog disparando
  NO MEIO do install. **Confirmado em docs** (`cachetrim-yarn-atomic/SPECv2.md` A0/A2;
  `ephemeral-clean-slate-ci/DATA-REPORT.md`).

### Raiz da contenção (medida)

- Jobs concorrentes estouram os 7 G de RAM → o kernel sob memory pressure
  atrasa/mata o `sshd` (que precisa fork + pty + escrita `utmp`) → **wedge**,
  e o canal de manutenção SSH degrada exatamente quando é mais necessário.
  **Confirmado em docs** (`guest-access-resilience/SPEC.md` B1; `runner-memory-admission`).

### Estado atual — JÁ SHIPADO (a FUNDAÇÃO desta arquitetura)

- **cachetrim backstop + atômico** — `internal/cachetrim` + `internal/civm/civm.go`:
  `DefaultCacheYarnMaxGB=12`, `DefaultCacheGoBuildMaxGB=12`, `PackageDepth=2` (yarn,
  atômico por diretório-pacote), `WipeWhole` (go-build/golangci, refs cruzadas),
  trim no-op durante o working-set normal sob o backstop cap. **Confirmado no codebase**
  (`internal/civm/civm.go:69-90`, `internal/cachetrim/`).
- **gate self-heal** (devctl/ci-router clean+retry) — última rede contra corrupção
  residual. **Confirmado em docs** (`host-volume-reclaim-liveness`).
- **`cpus:1`** — `NEXT_BUILD_CPUS=1` na VM civm. **Confirmado nos dados** (`#13`).
- **`internal/admit`** — heavy-slot flock `/run/civm/admit-heavy-{1..2}.lock`,
  `DefaultAdmitMaxHeavy=2`, RAM-bound (`MemoryMax` efetivo `(MemTotal−2048)/2`),
  gated por memwatchdog (fail-closed). **Confirmado no codebase**
  (`internal/civm/civm.go:149-167`, `internal/admit/`, `internal/memwatchdog/`).
- **autoreclaim host-side** — `deploy/windows/civm-vhdx-autoreclaim.ps1` +
  `civm-vhdx-optimize.ps1` (gate 2 fases pós-`Stop-VM`, anti-fantasma,
  `EmergencyAdmits` fail-closed) + `civm-host-metrics.ps1`. **Confirmado no codebase**.
- **`internal/dockerlock`** — 1 flock global + heartbeat, serializa docker-heavy
  box-wide; `dockerPruneSafe` (`internal/cleanup/cleanup.go:525`) faz hoje só
  `docker image prune -f` (dangling) + `docker builder prune -f --filter until=24h`,
  com early-return `deferred-by-docker-heavy-lock` quando o lock está ativo.
  **Confirmado no codebase** (`internal/cleanup/cleanup.go:25-28,108-192,517-525`).
- **hooks job-started/completed** — `internal/hook` (`reclaimWorkspaceOwnership`
  não-destrutivo + `cleanWorkRoot` gated por disco que HOJE preserva o workspace
  ativo + `_temp` porque o `$HOME` é compartilhado; `workRoots()` proíbe fallback
  global após o civm#117 ter apagado checkout de sibling MID-JOB). **Confirmado no
  codebase** (`internal/hook/hook.go:210,279,335,475,590`).
- **`registry:2` pull-through** — `deploy/bin/setup-registry-cache.sh`,
  content-addressed, sobrevive a prune. É a PROVA do padrão certo já no box.
  **Confirmado em docs** (`ephemeral-clean-slate-ci/SPEC.md`).

### Estado futuro — NÃO IMPLEMENTADO (a MIGRAÇÃO desta arquitetura)

- `$HOME`/`_work`/cache **por-runner** (units systemd / bootstrap) — pré-requisito
  duro do wipe-por-job e da aposentadoria do lock. **Inferência / proposta**
  (`ephemeral-clean-slate-ci` RF-1, sem IMPL).
- **managed cache content-addressed com backend LOCAL** (`setup-ci-cache.sh` +
  action de fork nos peers). **Inferência / proposta** (RF-2).
- **wipe-por-job** reabilitado no `job-completed` (gated por isolamento provado).
  **Inferência / proposta** (RF-3).
- **`docker volume prune -f`** como 3ª perna do `dockerPruneSafe` +
  `docker image prune -a -f --filter until=168h` no caminho BUSY (fia a constante
  órfã `DefaultDockerImagePruneFilter`, `internal/civm/civm.go:90`, ZERO call-sites
  hoje). **Confirmado no codebase** (constante órfã) + **Inferência** (uso proposto).
- **`--threshold-pct=1`** no invoke do autoreclaim (hoje `=0`, que vira 60 em
  `diskwatchdog.go:135`). **Confirmado no codebase** (`civm-vhdx-autoreclaim.ps1:412`,
  `internal/diskwatchdog/diskwatchdog.go:135`) + **Inferência** (correção proposta).
- **guest-access serial OOB** (`internal/serialrecover` + `civmctl serial-recover`
  + `civm-serial-console.ps1`). **Inferência / proposta** (`guest-access-resilience`,
  sem IMPL).

## Opção recomendada

**UMA arquitetura, seis eixos, migração incremental ordenada por dependência dura,
com o `dockerlock` como kill-switch.**

A solução escolhida ataca a **raiz dupla** (estado compartilhado + RAM 7 G) em vez
de continuar a whack-a-mole de sintomas:

- **Corrupção** → isolamento efêmero per-runner (RF-1) + managed cache
  content-addressed (RF-2) + wipe-por-job (RF-3). Mata a fonte; o cachetrim
  atômico desce a backstop e o trim atômico é aposentado quando não houver mais
  cache mutável compartilhado.
- **Contenção** → admissão por RAM (RF-4), INTOCÁVEL e ortogonal; o efêmero
  resolve ESTADO, não PRESSÃO. O teto de 2 heavies é RAM-bound, nunca sobe em
  nome do efêmero.
- **Pressão de disco** → disk budget com Docker prune endurecido (RF-5) + teto
  agregado (RF-6); o lever nº1 é o Docker (18 GB), não o cache.
- **Wedge** → guest-access serial OOB (RF-7), canal paralelo e aditivo.
- **OOM de build** → `cpus:1` (RF-8), já shipado.

**Motivo da escolha** (no contexto do box civm):

- O CI pago tem exatamente esta forma — runner com working-set FRESCO por job +
  managed cache content-addressed. Espelhar isso mata a corrupção por construção,
  não por remediação. **Confirmado em docs** (`ephemeral-clean-slate-ci/PRD.md`).
- A migração é incremental e cada camada já shipada **desce de papel** quando a
  raiz é morta (cachetrim vira backstop; lock vira kill-switch), preservando
  reversibilidade a cada fatia.

**Alternativas descartadas:**

- **Container ARC/DinD por job** — descartado: overhead de daemon Docker aninhado
  e RAM extra num box de 7 G. **Confirmado em docs** (`ephemeral-clean-slate-ci` DT-1).
- **VM-snapshot por job** — descartado por **aritmética**: 8 jobs ⇒ 8 VMs; piso
  realista 2–4 GB/VM ⇒ 16–32 GB > 7 GB total. **Confirmado em docs** (DT-1).
- **Managed cache no Azure (actions/cache puro)** — descartado: ~13 GB/job × N PRs
  × 8 runners pelo uplink residencial mata por **egress**, não por storage; backend
  LOCAL em loopback é o único viável. **Confirmado em docs** (DT-2).
- **Apertar os caps de cache (34→20 GB)** — descartado: aproxima o cap do
  working-set e re-introduz o race `ENOENT` que o cachetrim A2 curou; a fonte de
  alívio sob pressão é Docker, não cache. **Confirmado em docs** (`vm-disk-budget` B2).
- **Trocar só o transporte de acesso (rede → vsock/Hyper-V Sockets)** — descartado:
  o ator continua sendo processo de userspace do guest, que morre com a RAM; só o
  serial tem NENHUMA ponta como processo de rede do guest. **Confirmado em docs**
  (`guest-access-resilience` REJECT).
- **Remover o `dockerlock`** — descartado como kill-switch: preservado durante a
  migração e no eixo de reclaim do host (`Stop-VM`/`Optimize-VHD` que disputa o
  `V:`). **Confirmado em docs** (`ephemeral-clean-slate-ci` RF-5/DT-v2-5).

**Trade-offs aceitos:**

- Cache miss frio residual após eviction/warm-up do backend local (go ~5.7 GB +
  yarn 1.5 GB + playwright 0.6 GB por job) — custo MEDIDO e aceito vs corrupção.
  **Confirmado em docs** (`ephemeral-clean-slate-ci` C4).
- O teto de concorrência permanece 2 (RAM-bound); o managed cache acelera via
  cache-hit em loopback mas **não sobe o teto**. **Confirmado em docs** (DT-5).

## Requisitos funcionais

> Cada RF marcado **Confirmado no codebase** / **Confirmado em docs** /
> **Inferência**. Os RFs deste PRD são **agregadores**: cada um delega o detalhe
> ao spec de componente, e o critério de aceite é por EFEITO (Kahneman #13).

### RF-1 — Isolamento efêmero per-runner (`$HOME`/`_work`/cache disjuntos)

- **Descrição**: cada `actions.runner.*` roda com `HOME` próprio (ex.: `/home/runnerN`)
  e `GOCACHE`/`YARN_CACHE_FOLDER`/`npm`/`pnpm`/`golangci` sob esse `HOME`. A
  disjunção FÍSICA substitui a serialização lógica do `dockerlock` como proteção
  de cache. **Inferência / proposta** (deep-dive: `ephemeral-clean-slate-ci` RF-1;
  sem IMPL).
- **Critério de aceite (por EFEITO, #13)**: wipe do `_work` do runner N **não
  altera 1 byte** sob o `HOME` de M, provado no guest `gha-ubuntu-2404` (não probe,
  não "config existe").
- **Isolamento/concorrência**: pré-requisito DURO de RF-3 e RF-5 (gate binário).

### RF-2 — Managed cache content-addressed com backend LOCAL

- **Descrição**: blob por hash do lockfile (`key=hash(go.sum)`/`hash(yarn.lock)`),
  verificado, restaurado no início e salvo no fim do job, servido por backend
  S3-compatível LOCAL em loopback (espelha `setup-registry-cache.sh`), zero egress
  WAN. **Inferência / proposta** (deep-dive: `ephemeral-clean-slate-ci` RF-2).
- **Critério de aceite (por EFEITO)**: restore de blob em segundos via loopback;
  backend down/lento → **cache MISS imediato → build frio determinístico**, nunca
  espera indefinida (fail-open, espelha `commandActionWarn` do hook).
- **Isolamento/concorrência**: blob imutável é concorrência-seguro por construção.

### RF-3 — Wipe efêmero por-job (clean-slate do projeto)

- **Descrição**: com RF-1 entregue, o hook `job-completed` apaga o `_work` do
  PRÓPRIO runner por inteiro — não mais gated por disco, não mais preservando
  caches; `_actions`/`_tool` preservados; `job-started` mantém o chown
  não-destrutivo como rede de segurança. **Inferência / proposta** (deep-dive:
  `ephemeral-clean-slate-ci` RF-3; código atual em `internal/hook/hook.go:335`).
- **Critério de aceite (por EFEITO)**: GATE BINÁRIO — só habilitado quando RF-1
  está PROVADO por efeito (wipe de N não toca M MID-JOB); enquanto não, o hook
  mantém EXATAMENTE o comportamento atual (`cleanWorkRoot` preserva workspace ativo
  + `_temp`).
- **Isolamento/concorrência**: sem RF-1, wipe-por-job repete o civm#117 (apagou
  checkout de sibling MID-JOB) — abort trigger.

### RF-4 — Admissão por RAM (INTOCÁVEL e ortogonal)

- **Descrição**: o cap de admit (`DefaultAdmitMaxHeavy=2`, `MemoryMax` por slot)
  + memwatchdog são preservados; teto de concorrência = 2, RAM-bound não
  lock-bound. **Confirmado no codebase** (`internal/civm/civm.go:149-167`,
  `internal/admit/`; deep-dive: `runner-memory-admission`).
- **Critério de aceite**: o efêmero NÃO sobe o teto; removê-lo em nome do efêmero
  é PROIBIDO (abort trigger). Sob warm-up frio simultâneo dos 8 runners que exceda
  a RAM, o admit/memwatchdog recusa heavy.
- **Isolamento/concorrência**: heavy-slot flock `/run/civm/admit-heavy-{1..2}.lock`,
  fail-closed (CheckFn err → backoff, nunca admite).

### RF-5 — Disk budget com Docker prune endurecido (lever nº1 = Docker 18 GB)

- **Descrição**: `dockerPruneSafe` ganha a 3ª perna `docker volume prune -f` e, no
  caminho BUSY, troca `image prune -f` por `image prune -a -f --filter until=168h`
  (fia `DefaultDockerImagePruneFilter`). Solta imagens TAGGED-unused >7 d e volume
  efêmero (~17.9 GB) MESMO com host busy, sem tocar imagem em uso/recém-puxada
  (filtro por CREATED-date). **Confirmado no codebase** (constante órfã
  `internal/civm/civm.go:90`; `dockerPruneSafe` `cleanup.go:525`) + **Inferência**
  (uso; deep-dive: `vm-disk-budget` RF-2, `ephemeral-clean-slate-ci` RF-4).
- **Critério de aceite (por EFEITO, #13)**: integration contra daemon real provando
  o EFEITO (imagem tagged >7 d SUMIU; volume desanexado colhido) PAREADO com o
  positivo (imagem EM USO por container vivo OU CREATED <7 d / volume com refcount>0
  **SOBREVIVE**) — o par positivo é GATE DE MERGE.
- **Isolamento/concorrência**: `image/volume prune -a` exclui por definição
  qualquer recurso referenciado por container; o early-return
  `deferred-by-docker-heavy-lock` (`cleanup.go:192`) impede o prune enquanto um
  job docker-heavy segura o lock.

### RF-6 — Teto agregado + folga-alvo determinística + `threshold-pct=1`

- **Descrição**: UM bloco soma todos os consumidores do `V:` contra 108/120 GB e
  prova a folga-alvo no pior caso (WARN `DefaultHostVolumeWarnFreeGB=30`, CRIT
  `DefaultHostVolumeCritFreeGB=10`, banda 20 GB); os caps backstop (34 GB) ficam
  INALTERADOS (invariante A2); o autoreclaim passa `--threshold-pct=1` (mínimo
  válido) em vez de `=0` (que reseta a 60). **Confirmado no codebase** (constantes
  `civm.go:97-98`; bug `civm-vhdx-autoreclaim.ps1:412`) + **Inferência** (deep-dive:
  `vm-disk-budget` RF-1/RF-3/RF-4).
- **Critério de aceite**: o somatório dos caps respeita o teto agregado SEM apertar
  cap a ponto de o trim morder o working-set; folga ≥30 GB livres ABAIXO de F3.
- **Isolamento/concorrência**: complementar ao guest-prune do host-volume-reclaim;
  recusa de job (exit 75) permanece o fail-safe no piso CRIT.

### RF-7 — Guest-access serial OOB (mata o wedge como ponto cego)

- **Descrição**: canal serial via named pipe Hyper-V (`\\.\pipe\civm-console` +
  `serial-getty@ttyS0`), acionado do host/WSL2 por `sudo.exe`, para quando o `sshd`
  morre na starvation. **Inferência / proposta** (deep-dive:
  `guest-access-resilience`; sem IMPL).
- **Critério de aceite (por EFEITO, #13)**: do host abrir o pipe, autenticar
  (PAM `emdev`, sem `--autologin root`) e rodar comando que muta estado observável
  (`df` sobe) COM o `sshd` de fato morto sob carga; par positivo (df sobe via
  serial) + par negativo (`ssh true` dá Connection timed out no mesmo instante),
  ≥3 incidentes. `Set-VMComPort exit 0` NÃO conta.
- **Isolamento/concorrência**: canal PARALELO e ADITIVO, fora do hot path de
  reclaim; classificação tipada `ClassifyAttempt` (5 outcomes mutuamente
  exclusivos, timeout finito em cada estágio).

### RF-8 — `cpus:1` no build (mata o OOM de build)

- **Descrição**: `NEXT_BUILD_CPUS=1` na VM civm limita o paralelismo do next build
  que estourava a RAM. **Confirmado nos dados** (`#13`; já aplicado).
- **Critério de aceite**: build do next app não OOM sob admissão de 2 heavies.
- **Isolamento/concorrência**: ortogonal; reduz pico de RAM por job heavy.

## Requisitos não-funcionais

- **RNF-1 — Robustez (corrupção zero por construção)**: após RF-1+RF-2+RF-3, não
  há cache mutável compartilhado; a corrupção `ENOENT`/"can't import facts" é
  impossível por construção, não por remediação. Validado por EFEITO (#13).
  **Confirmado em docs** (`ephemeral-clean-slate-ci`).
- **RNF-2 — Rapidez (cache-hit em loopback)**: o managed cache restaura blob em
  segundos vs build frio; o cache miss frio residual é orçado e ACEITO (número
  declarado). O teto de concorrência NÃO sobe (RAM-bound). **Confirmado em docs** (C4).
- **RNF-3 — Justiça (sem starvation cross-projeto)**: o `dockerlock`/admit
  serializa docker-heavy box-wide com heartbeat; nenhum projeto monopoliza os 2
  slots heavy indefinidamente (`DefaultAdmitWaitMinutes=30`). **Confirmado no
  codebase** (`civm.go:161`).
- **RNF-4 — Fail-safe preservado (Kahneman #15/#16)**: a recusa de job no piso
  CRIT (exit 75) permanece o backstop; o curador (trim/admit/reclaim) não morre
  com o recurso que protege; todo efeito atrás de retry/replay é idempotente
  (`register-*.ps1`, `setup-*.sh`). **Confirmado no codebase**.
- **RNF-5 — Observabilidade**: `slog` estruturado no guest, log do host em
  `V:\civm-hyperv-maintenance.log`; eventos `ephemeral_cache_backend_down` (Warn),
  `serial_recover_*`, `deferred-by-docker-heavy-lock`, contadores de bytes
  reclamados. Sem PII, sem segredo, sem label de alta cardinalidade.
- **RNF-6 — Segurança/privilégio**: tasks SYSTEM com direito Hyper-V mínimo; senha
  PAM do serial vem de `CIVM_SERIAL_PASS` (env, nunca no repo); `--autologin root`
  PROIBIDO; backend de cache em loopback `127.0.0.1` only. **Confirmado em docs**
  (`guest-access-resilience` RF-2/B4).

## Fluxos

### Happy path — job de PR no box (estado-alvo)

1. **Admissão (RF-4)** — `civmctl admit` adquire 1 dos 2 slots heavy
   (`/run/civm/admit-heavy-{1,2}.lock`) gated por memwatchdog; light flui sem slot.
   Guest Go (`internal/admit`).
2. **Restore de cache (RF-2)** — a action de cache restaura o blob por hash do
   lockfile do backend local em loopback; backend down → cache MISS → build frio.
   Peer repo (action de fork) + guest backend (`setup-ci-cache.sh`).
3. **Build/test isolado (RF-1)** — o runner roda sob `HOME`/`_work`/cache próprios;
   nenhum byte compartilhado com siblings. Units systemd / bootstrap.
4. **Save de cache (RF-2)** — blob salvo no fim do job (content-addressed,
   imutável). Action de cache.
5. **Wipe por-job (RF-3)** — `job-completed` apaga o `_work` do próprio runner;
   `_actions`/`_tool` preservados. `internal/hook`.
6. **Disk budget (RF-5/RF-6)** — `dockerPruneSafe` (image+volume prune seguro)
   roda fora do lock docker-heavy; autoreclaim host-side com `--threshold-pct=1`
   mantém o `V:` ≥ WARN. `internal/cleanup` + `deploy/windows`.

### Fluxo alternativo — cache miss frio simultâneo

- No warm-up do backend ou após eviction, TODOS os jobs dão cache miss frio: cada
  um faz build frio (go ~5.7 GB + yarn 1.5 GB + playwright 0.6 GB). O warm-up
  CONTROLADO (espelha `setup-registry-cache.sh --warm`) pré-aquece o working-set
  conhecido; o residual é orçado. Se o warm-up frio dos 8 exceder a RAM, o admit
  recusa heavy (o efêmero NÃO sobe o teto).

### Fluxo de erro — `sshd` wedge sob starvation

- **Trigger**: jobs concorrentes estouram 7 G → kernel mata/atrasa `sshd`.
- **Resultado**: `ssh` dá Connection timed out; o guest-access serial OOB (RF-7)
  abre o named pipe do host, autentica via PAM e roda `civmctl cleanup --execute`/
  `disk-watchdog`/`journalctl` dentro do guest. Worst-case tipado: power-cycle
  host-side (`Invoke-GuestUnreachableForcedReboot`) como último recurso.
- **Log**: `serial_recover_*` em `V:\civm-hyperv-maintenance.log` + `wtmp`.
- **Consistência**: canal aditivo, não muta o hot path de reclaim.

### Fluxo de erro — backend de cache indisponível/lento

- **Trigger**: backend local fora ou lento além do timeout duro.
- **Resultado**: cache MISS IMEDIATO → build frio determinístico; nunca espera
  indefinida nem build sobre estado parcial.
- **Log**: `ephemeral_cache_backend_down` (Warn).

## Modelo de dados

> **N/A — sem banco.** Estado = arquivos + blobs. Sem estado persistido legado
> obrigatório → backfill = **N/A — Day-0** (estado efêmero).

**Estado/constantes existentes reutilizados** (`internal/civm/civm.go`):

- `DefaultCacheYarnMaxGB=12`, `DefaultCacheGoBuildMaxGB=12`, `PackageDepth`,
  `WipeWhole` (cap backstop, INALTERADOS por RF-6/A2).
- `DefaultAdmitMaxHeavy=2`, `DefaultAdmitHostReserveMB=2048`,
  `DefaultAdmitSlotPathPrefix`, `DefaultAdmitDockerSlotPath` (RF-4, INTOCÁVEIS).
- `DefaultHostVolumeWarnFreeGB=30`, `DefaultHostVolumeCritFreeGB=10`,
  `DefaultDockerImagePruneFilter="until=168h"` (RF-5/RF-6; a constante de prune é
  órfã hoje e passa a ter call-site).

**Estado/blobs novos** (delegados aos deep-dives):

- Backend de cache local: blobs content-addressed por hash do lockfile (RF-2,
  `ephemeral-clean-slate-ci`).
- `HOME`/`_work`/cache por-runner: árvores de diretório por runner (RF-1).
- `civm-serial-recover-last.json` (host): outcome do último serial-recover, SEM
  senha (RF-7, `guest-access-resilience`).

## API / Interfaces

> **Sem HTTP/OpenAPI.** Interfaces = CLI `civmctl` + componente host + arquivos/locks.

- **`civmctl admit`** (RF-4) — adquire/libera slot heavy; read/muta `/run/civm`.
  Existente. **Confirmado no codebase**.
- **`civmctl cleanup --execute`** / **`disk-watchdog`** (RF-5/RF-6) —
  `dockerPruneSafe` ganha volume prune + image prune `-a` no BUSY. Existente,
  modificado. **Confirmado no codebase**.
- **`civmctl serial-recover`** (RF-7) — host/WSL2-only; dispara
  `civm-serial-console.ps1` via `exec.CommandContext` sem shell. NOVO (deep-dive
  `guest-access-resilience`). **Inferência / proposta**.
- **Componente host** — `civm-vhdx-autoreclaim.ps1` (`--threshold-pct=1`),
  `civm-serial-console.ps1` + `register-civm-serial-console.ps1`, `setup-ci-cache.sh`.
- **Action de cache** (peer repos, não civm) — fork tipo `runs-on/cache` apontando
  para o backend local.

## Dependências e riscos

- **Pré-requisitos**: RF-1 (isolamento físico) é pré-requisito DURO de RF-3 e RF-5
  (gate binário; sem ele, wipe-por-job repete civm#117). RF-7 reusa `sudo.exe`
  através-do-hypervisor (já provado por `Stop-VM`/`Optimize-VHD`).
- **Riscos técnicos com mitigação**:
  - Backend de cache down/lento → cache MISS imediato (fail-open, RF-2 C1).
  - Wipe-por-job sem isolamento → mata sibling MID-JOB → GATE BINÁRIO (RF-3 C2).
  - `volume prune` tocando volume ativo → par positivo é GATE DE MERGE (RF-5 C3).
  - Cache miss frio simultâneo dos 8 → warm-up controlado + admit recusa (RF-2 C4).
  - Login serial sob starvation NÃO-medido → aceite por 2 números medidos em ≥3
    incidentes; counterfactual de rollback (PAM >60 s em 3/3 → serial deixa de ser
    PRIMARY) (RF-7 B1).
- **Breaking changes**: aposentar o `dockerlock` do eixo cache (RF-5/migração) é
  reversível via kill-switch; o pacote NÃO é deletado.
- **Rollout (slices)**: ver Estratégia de implementação.
- **Rollback**: app (`civmctl` anterior; subcomandos novos viram no-op), host
  (`schtasks /delete`; reverter `Set-VMComPort` em janela), estado (N/A — Day-0).
  PROIBIDO: deixar a VM Off; `--autologin root`; subir o teto de admit.

## Estratégia de implementação

> Migração UNIFICADA incremental, ordem por dependência DURA. O `dockerlock`
> permanece kill-switch entre as fatias e a evidência.

1. **Slice 0 — baseline no guest** (`du`, `docker system df`, RAM, login serial com
   VM saudável). BASELINE, NÃO conta como aceite (#13/#59).
2. **Fatia 1 — Docker prune endurecido (RF-5/RF-6)** — `dockerPruneSafe` + volume
   prune + image prune `-a` no BUSY + `--threshold-pct=1`. Independente, baixo
   risco. Par #13 (em-uso sobrevive) é GATE DE MERGE. Deep-dives `vm-disk-budget`,
   `ephemeral-clean-slate-ci` RF-4.
3. **Fatia 2 — isolamento per-runner (RF-1)** — `HOME`/`_work`/cache disjuntos nos
   units systemd. Pré-requisito DURO; POC supervisionado no guest; prova por efeito
   (wipe de N não toca M).
4. **Fatia 3 — wipe-por-job (RF-3)** — reabilitar no `job-completed`, GATEADO por
   RF-1 provado verde. Enquanto não, hook mantém comportamento atual.
5. **Fatia 4 — managed cache local (RF-2)** — `setup-ci-cache.sh` + action de fork
   nos peers + warm-up controlado.
6. **Fatia 5 — aposentar o lock no eixo cache (RF-5/migração)** — remover do
   caminho de prune; kill-switch por janela; só após 2 jobs docker-heavy de runners
   isolados rodarem concorrentes SEM colisão/corrupção no guest.
7. **Paralelo — guest-access serial OOB (RF-7)** — `internal/serialrecover` +
   `civmctl serial-recover` + `.ps1` + register one-time. Independente do trilho de
   cache; aceite por 2 números medidos em ≥3 incidentes reais.

- **Já validável cedo**: Fatia 1 (integration docker) e Slice 0.
- **Janela / one-time**: `Set-VMComPort` (RF-7) exige VM Off; isolamento per-runner
  (RF-1) exige re-registro de units.

## Documentos a atualizar (sync rule)

- `internal/civm/civm.go` (constante de prune ganha call-site; demais inalteradas).
- `internal/cleanup/cleanup.go` (`dockerPruneSafe`), `internal/hook/hook.go` (wipe).
- `deploy/windows/civm-vhdx-autoreclaim.ps1` (`--threshold-pct=1`),
  `deploy/windows/civm-serial-console.ps1` + register, `deploy/bin/setup-ci-cache.sh`.
- `runbooks/MULTI-PROJECT-RUNNER.md`, `runbooks/RUNBOOK-GUEST-SERIAL-RECOVERY.md`.
- `disciplines/INVARIANTS.md` (novos gates), os deep-dives por-componente, e
  `docs/specs/civm-runner-architecture/IMPL.md` (registro).
- `README.md` ≡ `AGENTS.md` ≡ `CODEX.md` ≡ `rules/*.md` quando contrato/convenção
  mudar.

## Fora de escopo

- **F3 — working-set ativo > 108 GB guest / 120 GB V:** — limite de HARDWARE que o
  efêmero NÃO resolve; abaixo dele a folga é determinística, no piso CRIT a recusa
  de job (exit 75) é o fail-safe. **Confirmado em docs** (`vm-disk-budget` B3,
  `host-volume-reclaim-liveness`).
- **Subir o teto de concorrência** — RAM-bound; fora de escopo (DT-5).
- **Gate de admit por disco no guest** — follow-up, fora de escopo
  (`vm-disk-budget`).
- **Cache per-runner com `CIVM_RUNNER_SLOT` no path como cura definitiva de escrita
  concorrente** — follow-up #16, fora de escopo (`cachetrim-yarn-atomic`).
- **Isolamento de DAEMON Docker (COMPOSE_PROJECT_NAME/portas ephemeral)** —
  ORTOGONAL; deep-dive `multi-project-isolation`, não re-especificado aqui.
- **Eixo de DETALHE de cada componente** — vive nos specs referenciados; aqui só a
  tese, a ordem e os critérios por-efeito.

## Referência cognitiva (Kahneman)

- **#13 Ilusão de validade** — TODO critério de aceite é por EFEITO (df sobe,
  imagem sumiu, byte não mudou), nunca "código existe"/"endpoint registrado".
  Toda recusa é PAREADA com seu positivo (volume em-uso sobrevive; same-runner não
  é tocado). A própria reclassificação do A2 (benigno → crítico) veio de validação
  ao vivo, não de auditoria de gabinete.
- **#15 Fail-safe + curador independente** — a recusa de job (exit 75) é o backstop;
  o curador (trim/admit/reclaim) não morre com o recurso que protege.
- **#16 Idempotência** — `register-*.ps1`, `setup-*.sh` reconciliam estado (rodar
  2× = rodar 1×); todo efeito atrás de retry/replay é idempotente.
- **#5 Availability/worst-case** — a arquitetura é desenhada para o box CHEIO e a
  rajada concorrente, não o happy path.
- **#1 WYSIATI** — declarado: OS+`_work`+go-mod=36 GB é o número MENOS medido
  (subtração); a banda do uplink residencial NÃO foi medida (a conclusão "egress
  mata" vale pelo MECANISMO, não por benchmark).

## Política Day-0

> O box `civm` JÁ roda em produção (runner self-hosted), mas SEM estado persistido
> legado obrigatório (estado = arquivos efêmeros, blobs content-addressed, locks).

- Toda peça é especificada como solução principal e única, no formato final Day-0.
- PROIBIDO: shim, dual-reader/writer, camada de compatibilidade, backfill para
  estado inexistente, código morto. A constante de prune órfã hoje (`civm.go:90`)
  NÃO é compatibilidade — é uma constante que será fiada ao call-site Day-0.
- O `dockerlock` preservado como kill-switch NÃO é compatibilidade legada: é
  reversibilidade operacional com prazo (aposentado do eixo cache após evidência),
  exceção Day-0 documentada com motivo + abort trigger + rollback.
