# SPEC — CI efêmero clean-slate

> SSDV3 PASSO 2. Traduz `PRD.md` em arquivos, predicados Go testáveis, diffs e
> thresholds. Links Kahneman nos passos críticos. Implementa RF-1..RF-5 /
> RNF-1..RNF-5 do PRD.

## Princípio de design

As **DECISÕES** (este job pode wipar? o volume é órfão? o cache hit é íntegro?)
vão para Go puro e testável em `internal/**` (regra dura SSDV3 — reuso de
`internal/hostdisk`, `internal/cleanup`). A **ORQUESTRAÇÃO** (config de unit,
action de cache, daemon Docker) fica na borda. Nenhum reclaim novo é criado:
estende-se `dockerPruneSafe` (RF-4) e o hook (RF-3); o backend de cache reusa o
padrão do `registry:2` (RF-2). O managed cache content-addressed é o MATADOR de
corrupção; o efêmero por-runner é o clean-slate; juntos colapsam a saga de 4
camadas em `efêmero + managed-cache`.

## Escopo fechado desta implementação

**Entra agora (ordem de implementação):**

- RF-4: 3ª perna `volume prune -f` em `dockerPruneSafe` + par #13. Independente,
  mais barato, compõe com `vm-disk-budget`.
- RF-1: `$HOME`/cache/`_work` por-runner na config dos units (POC supervisionado).
- RF-3: reabilitar wipe-por-job no hook job-completed (depende de RF-1 provado).
- RF-2: backend de cache LOCAL + action de fork nos workflows peer.
- RF-5: aposentar o `dockerlock` COMO proteção-de-cache (kill-switch por janela).

**Fora agora:** runner JIT pleno; F3; isolamento de daemon Docker; subir teto de
concorrência (tudo no §Fora de escopo do PRD).

**Dependências assumidas prontas:** `registry:2` pull-through
(`setup-registry-cache.sh`), `safedelete` guardado, `cachetrim` family-cap,
`admit`/`memwatchdog`.

## Matriz de rastreabilidade PRD → SPEC

| PRD | Implementação no SPEC |
| --- | --- |
| RF-1 (HOME por-runner) | ITEM-4 |
| RF-2 (managed cache local) | ITEM-6 |
| RF-3 (wipe por-job) | ITEM-5 |
| RF-4 (Docker clean-slate) | ITEM-2, ITEM-3 |
| RF-5 (aposentar lock) | ITEM-7 |
| RNF-5 (cap de RAM intocável) | ITEM-1 (decisão), preservado em todos |

## Decisões técnicas

