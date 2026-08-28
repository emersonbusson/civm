---
slug: multi-project-isolation
title: Isolamento docker-heavy multi-projeto como primitivo do civm
milestone: —
issues: []
---

# PRD — Isolamento docker-heavy multi-projeto como primitivo do civm

> Tipo: mudança de plataforma do runner (`civmctl` + hooks + runbook + templates). Sem schema de banco, sem endpoint HTTP de produto, sem evento de domínio.
> Política Day-0: civm não tem produção viva com dados legados obrigatórios; backfill = N/A. Solução primária única, sem dual-path "disciplina-OU-primitivo".
> Origem: auditoria disparada por `acme#1006` (gate `tenant-isolation-smoke` instável por contenção/colisão no runner compartilhado). Corrige a **raiz no civm**, não remenda em cada peer.

---

## 1. Resumo

O civm é um runner self-hosted compartilhado por múltiplos repos peer (acme, peer) numa **única VM, com um único daemon Docker e um único disco** (Confirmado em docs — `runbooks/MULTI-PROJECT-RUNNER.md:393-397`). Hoje, todo o isolamento de trabalho docker-heavy entre projetos concorrentes é **delegado à disciplina do consumidor**: o runbook manda "sempre passar `--project-name <repo>-<run-id>`" e "nunca bindar portas fixas", mas o civm **não fornece, não injeta e não verifica** nada disso (Confirmado em docs — `MULTI-PROJECT-RUNNER.md:398-424`).

É uma guardrail por convenção, não por construção — e falha de forma observável. O `acme#1006` existe porque o acme não seguiu a regra não-fornecida: `infra/docker-compose.yml` fixa `name: acme`, 24 `container_name: acme-*` e portas de host estáticas, sem nenhum `COMPOSE_PROJECT_NAME` (Confirmado no codebase — acme `infra/docker-compose.yml:1`, `tools/devctl/internal/ci/run.go:155-162,731-751`). Resultado: dois jobs docker-heavy co-residentes colidem em nome de container, rede, volume e porta de host, e o build de ~40 min satura o daemon/disco — derrubando justamente o gate de **isolamento de tenant**, a defesa de maior severidade do produto.

O problema é estrutural e **comum a todos os peers**, não específico do acme. Vitae tem o mesmo footgun latente assim que rodar `docker compose` no runner. A auditoria confirmou que o civm **não tem nenhum primitivo de serialização** para trabalho docker-heavy (activeruns/reversewatchdog/idle são read-only; capacity é hard-fail passivo a 90%; cleanup dispara a 60%) (Confirmado no codebase — `internal/activeruns/activeruns.go`, `internal/reversewatchdog/reversewatchdog.go`, `internal/civm/civm.go:37-40`), e que um lock segurado por um bring-up longo pode **starvar o próprio disk-watchdog/cleanup do civm** até o hard-fail de 90% (Confirmado em docs — acme `docs/specs/M48/1006-...PRD.md:307`).

Este PRD move o isolamento docker-heavy **para dentro do civm como primitivo de primeira classe**: **fornecido** (cada runner recebe identidade e faixa de porta disjuntas via `.env`), **enforced** (um linter recusa compose/workflow de peer que viole as invariantes) e **observável** (lock-wait/hold, colisões, faixa de porta em `capacity --json`/`hooks.jsonl`). Objetivo: acme/peer com CI docker-heavy **correto por padrão, livre de colisão e fail-safe**, consumindo um contrato único publicado em vez de cada repo re-implementar (errado) a disciplina.

Valor: o civm passa a "funcionar de forma linda nos repos" — o peer não precisa entender a física do daemon compartilhado; consome `CIVM_PORT_BASE`/`COMPOSE_PROJECT_NAME` e `civmctl lock`, e o `ci-guard` o impede de mergear config colidível. O `acme#1006` deixa de ser fix local e vira **consumidor fino** deste primitivo.

---

## 2. Contexto técnico

### Componentes envolvidos e papel na topologia civm

| Componente | Papel | Estado |
| --- | --- | --- |
| `internal/hook` (`hook.go`, `install.go`) | hooks job-started/job-completed + escrita do `.env` por runner | Confirmado no codebase |
| `internal/runner` (`runner.go`) + `cmd/civmctl/runner.go` | `civmctl runner add` (labels, short, config.sh) | Confirmado no codebase |
| `internal/civm/civm.go` | constantes `Default*` (thresholds, caches, timeouts) | Confirmado no codebase |
| `internal/capacity` (`capacity.go`) | `civmctl capacity --json` (struct `Report`) | Confirmado no codebase |
| `internal/cleanup`, `internal/diskwatchdog` | cleanup/watchdog de disco | Confirmado no codebase |
| `internal/diskaudit` | varredura de disco (padrão Glob/WalkDir reutilizável) | Confirmado no codebase |
| `cmd/civmctl/main.go` | dispatch por `switch os.Args[1]` (26 subcomandos) | Confirmado no codebase |
| `runbooks/MULTI-PROJECT-RUNNER.md` | contrato publicado de multi-projeto | Confirmado em docs |
| `templates/CIVM-USAGE.md`, `templates/*-ci-router.yml.template` | template portável de consumo | Confirmado no codebase |

