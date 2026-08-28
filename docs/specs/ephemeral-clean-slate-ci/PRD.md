---
slug: ephemeral-clean-slate-ci
title: CI efêmero clean-slate — managed cache content-addressed + wipe efêmero por-job que substitui o per-PR lock
milestone: —
issues: []
---
# PRD — CI efêmero clean-slate (como o CI pago) viável no box de 7G/1 VM

> SSDV3 PASSO 1. Estende a saga de corrupção (`docs/specs/cachetrim-yarn-atomic`,
> `multi-project-isolation`, `host-volume-reclaim-liveness`, `vm-disk-budget`)
> com a arquitetura que SUBSTITUI o per-PR lock como proteção-de-cache: cada job
> recebe um working-set FRESCO e VERIFICADO (managed cache content-addressed) e o
> estado efêmero é destruído no fim. Mata a corrupção na RAIZ, não na consequência.

## Resumo

O runner civm roda 8 self-hosted runners de 7 projetos numa única VM Hyper-V,
todos no MESMO `$HOME` do user `emdev` — **estado compartilhado e persistente**.
A saga das 4 camadas (backstop cap, trim atômico, hooks, gate self-heal) + o
per-PR lock (`internal/dockerlock`) existe por UMA causa medida (#13): caches de
FILESYSTEM MUTÁVEL compartilhado (yarn v1 dir-pacote, go-build par `-a`/`-d`)
corromperam sob trim/escrita concorrente. O insight do user é a cura: **CI pago =
runner com working-set FRESCO por job + managed cache content-addressed
(blob verificado por hash, baixado/subido por job), não filesystem compartilhado
vivo.** Zero estado mutável compartilhado → zero corrupção.

Este PRD adota a forma **viável no box de 7 GB/1 VM**, recusando explicitamente as
formas inviáveis: managed cache com backend LOCAL (não o Azure do GitHub, que
mataria por egress residencial), e nível de efemeridade **wipe-por-job com
`$HOME`/`_work` isolados por runner** (não VM-snapshot, inviável por aritmética de
RAM). Valor operacional: o runner para de morrer por corrupção silenciosa de
cache, a saga colapsa de 4 camadas para `efêmero + managed-cache`, e o per-PR lock
é aposentado COMO proteção-de-cache.

## Contexto técnico

### Componentes (Confirmado no codebase)

- `internal/hook/hook.go` — hooks job-started/job-completed. HOJE: `cleanWorkRoot`
  **preserva** o workspace ativo e os caches (não faz wipe total) porque o `$HOME`
  é compartilhado (um wipe em job-started matou sibling MID-JOB, civm#117). O
  `workRoots()` proíbe fallback global pela mesma razão.
- `internal/cleanup/cleanup.go` — disk-watchdog. `dockerPruneSafe` (image+builder
  prune, refcount-safe, roda host-busy) vs `dockerPrune` (`system prune -af
  --volumes`, idle-gated). `cacheTrimActions` aplica `cachetrim` como root.
- `internal/cachetrim` — política única de cap por-família (PackageDepth/WipeWhole).
- `internal/dockerlock` — o per-PR lock: 1 flock global + heartbeat, serializa
  docker-heavy box-wide. Existe "só para contornar o box PERSISTENTE".
- `internal/admit` — admissão por RAM: `DefaultAdmitMaxHeavy=2`, MemoryMax 2560 MB/
  heavy, liveness = flock. O TETO de concorrência REAL do box.
- `deploy/bin/setup-registry-cache.sh` — `registry:2` pull-through cache
  content-addressed de docker.io; sobrevive a prune. **A prova do padrão certo já
  rodando no box** — o managed cache de yarn/go estende o mesmo princípio.

### Confirmado na documentação oficial

- `actions/cache` em runner self-hosted sobe/baixa para o Azure Blob do GitHub
  **pela rede** (GH community #18549 + docs). Local-disk só via FORK
  (`tespkg/actions-cache`, `runs-on/cache`, `falcondev-oss/github-actions-cache-server`)
  ou backend S3.
- `docker {image,builder,volume} prune -f` (sem `-a`, sem `--volumes` global) só
  toca o grafo NÃO-REFERENCIADO; o daemon nunca remove recurso com refcount>0.

### Sendo proposto (Inferência)

- 1 backend S3 LOCAL content-addressed (MinIO **ou** o `registry:2` já presente
  estendido) + uma action de cache de fork apontada para ele.
- `$HOME`/`_work`/cache **por-runner** (`HOME=/home/runnerN` ou subdir dedicado).
- Reabilitar o wipe-por-job no hook job-completed agora que o isolamento o torna
  seguro.
- Aposentar o per-PR lock COMO proteção-de-cache (mantê-lo só para o eixo
  Optimize-VHD/reclaim do host).

## Opção recomendada

**Nível de efemeridade: (a) wipe efêmero por-job com `$HOME`/`_work`/Docker-data
isolados por runner + managed cache content-addressed com backend LOCAL.**

Por quê: é a ÚNICA das 3 opções que cabe nos 7 GB HOJE (+0 RAM) enquanto mata a
causa-raiz medida (estado compartilhado). Com cada runner num `$HOME` próprio, o
wipe-por-job de UM runner é **fisicamente incapaz** de tocar o sibling — elimina a
classe inteira do civm#117. O managed cache content-addressed substitui o FS
mutável compartilhado por blob verificado, latência de loopback, zero egress.

### Alternativas descartadas

| Alternativa | Por que descartada |
| --- | --- |
| `actions/cache` puro (backend Azure do GitHub) | resolve a corrupção mas é INVIÁVEL por **egress**: ~13 GB/job re-subidos pelo uplink residencial × N PRs × 8 runners. Descartada por custo, não por mecanismo (DATA-REPORT §6). |
| (b) container-por-job (ARC/k3s ou DinD supervisor) | cabe em RAM mas re-introduz DinD num único kernel (contenção de I/O) + control plane (~512 MB-1 GB permanente) ou supervisor caseiro. Infra MÉDIA, overkill sobre (a) num box de 1 VM. Reservado a repo de TERCEIRO não-confiável (modelo de confiança hoje é 1 operador). |
| (c) VM-snapshot-reset por job (Hyper-V checkpoint) | IDEAL em isolamento, mas **matematicamente inviável**: 8 jobs ⇒ 8 VMs ⇒ 16-32 GB RAM ≫ 7 GB. Descartada por ARITMÉTICA (DATA-REPORT §7). |
| wipe-leve do `$HOME` compartilhado (sem isolar por runner) | **insuficiente**: deixa o daemon Docker/overlayfs, `/tmp`, systemd e os 8 runners co-residentes compartilhados; a classe de corrupção sobrevive. Não é "ambiente novo destruído no fim", é o mesmo box com vassoura mais forte. |
| Remover o cap de `admit` "porque o efêmero resolve tudo" | reintroduz OOM/swap-thrash no box de 7 GB. O efêmero resolve estado, não RAM. PROIBIDO. |

### Trade-offs aceitos

- Cache miss frio = build cheio (go ~5.7 GB mod+compile, yarn 1.5 GB
  node_modules): mais lento que o hit, mas DETERMINÍSTICO e limpo. É o trade certo
  vs corrupção parcial (#15: fail-fast determinístico).
- O `$HOME` por-runner multiplica o footprint de cache em disco antes da
  mitigação; mitigado a ~1 cópia pelo pull-through + managed cache content-addressed.

## Requisitos funcionais

- **RF-1 — `$HOME`/`_work`/cache por-runner.** Cada `actions.runner.*` DEVE rodar
  com `HOME` próprio (ou subdir dedicado) e GOCACHE/yarn/npm/pnpm/golangci sob
  esse HOME, de modo que o estado de cache de um runner seja fisicamente disjunto
  do sibling.
  - **Critério de aceite:** dado runner-N e runner-M ativos, um wipe do `_work`/
    cache de N **não altera nem um byte** sob o HOME de M (provado por efeito, #13).
  - **Isolamento/concorrência:** disjunção física substitui serialização lógica.

- **RF-2 — Managed cache content-addressed com backend LOCAL.** O cache entre jobs
  DEVE ser um BLOB content-addressed (chave = hash do lockfile), verificado por
  integridade, restaurado no início e salvo no fim do job, servido por um backend
  S3-compatível LOCAL (loopback/LAN), nunca pelo Azure do GitHub.
  - **Critério de aceite:** cache hit restaura um working-set verificado sem
    reabrir FS mutável compartilhado; cache miss recompila limpo; zero egress WAN.
  - **Isolamento/concorrência:** blob imutável/verificado é concorrência-seguro por
    construção (não há trim atômico a fazer).

- **RF-3 — Wipe efêmero por-job (clean-slate do projeto).** Com RF-1 entregue, o
  hook job-completed DEVE apagar o `_work` do PRÓPRIO runner por inteiro (não mais
  gated por disco, não mais preservando caches), porque não há sibling no mesmo
  HOME. job-started mantém o chown não-destrutivo como rede de segurança.
  - **Critério de aceite:** após job-completed, o `_work` do runner está vazio; o
    sibling permanece intacto; nenhum job é recusado por isso.

- **RF-4 — Docker clean-slate inteligente (apaga efêmero ~18 GB, mantém base).** O
  reclaim de Docker DEVE apagar o efêmero do job (dangling images + old build
  cache + unused volumes ≈ 17.9 GB no #13) MANTENDO as imagens-base estáveis e o
  `registry:2` pull-through (content-addressed). O wipe-total (`system prune -af
  --volumes`) fica SÓ idle + SÓ acima do threshold (consome imagens stale de
  projetos peer).
  - **Critério de aceite:** prune-seguro libera ~17.9 GB sem re-pull da base; um
    container vivo mantém seu named volume (par #13).
  - **Isolamento/concorrência:** refcount-safe; roda concorrente sem lock.

- **RF-5 — Aposentar o per-PR lock COMO proteção-de-cache.** Com RF-1+RF-3+RF-4, a
  serialização box-wide do `dockerlock` deixa de ser necessária para o PRUNE
  (refcount-safe + isolamento físico). O lock é aposentado NESSE eixo; permanece
  SÓ para o reclaim de host (Stop-VM/Optimize-VHD), que de fato disputa o `V:`.
  - **Critério de aceite:** 2 jobs docker-heavy de runners isolados rodam
    concorrentes sem o lock e sem colisão de cache/corrupção.

## Requisitos não-funcionais

- **Performance:** cache hit = restore de blob (segundos-dezenas de segundos,
  loopback); cache miss = 1 build frio determinístico. Sem egress WAN.
- **Segurança/privilégio:** backend de cache escuta só em loopback/LAN (como o
  `registry:2` já faz, `127.0.0.1:5000`); sem credencial hardcoded; o wipe
  por-runner usa o `safedelete` guardado já existente — nunca amplia o blast radius.
- **Observabilidade:** cada decisão de cache/wipe/prune emite evento estruturado
  (`slog`/JSON, família `ephemeral_*`), bytes reclamados via `parseTotalReclaimed`.
- **Resiliência (worst-case, #5/#16):** backend de cache fora → cache MISS → build
  frio (NUNCA build sobre estado corrompido). O piso é fail-safe.
- **RAM (intocável):** o cap de `admit` (MaxHeavy=2, MemoryMax 2560 MB/heavy) e o
  `memwatchdog` (Critical 8%/swap 1536 MB) NÃO são tornados dispensáveis pelo
  efêmero. O teto de 2 jobs é RAM-bound, não lock-bound.

## Fluxos

### Happy path (cache hit)

1. Runner-N inicia job. job-started (`hook.go`): chown não-destrutivo do checkout
   reusado (rede de segurança RF-1/RF-3).
2. A action de cache LOCAL (RF-2) restaura o blob content-addressed (key =
   hash(go.sum)/hash(yarn.lock)) para GOCACHE/go-mod/yarn SOB o HOME de runner-N.
3. O build roda sobre um working-set FRESCO e VERIFICADO.
4. A action de cache salva o blob atualizado (imutável por hash).
5. job-completed (`hook.go`): wipe do `_work` de runner-N por inteiro (RF-3) +
   prune-seguro de Docker (RF-4). Sibling intocado.

### Fluxos alternativos

- **Cache miss frio (RF-2):** restore não encontra o blob → build recompila limpo
  → salva o blob novo. Determinístico, mais lento, nunca corrupto.
- **Docker idle + threshold (RF-4):** wipe-total `system prune -af --volumes`
  colhe imagens stale de projetos peer. Só idle, só acima do threshold.

### Fluxos de erro

| Condição | Resultado | Log | Consistência |
| --- | --- | --- | --- |
| Backend de cache LOCAL fora | cache MISS → build frio | `ephemeral_cache_backend_down` (Warn) | nunca build sobre estado corrompido (#16) |
| Wipe de runner-N falha (EACCES root-owned) | `safedelete` escala guardado; erro terminal vira sentinela visível, exit 0 | `work_root` action error | post-job nunca falha o job (hook.go já garante) |
| `volume prune -f` com container vivo | volume com refcount>0 é PRESERVADO pelo daemon | `docker_prune_safe` | par #13 obrigatório (volume vivo sobrevive) |
| Pressão de RAM (>7 GB) | `admit`/`memwatchdog` recusa heavy | watchdog Critical | piso fail-safe; efêmero não muda isso |

## Modelo de dados

> **N/A — sem banco.** Estado em arquivos/locks/blobs.

**Estado novo / alterado**

```text
HOME por-runner (RF-1): /home/runnerN  (ou subdir dedicado por slot)
  GOCACHE / YARN_CACHE_FOLDER / npm / pnpm / golangci SOB esse HOME.
  Escrita: ambiente do runner (config do unit systemd) — não é arquivo civm.
```

```text
Backend de cache LOCAL (RF-2): bucket/registry content-addressed
  no V: (cap ~80 GB = 8 runners × ~10 GB), lifecycle de eviction por
  último-acesso (espelha os 7 dias do GitHub). Blob imutável por hash.
```

**Alterações em constantes (`internal/civm/civm.go`)** — ver SPEC; nenhuma
constante nova obrigatória no PRD (reuso de `DefaultAdmitMaxHeavy`,
`DefaultHostVolume*`). Eventual `DefaultCiCacheBackendAddr`/`DefaultCiCacheCapGB`
só se a action exigir, decidido no SPEC.

**Estado scope:** guest-local (`$HOME` por-runner) + blob no `V:`. Backfill =
**N/A — Day-0** (estado de cache é efêmero/regenerável).

## API / Interfaces

> **Sem endpoint HTTP.** Interfaces = config de runner + hooks + action de cache.

- **Hooks (`internal/hook`):** job-completed passa a fazer wipe total do `_work`
  do próprio runner (RF-3); o cap de `cachetrim` vira backstop, não mecanismo
  primário.
- **Disk-watchdog (`internal/cleanup`):** `dockerPruneSafe` ganha a 3ª perna
  (`volume prune -f`) — RF-4 (alinhado a `vm-disk-budget`).
- **Action de cache (workflows dos peers):** substitui `cache:false` +
  `GOCACHE/YARN_CACHE_FOLDER=$HOME/.cache/...-$GITHUB_JOB` por uma action de fork
  apontada para o backend LOCAL. Mudança nos REPOS peer (acme), não no civm — o
  civm provê o backend + a config de runner.

## Dependências e riscos

- **Pré-requisitos:** RF-1 (isolamento por-runner) é pré-requisito DURO de RF-3 e
  RF-5 — sem ele o wipe-por-job mata sibling e o lock não pode sair.
- **Riscos:**
  - `volume prune -f` mal-calibrado remove dado de volume legítimo desanexado →
    mitigação: par #13 (volume vivo sobrevive) + os 11 containers acme up há 12 h
    mantêm refcount>0.
  - Footprint de cache multiplicado por-runner antes da mitigação → medir o `V:`
    após o split; `cachetrim`/`vm-disk-budget` (NÃO redesenhados) são o backstop.
  - Aposentar o lock cedo demais (antes de RF-1 provado no guest) → mantê-lo como
    kill-switch por uma janela, remover só com evidência de efeito no guest real.
- **Breaking changes:** config dos units de runner (HOME por-runner) é one-time;
  exige re-registro/restart dos 8 runners.
- **Rollout (slices):** ver Estratégia. **Rollback:** RF-1 reverte a config de
  unit; RF-3 reverte o wipe no hook; RF-4 reverte a 3ª perna do prune; o lock
  volta como kill-switch. Nada `forward-only`.

## Estratégia de implementação

1. **Slice 0 (sem código):** medir no guest real `gha-ubuntu-2404` o footprint
   de cache atual, o `docker system df` ao vivo e o teto de RAM. Baseline.
2. **Slice 1 (RF-4, mais barato/independente):** 3ª perna `volume prune -f` em
   `dockerPruneSafe` + sub-test par #13. Compõe com `vm-disk-budget`.
3. **Slice 2 (RF-1):** `$HOME`/cache por-runner na config dos units; POC de 1 PR;
   provar por efeito que wipe de N não toca M MID-JOB.
4. **Slice 3 (RF-3):** reabilitar wipe-por-job no hook job-completed (depende de
   RF-1 provado).
5. **Slice 4 (RF-2):** backend de cache LOCAL + action de fork nos workflows peer.
6. **Slice 5 (RF-5):** aposentar o per-PR lock COMO proteção-de-cache (mantê-lo no
   eixo Optimize-VHD); remover o kill-switch só com evidência.

## Fora de escopo

- **Runner JIT/efêmero pleno** (`config.sh --ephemeral`): a analogia 1:1 com CI
  pago. Exige novo desenho de registro/segurança e, sem managed cache externo,
  re-pull/re-warm por job é inviável no orçamento de 7 GB/108 GB. É item de
  rollback trigger do `MULTI-PROJECT-RUNNER.md`, não esta entrega.
- **F3** (working-set ativo > capacidade do disco): limite de hardware; mitigação
  é disco maior ou menos concorrência de CI (repo acme).
- **Isolamento de DAEMON Docker** (project-name/portas ephemeral): outro problema
  (`multi-project-isolation`); este PRD aposenta o lock SÓ no eixo cache.
- **Subir o teto de concorrência:** é RAM-bound; o efêmero não muda 2 jobs heavy.

## Hipóteses que exigirão disciplina no SPEC

- RF-1/RF-3: provar por EFEITO (wipe de N não toca M), não por "o hook rodou"
  (#13). RF-4: par positivo/negativo do volume prune (#13). RF-2/RF-5: backend
  fora = cache miss = build frio, nunca build sobre corrupção (#16). Cache miss =
  fail-fast determinístico (#15).
