---
slug: multi-project-isolation
title: Isolamento docker-heavy multi-projeto como primitivo do civm
milestone: —
issues: []
---

# SPECv2 — Isolamento docker-heavy multi-projeto como primitivo do civm

> Versão melhorada após auditoria do Passo 2.5.
> Baseline preservado: `SPEC.md`.
> Motivo: a auditoria (4 perspectivas, verificada contra o código) deu `no-go` por blockers de **precisão**, não de arquitetura: semântica de heartbeat/HOLD que podia matar job vivo, TOCTOU na alocação de porta e no `IsActive` (PID reuse), matriz de exit codes incompleta, troca de assinatura de `upsertEnv` sem rollout escalonado, severidade/waiver do `ci-guard` indefinidos, "docker-heavy" sem definição, rollback trigger e janela de portas sem métrica/evidência executável, `ITEM-7` fora do Mapa Kahneman, e `#12 Priming` citado por engano. Este v2 **fecha cada blocker com decisão explícita**. A estrutura, os ITENS e a ordem do `SPEC.md` permanecem; onde houver conflito, **o v2 prevalece**.

## Como ler este v2

`SPEC.md` (baseline) define os ITENS e a estrutura. Este `SPECv2.md` é a **camada vinculante de resolução**: cada `DT-v2-N` fecha um blocker do Passo 2.5 e, quando aplicável, substitui o trecho correspondente do baseline. PASSO 3 implementa `SPEC.md` **com os overrides deste v2**.

---

## Resolução dos blockers do Passo 2.5 (decisões fechadas)