### Estado atual confirmado no codebase

- **Daemon Docker único por VM; runner = 1 job por vez.** Cada `actions.runner.<owner>-<repo>.civm-<short>.service` roda sequencialmente um job (Confirmado em docs — `MULTI-PROJECT-RUNNER.md:51-79,393-397`). N runners (default = nº de repos) rodam N jobs concorrentes, todos no **mesmo** `docker.sock` e disco.
- **`.env` por runner é o único ponto de injeção persistente.** `internal/hook/install.go:upsertEnv()` (linhas 136-156) **reescreve** `/home/*/actions-runner*/.env`, removendo só as chaves `ACTIONS_RUNNER_HOOK_*` e **preservando todo o resto** dos pares `KEY=VALUE`; depois reanexa os dois paths de hook. Logo, chaves arbitrárias por runner **podem** ser injetadas estendendo o slice preservado antes do `WriteFile` (Confirmado no codebase — `internal/hook/install.go:136-156`). O actions-runner carrega o `.env` no processo do serviço no boot, e jobs herdam o ambiente (Confirmado em docs — comportamento do GitHub self-hosted runner; o código já depende disso ao injetar `ACTIONS_RUNNER_HOOK_*`).
- **Hooks de job NÃO conseguem exportar env para os steps do job.** `job-started`/`job-completed` rodam em **processo separado** (script `.sh` → `civmctl hook <event>`); o código só **lê** `os.Getenv` para decidir cleanup, nunca exporta de volta (Confirmado no codebase — `internal/hook/hook.go`, `install.go:168-170`). Consequência: o civm fornece identidade/porta **estável por runner** via `.env`, mas **não** valor per-run dinâmico via hook; a unicidade per-run (`$GITHUB_RUN_ID`) é concatenação fina do consumidor.
- **Identidade estável por runner já existe:** `AddOptions.Short` (`internal/runner/runner.go:19-32`), diretório `~/actions-runner-<short>`, nome `civm-<short>` (linha 93). Não há índice/slot numérico nem grupo de runner — só labels (Confirmado no codebase — `runner.go:87-93`).
- **Labels já são configuráveis:** `runner add --label` (default `civm`, CSV) repassado a `config.sh --labels` (Confirmado no codebase — `cmd/civmctl/runner.go:180-212`, `internal/runner/runner.go:142-145`). `civmctl runner add --label civm,civm-e2e --short e2e` já funciona hoje.
- **Não existe primitivo de lock em Go.** Busca por `flock`/`syscall.Flock` retornou zero no código; o civm usa `flock` só no **shell** dos seus timers (`flock /run/civmctl-cleanup.lock`, `/run/civmctl-runner-watchdog.lock`) (Confirmado no codebase — `internal/hook/hook.go:19` importa `syscall` só para `Statfs_t`; Confirmado em docs — `MULTI-PROJECT-RUNNER.md:262-265`).
- **Não existe linter de arquivos de repo consumidor.** `internal/drift` varre apenas o README upstream do actions/runner-images; nada lê `docker-compose*.yml` ou `.github/workflows/*` (Confirmado no codebase — `internal/drift/drift.go:98-122`). O padrão `Glob`/`WalkDir` + funções injetadas de `internal/diskaudit` e `internal/hook` é reutilizável para um linter novo.
- **Thresholds atuais** (`internal/civm/civm.go:15-62`, Confirmado no codebase): `DefaultPreCleanupPct=60`, `DefaultHardFailPct=90`, `DefaultWatchdogThresholdPct=60`, `DefaultCapacityMaxDiskPct=90`, `DefaultReverseMaxAgeHours=2`, caches (go-build 5GB, npm/yarn 3GB, pnpm 5GB), prune (buildx `until=24h`, image `until=168h`), `DefaultRunnerVersion=2.336.0`.
- **`capacity --json` → `Report{ DiskPath, DiskUsedPct, DiskFreeGB, DiskTotalGB, RunnerServices, RunnerWorkers, AcceptingJobs, Reason }`** (Confirmado no codebase — `internal/capacity/capacity.go:17-26`). Endpoint de readiness leve consumido por dashboards/Busson.
- **Dispatch civmctl = `switch os.Args[1]`** (não cobra); 26 subcomandos; subgrupos existem (`runner add`, `hook install`, `ci local-report`, `metrics dump`) (Confirmado no codebase — `cmd/civmctl/main.go:27-96`). Novo comando = `case` + `runX(args) int` + `printHelp()`.
- **Contrato de hook hoje:** `job-started` checa pressão de disco (pré-cleanup 60% → limpa; hard-fail 90% → rejeita o job); `job-completed` poda docker + trim de caches; eventos em `/var/log/civm/hooks.jsonl` (Confirmado em docs — `MULTI-PROJECT-RUNNER.md:812-829`). **Nenhuma** coordenação com lock docker-heavy de consumidor.

### Confirmado na documentação oficial

