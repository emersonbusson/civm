---
slug: multi-project-isolation
title: Isolamento docker-heavy multi-projeto como primitivo do civm
milestone: —
issues: []
---

# SPEC — Isolamento docker-heavy multi-projeto como primitivo do civm

> Gerado de `docs/specs/multi-project-isolation/PRD.md` (PASSO 2 SSDV3).
> Fecha decisões, remove ambiguidade e traduz RF-1..RF-7 em mudanças exatas no repo `civm`.
> Disciplinas: `disciplines/KAHNEMAN-DISCIPLINES.md`. Validação: `go test`, `golangci-lint run`, `npm run docs:check`.

## Escopo fechado desta implementação

**Entra agora:**

- Primitivo de identidade por runner injetado no `.env` (`CIVM_RUNNER_SLOT`, `CIVM_PORT_BASE`, `COMPOSE_PROJECT_NAME`) — RF-1.
- Pacote `internal/dockerlock` + subcomando `civmctl lock` (flock + heartbeat + stale) — RF-2.
- Cleanup/disk-watchdog/job-started lock-aware (não-starvação) — RF-3.
- Pacote `internal/ciguard` + subcomando `civmctl ci-guard` (lint de compose/workflow do consumidor) — RF-4.
- Classe de runner dedicada via label (`civm-e2e`) formalizada + check no `doctor` — RF-5.
- Observabilidade: `capacity.Report` estendido + linhas `lock_*` em `hooks.jsonl` — RF-6.
- Contrato único publicado: runbook, templates, checklist — RF-7.

**Fica fora agora:**