| # | Blocker (severidade) | Decisão vinculante (override do baseline) |
| --- | --- | --- |
| DT-v2-1 | **CRÍTICO — heartbeat/HOLD podia matar job vivo** (ITEM-4/DT-5) | O heartbeat é estendido **enquanto o processo holder estiver vivo** (toda a vida do comando em `--exec`), não para no HOLD budget. `HoldBudget` vira **apenas sinal de alarme** (`over_budget=true` no `lock_release`), **nunca** gatilho de reclaim. Staleness (reclaimável) = **PID morto** OU **heartbeat não atualizado há > 3× `DefaultDockerHeavyHeartbeatSeconds`** (a goroutine de heartbeat morreu/travou). Logo um `--exec` longo porém vivo mantém heartbeat fresco → cleanup **nunca** reclama de processo vivo. |
| DT-v2-2 | **CRÍTICO — TOCTOU na `portblock.Allocate`** (ITEM-3/DT-1) | `Allocate` faz `flock(LOCK_EX)` no próprio `StatePath` durante **todo** o ciclo read→find→write; persiste via **temp + `os.Rename` atômico**; libera o flock no fim. Exaustão da janela → `ErrPortWindowExhausted` (falha o `hook install`; operador remove runner morto). Sem auto-eviction no v1 (documentado). |
| DT-v2-3 | **CRÍTICO — TOCTOU + reuse de PID no `IsActive`** (ITEM-4/DT-4) | `Heartbeat` ganha `pid` **e** `pidStartTicks` (campo 22 de `/proc/<pid>/stat`). `IsActive` = unmarshal ok **E** `syscall.Kill(pid,0)==nil` **E** `pidStartTicks` confere. Unmarshal error / `ESRCH` / mismatch → **stale**. `reclaimStale` é idempotente e remove só `.hb` comprovadamente stale; heartbeat corrompido (unmarshal falha) → stale + log `reclaimed-corrupt-heartbeat`. |
| DT-v2-4 | **CRÍTICO — backoff de aquisição indefinido** (ITEM-4) | Aquisição = loop `flock(LOCK_EX\|LOCK_NB)` com **backoff linear 100 ms + jitter ±10 ms**, teto em `WaitBudget`. Sem exponencial (p99 < 1.2× p50). |
| DT-v2-5 | **CRÍTICO — `ITEM-7` ausente do Mapa Kahneman** | Adicionada linha no Mapa (abaixo). |
| DT-v2-6 | **CRÍTICO — rollback trigger vago** (linha 86) | Definido com métrica + janela observáveis (abaixo, §Rollback trigger v2). |
| DT-v2-7 | **HIGH — matriz de exit codes incompleta** (ITEM-8) | Enum completo (abaixo, §Exit codes). `75` (WAIT timeout) é intencional e não colide com `64` (usage). |
| DT-v2-8 | **HIGH — `upsertEnv` muda assinatura sem rollout** (ITEM-6) | Assinatura nova `upsertEnv(opts InstallOptions, envPath string, extra map[string]string)`; `extra` nil-safe; **rejeita com erro** se `extra` contiver qualquer chave `ACTIONS_RUNNER_HOOK_*`; ordem determinística (chaves de `extra` ordenadas). Rollout escalonado: `self-upgrade` do binário **primeiro**, depois `civmctl hook install --execute` (1 comando, sem timer no meio). |
| DT-v2-9 | **HIGH — severidade/waiver do `ci-guard`** (ITEM-5/DT-6) | R1/R2/R3 = **ERROR** (falham em `enforce`); R4 = **WARN** (só `report`, nunca `enforce` no v1). Waiver `# civm:ci-guard-allow <rule> <motivo>` suprime **aquela regra na próxima linha não-vazia/não-comentário**; v1 é line-based; `ci-guard` emite WARN `orphan-waiver` se o waiver não casar nenhum finding. |
| DT-v2-10 | **HIGH — "docker-heavy" sem definição** | Definido (abaixo, §Definição docker-heavy) — entra em `RF-2` e `templates/CIVM-USAGE.md`. |
| DT-v2-11 | **HIGH — janela de portas sem evidência** (DT-2) | Slice 0 vira **bloqueante**: cola `cat /proc/sys/net/ipv4/ip_local_port_range` (lower ≥ 32768) + grep dos host-ports dos peers; abort se algum cair em `[20000,32000)`. `portblock_test.go` assenta disjunção. |
| DT-v2-12 | **HIGH — `runnerSlot` ambíguo** (DT-7) | `func runnerSlot(dir string) string { b := filepath.Base(dir); if s := strings.TrimPrefix(b, "actions-runner-"); s != b && s != "" { return s }; return b }`. Sem realpath. Testes: `actions-runner-cmpx`→`cmpx`; `actions-runner`→`actions-runner`; `my-runner`→`my-runner`. |
| DT-v2-13 | **HIGH — risco de import cycle** (ITEM-7) | `dockerlock`, `portblock`, `ciguard` importam **só** `civm`+stdlib. `capacity` **pode** importar `dockerlock` (para `IsActive`); `dockerlock` **NÃO** importa `capacity`. Validação adiciona check de grafo de imports. |
| DT-v2-14 | **HIGH — `#12 Priming` citado por engano no `ci-guard`** | ITEM-5 usa **apenas `#5 Availability heuristic`**. `#12` removido. |
| DT-v2-15 | **HIGH — schema JSON de observabilidade impreciso** | Tabela exata de eventos (abaixo, §Observabilidade v2). |
| DT-v2-16 | **MEDIUM — backpressure do cleanup** (ITEM-10) | `IsActive` fresco → cleanup loga `deferred-by-docker-heavy-lock`, **retorna cedo sem erro** (no-op), exit 0; o cron/próximo hook reexecuta. Stale → `reclaimStale` + prossegue. Cleanup **nunca** hard-fail por defer. |
| DT-v2-17 | **MEDIUM — `--scope` não enumerado** | `--scope` ∈ {`docker-heavy`}; outro valor → `exitUsage`(64). Um único lock global `/run/civm/docker-heavy.lock`; `scope` é só rótulo de observabilidade. |
| DT-v2-18 | **MEDIUM — permissões do `.hb` / criação de `/run/civm`** | `bootstrap` cria `/run/civm` modo `0755`; `.hb` escrito `0640` (owner-rw, group-r; sem world). PID em claro é aceitável (não é segredo). |
| DT-v2-19 | **MEDIUM — exaustão da janela de portas** | `Allocate` retorna `ErrPortWindowExhausted` quando os 187 blocos estão ocupados; **falha o install** com mensagem orientando remover runner morto. Eviction automática fica para v2 (documentado). |
| DT-v2-20 | **MEDIUM — clock skew** | Staleness usa primariamente **liveness de PID + freshness do heartbeat (mtime/última escrita)**; `ExpiresAt` é secundário. Salto de relógio para trás não reclama lock de processo vivo (PID-alive prevalece). |
| DT-v2-21 | **HIGH — help de `lock`/`ci-guard` não especificado** (ITEM-9) | Strings exatas de help (abaixo, §Help). |
| DT-v2-22 | **MEDIUM — `HOLD=50` vs PRD `~40`** (ITEM-2) | `DefaultDockerHeavyLockBudgetMinutes=50` ancorado no baseline de profiling do Slice 0 (~40 min + 10 min de folga). Se o Slice 0 medir ≠, ajustar e registrar. Como HOLD agora é só alarme (DT-v2-1), o valor não causa kill — só `over_budget=true`. |