- **GitHub Actions self-hosted:** o arquivo `.env` no diretório do runner é carregado no ambiente do serviço e herdado pelos jobs; dedicar uma classe de job a um runner se faz por **label** adicional em `runs-on`/runner group; `pull_request` de fork roda sem segredos do repo e `pull_request_target` é proibido pelo contrato civm (Confirmado em docs — `.claude/rules`/`civm.md` do acme + docs.github.com).
- **Docker Compose:** precedência de project name `-p` > `COMPOSE_PROJECT_NAME` > `name:` do arquivo > basename do dir. `COMPOSE_PROJECT_NAME` **sobrepõe** `name:`. `container_name` é global por daemon e **impede** múltiplas instâncias/escala — incompatível com co-residência (Confirmado em docs — docs.docker.com "Specify a project name", "container_name"). Project name isola redes/volumes/nomes auto-gerados, **mas não host ports** (recurso global do host).

### O que está sendo proposto (Inferência / proposta)

Mover isolamento de **disciplina** para **primitivo civm**: (a) identidade + faixa de porta disjuntas injetadas por runner no `.env`; (b) lock box-wide docker-heavy como subcomando civmctl; (c) cleanup/watchdog lock-aware; (d) linter `ci-guard` que recusa violação; (e) classe de runner dedicada via label; (f) observabilidade; (g) contrato único publicado.

### Tenant scope

N/A para persistência. Em jogo está a **integridade dos gates** de cada peer que validam isolamento de tenant em runtime. Nenhum dado de tenant é tocado.

---

## 3. Opção recomendada

### Solução escolhida

**Isolamento docker-heavy como primitivo civm fornecido + enforced + observável, mantendo o daemon Docker único** (defense-in-depth, Day-0 único):

1. **Identidade de isolamento por runner (RF-1).** `civmctl hook install` grava no `.env` de cada runner: `CIVM_RUNNER_SLOT=<short>` (identidade estável), `CIVM_PORT_BASE=<base de bloco disjunto>` (faixa de ~64 portas por runner, sem sobreposição entre runners), e `COMPOSE_PROJECT_NAME=<slot>` como default. Como cada runner roda 1 job por vez, as faixas são **box-únicas a qualquer instante**. O consumidor offseta suas portas de host de `CIVM_PORT_BASE` e usa `COMPOSE_PROJECT_NAME=${CIVM_RUNNER_SLOT}-${GITHUB_RUN_ID}` para unicidade per-run.
2. **Lock box-wide docker-heavy (RF-2).** Novo `civmctl lock acquire|release|--exec` sobre `syscall.Flock` em `/run/civm/docker-heavy.lock`, com **orçamento de hold-time**, heartbeat/PID para auto-release de lock obsoleto, e exit codes estáveis. Substitui o flock repo-local divergente do acme (`/tmp/${owner}-${repo}-go-integration.lock`) por caminho único honrado por todos os peers.
3. **Cleanup/disk-watchdog lock-aware (RF-3).** `job-started`/cleanup/watchdog passam a enxergar lock docker-heavy ativo (heartbeat fresco) e **adiam** prune destrutivo enquanto o lock vive, até o orçamento; estourado o orçamento, o lock é force-released e o job falha fechado. Elimina a starvação documentada (hold de 40 min → hard-fail 90%).
4. **`civmctl ci-guard` (RF-4).** Subcomando read-only que varre `infra/**/docker-compose*.yml` + `.github/workflows/*.yml` do repo consumidor e **recusa** (exit 1) `container_name` fixo, bind de porta de host estática, ausência de `COMPOSE_PROJECT_NAME`/`--project-name`, e step docker-heavy sem `civmctl lock`. Transforma os imperativos do runbook em guardrail.
5. **Classe de runner dedicada (RF-5).** Formalizar `civmctl runner add --label civm,civm-e2e` para job E2E pesado em slot próprio; `civmctl doctor` checa existência quando o peer opta. Labels já funcionam — custo near-zero.
6. **Observabilidade (RF-6).** lock-wait/lock-hold, contador de colisão de project-name (= 0 esperado), faixa de porta por runner e gauge "docker-heavy ativo" em `capacity --json` (estende `Report`) e `hooks.jsonl`.
7. **Contrato único publicado (RF-7).** `MULTI-PROJECT-RUNNER.md`, `templates/CIVM-USAGE.md`, template `*-ci-router.yml`, `PEER-ADOPTION-CHECKLIST.md` reescritos para consumo copy-paste correto.

### Motivo da escolha