- dockerd rootless / `DOCKER_HOST` por runner (isolamento real de daemon) — deferido atrás de gate de expansão de disco/RAM.
- Implementação do lado acme (#1006) — consumidor, vira slice fina separada no repo acme.
- Reescrita de billing/release/parity/drift do civm.

**Dependências assumidas prontas:**

- `internal/hook/install.go` escreve `.env` por runner via `upsertEnv` preservando chaves não-`ACTIONS_RUNNER_HOOK_*` (Confirmado — `install.go:136-156`).
- `civmctl` despacha por `switch os.Args[1]` (Confirmado — `cmd/civmctl/main.go:40-95`).
- Labels já configuráveis em `runner add --label` (Confirmado — `cmd/civmctl/runner.go:180-212`).
- Padrão `Options{...Fn}` + `Glob`/`WalkDir` injetáveis reutilizável de `internal/diskaudit` e `internal/capacity`.

## Matriz de rastreabilidade PRD → SPEC

| PRD | Implementação no SPEC |
| --- | --- |
| RF-1 (identidade/porta por runner) | ITEM-2 (constantes), ITEM-3 (`internal/portblock`), ITEM-6 (`install.go`) |
| RF-2 (lock box-wide) | ITEM-2, ITEM-4 (`internal/dockerlock`), ITEM-8 (`cmd/civmctl/lock.go`), ITEM-9 (`main.go`) |
| RF-3 (cleanup lock-aware) | ITEM-10 (`cleanup.go`), ITEM-11 (`diskwatchdog`), ITEM-12 (`hook.go` job-started) |
| RF-4 (`ci-guard`) | ITEM-5 (`internal/ciguard`), ITEM-8b (`cmd/civmctl/ciguard.go`), ITEM-9 |
| RF-5 (runner dedicado) | ITEM-13 (`doctor` check), ITEM-14 (docs) |
| RF-6 (observabilidade) | ITEM-7 (`capacity.Report`), ITEM-4 (eventos lock em `hooks.jsonl`) |
| RF-7 (contrato publicado) | ITEM-14 (runbook/templates/checklist) |

## Decisões técnicas

| # | Decisão | Justificativa |
| --- | --- | --- |
| DT-1 | `CIVM_PORT_BASE` é um **bloco de 64 portas por runner**, base sticky persistida em `/var/lib/civm/port-blocks.json` (mapa `slot→base`), alocando o próximo bloco livre para `short` novo. | Base estável entre re-runs de `hook install` e disjunção determinística sem colisão por hash. Mesmo diretório de estado já usado pelo civm (`/var/lib/civm/runner-watchdog-reruns.json`). |
| DT-2 | Janela de portas civm = **[20000, 32000)** (≈187 blocos de 64). | Acima dos defaults conhecidos dos peers (minio 9020/9021, evolution 8110/8111, nginx 81, prometheus 9100, grafana 3011, ms-* 8081-8089) e **abaixo** da faixa ephemeral do kernel Linux (32768-60999) usada por testcontainers/`findAvailablePort`. Evita colisão com ambos. |
| DT-3 | `civmctl lock --exec -- <cmd>` é a forma **primária**; `acquire`/`release` existem para scripts shell. | `--exec` libera o lock por `defer` mesmo em falha/sinal do comando interno (fail-safe). O par acquire/release é frágil (release pode não rodar) — só para quem não pode envolver o comando. |
| DT-4 | Lock = `syscall.Flock(LOCK_EX\|LOCK_NB)` em `/run/civm/docker-heavy.lock` + arquivo heartbeat `/run/civm/docker-heavy.lock.hb` (JSON `{pid, scope, acquiredAt, expiresAt, repo, runId}`) atualizado a cada `DefaultDockerHeavyHeartbeatSeconds`. | flock dá exclusão entre processos; heartbeat dá detecção de stale (PID morto OU `expiresAt < now`) e a semântica de orçamento que cleanup/watchdog consultam. Não há helper flock pré-existente (Confirmado — busca zero). |
| DT-5 | Dois orçamentos: **WAIT** (`DefaultDockerHeavyLockWaitMinutes`, falha alto se não adquirir) e **HOLD** (`DefaultDockerHeavyLockBudgetMinutes`, após o qual o heartbeat não é mais estendido → cleanup pode reclamar). | Separa "esperei demais para começar" (fail-high) de "segurei demais" (perde proteção do watchdog → fail-closed). Resolve a starvação documentada no PRD §9 sem precisar matar o comando interno. |
| DT-6 | `ci-guard` tem `--mode=report\|enforce` e waiver inline por comentário `# civm:ci-guard-allow <rule> <motivo>` (espelha o padrão `invariant-waive` já usado no runbook). | Rollout report→enforce evita bloquear peers de surpresa; waiver documentado evita falso-positivo travando merge legítimo (Kahneman #5 worst-case). |
| DT-7 | Slot do runner = basename do dir menos prefixo `actions-runner-` (ex.: `/home/x/actions-runner-cmpx` → `cmpx`); fallback = basename completo. | Identidade estável já existente (`AddOptions.Short`, `internal/runner/runner.go:19-32`); o dir é o que `install.go` itera. |
| DT-8 | Label da classe pesada = `civm-e2e` adicionada via `runner add --label civm,civm-e2e`. Sem novo conceito de "grupo". | Labels CSV já suportados (Confirmado — `runner.go:142-145`); `runs-on: [self-hosted, civm, civm-e2e]` no peer roteia só ao runner dedicado. Custo near-zero. |

## Fronteira de atomicidade e política de rollback

- **Fronteira de atomicidade desta implementação:**
  - Atômico nesta entrega: aquisição/liberação de um único lock por processo (flock é atômico no kernel); escrita de cada `.env` (`os.WriteFile` substitui o arquivo inteiro); alocação de um bloco de porta novo (escrita do mapa sob lock do próprio arquivo de estado).
  - **Fora da atomicidade:** propagação do `.env` para jobs (depende de `systemctl restart` dos runners e do boot do serviço); consistência entre o `CIVM_PORT_BASE` injetado e o uso real pelo peer (garantida por `ci-guard`, não por transação). Estados parciais aceitos nesta fase: runner reiniciado mas peer ainda não adotou o contrato → degrada para o comportamento atual (sem o primitivo), nunca colide silenciosamente porque `ci-guard` recusa.
- **Política de rollback:**
  - **Rollback de app (binário):** `civmctl self-upgrade` para a versão anterior; `civmctl lock`/`ci-guard` viram no-ops se não invocados; `Report` estendido é aditivo (consumidores antigos ignoram campos novos).
  - **Rollback de "migração":** N/A — sem schema. Reverter as 3 chaves do `.env` = re-rodar `hook install` da versão anterior (reescreve `.env` sem as chaves). Apagar `/var/lib/civm/port-blocks.json` reseta a alocação (re-alocada no próximo `hook install`).
  - **Rollback de dados:** N/A — Day-0, sem produção viva, sem dados de tenant.
  - **Proibido em VM ativa:** force-release do lock enquanto há `Runner.Worker`/`docker compose` ativo sem passar pelo abort trigger de orçamento (poderia podar sob um build legítimo). O cleanup só reclama lock com heartbeat **vencido** (DT-5).
  - **`forward-only`?** Não — todas as mudanças são reversíveis por troca de binário + reescrita de `.env`; nenhuma é destrutiva irreversível.

## Mapa Kahneman por etapa crítica

| Etapa / ITEM | Disciplina | Link | Pergunta obrigatória | Evidência mínima | Abort trigger |
| --- | --- | --- | --- | --- | --- |
| ITEM-3/ITEM-6 (porta por runner) | #5 Availability heuristic | `disciplines/KAHNEMAN-DISCIPLINES.md` §"As 12 disciplinas" #5 | A janela [20000,32000) colide com algum default de peer OU com a faixa ephemeral do kernel? | `go test` provando blocos disjuntos + grep dos defaults de porta dos peers fora da janela + `cat /proc/sys/net/ipv4/ip_local_port_range` ≥ 32768 | Qualquer bloco sobrepor outro runner, um default de peer, ou a faixa ephemeral |
| ITEM-4/ITEM-10/ITEM-11/ITEM-12 (lock vs cleanup) | #5 Availability heuristic | idem #5 | Um lock segurado por bring-up de ~40 min ainda starva o disk-watchdog até 90%? | Teste com lock fresco (cleanup adia) e lock vencido (cleanup reclama) + evento `deferred-by-docker-heavy-lock`/`reclaimed-stale-lock` em `hooks.jsonl` | Cleanup podar sob lock **fresco**, ou disco atingir 90% sob lock sem evento |
| ITEM-4 (lock primitivo) | #2 Counterfactual obrigatório | idem #2 | Qual sinal numérico reverte o lock? | Rollback trigger numérico registrado (abaixo) + teste de stale-lock (PID morto) sem deadlock | lock-wait p95 causar cancelamento por `timeout-minutes` em algum peer |
| ITEM-5 (ci-guard enforce) | #5 + #12 Priming | idem #5/#12 | Quantos falso-positivos o enforce gera nos repos peer reais? | Rodar `ci-guard --mode=report` contra acme/peer e contar findings antes de enforce | Falso-positivo bloqueante não-allowlistado em repo conforme |
| ITEM-1 (baseline) | #3 Número não adjetivo | idem #3 | "~8 slots", "~40 min", "128GB" são medidos ou herdados de texto? | `civmctl capacity --json` do box vivo + profiling colado | Avançar sem medir o baseline |

**Rollback trigger numérico (fecha o PRD §9):** reverter a slice ofensora se, sobre **5 runs consecutivos** pós-deploy de cada peer: colisão de project-name/porta > 0; OU lock-wait p95 > 10 min; OU `ci-guard --mode=enforce` produzir ≥1 falso-positivo bloqueante não-waivable; OU disco atingir `DefaultHardFailPct` (90%) com lock docker-heavy fresco ativo.

## Checklist de segurança (pré-implementação)

- [ ] **Tenant isolation:** N/A (civm não tem tenant/DB). O alvo é a integridade dos gates dos peers — nenhum dado de tenant tocado.
- [ ] **SQL injection:** N/A — sem SQL.
- [ ] **Path/exec safety:** `ci-guard` só lê arquivos sob `--repo-root` (sem exec do conteúdo); `dockerlock` opera em `/run/civm/` root-owned; `--exec` usa `exec.CommandContext` sem shell.
- [ ] **Auth:** N/A — CLI local de operador; sem endpoint.
- [ ] **Rate limiting:** N/A.
- [ ] **Input validation:** flags validadas (`--budget`/`--scope`/`--repo-root` absolutos; `--mode` enum); `ci-guard` ignora paths fora do repo-root; `dockerlock` valida path do lock absoluto.
- [ ] **PII:** `hooks.jsonl`/`capacity` não logam segredo/PII; só pid/scope/wait/hold/slot/base.
- [ ] **Secrets:** o `.env` por runner **não** carrega segredo; só identidade/porta/projeto. Nenhuma credencial de VM em regra de peer.
- [ ] **Error messages:** exit codes estáveis; mensagens sem vazar caminho de segredo.

## Migrações SQL

**N/A — civm é CLI/systemd, sem banco.** Backfill = **N/A — Day-0, sem produção viva**. Único estado novo: `/var/lib/civm/port-blocks.json` (mapa `slot→base`), criado on-demand pelo `hook install`; não é migração de schema.

## Arquivos a CRIAR

### `internal/portblock/portblock.go`

- **Propósito:** alocação determinística e sticky de blocos de porta de host por slot de runner.
- **Requisitos cobertos:** RF-1, DT-1, DT-2.
- **Structs/Types/Interfaces:**
  - `type Options struct { StatePath string; BlockStart int; BlockSize int; WindowEnd int; ReadFileFn func(string)([]byte,error); WriteFileFn func(string,[]byte,os.FileMode)error; MkdirAllFn func(string,os.FileMode)error }`
  - `type Allocation struct { Slot string `json:"slot"`; Base int `json:"base"` }`
- **Funções:**
  - `func DefaultOptions() Options` → `StatePath="/var/lib/civm/port-blocks.json"`, `BlockStart=civm.DefaultRunnerPortBlockStart`, `BlockSize=civm.DefaultRunnerPortBlockSize`, `WindowEnd=civm.DefaultRunnerPortWindowEnd`, fns = os.*.
  - `func Allocate(opts Options, slot string) (int, error)` — passos: (1) ler+unmarshal mapa existente; (2) se `slot` presente, retornar base salvo; (3) senão, achar o menor bloco livre em `[BlockStart, WindowEnd)` step `BlockSize` não usado por outro slot; (4) persistir (`MkdirAll` dir + `WriteFile` JSON indentado 0o644); (5) erro se janela esgotada.
  - `func windowExhaustedErr(n int) error` — constante de erro local (goconst).
- **Dependências internas:** `internal/civm` (constantes).
- **Dependências externas:** stdlib (`encoding/json`, `os`, `path/filepath`).
- **Padrão de referência:** `internal/diskaudit/diskaudit.go` (Options + fns injetáveis) e `install.go` (escrita idempotente).
- **Testes requeridos:** `portblock_test.go` — slots distintos → bases disjuntas; mesmo slot 2×→ base estável (sticky); janela esgotada → erro; round-trip JSON; `t.TempDir()` para `StatePath`.

### `internal/dockerlock/dockerlock.go`

- **Propósito:** mutex box-wide docker-heavy via flock + heartbeat + detecção de stale.
- **Requisitos cobertos:** RF-2, RF-3 (consultoria via `IsActive`), DT-3, DT-4, DT-5.
- **Structs/Types/Interfaces:**
  - `type Options struct { LockPath string; HeartbeatPath string; Scope string; WaitBudget time.Duration; HoldBudget time.Duration; HeartbeatEvery time.Duration; Repo string; RunID string; NowFn func() time.Time; FlockFn func(fd int, how int) error; OpenFileFn func(string,int,os.FileMode)(*os.File,error); ... }`
  - `type Heartbeat struct { PID int `json:"pid"`; Scope string `json:"scope"`; AcquiredAt time.Time `json:"acquired_at"`; ExpiresAt time.Time `json:"expires_at"`; Repo string `json:"repo,omitempty"`; RunID string `json:"run_id,omitempty"` }`
  - `type Lock struct { /* file handle + opts + ticker */ }`
- **Funções:**
  - `func DefaultOptions() Options` — paths de `civm.DefaultDockerHeavyLockPath`/`.hb`, budgets de `civm.Default*`, `FlockFn=syscall.Flock`.
  - `func Acquire(ctx context.Context, opts Options) (*Lock, error)` — loop `LOCK_EX|LOCK_NB` com backoff até `WaitBudget`; ao adquirir, escreve heartbeat e inicia goroutine de heartbeat até `HoldBudget` (depois para de estender); `ErrWaitBudgetExceeded` se não adquirir.
  - `func (l *Lock) Release() error` — para heartbeat, `LOCK_UN`, fecha fd, remove `.hb`.
  - `func IsActive(opts Options) (bool, error)` — lê heartbeat: ativo sse `ExpiresAt > now` **e** PID vivo (`syscall.Kill(pid, 0)`); usado por cleanup/watchdog/job-started (RF-3).
  - `func reclaimStale(opts Options) (bool, error)` — remove `.hb` vencido (heartbeat morto), permitindo nova aquisição.
  - Sentinelas: `var ErrWaitBudgetExceeded = errors.New("docker-heavy lock wait budget exceeded")`.
- **Dependências externas:** stdlib (`syscall`, `os`, `time`, `context`, `encoding/json`, `errors`).
- **Padrão de referência:** uso de `syscall` em `internal/capacity/capacity.go:116-122`; shell `flock /run/civmctl-cleanup.lock` no runbook.
- **Testes requeridos:** `dockerlock_test.go` — 2 aquisições concorrentes serializam (segunda espera/erra por budget); stale (PID morto / `ExpiresAt` passado via `NowFn` fake) é reclamado sem deadlock; `IsActive` true sob heartbeat fresco / false sob vencido; `Release` em `defer` mesmo com erro.
- **Disciplina Kahneman:** #2 Counterfactual + #5 Availability — ver Mapa.

### `cmd/civmctl/lock.go`

- **Propósito:** subcomando `civmctl lock acquire|release|--exec`.
- **Requisitos cobertos:** RF-2.
- **Funções:** `func runLock(args []string) int` — parse flags (`--scope` default `docker-heavy`, `--budget` HOLD, `--wait`, `--exec`, `--json`, `--repo`, `--run-id`); modo `--exec -- <cmd...>`: `Acquire` → `exec.CommandContext` (stdout/stderr herdados) → `defer Release()`; propaga exit code do comando; `exitLockTimeout` (=75) em `ErrWaitBudgetExceeded`; `exitUsage` (=64) em flags inválidas. Emite linha `lock_acquire`/`lock_release` (wait/hold) em `hooks.jsonl` (RF-6).
- **Padrão de referência:** `cmd/civmctl/capacity.go` (parse + render), `cmd/civmctl/hook.go`.
- **Testes requeridos:** `main_test.go`/`integration_test.go` — `--exec true` retorna 0; `--exec false` propaga ≠0; flags inválidas → 64.

### `internal/ciguard/ciguard.go`

- **Propósito:** lint read-only de compose/workflow do repo consumidor contra as invariantes de isolamento.
- **Requisitos cobertos:** RF-4, DT-6.
- **Structs/Types/Interfaces:**
  - `type Options struct { RepoRoot string; Mode string /* report|enforce */; GlobFn func(string)([]string,error); ReadFileFn func(string)([]byte,error); WalkFn fs.WalkDirFunc /* opcional */ }`
  - `type Finding struct { File string `json:"file"`; Line int `json:"line"`; Rule string `json:"rule"`; Message string `json:"message"`; Remediation string `json:"remediation"` }`
  - `type Result struct { Findings []Finding `json:"findings"`; Violations int `json:"violations"`; Mode string `json:"mode"` }`
- **Funções:**
  - `func DefaultOptions(repoRoot string) Options`.
  - `func Scan(opts Options) (Result, error)` — varre `infra/**/docker-compose*.y?ml` e `.github/workflows/*.y?ml`; aplica regras R1-R4; respeita waiver `# civm:ci-guard-allow <rule> <motivo>` na mesma linha ou imediatamente acima.
  - Regras (cada uma uma função pura testável):
    - `R1-container-name`: linha `container_name:` em compose → violação ("nome fixo impede co-residência; remova").
    - `R2-static-host-port`: `ports:` com `HOST:CONTAINER` onde `HOST` é inteiro literal (não `${...}` nem omitido) → violação ("use `${CIVM_PORT_BASE}+N` ou porta ephemeral").
    - `R3-missing-project-name`: step de workflow que invoca `docker compose`/`docker-compose` sem `-p`/`--project-name`/`COMPOSE_PROJECT_NAME` no escopo → violação.
    - `R4-unlocked-docker-heavy`: step docker-heavy (`docker compose ... up`/`--build` ou `make up*`) não envolvido por `civmctl lock`/`flock` → violação (warning em `report`).
- **Dependências externas:** stdlib (`io/fs`, `path/filepath`, `regexp`, `bufio`).
- **Padrão de referência:** `internal/diskaudit/diskaudit.go:33-64` (Glob/WalkDir + fns injetáveis), `internal/drift/drift.go` (regex line-scan).
- **Testes requeridos:** `ciguard_test.go` — fixtures conforme (0 findings) e violador (1 por regra); waiver suprime finding; `--mode=report` não falha, `enforce` falha; table-driven.
- **Disciplina Kahneman:** #5 + #12 — ver Mapa.

### `cmd/civmctl/ciguard.go`

- **Propósito:** subcomando `civmctl ci-guard`.
- **Funções:** `func runCIGuard(args []string) int` — flags `--repo-root` (default `.`), `--mode` (default `report`), `--json`; `Scan` → render texto/JSON; exit `1` se `Mode==enforce && Violations>0`, senão `0`; `exitUsage` em flag inválida.
- **Testes requeridos:** dispatch + exit codes.

## Arquivos a MODIFICAR

### `internal/civm/civm.go` — ITEM-2

- **O que muda:** adicionar constantes ao bloco `const (...)` (linhas 15-62).
- **Requisitos cobertos:** RF-1, RF-2, DT-1, DT-2, DT-5.
- **Depois (acrescentar):**
  ```go
  // Isolamento docker-heavy multi-projeto (docs/specs/multi-project-isolation).
  DefaultDockerHeavyLockPath          = "/run/civm/docker-heavy.lock"
  DefaultDockerHeavyLockBudgetMinutes = 50 // HOLD: além disso, heartbeat não é estendido
  DefaultDockerHeavyLockWaitMinutes   = 75 // WAIT: além disso, falha alto ao adquirir
  DefaultDockerHeavyHeartbeatSeconds  = 30
  DefaultRunnerPortBlockStart         = 20000
  DefaultRunnerPortBlockSize          = 64
  DefaultRunnerPortWindowEnd          = 32000 // < faixa ephemeral do kernel (32768+)
  DefaultPortBlockStatePath           = "/var/lib/civm/port-blocks.json"
  ```
- **Impacto:** aditivo; nenhum caller existente quebra.
- **Testes requeridos:** consumidos indiretamente por portblock/dockerlock tests.
- **Disciplina Kahneman:** #5 — janela vs defaults/ephemeral (ver Mapa).

### `internal/hook/install.go` — ITEM-6

- **O que muda:** injetar `CIVM_RUNNER_SLOT`, `CIVM_PORT_BASE`, `COMPOSE_PROJECT_NAME` no `.env` de cada runner.
- **Requisitos cobertos:** RF-1, DT-1, DT-7.
- **Função/bloco afetado:** `Install` (loop linhas 102-113) e `upsertEnv` (136-156).
- **Antes:** `upsertEnv(opts InstallOptions, envPath string) error` — strip de `ACTIONS_RUNNER_HOOK_*` + reanexa os 2 paths.
- **Depois:**
  - Assinatura: `func upsertEnv(opts InstallOptions, envPath string, extra map[string]string) error` — além de `ACTIONS_RUNNER_HOOK_*`, também faz strip das chaves presentes em `extra` (prefixo `KEY=`) antes de reanexar; reanexa os 2 hooks **e** cada par de `extra` (ordenado para determinismo).
  - No loop de `Install`: para cada `runner` válido, `slot := runnerSlot(runner)`; `base, err := portblock.Allocate(portblock.DefaultOptions(), slot)`; `extra := map[string]string{"CIVM_RUNNER_SLOT": slot, "CIVM_PORT_BASE": strconv.Itoa(base), "COMPOSE_PROJECT_NAME": slot}`; passar `extra` a `upsertEnv`.
  - Nova função `func runnerSlot(dir string) string` — `strings.TrimPrefix(filepath.Base(dir), "actions-runner-")`; se vazio, `filepath.Base(dir)`.
  - Estender `InstallResult` com `PortBlocks map[string]int `json:"port_blocks,omitempty"`` para observabilidade do install.
- **Impacto:** `upsertEnv` ganha 1 parâmetro — atualizar o único caller (linha 109) e os testes de `install_test.go`. Idempotente (re-run reescreve os mesmos valores; base sticky).
- **Testes requeridos:** `install_test.go` — `.env` ganha as 3 chaves preservando `ACTIONS_RUNNER_HOOK_*` e demais; 2 runners → `CIVM_PORT_BASE` disjuntos; re-run idempotente; `runnerSlot` casos (`actions-runner-cmpx`→`cmpx`, `actions-runner`→`actions-runner`).
- **Disciplina Kahneman:** #5 — ver Mapa.

### `internal/capacity/capacity.go` — ITEM-7

- **O que muda:** estender `Report` (linhas 17-26) com sinais de isolamento.
- **Requisitos cobertos:** RF-6.
- **Antes:** `Report{ DiskPath, DiskUsedPct, DiskFreeGB, DiskTotalGB, RunnerServices, RunnerWorkers, AcceptingJobs, Reason }`.
- **Depois (acrescentar campos, aditivo, `omitempty`):**
  ```go
  DockerHeavyLockActive bool   `json:"docker_heavy_lock_active"`
  DockerHeavyLockHolder string `json:"docker_heavy_lock_holder,omitempty"` // "<repo>#<runId>"
  RunnerPortBlocks      map[string]int `json:"runner_port_blocks,omitempty"` // slot->base
  ```
  Em `Check`: setar `DockerHeavyLockActive` via `dockerlock.IsActive(dockerlock.DefaultOptions())`; popular `RunnerPortBlocks` lendo `/var/lib/civm/port-blocks.json` (best-effort, erro→omitido).
- **Impacto:** aditivo; `RenderText` ganha 1 linha; consumidores JSON antigos ignoram campos novos. `capacity` não pode criar import cycle com `dockerlock` (ambos importam só `civm`/stdlib — OK).
- **Testes requeridos:** serialização do `Report` estendido; `Check` com lock mockado ativo/inativo.

### `internal/cleanup/cleanup.go` — ITEM-10

- **O que muda:** tornar o prune destrutivo lock-aware.
- **Requisitos cobertos:** RF-3, DT-5.
- **Função/bloco afetado:** `Run()` (sequência linhas 96-118), antes de `dockerPrune` (337-345) / dentro de `ensureIdle()`.
- **Depois:** antes de qualquer mutação destrutiva (`dockerPrune`, `rm -rf`), checar `dockerlock.IsActive(...)`: se **ativo (heartbeat fresco)**, **adiar** o prune e logar evento `deferred-by-docker-heavy-lock` (sem mutar); se **vencido**, `reclaimStale` + prosseguir. Adicionar campo de resultado/evento indicando o defer.
- **Impacto:** cleanup já gated por `ensureIdle`; a checagem de lock é segunda condição. Sem mudança de assinatura pública se `IsActive` for chamado internamente. `flock /run/civmctl-cleanup.lock` (shell) e o `dockerlock` são paths distintos.
- **Testes requeridos:** `cleanup_test.go` — lock fresco → prune adiado + evento; lock vencido → prune ocorre; sem lock → comportamento atual.
- **Disciplina Kahneman:** #5 — ver Mapa (abort: podar sob lock fresco).

### `internal/diskwatchdog/*.go` — ITEM-11

- **O que muda:** mesma consciência de lock antes do cleanup agressivo (threshold 60%).
- **Requisitos cobertos:** RF-3.
- **Depois:** o watchdog consulta `dockerlock.IsActive`; sob lock fresco, **não** dispara prune agressivo (adia + evento); sob lock vencido OU disco ≥ `DefaultHardFailPct`, prossegue (fail-closed). Reusa a lógica de ITEM-10.
- **Impacto:** evita que o watchdog horário derrube um bring-up legítimo segurando o lock.
- **Testes requeridos:** watchdog com lock fresco (adia) vs disco ≥90% (prossegue mesmo com lock).

### `internal/hook/hook.go` (job-started) — ITEM-12

- **O que muda:** o gating de disco do `job-started` (pré-cleanup 60% → limpa; 90% → rejeita) passa a respeitar lock fresco no passo de limpeza.
- **Requisitos cobertos:** RF-3.
- **Depois:** ao decidir limpar paths de cache/workspace em pressão de disco, pular prune Docker destrutivo se `dockerlock.IsActive` (lock fresco); manter a rejeição hard-fail a 90% (fail-closed) independentemente.
- **Impacto:** o `job-started` de um job que vai adquirir o lock não deve podar o estado de um job docker-heavy concorrente já segurando o lock.
- **Testes requeridos:** `hook_test.go` — job-started com lock fresco não poda docker; 90% ainda rejeita.

### `cmd/civmctl/main.go` — ITEM-9

- **O que muda:** registrar os 2 subcomandos novos no `switch` (linhas 40-95) + entradas no `printHelp`.
- **Requisitos cobertos:** RF-2, RF-4.
- **Depois (acrescentar cases):**
  ```go
  case "lock":
      os.Exit(runLock(args))
  case "ci-guard":
      os.Exit(runCIGuard(args))
  ```
  + 2 linhas em `COMANDOS` e exemplos em `printHelp`.
- **Impacto:** aditivo; segue o padrão `switch` existente (não-cobra).
- **Testes requeridos:** `main_test.go` — dispatch de `lock`/`ci-guard`; `comando desconhecido` inalterado.

### `cmd/civmctl/doctor.go` (+ `internal/doctor`) — ITEM-13

- **O que muda:** `doctor` reporta presença/ausência do runner com label `civm-e2e` quando o peer declara que o usa (flag `--expect-e2e` ou inferência por labels via `gh api`).
- **Requisitos cobertos:** RF-5.
- **Depois:** novo check `RUNNER_E2E_LABEL` em severidade `ok`/`warn` (ausente quando esperado). Reusa o padrão `hook_checks` do `doctor`.
- **Impacto:** aditivo no JSON do `doctor`.
- **Testes requeridos:** `doctor` com/sem runner `civm-e2e`.

### `cmd/civmctl/runner.go` — ITEM-13b

- **O que muda:** documentar (help/exemplo) `runner add --label civm,civm-e2e`. **Sem mudança de código** — `--label` CSV já suportado (Confirmado — `runner.go:180-212`).
- **Requisitos cobertos:** RF-5.
- **Impacto:** só ajuda/exemplo.

## Arquivos a DELETAR (se houver)

| Arquivo | Motivo |
| --- | --- |
| — | Nenhum. Mudança aditiva; o flock shell `/run/civmctl-cleanup.lock` permanece (escopo distinto do `docker-heavy.lock`). |

## Observabilidade

**Eventos estruturados** (`/var/log/civm/hooks.jsonl` + render dos subcomandos):

| Evento | Campos |
| --- | --- |
| `lock_acquire` | `scope`, `repo`, `run_id`, `wait_ms`, `pid` |
| `lock_release` | `scope`, `repo`, `run_id`, `hold_ms`, `over_budget` (bool) |
| `lock_wait_budget_exceeded` | `scope`, `repo`, `run_id`, `waited_ms` |
| `deferred-by-docker-heavy-lock` | origem (`cleanup`/`disk-watchdog`/`job-started`), `holder` |
| `reclaimed-stale-lock` | `scope`, `holder_pid`, `expired_at` |

**Campos em `capacity --json` (ITEM-7):** `docker_heavy_lock_active`, `docker_heavy_lock_holder`, `runner_port_blocks`.

**Sem PII, sem segredo, sem label de alta cardinalidade** (sem repo/slug como label de métrica; só nos eventos JSON de auditoria).

## Contratos e documentação viva

| Documento | Atualização | Motivo |
| --- | --- | --- |
| `runbooks/MULTI-PROJECT-RUNNER.md` | Alterar | §"Riscos compartilhados" (imperativos) → §"Isolamento fornecido pelo civm": as 3 chaves de `.env`, path do lock, exit codes, contrato do `ci-guard`, label `civm-e2e` |
| `templates/CIVM-USAGE.md` | Alterar | consumo de `CIVM_PORT_BASE`/`COMPOSE_PROJECT_NAME` + `civmctl lock` + `civmctl ci-guard` |
| `templates/acme-ci-router.yml.template`, `templates/ci-router.yml.template` | Alterar | step `ci-guard` no pré-flight + wrap docker-heavy em `civmctl lock --exec` |
| `runbooks/PEER-ADOPTION-CHECKLIST.md`, `runbooks/ORG-RUNNER-ADOPTION.md` | Alterar | passos de adoção do primitivo |
| `cmd/civmctl/main.go` `printHelp` | Alterar | comandos `lock`/`ci-guard` |
| `disciplines/KAHNEMAN-DISCIPLINES.md` | N/A | sem nova disciplina/âncora |
| `docs/INDEX.md` | Regenerar | `npm run docs:index` (novo spec) |
| `AGENTS.md` / `CODEX.md` | Alterar | se mudar boundary "civm fornece isolamento" (regra de trabalho) |
| `docs/config-reference.json` | N/A | civm não usa esse arquivo (é do acme) |
| `docs/openapi/*`, SDK, eventos Redis | N/A | sem contrato de produto |

**Lado consumidor (repo acme, fora deste PR):** `docs/specs/M48/1006-...`, `.claude/rules/civm.md`, `docs/CIVM.md`, `infra/docker-compose*.yml`, `tools/devctl`.

## Ordem de implementação

1. **ITEM-1 — Baseline (Slice 0):** `civmctl capacity --json` do box + profiling (sem código). Colar no IMPL.
2. **ITEM-2 — Constantes** (`internal/civm/civm.go`).
3. **ITEM-3 — `internal/portblock`** + testes.
4. **ITEM-4 — `internal/dockerlock`** + testes.
5. **ITEM-6 — `.env` injection** (`internal/hook/install.go`) + testes.
6. **ITEM-8 — `cmd/civmctl/lock.go`** + **ITEM-8b — `cmd/civmctl/ciguard.go`**.
7. **ITEM-5 — `internal/ciguard`** + testes.
8. **ITEM-9 — dispatch** (`cmd/civmctl/main.go`) + help.
9. **ITEM-7 — `capacity.Report`** estendido + testes.
10. **ITEM-10/11/12 — lock-aware** cleanup/watchdog/job-started + testes.
11. **ITEM-13/13b — `doctor` check + runner label** docs.
12. **ITEM-14 — docs vivas** (runbook/templates/checklist) + `npm run docs:index`.
13. **Prova:** rodar `ci-guard --mode=report` contra acme/peer; adotar no acme#1006 (slice separada).

## Plano de testes

**Go (civm) — unitários:**

- `portblock`: disjunção, sticky, janela esgotada, round-trip JSON.
- `dockerlock`: concorrência serializa, stale (PID morto / `ExpiresAt` via `NowFn`), `IsActive` fresco/vencido, `Release` em `defer`.
- `hook/install`: 3 chaves no `.env` preservando o resto, bases disjuntas entre runners, idempotência, `runnerSlot`.
- `ciguard`: 1 violação por regra (R1-R4), waiver suprime, report vs enforce.
- `capacity`: `Report` estendido serializa; `Check` com lock mockado.
- `cmd/civmctl`: dispatch de `lock`/`ci-guard`, exit codes (0 / inner-code / 75 / 64).

**Go — integração (VM, `-race`):**

- Dois processos `civmctl lock --exec` concorrentes serializam; segundo respeita WAIT budget.
- Lock fresco → `cleanup`/`disk-watchdog` adiam prune (evento); lock vencido → reclama.
- `hook install --execute` em runners fake (`t.TempDir` + glob) injeta `.env`.

**Atomicidade/concorrência:**

- flock sob 2 goroutines/processos; stale-reclaim sem deadlock; heartbeat para de estender após HOLD budget.

**Manuais (evidência das etapas críticas do Mapa Kahneman):**

- `civmctl capacity --json` do box vivo (baseline ITEM-1) colado no IMPL.
- `cat /proc/sys/net/ipv4/ip_local_port_range` ≥ 32768 (DT-2) + grep dos defaults de porta dos peers fora de [20000,32000).
- `civmctl ci-guard --mode=report` contra acme/peer: contagem de findings antes de habilitar enforce.

## Checklist de validação

**Go (civm)**

- [ ] `gofmt -w ./...`
- [ ] `golangci-lint run -c .golangci.yml ./...`
- [ ] `go test ./... -race -count=1`
- [ ] `go build -o /tmp/civmctl ./cmd/civmctl` (compila com os 2 subcomandos novos)

**Docs**

- [ ] `npm run docs:index` (regenera `docs/INDEX.md`)
- [ ] `npm run docs:check` (sincronia em CI)

**Gates cognitivos**

- [ ] Cada etapa crítica aponta `disciplines/KAHNEMAN-DISCIPLINES.md` (Mapa preenchido)
- [ ] Pergunta obrigatória, evidência mínima e abort trigger registrados por etapa crítica
- [ ] Rollback trigger numérico definido (colisão>0 / lock-wait p95>10min / falso-positivo enforce / disco 90% sob lock fresco) sobre 5 runs
- [ ] Sem linguagem vaga em pontos críticos sem critério observável