---

## Exit codes (`civmctl lock`) — fecha DT-v2-7

Adicionar a `cmd/civmctl/main.go` (junto de `const exitUsage = 64`):

```go
const (
    exitLockWaitTimeout = 75 // ErrWaitBudgetExceeded (não adquiriu dentro do WAIT budget)
    exitLockInternal    = 77 // erro de flock/heartbeat/IO no lock
)
```

| Código | Significado |
| --- | --- |
| `0` | `--exec`: comando interno terminou com sucesso (exit 0) |
| _exit do comando_ | `--exec`: propaga o exit code do comando interno em falha |
| `64` (`exitUsage`) | flags inválidas / `--scope` desconhecido |
| `75` (`exitLockWaitTimeout`) | não adquiriu o lock dentro de `WaitBudget` |
| `77` (`exitLockInternal`) | falha de flock/heartbeat/IO |

Sem código para "HOLD expirado" — por DT-v2-1 não há force-kill; HOLD só marca `over_budget`.

## Definição de "docker-heavy" — fecha DT-v2-10 (entra em RF-2 + CIVM-USAGE.md)

- **É docker-heavy (envolver em `civmctl lock --exec`):** `docker compose up/down/run`, `docker build`, `docker buildx`, `docker pull` — qualquer operação que aloca recursos do daemon (imagem/container/rede/volume) e pode colidir com job concorrente.
- **NÃO é:** `docker ps`, `docker logs`, `docker inspect`, `docker version` (read-only).

## Rollback trigger v2 — fecha DT-v2-6 / DT-v2-1 contexto

Avaliado sobre o **primeiro conjunto de 5 runs consecutivos de CADA peer (acme, peer) nas primeiras 48h** após o deploy do binário. Reverter a slice ofensora (`self-upgrade` versão anterior + `rm /var/lib/civm/port-blocks.json`) se **qualquer**:

- **Colisão de container:** `docker ps --format '{{.Names}}' | sort | uniq -d` retorna ≥1 cross-runner; OU
- **Colisão de porta:** bind falha com `EADDRINUSE` em `[20000,32000)` no journal; OU
- **Colisão de projeto:** mesmo `COMPOSE_PROJECT_NAME` em > 1 runner ativo; OU
- **Lock-wait p95 > 600000 ms** (10 min) sobre os 5 runs (p95 dos `wait_ms` em `hooks.jsonl`, por peer); OU
- **`ci-guard --mode=enforce`** gerar ≥1 falso-positivo bloqueante não-waivável em repo conforme; OU
- **Disco atingir `DefaultHardFailPct` (90%)** com lock docker-heavy **fresco** ativo.

Abort imediato (não espera 5 runs): qualquer `lock-wait` único exceder `WaitBudget` (75 min) → investigar antes de continuar o rollout.

## Observabilidade v2 (JSON exato em `hooks.jsonl`) — fecha DT-v2-15

| Evento | Nível | Campos JSON |
| --- | --- | --- |
| `lock_acquire` | info | `{timestamp, event:"lock_acquire", scope, repo, run_id, wait_ms, pid}` |
| `lock_release` | info | `{timestamp, event:"lock_release", scope, repo, run_id, hold_ms, over_budget(bool), pid}` |
| `lock_wait_budget_exceeded` | error | `{timestamp, event:"lock_wait_budget_exceeded", scope, repo, run_id, waited_ms}` |
| `deferred-by-docker-heavy-lock` | warning | `{timestamp, event:"deferred-by-docker-heavy-lock", source:"cleanup\|disk-watchdog\|job-started", holder_pid, holder_repo}` |
| `reclaimed-stale-lock` | warning | `{timestamp, event:"reclaimed-stale-lock", holder_pid, reason:"pid-dead\|heartbeat-stale\|corrupt"}` |

Escrita de evento é **best-effort**: falha de IO em `hooks.jsonl` **não** falha o cleanup/lock (log para stderr e segue). Sem PII, sem segredo, sem label de alta cardinalidade.

## Help (`cmd/civmctl/main.go printHelp`) — fecha DT-v2-21

Adicionar em `COMANDOS`:

```
  lock            Serializa trabalho docker-heavy (acquire/release/--exec com heartbeat + budget)
  ci-guard        Lint de compose/workflow do peer contra invariantes de isolamento
```