- **Ataca a raiz medida, não o sintoma.** A auditoria provou que a delegação por disciplina falha (acme#1006) e que não há primitivo nenhum. Fornecer + enforced + observável é a única forma de o civm garantir o invariante para **todos** os peers em vez de torcer para cada um implementar certo.
- **Respeita a física verificada.** Daemon único (mantido), runner sequencial, `.env` per-runner estático, hook sem env-para-step. O design encaixa nesses fatos confirmados em código, sem inventar capacidade inexistente.
- **Reuso máximo.** Labels (já existem), padrão Glob/WalkDir (`diskaudit`), dispatch por `switch`, `.env` writer (`upsertEnv`), thresholds (`civm.go`). Só o lock e o linter são código novo.
- **Fail-safe por construção.** Lock indisponível/estourado → job falha alto, nunca pula o gate; `ci-guard` recusa config colidível antes do merge; cleanup lock-aware não derruba bring-up legítimo nem deixa o disco estourar silenciosamente.
- **Resolve a pergunta de fronteira.** "O civm deveria resolver porta?" — sim: o civm **aloca** faixas disjuntas (o que só ele garante box-wide) e **verifica** o uso; o consumidor só referencia a faixa. O civm não edita o compose de cada repo, mas entrega base segura e recusa quem não a usou.

### Alternativas descartadas

| Alternativa | Por que descartada |
| --- | --- |
| **Manter "isolamento por disciplina" só adicionando o linter** | Meia-medida: não dá ao consumidor primitivo de porta/lock; cada repo ainda hand-rolla portas e flock, divergindo. O acme#1006 nasceu desse modelo. |
| **dockerd rootless / `DOCKER_HOST` por runner (isolamento real de daemon)** | Eliminaria toda disciplina de porta, mas multiplica imagens/cache/disco numa VM que já bate hard-fail a 90%; caveats de rede/perf rootless. Inviável no VM 128GB single-daemon atual. **Deferido** atrás de gate de expansão de disco/RAM. |
| **Injeção per-run dinâmica via hook de job** | Impossível: hooks rodam em processo separado e não exportam env para steps (Confirmado no codebase — `hook.go`). |
| **Fix só no acme (#1006), runbook continua imperativo** | Deixa peer exposto ao mesmo footgun; mantém caminho de lock divergente; contraria o pedido de corrigir a raiz no civm. |
| **Lock como `sync.Mutex`/flag em `/tmp`** | `sync.Mutex` não cruza processos; flag em `/tmp` não tem semântica de stale/heartbeat. `syscall.Flock` em `/run/civm/` é o primitivo correto e auditável. |
| **`COMPOSE_PROJECT_NAME` per-runner sem `$GITHUB_RUN_ID`** | Protege cross-repo, mas um leftover de job crashado no mesmo runner colidiria com o próximo bring-up. Per-run (`<slot>-<run_id>`) + remoção de `container_name` fixo no consumidor fecha o gap. |

### Trade-offs aceitos

- **O consumidor ainda precisa mudar** (offsetar portas de `CIVM_PORT_BASE`, usar `COMPOSE_PROJECT_NAME`, envolver bring-up no lock, remover `container_name` fixo). Aceito: o civm não edita o YAML de cada peer; fornece o primitivo e recusa o uso errado via `ci-guard`.
- **O lock box-wide serializa trabalho docker-heavy a 1 por vez**, trocando throughput por confiabilidade. Aceito: o detector de changes de cada peer já limita frequência de jobs pesados; o orçamento de hold limita o pior caso; a classe de runner dedicada dá slot próprio ao pesado.
- **`CIVM_PORT_BASE` por runner reduz o universo de portas** por slot a bloco finito (~64). Aceito: suficiente para a maior stack (acme publica ~13 portas de host) com folga.
- **Daemon único permanece** — sem isolamento de kernel entre jobs concorrentes além de namespaces de container do Docker. Aceito como Day-0; isolamento de daemon deferido com gate.

---

## 4. Requisitos funcionais

### RF-1 — Identidade de isolamento por runner injetada pelo civm

`civmctl hook install` grava no `.env` de cada runner descoberto: `CIVM_RUNNER_SLOT`, `CIVM_PORT_BASE`, `COMPOSE_PROJECT_NAME` (default = slot), preservando todas as outras chaves (incluindo `ACTIONS_RUNNER_HOOK_*`).

- **Critério de aceite:** após `civmctl hook install --execute`, cada `/home/*/actions-runner*/.env` contém as 3 chaves; runners distintos recebem `CIVM_PORT_BASE` **disjuntos** (blocos sem sobreposição); reexecução é idempotente (mesmos valores). Teste unitário sobre `upsertEnv`/atribuição de base assertando disjunção e idempotência.
- **Tenant isolation:** indireto — garante que jobs co-residentes de peers distintos não colidam, preservando a validade dos gates de isolamento. Não toca dados de tenant.

### RF-2 — Lock box-wide docker-heavy como primitivo civmctl

`civmctl lock acquire|release` (e/ou `civmctl lock --exec -- <cmd>`) sobre `syscall.Flock` em `/run/civm/docker-heavy.lock`, com orçamento de hold-time configurável, auto-release de lock obsoleto (PID morto/heartbeat vencido) e exit codes estáveis.

- **Critério de aceite:** dois processos disputando o lock serializam (o segundo bloqueia/enfileira até o budget); lock de PID morto é recuperado sem deadlock; `--exec` libera o lock mesmo se o comando falhar (defer). Teste de concorrência (2 goroutines/processos) + teste de stale-lock com fake clock/PID.
- **Tenant isolation:** N/A (mecanismo de CI). **Nunca** pode resultar em pular um gate.

### RF-3 — Cleanup/disk-watchdog lock-aware (anti-starvação)

`job-started`, `civmctl cleanup` e `civmctl disk-watchdog` passam a detectar lock docker-heavy ativo (heartbeat fresco) e **adiam prune destrutivo** enquanto o lock vive, até o orçamento; estourado o orçamento, force-release + falha fechada.

- **Critério de aceite:** com lock fresco ativo, cleanup adia prune e loga `deferred-by-docker-heavy-lock`; com lock estourado/obsoleto, cleanup prossegue; nunca atinge hard-fail 90% silenciosamente sem evento. Teste com lock mockado nos dois estados.
- **Tenant isolation:** N/A. Não pode pular gate nem deixar disco estourar sem sinal.

### RF-4 — `civmctl ci-guard`: linter de compose/workflow do consumidor

Subcomando read-only que varre `infra/**/docker-compose*.yml` + `.github/workflows/*.yml` e recusa: `container_name` fixo; bind de host port estático; ausência de `COMPOSE_PROJECT_NAME`/`--project-name` em invocação compose de CI; step docker-heavy sem `civmctl lock`.

- **Critério de aceite:** `civmctl ci-guard --repo-root <path>` retorna `0` para um repo conforme e `1` com `file:line` + remediação para cada violação; `--json` estruturado. Testes table-driven com fixtures conforme/violador (reuso do padrão `diskaudit`).
- **Tenant isolation:** indireto — bloqueia config colidível antes do merge, protegendo o gate.

### RF-5 — Classe de runner dedicada por label

Formalizar `civmctl runner add --label civm,civm-e2e --short <s>` para job E2E pesado; `civmctl doctor` reporta presença/ausência do runner dedicado quando o peer declara que o usa.

- **Critério de aceite:** runner registrado com labels múltiplos aparece online com ambos os labels; `doctor` distingue presença; reversível removendo o label. Teste de `ValidateLabels` + verificação manual documentada.
- **Tenant isolation:** N/A (agendamento de CI).

### RF-6 — Observabilidade do isolamento

Emitir lock-wait/lock-hold, contador de colisão de project-name (= 0 esperado), `CIVM_PORT_BASE` efetivo por runner e gauge "docker-heavy ativo"; superfície em `capacity --json` (estende `Report`) e `hooks.jsonl`.

- **Critério de aceite:** `capacity --json` inclui os novos campos; `hooks.jsonl` ganha linhas `lock_acquire`/`lock_release` com wait/hold; documentado como base dos rollback triggers numéricos. Teste de serialização do `Report` estendido.
- **Tenant isolation:** N/A.

### RF-7 — Contrato único publicado e consumo dos peers

Reescrever `MULTI-PROJECT-RUNNER.md` (§Riscos compartilhados → §Isolamento fornecido), `templates/CIVM-USAGE.md`, template `*-ci-router.yml` e `PEER-ADOPTION-CHECKLIST.md` para consumo copy-paste; acme/peer passam a consumir `CIVM_PORT_BASE`/`COMPOSE_PROJECT_NAME` + `civmctl lock` + `civmctl ci-guard`.

- **Critério de aceite:** runbook descreve as 3 chaves de `.env`, o caminho do lock, os exit codes e o contrato do `ci-guard`; `PEER-ADOPTION-CHECKLIST.md` lista os passos; `docs/INDEX.md` regenerado (`npm run docs:check`).
- **Tenant isolation:** N/A (documentação).

---

## 5. Requisitos não-funcionais

### Performance

- **Alvo primário:** trabalho docker-heavy multi-projeto **deterministicamente sem colisão** (porta/nome/rede/volume) e gates dos peers verdes sob carga concorrente. Métrica de sucesso: colisão de project-name/porta → **0** sobre N runs; sem cancelamento por timeout induzido por contenção.
- Overhead do `.env`/`ci-guard`/lock desprezível (< alguns segundos) no caminho do job. `ci-guard` é varredura de arquivos local.
- p50/p95 de lock-wait capturados (RF-6) e usados como baseline; serialização só onde necessário (docker-heavy), nunca em jobs leves.

### Segurança

- **Sem `pull_request_target`** com runner self-hosted rodando código de PR (Confirmado em docs — `civm.md` do acme + governance civm). `ci-guard` e o linter rodam sobre o checkout do PR sem segredos extras.
- O `.env` por runner **não** carrega segredo; só identidade/porta/projeto. Nenhum IP/credencial de VM em regra de peer.
- `civmctl lock` opera em `/run/civm/` (root-owned, fora do workspace de job); não expõe estado a jobs além do contrato CLI.

### Observabilidade

- lock-wait/lock-hold (gauge/histograma), colisão de project-name (counter, = 0), `CIVM_PORT_BASE` por runner, gauge docker-heavy ativo — em `capacity --json` + `hooks.jsonl`. Sem PII, sem segredo, sem label de alta cardinalidade.

### Escalabilidade

- `CIVM_PORT_BASE` por runner torna a stack horizontalmente segura para N runners co-residentes. Bloco de ~64 portas/runner cobre a maior stack peer com folga.
- O lock limita trabalho docker-heavy concorrente a 1 para proteger daemon/disco único; o detector de changes de cada peer limita frequência; a classe de runner dedicada dá slot próprio ao pesado.
- Com N peers: o lock só agrega valor se peers de fato rodam compose docker-heavy no mesmo box (lacuna aberta — confirmar peer).

### LGPD

- N/A. Nenhum dado pessoal processado, armazenado ou logado. Suítes E2E dos peers usam tenants/seeds de teste efêmeros.

### Resiliência

- **Fail-safe é invariante:** lock indisponível/estourado → job falha alto, nunca pula gate; stack unhealthy → falha fechada; `ci-guard` recusa config colidível. Cleanup lock-aware adia prune sob lock fresco mas force-release + falha sob lock estourado, sem deixar disco atingir 90% sem evento.
- Worst-cases (lock starvado, disco a 90% sob lock, runner dedicado down, daemon down) documentados em runbook + `DEGRADATION-MATRIX.md` (se existir no peer).

---

## 6. Fluxos

### Happy path (job docker-heavy de um peer, após a mudança)

1. Operador roda `civmctl hook install --execute` na VM → cada runner `.env` ganha `CIVM_RUNNER_SLOT`/`CIVM_PORT_BASE`/`COMPOSE_PROJECT_NAME` (RF-1). (One-time/idempotente.)
2. PR aberto no peer → job docker-heavy agenda no runner `civm`/`civm-e2e` (RF-5). O job herda as 3 chaves do `.env` do runner.
3. Step de pré-flight do peer roda `civmctl ci-guard --repo-root .` (RF-4) → recusa se houver `container_name` fixo, porta estática, project-name ausente ou docker-heavy sem lock.
4. O peer adquire o lock: `civmctl lock --exec --scope docker-heavy --budget 50m -- <bring-up>` (RF-2). Bloqueia/enfileira se outro docker-heavy estiver ativo no box.
5. Bring-up: `docker compose -p "${COMPOSE_PROJECT_NAME}-${GITHUB_RUN_ID}" ... up` com portas de host offsetadas de `${CIVM_PORT_BASE}`. Projeto/rede/containers/portas disjuntos de qualquer outro runner.
6. Cleanup/watchdog veem o lock fresco e **adiam** prune destrutivo (RF-3).
7. Healthcheck do peer → suíte do peer (ex.: tenant-isolation smoke do acme) usando as portas offsetadas.
8. `civmctl lock` libera (defer do `--exec`) quando o bring-up estabiliza/termina; cleanup volta a podar.
9. `job-completed` hook poda docker recuperável + trim de caches (já existente).

### Fluxos alternativos

- **Runner dedicado `civm-e2e` ausente:** job pesado roda no `civm` geral; RF-1 (porta/projeto) + RF-2 (lock) carregam a maior parte do benefício; `doctor` sinaliza ausência.
- **Peer sem docker-heavy (ex.: stack serverless):** `ci-guard` passa trivialmente; lock não é adquirido; nenhum overhead.

### Fluxos de erro

| Condição | Resultado | Log/severidade | Consistência |
| --- | --- | --- | --- |
| `ci-guard` acha violação | exit 1 com `file:line` + remediação; CI vermelho | `error` estruturado | Nenhuma mudança; bloqueia merge colidível |
| Lock indisponível/contendido além do budget | job falha alto (não pula gate) | `warning` ao enfileirar; `error` ao estourar | Nenhuma — projeto isolado por run |
| Lock obsoleto (PID morto) | auto-release + adquire | `warning` stale-recovered | Nenhuma |
| Disco a 90% sob lock estourado | force-release + cleanup + job falha fechado | `error` hard-fail com evento | `job-completed` poda no defer |
| Runner `civm-e2e` down | job aguarda/falha alto, não roda em slot sem guarda | `error`/`queued` | Nenhuma |

---

## 7. Modelo de dados

**N/A — civm é CLI/systemd, sem banco.** Nenhuma tabela, índice ou constraint. Backfill = **N/A — Day-0, sem produção viva**.

Estados persistidos (não-relacionais, inalterados em formato exceto onde anotado):
- `/home/*/actions-runner*/.env` — ganha 3 chaves novas (`CIVM_RUNNER_SLOT`, `CIVM_PORT_BASE`, `COMPOSE_PROJECT_NAME`) preservando o resto (Confirmado no codebase — `install.go:136-156`).
- `/run/civm/docker-heavy.lock` — arquivo de lock novo (flock + heartbeat/PID).
- `/var/log/civm/hooks.jsonl` — ganha linhas `lock_acquire`/`lock_release`.
- Mapa slot→`CIVM_PORT_BASE`: determinístico a partir do `short` (sem estado persistente novo) ou tabela leve em `/var/lib/civm/` (decisão fechada no SPEC).

---

## 8. API / Interfaces

Sem endpoint HTTP, schema OpenAPI/SDK ou evento Redis. Interfaces são contratos de CLI/`.env`/lock:

### CLI civmctl (novo/estendido)

| Interface | Mudança |
| --- | --- |
| `civmctl lock acquire\|release\|--exec` | **novo** subcomando (RF-2): flock `/run/civm/docker-heavy.lock`, `--scope`, `--budget`, exit codes |
| `civmctl ci-guard [--repo-root] [--json]` | **novo** subcomando (RF-4): linter compose/workflow |
| `civmctl hook install` | estende `.env` writer com as 3 chaves (RF-1) |
| `civmctl runner add --label civm,civm-e2e` | já suportado; formalizado (RF-5) |
| `civmctl cleanup` / `disk-watchdog` / `hook job-started` | lock-aware (RF-3) |
| `civmctl capacity --json` | `Report` estendido com campos de lock/porta (RF-6) |

### Contrato `.env` por runner (consumido pelos peers)

| Chave | Significado | Default |
| --- | --- | --- |
| `CIVM_RUNNER_SLOT` | identidade estável do runner (= `short`) | — |
| `CIVM_PORT_BASE` | base do bloco de portas de host disjunto | — |
| `COMPOSE_PROJECT_NAME` | nome de projeto default (slot); peer concatena `-${GITHUB_RUN_ID}` | `<slot>` |

### Variáveis de ambiente / paths novos

- `/run/civm/docker-heavy.lock` (path do lock — publicado, consumido pelos peers).
- `CIVM_DOCKER_HEAVY_LOCK` (override opcional do path; default acima) — se introduzido, documentar.
- `CIVM_PORT_BASE`, `CIVM_RUNNER_SLOT` (env por runner, acima).

### Impacto em OpenAPI / SDK / BFF / Eventos

**N/A.** Sem mudança de contrato de produto.

---

## 9. Dependências e riscos

### Pré-requisitos

- Confirmar se peer roda compose docker-heavy no box (lacuna aberta) — define quanto o lock agrega cross-repo.
- Operador roda `civmctl hook install --execute` + `self-upgrade` para distribuir o binário com as novas chaves/lock.

### Riscos técnicos (com mitigação)

| Risco | Mitigação |
| --- | --- |
| Lock segurado por bring-up longo starva cleanup/watchdog (hard-fail 90%) | RF-3 lock-aware + orçamento de hold-time + force-release; evento explícito |
| `CIVM_PORT_BASE` colide com portas default do compose do peer (minio 9020/9021, nginx 81, etc.) | Bloco por runner escolhido fora dos defaults conhecidos; `ci-guard` valida que o peer mapeou tudo no bloco |
| `.env` injeção quebra runner existente | `upsertEnv` preserva chaves; idempotente; teste de round-trip; `doctor` valida `.env` |
| `ci-guard` falso-positivo bloqueia peer legítimo | Regras conservadoras + `--json` + allowlist por arquivo documentada; rollout em modo report antes de enforce |
| Acoplamento: peers precisam adotar o contrato | RF-7 publica contrato + checklist; acme#1006 é o primeiro consumidor de prova |
| Daemon único permanece SPOF de isolamento de kernel | Aceito Day-0; isolamento de daemon deferido atrás de gate de expansão |

### Impacto em componentes existentes

`internal/hook` (escrita `.env` + lock-aware), `internal/cleanup`/`internal/diskwatchdog` (lock-aware), `internal/capacity` (`Report` estendido), `cmd/civmctl/main.go` (2 subcomandos novos no `switch`), runbooks/templates. Nenhum runtime de produto de peer afetado além do CI.

### Breaking changes

Nenhum breaking de produto. Mudança de comportamento de plataforma de CI: peers que **não** adotarem o contrato continuam como hoje (degradado, sem o primitivo); `ci-guard` só bloqueia quando rodado pelo peer.

### Estratégia de rollout

Por dependência: **Slice 0** (profiling/baseline) → **Slice 1** (RF-1 `.env` + RF-2 lock, civmctl) → **Slice 2** (RF-3 lock-aware + RF-6 observabilidade) → **Slice 3** (RF-4 `ci-guard` em modo report → enforce) → **Slice 4** (RF-5 runner dedicado) → **Slice 5** (RF-7 docs/templates + adoção acme como prova). Cada slice atrás de validação na VM.

### Estratégia de rollback

Reversível por-slice, sem estado de infra a desfazer: remover as 3 chaves do `.env` (re-`hook install` versão anterior); `civmctl lock` é no-op se não invocado; `ci-guard` é opt-in no peer; `Report` estendido é aditivo. **Rollback trigger numérico (fechar no SPEC):** reverter a slice ofensora se colisão de project-name/porta > 0 sobre N runs, OU lock causar cancelamento por timeout, OU `ci-guard` gerar falso-positivo bloqueante não-allowlistado.

### Hipóteses que exigirão disciplina explícita no SPEC (`disciplines/KAHNEMAN-DISCIPLINES.md`)

- **#3 (número, não adjetivo):** "~40 min", "~8 slots", "128GB" vêm de texto de issue/runbook; Slice 0 deve **medir** (`civmctl capacity --json` + profiling do bring-up de cada peer).
- **#2 (counterfactual):** definir números concretos (colisão, lock-wait p95, falso-positivo) no SPEC.
- **#5 (availability/worst-case):** lock starvando watchdog; `.env` quebrando runner; `ci-guard` falso-positivo; daemon único — todos com mitigação acima e linha em runbook.

---

## 10. Estratégia de implementação

| Slice | Conteúdo | Depende de | Validável cedo |
| --- | --- | --- | --- |
| **Slice 0 — Baseline** | `civmctl capacity --json` do box vivo + profiling do bring-up dos peers (medir slots, disco, tempo). Sem código. | — | Output colado no SPEC; de-risca claims numéricos |
| **Slice 1 — Primitivos (RF-1, RF-2)** | `.env` writer estendido + atribuição de `CIVM_PORT_BASE` disjunto; `civmctl lock` (flock + budget + stale). | Slice 0 | Teste unit/concorrência local; round-trip de `.env` |
| **Slice 2 — Resiliência + obs (RF-3, RF-6)** | cleanup/watchdog lock-aware; `Report` estendido; linhas `hooks.jsonl`. | Slice 1 | Teste com lock mockado fresco/estourado |
| **Slice 3 — Enforcement (RF-4)** | `civmctl ci-guard` (report → enforce). | Slice 1 | Fixtures conforme/violador; rodar contra acme real |
| **Slice 4 — Runner dedicado (RF-5)** | formalizar label `civm-e2e` + check no `doctor`. | Slice 1 | `runner add --label` + verificação online |
| **Slice 5 — Contrato + adoção (RF-7)** | runbook/templates/checklist; acme#1006 consome (prova). | Slices 1-4 | `npm run docs:check`; smoke do acme verde |

**Validável cedo:** Slice 0 (sem código) e Slice 1 (local). **Exige VM/coordenação:** Slices 2-5. **Backfill/migração:** N/A — Day-0.

---

## 11. Documentos a atualizar (mesmo PR da slice correspondente)

- `docs/specs/multi-project-isolation/{PRD.md (este), SPEC.md, IMPL.md}`
- `runbooks/MULTI-PROJECT-RUNNER.md` (§Riscos compartilhados → §Isolamento fornecido; lock; `.env`; `ci-guard`; label `civm-e2e`)
- `templates/CIVM-USAGE.md` + `templates/acme-ci-router.yml.template` + `templates/ci-router.yml.template`
- `runbooks/PEER-ADOPTION-CHECKLIST.md` + `runbooks/ORG-RUNNER-ADOPTION.md`
- `disciplines/KAHNEMAN-DISCIPLINES.md` (link de etapas críticas — só se nova âncora)
- `AGENTS.md`/`CODEX.md` (se mudar regra de trabalho/boundary)
- `docs/INDEX.md` (regenerado via `npm run docs:index`)
- Lado consumidor (acme, fora deste repo): `docs/specs/M48/1006-...`, `.claude/rules/civm.md`, `docs/CIVM.md`

## 12. Fora de escopo

| Item | Motivo |
| --- | --- |
| **dockerd rootless / `DOCKER_HOST` por runner** | Inviável no VM single-daemon disk-pressured; deferido atrás de gate de expansão de disco/RAM |
| **Reescrever billing/release/parity/drift do civm** | Fora do problema de isolamento docker-heavy; manter foco |
| **Migrar para N VMs (1 por repo)** | Capex; só se rollback trigger de crosstalk/disco disparar (já no runbook) |
| **Mudança em produto de peer (schema/endpoint/SDK)** | Puramente plataforma de CI |
| **Implementar o fix no acme aqui** | acme é consumidor; o SPEC do acme#1006 vira slice fina separada |

## 13. Critérios de aceitação

- Dois jobs docker-heavy de peers distintos co-residentes → **0** colisão de container/rede/volume/porta (RF-1/RF-2).
- `civmctl ci-guard` recusa (exit 1) compose com `container_name` fixo/porta estática/sem project-name/docker-heavy sem lock; passa repo conforme (RF-4).
- Lock serializa docker-heavy; lock obsoleto não deadlocka; cleanup não é starvado nem deixa disco a 90% sem evento (RF-2/RF-3).
- `capacity --json` expõe lock/porta; `hooks.jsonl` registra lock (RF-6).
- Runner `civm-e2e` registrável e detectável (RF-5).
- Runbook/templates/checklist publicam o contrato; `npm run docs:check` verde (RF-7).
- acme#1006 consome o primitivo e seu `tenant-isolation-smoke` fica verde sob concorrência (prova de Slice 5).

## 14. Validação

- **Unit (Go):** `go test ./... -race -count=1` no civm — `.env` round-trip/disjunção (RF-1), lock concorrência/stale (RF-2), cleanup lock-aware (RF-3), `ci-guard` fixtures (RF-4), `Report` serialização (RF-6).
- **Integração/VM:** dois bring-ups docker-heavy concorrentes na VM → zero colisão; lock-wait/hold medidos; disco não estoura sob lock.
- **Lint/format:** `golangci-lint run` (`.golangci.yml`), `gofmt`.
- **Docs:** `npm run docs:check` (índice), markdown links.
- **Gates cognitivos:** cada etapa crítica do SPEC aponta `disciplines/KAHNEMAN-DISCIPLINES.md` com pergunta obrigatória, evidência mínima e abort trigger.
- **Prova end-to-end:** acme `tenant-isolation-smoke` verde sob concorrência consumindo o primitivo (Slice 5).