| # | Decisão | Justificativa |
| --- | --- | --- |
| DT-1 | Nível de efemeridade = **(a) wipe-por-job + isolamento por-runner**, NÃO container/VM-snapshot | única que cabe em 7 GB (+0 RAM) e mata a raiz; (c) inviável por aritmética (DATA-REPORT §7), (b) overkill p/ 1 operador |
| DT-2 | Managed cache com backend **LOCAL** (S3-compatível: MinIO ou `registry:2` estendido + action de fork), NÃO `actions/cache` puro | egress Azure residencial mata (~13 GB/job × N × 8); local = loopback, zero egress (DATA-REPORT §6) |
| DT-3 | RF-4 = `dockerPruneSafe` ganha SÓ `volume prune -f` (3ª perna); wipe-total fica idle-gated | prune-seguro já colhe ~17.9 dos ~18 GB sem re-pull; o `-a --volumes` paga re-pull da base p/ liberar o MESMO — quase puro downside em regime normal |
| DT-4 | Aposentar o lock SÓ no eixo cache; mantê-lo no eixo Optimize-VHD | refcount + isolamento físico tornam a serialização de PRUNE desnecessária; mas Stop-VM/Optimize-VHD ainda disputa o `V:` |
| DT-5 | Cap de `admit` (MaxHeavy=2) e `memwatchdog` são INTOCÁVEIS | efêmero resolve estado (corrupção), não pressão (RAM); removê-los reintroduz OOM |
| DT-6 | RF-1 é pré-requisito DURO de RF-3 e RF-5 | sem HOME disjunto, wipe-por-job mata sibling (civm#117) e o lock não pode sair |

## Fronteira de atomicidade e política de rollback

- **Atômico nesta implementação:** cada `volume prune -f` é uma operação Hyper-V/
  daemon única (DT-3); cada blob de cache é imutável por hash (RF-2); cada wipe de
  `_work` de um runner é independente do sibling (RF-1 garante disjunção física).
- **FORA da atomicidade:** o ciclo restore-cache → build → save-cache não é
  transacional (um build morto deixa o blob anterior intacto — correto); a entrega
  do backend de cache é best-effort (fora → cache miss).
- **Estados parciais aceitos:** cache miss (build frio), backend down (build frio),
  volume desanexado preservado (refcount).
- **Política de rollback:**
  - app: reverter a 3ª perna do prune (RF-4) e o wipe no hook (RF-3) é um
    `git revert` do binário civmctl.
  - host/config: reverter `HOME` por-runner na config dos units (RF-1) + restart.
  - estado: N/A — Day-0 (cache regenerável).
  - lock (RF-5): volta como kill-switch (mantido por uma janela; não removido até
    evidência de efeito no guest).
  - **proibido:** remover o cap de `admit`/`memwatchdog`; aposentar o lock antes de
    RF-1 provado por efeito no guest.

## Mapa Kahneman por etapa crítica

| ITEM | Disciplina | Link | Pergunta obrigatória | Evidência mínima | Abort trigger |
| --- | --- | --- | --- | --- | --- |
| ITEM-2/3 (RF-4) | #13 existência≠função | `disciplines/KAHNEMAN-DISCIPLINES.md` | "o `volume prune -f` preserva um volume ANEXADO a um container vivo?" | sub-test PAR: container vivo mantém named volume (positivo) + volume desanexado é colhido (negativo) | sem o par positivo → não mergeia (codifica "unused=descartável" sem prova) |
| ITEM-4 (RF-1) | #13 + #16 | `disciplines/KAHNEMAN-DISCIPLINES.md` | "wipe do runner-N deixa o runner-M intacto MID-JOB?" | teste de EFEITO no guest: wipe de N, checksum/inode do HOME de M inalterado durante job vivo de M | M alterado → RF-1 falhou; NÃO prosseguir p/ RF-3/RF-5 |
| ITEM-5 (RF-3) | #16 fail-safe | `disciplines/KAHNEMAN-DISCIPLINES.md` | "o wipe-por-job pode falhar o job que ele segue?" | job-completed com wipe falho → `cleanup-degraded` + exit 0 (hook.go já garante) | wipe que faz job-completed exit≠0 → regressão do incidente 2026-06-10 |
| ITEM-6 (RF-2) | #16 + #15 | `disciplines/KAHNEMAN-DISCIPLINES.md` | "backend de cache fora derruba o CI ou cai p/ build frio?" | backend down → cache MISS → build determinístico; NUNCA build sobre estado parcial | backend down causar build sobre cache corrupto/parcial → viola o piso |
| ITEM-7 (RF-5) | #13 | `disciplines/KAHNEMAN-DISCIPLINES.md` | "2 jobs docker-heavy de runners isolados colidem sem o lock?" | efeito no guest: 2 jobs concorrentes sem lock, sem colisão de cache/corrupção | colisão observada → manter o lock; lock só sai com prova |
| (todos) | #3 número não adjetivo | `disciplines/KAHNEMAN-DISCIPLINES.md` | "o teto de concorrência mudou?" | RAM-bound = 2 (MaxHeavy), não lock-bound; medido | remover cap de `admit` → OOM (proibido, DT-5) |

## Checklist de segurança (pré-implementação)

- [ ] Backend de cache escuta só loopback/LAN (`127.0.0.1`, como `registry:2`); sem credencial hardcoded
- [ ] Wipe por-runner usa o `safedelete` guardado (`workChildGuard`) — nunca amplia blast radius; root-owned _work escala só via wrapper validado
- [ ] `dockerPruneSafe` continua refcount-safe (sem `-a`, sem `--volumes` global) — só `image`+`builder`+`volume` prune `-f`
- [ ] Exec safety: `cleanup`/`hook` já usam `exec.CommandContext` sem shell
- [ ] Fail-closed: backend de cache fora → cache miss (build frio), nunca build sobre estado corrompido
- [ ] RAM: cap de `admit` (MaxHeavy=2) e `memwatchdog` (Critical 8%) preservados — efêmero NÃO os remove
- [ ] Aposentar o lock só no eixo cache; manter no eixo Optimize-VHD; kill-switch por janela

## ITEM-1 — Decisão de RAM (sem código; trava o invariante DT-5)

Registrar no IMPL que o efêmero NÃO altera o teto de concorrência. Reuso integral
de `internal/admit` (`DefaultAdmitMaxHeavy=2`, MemoryMax 2560 MB/heavy) e
`internal/memwatchdog`. Nenhuma constante removida.

- **Disciplina Kahneman:** #3 Número não adjetivo / #16 fail-safe.
  - **Pergunta:** "alguém vai ler 'efêmero resolve tudo' e remover o cap de RAM?"
  - **Evidência mínima:** o IMPL afirma com número (teto=2, RAM-bound) e o cap
    permanece no código.
  - **Abort trigger:** qualquer PR que remova `admit`/`memwatchdog` em nome do
    efêmero → bloqueado.

## ITEM-2 — RF-4: 3ª perna `volume prune -f` em `dockerPruneSafe`

**Arquivo a MODIFICAR:** `internal/cleanup/cleanup.go`, função `dockerPruneSafe`.

- **O que muda:** hoje faz `docker image prune -f` + `docker builder prune -f
  --filter until=24h`. Adicionar `docker volume prune -f` (só volumes não-anexados
  a nenhum container → ~4 GB no #13).
- **Antes:**
  ```go
  a := Action{Name: "docker_prune_safe", Path: "(docker unused: dangling images + old build cache)"}
  images, err := opts.RunFn(ctx, "docker", "image", "prune", "-f")
  ...
  cache, err := opts.RunFn(ctx, "docker", "builder", "prune", "-f", "--filter", "until=24h")
  ...
  a.BytesFreed = parseTotalReclaimed(string(images)) + parseTotalReclaimed(string(cache))
  ```
- **Depois:**
  ```go
  a := Action{Name: "docker_prune_safe", Path: "(docker unused: dangling images + old build cache + unused volumes)"}
  images, err := opts.RunFn(ctx, "docker", "image", "prune", "-f")
  ...
  cache, err := opts.RunFn(ctx, "docker", "builder", "prune", "-f", "--filter", "until=24h")
  ...
  volumes, err := opts.RunFn(ctx, "docker", "volume", "prune", "-f")
  if err != nil { a.Err = err; return a }
  a.BytesFreed = parseTotalReclaimed(string(images)) + parseTotalReclaimed(string(cache)) + parseTotalReclaimed(string(volumes))
  ```
- **Por quê:** fecha as 3 pernas do "efêmero por-run" liberando ~17.9 GB completos
  no ramo host-busy non-blocking, sem re-pull da base (RF-4).
- **Impacto:** assinatura inalterada; `dockerPrune` (wipe-total idle) inalterado.
  Compõe com `vm-disk-budget` (o prune-por-job vira o atuador dos 18 GB).
- **Testes requeridos:** ver ITEM-3.
- **Disciplina Kahneman:** #13 — ver mapa, ITEM-2/3.

## ITEM-3 — RF-4: par #13 do volume prune

**Arquivo a MODIFICAR:** `internal/cleanup/cleanup_test.go` (ou o `_test.go` que
cobre `dockerPruneSafe`).

- **O que muda:** sub-test table-driven com `RunFn` injetada:
  - **positivo (o crítico):** simular um `docker volume prune -f` que NÃO remove o
    volume anexado a um container vivo (refcount>0) — afirmar que o volume vivo
    sobrevive e o BytesFreed só soma os desanexados.
  - **negativo:** volume desanexado é colhido (BytesFreed soma a 3ª saída).
  - **erro:** `volume prune` retorna erro → `a.Err` setado, prune não silencioso.
- **Por quê:** sem o par positivo, o teste codifica a mesma premissa do código
  ("unused = descartável") — que é justamente o que precisa ser provado para um
  volume de backup órfão temporariamente down (#13, testing.md §13).
- **Padrão de referência:** o estilo de `dockerPruneSafe` em
  `civm-disk-watchdog-busy-cleanup/SPEC.md §Testes`.

## ITEM-4 — RF-1: `$HOME`/`_work`/cache por-runner

**Arquivo a MODIFICAR:** a config dos units systemd dos runners (em
`deploy/systemd/` / `register-*`) + o caminho de bootstrap que materializa o
ambiente de cada `actions.runner.*`.

- **O que muda:** cada runner-N roda com `HOME=/home/runnerN` (ou subdir dedicado
  por slot), e `GOCACHE`/`YARN_CACHE_FOLDER`/`npm_config_cache`/pnpm-store/golangci
  sob esse HOME. Hoje os 8 runners compartilham o `$HOME` do `emdev`.
- **Por quê:** disjunção física substitui serialização lógica — o wipe-por-job de
  N fica incapaz de tocar M (RF-1, elimina a classe do civm#117).
- **Impacto:** `internal/hook` e `internal/cleanup` já derivam os homes dos
  `_work` roots descobertos (`cacheHomeRoots`, `workCleanupRoots`) — o split de
  HOME é honrado por construção; nenhum hard-code de `/home/runner`. `cachetrim`
  passa a aplicar o cap por-HOME (mais homes, mesmo budget por-família dividido).
- **Testes requeridos:** teste de EFEITO no guest (não unit): wipe de N, HOME de M
  inalterado MID-JOB. Reuso da malha de descoberta de roots já testada.
- **Disciplina Kahneman:** #13 + #16 — ver mapa, ITEM-4. **Abort:** M alterado →
  NÃO prosseguir.

## ITEM-5 — RF-3: reabilitar wipe-por-job no hook job-completed

**Arquivo a MODIFICAR:** `internal/hook/hook.go`, `cleanWorkRoot` /
`cleanup(opts, ctx, purgeCaches)`.

- **O que muda:** com RF-1 entregue (HOME disjunto), o job-completed pode apagar o
  `_work` do PRÓPRIO runner por inteiro — não mais gated por disco, não mais
  preservando caches/`_temp`/workspace (esse compromisso só existia pelo `$HOME`
  compartilhado). `_actions`/`_tool` continuam preservados (caches de toolchain do
  próprio runner). job-started mantém `reclaimWorkspaceOwnership` (chown
  não-destrutivo) como rede de segurança.
- **Por quê:** clean-slate do projeto por job (RF-3); o cap de `cachetrim` vira
  backstop, não mecanismo primário.
- **Impacto:** o caminho job-STARTED de preservação do workspace ativo
  (`preserveActiveWorkspace`) permanece — só o job-COMPLETED ganha o wipe total.
  Mantém o contrato "post-job nunca falha o job" (`DecisionCleanupDegraded`,
  exit 0).
- **Testes requeridos:** unit (wipe total em job-completed com HOME isolado não
  toca sibling — via roots injetados) + o teste de efeito do ITEM-4.
- **Disciplina Kahneman:** #16 — ver mapa, ITEM-5. **Abort:** wipe que faz
  job-completed exit≠0.

## ITEM-6 — RF-2: managed cache content-addressed com backend LOCAL

**Arquivo a CRIAR/ESTENDER:** um setup análogo a
`deploy/bin/setup-registry-cache.sh` para o cache de yarn/go (ex.:
`deploy/bin/setup-ci-cache.sh`) que provisiona um backend S3-compatível LOCAL
(MinIO **ou** estende o `registry:2`/um cache-server de fork), escutando em
loopback, com bucket `ci-cache`, lifecycle de eviction por último-acesso (espelha
os 7 dias do GitHub), cap ~80 GB (8 runners × ~10 GB) dentro do `V:` de 120 GB.

- **Propósito:** servir blobs content-addressed (key = hash(go.sum)/hash(yarn.lock))
  por loopback, zero egress WAN.
- **Requisitos cobertos:** RF-2, DT-2.
- **Esqueleto vinculante (espelha `setup-registry-cache.sh`):**
  - idempotente: rodar de novo reconcilia, nunca duplica.
  - escuta só em loopback (`127.0.0.1`), sem credencial hardcoded.
  - sobrevive a `docker prune` (volume nomeado + restart=always + tagged em uso).
  - lifecycle de eviction por último-acesso, cap ~80 GB.
- **Workflows peer (acme):** trocar `cache:false` +
  `GOCACHE/YARN_CACHE_FOLDER=$HOME/.cache/...-$GITHUB_JOB` por uma action de cache
  de fork (`runs-on/cache` / `tespkg/actions-cache@v1`) apontada para o backend
  LOCAL, com `key=hash(go.sum)/hash(yarn.lock)` e `path=GOCACHE/go-mod/yarn`. Essa
  mudança vive nos REPOS peer, não no civm.
- **Padrão de referência:** `deploy/bin/setup-registry-cache.sh` (idempotência,
  loopback, restart=always, sobrevivência a prune).
- **Disciplina Kahneman:** #16 + #15 — ver mapa, ITEM-6. **Abort:** backend down
  causar build sobre cache parcial.

## ITEM-7 — RF-5: aposentar o per-PR lock COMO proteção-de-cache

**Arquivo a MODIFICAR:** os call-sites do `dockerlock` no eixo de PRUNE
(`internal/cleanup/cleanup.go` `Run` — o early-return `deferredByDockerHeavyLock`)
+ os call-sites de jobs docker-heavy que adquirem o lock só para serializar prune.

- **O que muda:** com RF-1 (isolamento físico) + RF-4 (prune refcount-safe), o
  prune é concorrência-seguro por construção e NÃO precisa da serialização
  box-wide. Remover o lock do CAMINHO de prune; manter o pacote `dockerlock`
  intacto para o eixo de reclaim do host (Stop-VM/Optimize-VHD, que disputa o `V:`)
  e como kill-switch por uma janela.
- **Por quê:** o lock existia "só para contornar o box PERSISTENTE"
  (`civm.go:134`); jobs isolados não têm o quê colidir nem corromper (RF-5).
- **Impacto:** `internal/dockerlock` NÃO é deletado (eixo Optimize-VHD usa). O
  early-return de cleanup deixa de gatear por lock; o prune-seguro roda concorrente.
- **Testes requeridos:** teste de efeito no guest (2 jobs docker-heavy isolados
  concorrentes sem lock, sem colisão) + regressão Go.
- **Disciplina Kahneman:** #13 — ver mapa, ITEM-7. **Abort:** colisão observada →
  manter o lock.

## Mudanças de estado / constantes

**Arquivo:** `internal/civm/civm.go` — nenhuma constante OBRIGATÓRIA nova. Reuso de
`DefaultAdmitMaxHeavy`, `DefaultAdmitHostReserveMB`, `DefaultHostVolume*`. SE a
action/backend exigir, ITEM-6 adiciona `DefaultCiCacheBackendAddr` (loopback) e
`DefaultCiCacheCapGB` (~80) — commit explícito com a medição do `V:` pós-split
anexada (#3 número não adjetivo).

- **Política Day-0:** consolidar a constante correta desde já; sem dual-path.
- **Migração de estado:** N/A — Day-0 (cache regenerável).

## Arquivos a CRIAR

**`deploy/bin/setup-ci-cache.sh`** (ITEM-6)
- **Propósito:** provisiona o backend de cache LOCAL content-addressed.
- **Requisitos cobertos:** RF-2, DT-2.
- **Padrão de referência:** `deploy/bin/setup-registry-cache.sh`.
- **Testes requeridos:** parse/lint shell + smoke idempotente (rodar 2× reconcilia).

## Arquivos a MODIFICAR

- `internal/cleanup/cleanup.go` — ITEM-2 (`dockerPruneSafe` 3ª perna), ITEM-7
  (remover lock do eixo prune).
- `internal/cleanup/cleanup_test.go` — ITEM-3 (par #13 do volume prune).
- `internal/hook/hook.go` — ITEM-5 (wipe-por-job em job-completed).
- config dos units systemd / bootstrap — ITEM-4 (HOME por-runner).
- workflows peer (acme, fora do civm) — ITEM-6 (action de cache de fork).

## Arquivos a DELETAR

| Arquivo | Motivo |
| --- | --- |
| (nenhum) | `dockerlock` é PRESERVADO (eixo Optimize-VHD); só sai do eixo cache |

## Observabilidade

**Logs estruturados** (`slog`/JSON, sem PII, sem label de alta cardinalidade):

| Evento | Level | Campos |
| --- | --- | --- |
| `docker_prune_safe` | Info | `bytes_freed` (3 pernas somadas) |
| `ephemeral_workroot_wiped` | Info | `work_root`, `bytes_freed` |
| `ephemeral_cache_restore` | Info | `key`, `hit` (bool), `bytes` |
| `ephemeral_cache_backend_down` | Warn | `addr` (cache miss → build frio) |
| `ephemeral_lock_retired_cache_axis` | Info | (RF-5: prune sem lock) |

## Contratos e documentação viva (sync rule)

| Documento | Atualização | Motivo |
| --- | --- | --- |
| `internal/cleanup/cleanup.go` (`dockerPruneSafe` Path) | Alterar | 3ª perna do prune |
| `internal/hook/hook.go` (comentários `cleanWorkRoot`) | Alterar | wipe-por-job agora seguro (HOME isolado) |
| `internal/civm/civm.go` | Alterar / N/A | só se ITEM-6 exigir constante de cache |
| `deploy/bin/setup-ci-cache.sh` + register | Criar | backend de cache local |
| `runbooks/MULTI-PROJECT-RUNNER.md` | Alterar | HOME por-runner; lock aposentado no eixo cache |
| `docs/specs/vm-disk-budget/*` | Alterar / N/A | o prune-por-job vira o atuador dos 18 GB |
| `README.md` / `AGENTS.md` / `CODEX.md` / `rules/*.md` | Alterar | sync rule (HOME por-runner, lock no eixo cache) |
| `docs/specs/ephemeral-clean-slate-ci/IMPL.md` | Criar | registro do que foi feito |

## Ordem de implementação

1. Slice 0 — baseline no guest real (`du`, `docker system df`, RAM).
2. ITEM-2 + ITEM-3 (RF-4: 3ª perna + par #13).
3. ITEM-4 (RF-1: HOME por-runner; POC supervisionado, teste de efeito).
4. ITEM-5 (RF-3: wipe-por-job em job-completed, depende de RF-1 provado).
5. ITEM-6 (RF-2: backend de cache local + action de fork nos peers).
6. ITEM-7 (RF-5: aposentar o lock no eixo cache; kill-switch por janela).
7. Documentação viva (runbook + sync rule).

## Plano de testes

**Guest (Go):**
- ITEM-3: `dockerPruneSafe` par #13 (volume vivo sobrevive / desanexado colhido /
  erro não-silencioso), `RunFn` injetada.
- ITEM-5: wipe-por-job em job-completed com roots isolados injetados não toca
  sibling; `cleanup-degraded`+exit 0 em wipe falho.
- Regressão: `go test ./... -race -count=1` verde; `golangci-lint` 0 issues.

**Host/guest (efeito, supervisionado):**
- ITEM-4: wipe de runner-N → HOME de runner-M inalterado MID-JOB (checksum/inode).
- ITEM-6: backend de cache down → cache MISS → build frio determinístico (nunca
  build sobre parcial).
- ITEM-7: 2 jobs docker-heavy de runners isolados concorrentes sem lock → sem
  colisão/corrupção.

**Manuais (evidência das etapas críticas):**
- `docker system df` antes/depois do prune-seguro colado no IMPL (~17.9 GB).
- log `ephemeral_cache_restore` com hit/miss + bytes.
- evidências do mapa Kahneman (par #13 do volume; efeito do isolamento por-runner).

## Checklist de validação

**Guest (Go):**
- [ ] `gofmt -w ./...`
- [ ] `golangci-lint run -c .golangci.yml ./...`
- [ ] `go vet ./...`
- [ ] `go test ./... -race -count=1`
- [ ] `go test -count=1 -cover ./internal/...` (≥80% por package tocado)
- [ ] `govulncheck ./...`
- [ ] `go build -ldflags='-s -w' -o /tmp/civmctl ./cmd/civmctl` (< 10 MB)

**Host/shell:**
- [ ] `setup-ci-cache.sh` parse/lint + smoke idempotente (rodar 2×)
- [ ] POC supervisionado: efeito do isolamento por-runner provado ANTES de RF-3/RF-5

**Docs:**
- [ ] Links locais resolvem
- [ ] Sync rule: README ≡ AGENTS ≡ CODEX ≡ rules no mesmo commit

**Gates cognitivos:**
- [ ] Cada ITEM crítico aponta para `disciplines/KAHNEMAN-DISCIPLINES.md`
- [ ] Cada ITEM crítico registra pergunta obrigatória, evidência mínima, abort trigger
- [ ] Sem linguagem vaga em pontos críticos sem critério observável

## Links Kahneman (passos críticos)

- ITEM-2/3, ITEM-4, ITEM-7: **#13** (`disciplines/KAHNEMAN-DISCIPLINES.md`) —
  sucesso é EFEITO medido (volume vivo sobrevive; wipe de N não toca M; jobs sem
  lock não colidem), nunca "a função foi chamada".
- ITEM-4, ITEM-5, ITEM-6: **#16** — a cura não pode morrer com o recurso; backend
  down = cache miss (build frio), nunca build sobre corrupção.
- ITEM-6: **#15** — cache miss é fail-fast determinístico (recompila), não
  corrupção silenciosa.
- ITEM-1 (todos): **#3** — o teto de concorrência é número medido (2, RAM-bound),
  não adjetivo; o cap de `admit` fica.