Adicionar em `EXEMPLOS`:

```
  civmctl lock --exec --scope docker-heavy --budget 50m --wait 75m -- make up-local
  civmctl ci-guard --repo-root . --mode report --json
```

---

## Especificações que substituem o baseline (código-nível)

### ITEM-4 `internal/dockerlock/dockerlock.go` (override) — DT-v2-1/3/4

- `type Heartbeat struct { PID int `json:"pid"`; PIDStartTicks uint64 `json:"pid_start_ticks"`; Scope string `json:"scope"`; AcquiredAt time.Time `json:"acquired_at"`; ExpiresAt time.Time `json:"expires_at"`; Repo string `json:"repo,omitempty"`; RunID string `json:"run_id,omitempty"` }`
- `func Acquire(ctx, opts) (*Lock, error)`: backoff linear 100 ms + jitter ±10 ms até `WaitBudget` (`ErrWaitBudgetExceeded` se estourar). Ao adquirir: escreve `.hb` (`0640`) com `pidStartTicks` lido de `/proc/self/stat`; inicia goroutine que **reescreve o `.hb` a cada `HeartbeatEvery` enquanto o processo vive** (parada só no `Release()` ou no fim do processo); ao cruzar `HoldBudget`, **continua** o heartbeat porém marca `over_budget`.
- `func (l *Lock) Release() error`: para a goroutine (channel/cancel), `LOCK_UN`, fecha fd, remove `.hb`. Idempotente.
- `func IsActive(opts) (bool, error)`: lê `.hb`; ativo sse unmarshal ok **e** `Kill(pid,0)==nil` **e** `pidStartTicks` confere. Caso contrário stale.
- `func reclaimStale(opts) (bool, error)`: remove `.hb` stale (idempotente, re-entrante).
- **Signal handling:** `runLock --exec` instala handler de `SIGTERM/SIGINT` que chama `Release()` antes de sair; se for `SIGKILL` (sem defer), a recuperação é via stale-detection (janela ≤ 3× `HeartbeatEvery`, documentada como aceitável).

### ITEM-3 `internal/portblock/portblock.go` (override) — DT-v2-2/19

- `Allocate(opts, slot)`: abre `StatePath` com `O_CREATE|O_RDWR`, `flock(LOCK_EX)`; lê+unmarshal; se `slot` presente, retorna base; senão acha o menor bloco livre em `[BlockStart, WindowEnd)` step `BlockSize`; se nenhum → `ErrPortWindowExhausted`; escreve **temp no mesmo dir + `os.Rename`**; `flock(LOCK_UN)`. Erros sentinela: `var ErrPortWindowExhausted = errors.New("civm port window exhausted")`.

### ITEM-6 `internal/hook/install.go` (override) — DT-v2-8/12

- `upsertEnv(opts, envPath, extra map[string]string)`: se `extra` contiver chave com prefixo `ACTIONS_RUNNER_HOOK_` → retorna erro `extra must not contain ACTIONS_RUNNER_HOOK_* keys`. Strip de `ACTIONS_RUNNER_HOOK_*` **e** das chaves de `extra`; reanexa os 2 hooks; depois as chaves de `extra` **em ordem alfabética** (determinismo). Caller (linha 109): `extra := map[string]string{"CIVM_RUNNER_SLOT": slot, "CIVM_PORT_BASE": strconv.Itoa(base), "COMPOSE_PROJECT_NAME": slot}; if err := upsertEnv(opts, envPath, extra); err != nil {...}`.
- `runnerSlot` conforme DT-v2-12.

### ITEM-5 `internal/ciguard/ciguard.go` (override) — DT-v2-9/14

- Regras: R1 `container_name` (ERROR), R2 host-port estática (ERROR), R3 compose sem project-name (ERROR), R4 docker-heavy sem lock (**WARN, só report**). `Finding` ganha `severity` (`error|warn`). `Scan` retorna `Violations` = count de `error` não-waivados. `Mode==enforce` → exit 1 se `Violations>0`; R4 nunca conta como violation em enforce.
- Waiver: comentário `# civm:ci-guard-allow <rule> <motivo>` suprime `<rule>` na próxima linha significativa; `orphan-waiver` (WARN) se não casar.
- Disciplina: **#5 apenas**.

### ITEM-7 `internal/capacity/capacity.go` (override) — DT-v2-13

- `Report` ganha `DockerHeavyLockActive bool` + `DockerHeavyLockHolder string` (omitempty); `Check` chama `dockerlock.IsActive(dockerlock.DefaultOptions())` (erro → `false`, sem falhar). `runner_port_blocks` lido best-effort de `/var/lib/civm/port-blocks.json`. **Invariante de import:** `capacity → dockerlock` permitido; `dockerlock → capacity` proibido.

---

## Mapa Kahneman v2 (adições/overrides)

| Etapa / ITEM | Disciplina | Link | Pergunta obrigatória | Evidência mínima | Abort trigger |
| --- | --- | --- | --- | --- | --- |
| **ITEM-7 (capacity.Report)** _(adicionado — DT-v2-5)_ | #3 Número não adjetivo | `disciplines/KAHNEMAN-DISCIPLINES.md` #3 | Quais campos de lock/porta `capacity --json` instrumenta e batem com `IsActive`? | `go test` serializando `Report` com lock mockado ativo/vencido + `civmctl capacity --json` no box mostrando `docker_heavy_lock_active` coerente | `capacity` não expor o campo OU divergir do `dockerlock.IsActive` |
| **ITEM-3/ITEM-6 (porta)** _(override — DT-v2-11)_ | #5 Availability heuristic | idem #5 | A janela `[20000,32000)` colide com default de peer ou faixa ephemeral? | Slice 0 cola `ip_local_port_range` (lower ≥ 32768) + grep de host-ports dos peers fora da janela + `portblock_test.go` disjunção | Qualquer porta de peer ou ephemeral em `[20000,32000)` |
| **ITEM-4 (lock)** _(override — DT-v2-1/6)_ | #2 Counterfactual | idem #2 | Lock pode matar job vivo OU reclamar de PID reusado? | Teste: heartbeat fresco de PID vivo nunca é reclamado; PID morto/`pidStartTicks` divergente → stale; concorrência inter-processo respeita `WaitBudget` | cleanup reclamar lock de PID vivo; `IsActive` true para PID reusado |

ITEM-5 (ci-guard) no Mapa: trocar disciplina para **#5 apenas** (DT-v2-14).

---

## Ordem de implementação v2 (override)

Inalterada do baseline (1→13), com **ITEM-1 (Slice 0) agora bloqueante** para ITEM-3 (evidência de janela de portas, DT-v2-11). Sequência crítica: ITEM-2 → ITEM-3 (`portblock`, com flock) → ITEM-4 (`dockerlock`) → ITEM-6 (`install.go`, usa `portblock`) → demais.

## Plano de testes v2 (adições)

- **Concorrência OS-level (não só goroutine):** 2 processos `civmctl lock --exec` via `exec.Command` disputando `/run/civm/docker-heavy.lock`; o 2º respeita `WaitBudget` (±5%), sem tight-loop nem deadlock; `-race`.
- **Stale/PID-reuse:** heartbeat com PID vivo nunca reclamado; PID morto e `pidStartTicks` divergente → stale; `.hb` corrompido → stale + log.
- **portblock:** TOCTOU sob 2 `Allocate` concorrentes (flock serializa); exaustão → `ErrPortWindowExhausted`; sticky/idempotente; temp+rename atômico.
- **upsertEnv:** rejeita `extra` com `ACTIONS_RUNNER_HOOK_*`; ordem determinística; preserva chaves; bases disjuntas entre runners.
- **ci-guard:** R1/R2/R3 ERROR, R4 WARN; waiver por linha; `orphan-waiver`; `report` não falha, `enforce` falha só em ERROR.
- **Import graph:** `go list -deps ./internal/dockerlock | grep -q internal/capacity` deve ser **vazio**.

## Checklist de validação v2 (adições)

- [ ] `go test ./... -race -count=1` (inclui concorrência OS-level do lock)
- [ ] `go vet` + `golangci-lint run -c .golangci.yml ./...`
- [ ] Import-cycle check: `dockerlock`/`portblock`/`ciguard` não importam `capacity`
- [ ] Slice 0 evidência (port range + grep peers) colada no `IMPL.md`
- [ ] Exit codes documentados em comentário + teste (`64/75/77`)
- [ ] Mapa Kahneman inclui ITEM-7; ITEM-5 usa #5 apenas
- [ ] `npm run docs:index` + `npm run docs:check`

## Veredito

`go` **condicional**: pronto para PASSO 3 desde que o **Slice 0 (evidência de janela de portas)** seja colado e a sequência crítica de implementação (ITEM-2→3→4→6) seja respeitada. Todos os `*DECISION*` do Passo 2.5 estão fechados acima.
