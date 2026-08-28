---
slug: runner-memory-admission
title: Admissão de jobs por memória (civmctl admit) — 2 heavy no máximo, light flui
milestone: —
issues: []
---

# SPECv2 — Admissão de jobs por memória (`civmctl admit`)

> Versão melhorada após o Passo 2.5 (red-team). Baseline preservado: `SPEC.md`.
> A auditoria deu **no-go** e a camada vinculante abaixo (DT-v2-N) prevalece
> sobre o `SPEC.md` onde houver conflito.
>
> Por que o `no-go` (resumo, pra não reincidir):
> 1. **CRÍTICO — reserva estática ≠ RSS real.** `HeavyReserveMB=3500` é só
>    contabilidade; nada limita o RSS do processo. Dois "heavy" que reservam
>    3500 mas usam 6 GB cada estouram a VM com o ledger mostrando "cabe". É o
>    "scratch nunca medido" do reclaim, com RAM no lugar de disco.
> 2. **CRÍTICO — `MemAvailable` é point-in-time.** Lido 1× na admissão (quando o
>    job ainda é pequeno; builds picam tarde) e nunca re-amostrado. `MemAvailable`
>    inclui page cache → leitura otimista → over-admite. Gateia o START, não o
>    steady-state.
> 3. **CRÍTICO — fail-open satura.** Esperar 30 min = a VM está saturada há
>    30 min; admitir mesmo assim joga um 3º heavy numa VM já `critical` → causa o
>    OOM que o gate deveria evitar. (`dockerlock` NÃO faz fail-open — retorna
>    `ErrWaitBudgetExceeded`.)
> 4. **HIGH — regressão de liveness.** O `dockerlock` já usa `3×` de staleness
>    (dockerlock.go:60) e `PIDStartTicks` (dockerlock.go:67, 407-415) contra
>    false-evict sob carga e PID-reuse. O `SPEC.md` voltou a `2×` e PID puro.
> 5. **HIGH — deadlock com o docker-heavy lock.** Job docker já é guardado por
>    `civmctl lock --scope docker-heavy` (75 min); envelopar também em admit
>    (30 min) aninha dois locks com budgets diferentes e ordem indefinida.
> 6. **HIGH — opt-in sem chokepoint.** Job não-envelopado consome RAM invisível
>    ao ledger → contabilidade sistematicamente errada; admit não é o runner-hook
>    (que o runner chama sozinho), então não há ponto de enforcement.

## A virada do design (o reframe central)

`civmctl admit` **NÃO é o mecanismo de segurança de OOM**. Ele é um **limitador
de concorrência + ordering** com gate de pressão ao vivo. A segurança real de
RSS é delegada ao **cgroup por job** (`systemd-run --scope -p MemoryHigh/MemoryMax`),
que o kernel enforça — assim a "reserva" deixa de ser ficção e vira **limite
comportamental real**. Sem o cgroup, qualquer contabilidade em MB é falsa
segurança (#1/#2). Com ele, a conta `MemAvailable − Σreserva >= reserva` passa a
ser válida porque cada job **não pode** exceder sua reserva.

---

## Resolução dos blockers (decisões fechadas)

| # | Blocker (sev) | Decisão vinculante |
| --- | --- | --- |
| DT-v2-1 | **CRÍTICO — RSS não-limitado** (#1/#2) | `admit` envelopa o `<cmd>` em `sudo systemd-run --scope --wait -p MemoryHigh=<reserve>M -p MemoryMax=<reserve+ceil>M -p MemorySwapMax=0 -- <cmd>` (cgroup v2; o runner Ubuntu 24.04 tem; `emdev` tem `NOPASSWD: ALL`). `MemoryHigh` **throttla** o job perto da reserva (reclaim, sem matar); `MemoryMax` é o teto duro; `MemorySwapMax=0` impede que UM job sozinho mande a VM pra swap. Agora a reserva é **enforçada pelo kernel** — a conta de admissão vira sound. |
| DT-v2-2 | **CRÍTICO — fail-open** (#3) | **Fail-closed na RAM, nunca na contagem.** Após `WaitBudget`: pode relaxar o teto de **contagem** `MaxHeavy` (drena backlog), mas **NUNCA** o check de RAM nem o de pressão. Se `memwatchdog.Check()==Critical` OU `MemAvailable−Σreserva < reserve`, **recusa** (continua esperando / retorna não-zero como o `dockerlock`). `MemFn` erro → **fail-closed** (RAM desconhecida = sem RAM), com backoff. Remove o "nunca retorna não-zero". |
| DT-v2-3 | **HIGH — liveness por flock, não por heartbeat** (#4/#5) | Trocar o ledger-com-heartbeat por **N arquivos de slot com flock** (`/run/civm/admit-heavy-{1..MaxHeavy}.lock`). Admitir heavy = `flock(LOCK_EX\|LOCK_NB)` num slot livre. **O flock É a liveness:** o kernel libera no `close`/morte do holder, então um holder vivo-mas-starvado **nunca** é falso-reclamado e um PID reusado **nunca** parece vivo (sem heartbeat, sem `kill -0`, sem PID-reuse). Reusa a primitiva já provada do `dockerlock` (fd-flock + heartbeat-de-observabilidade), N vezes. Heartbeat JSON vira **só observabilidade** (`capacity --json`), nunca decisão. |
| DT-v2-4 | **HIGH — coexistência com docker-heavy lock** (#7) | **Subsumir.** Um job docker-heavy usa **só** `civmctl admit --weight=heavy` (não aninha com `civmctl lock --scope docker-heavy`). O slot heavy do admit já serializa concorrência; para a contenção do **daemon docker** especificamente (1 build de imagem por vez), o admit ganha `--exclusive=docker` que adquire também o lock docker-heavy existente **na ordem fixa admit-outermost** (admit → docker-heavy → cmd), eliminando o aninhamento ambíguo. Runbook proíbe `lock( admit( ... ) )`. |
| DT-v2-5 | **HIGH — opt-in/enforcement honesto** (#8) | Rebaixar as claims do PRD: admit é **gate cooperativo** que coordena só jobs **participantes**; NÃO protege contra job não-envelopado. A rede de segurança de OOM é (a) o cgroup por job (DT-v2-1) e (b) o `mem-watchdog`. O caminho de enforcement box-wide (wrapper de step injetado pelo runner, análogo ao `job-started`) fica registrado como evolução, NÃO entregue aqui. "Contrato auditável" → "auditável para jobs cooperantes". |
| DT-v2-6 | **HIGH — memwatchdog ativo, não só observacional** (#2/#3) | A decisão de admissão **consulta `memwatchdog.Check()` ao vivo** a cada tentativa: `Critical` → recusa/espera independente de contagem; `Warn` → não admite novo heavy (só light). O `mem-watchdog` passa de "rede observacional" a **input ativo do gate**. |
| DT-v2-7 | **MEDIUM — backoff stateless / não-FIFO** (#6) | Registrar explicitamente que o backoff não segura reserva e não é FIFO (nota de fairness/starvation). A atomicidade de uma tentativa é garantida pelo flock do slot (DT-v2-3); o teste de concorrência prova o **teto de contagem**, e o cgroup (DT-v2-1) prova o **teto de RAM** — não o `MemAvailable` instantâneo. |

---

## Constantes (override do SPEC.md ITEM-1)

```go
DefaultAdmitMaxHeavy          = 2     // teto de heavy = nº de slots de flock
DefaultAdmitHeavyHighMB       = 3500  // MemoryHigh por heavy (throttle) — calibrar
DefaultAdmitHeavyMaxMB        = 5000  // MemoryMax por heavy (teto duro)
DefaultAdmitWaitMinutes       = 30    // WaitBudget; após isso relaxa SÓ a contagem
DefaultAdmitSlotPathPrefix    = "/run/civm/admit-heavy-"   // + "{1..MaxHeavy}.lock"
DefaultAdmitHeartbeatSeconds  = 30    // observabilidade (capacity), nunca decisao
```

Invariante: `HeavyHighMB < HeavyMaxMB`; `MaxHeavy >= 1`. `MaxHeavy × HeavyMaxMB`
deve caber em `MemTotal − reserva-do-host` (checagem de sanidade no bootstrap).

## ITEMs que substituem o baseline

### ITEM-3 (override) — `internal/admit/admit.go`: slots de flock + cgroup
- `Acquire(ctx, opts)`: light → retorna na hora (sem slot). heavy → loop de
  tentativas: (a) `memwatchdog.Check()` — `Critical`/`Warn` recusa; (b)
  `MemAvailable − Σreserva-dos-slots-tomados >= HeavyHighMB`; (c) `flock NB` num
  slot livre `admit-heavy-{i}.lock`. Sucesso nos três → retorna `Admission`
  segurando o fd do slot. Backoff (DT-v2-7) entre tentativas; após `WaitBudget`
  relaxa só (a-contagem), nunca (a-Critical)/(b). `MemFn`/`Check` erro →
  fail-closed (DT-v2-2).
- `Admission.Release()`: fecha o fd do slot (libera o flock) — idempotente. NÃO
  depende de heartbeat. (Heartbeat JSON opcional só para `capacity`.)
- A reserva "viva" = nº de slots com flock tomado (contado por tentativa de
  `flock NB` em cada slot, padrão `dockerlock`).

### ITEM-4 (override) — `cmd/civmctl/admit.go`: cgroup wrapper
- Em vez de `exec.CommandContext(cmd)`, roda
  `sudo systemd-run --scope --wait -p MemoryHigh=<HighMB>M -p MemoryMax=<MaxMB>M
  -p MemorySwapMax=0 -- <cmd>` (DT-v2-1), propagando exit code e stdio.
- `--exclusive=docker` (DT-v2-4): adquire o lock docker-heavy existente
  **dentro** do slot admit (ordem fixa), antes do cmd.
- Trap de sinal → Release (fecha o slot) + repassa ao filho.

### ITEM-7 (novo) — sanidade de cgroup no bootstrap/doctor
- `civmctl doctor` reporta se `systemd-run --scope -p MemoryMax` funciona (cgroup
  v2 + delegação/sudo) — pré-condição de DT-v2-1; se não, `admit` degrada para
  limitador-de-contagem-só com WARN explícito (sem falsa promessa de RSS).

---

## Mapa Kahneman v2

| Etapa / DT | Disciplina | Pergunta | Evidência | Abort trigger |
| --- | --- | --- | --- | --- |
| DT-v2-1 cgroup | #5 Availability | a reserva limita o RSS real? | `systemd-run -p MemoryHigh/Max`; teste com job que aloca > reserva é throttado/morto, não estoura a VM | reserva sem cgroup = ficção |
| DT-v2-2 fail-closed | #2 Counterfactual | satura além do WaitBudget? | recusa em `Critical`/RAM insuficiente; relaxa só contagem | admitir heavy em `Critical` |
| DT-v2-3 flock-liveness | #5 | holder starvado é falso-reclamado? | flock é a liveness (kernel libera na morte); teste de morte SIGKILL libera o slot; holder vivo nunca reclamado | reclamar slot com flock vivo |
| DT-v2-6 watchdog ativo | #5 | o gate vê a pressão ao vivo? | `memwatchdog.Check()` por tentativa | admitir com `Check()==Critical` |
| DT-v2-1 calibração | #3 Número | por que High=3500/Max=5000? | RSS de pico medido sob carga | High < pico medido |

## Matriz de rastreabilidade v2 (adições)

| Requisito | ITEM | Teste |
| --- | --- | --- |
| RSS limitado (DT-v2-1) | ITEM-4, ITEM-7 | job que aloca 8 GB sob `MemoryMax=5000` é throttado/OOM-killed isolado, VM não vai a swap |
| fail-closed (DT-v2-2) | ITEM-3 | `Critical` recusa; `MemFn` erro → backoff, não admite |
| liveness por flock (DT-v2-3) | ITEM-3 | SIGKILL do holder libera o slot na próxima tentativa; sem heartbeat |
| watchdog ativo (DT-v2-6) | ITEM-3 | `Check()==Warn` bloqueia novo heavy, light passa |
| ordem docker (DT-v2-4) | ITEM-4 | `--exclusive=docker` adquire admit→docker-heavy nessa ordem; runbook proíbe inversa |

## Plano de testes v2 (adições/overrides)

- **`internal/admit` (hermético):** flock-slot via `FlockFn` injetado — heavy
  toma slot livre, 3º heavy sem slot espera; `MemFn`/`CheckFn` injetados —
  `Critical` recusa, RAM insuficiente recusa, boundary exato admite; Release
  fecha o slot e é idempotente; morte (fd fechado) libera o slot.
- **`cmd/civmctl` (integração, na VM):** `systemd-run --scope -p MemoryMax` real
  bounda um job que aloca além do teto (throttle/OOM isolado, VM estável);
  `--exclusive=docker` respeita a ordem.
- **`doctor`:** detecta ausência de cgroup-delegation e degrada com WARN.

## Checklist de validação v2 (adições)

- [ ] `go test ./... -race -count=1` + `golangci-lint` + `govulncheck`
- [ ] cobertura ≥80% (`internal/admit`)
- [ ] **na VM:** job que aloca > `MemoryMax` é contido pelo cgroup (VM não vai a
  swap) — prova de DT-v2-1
- [ ] SIGKILL do holder libera o slot (flock) — prova de DT-v2-3
- [ ] `civmctl doctor` reporta status de cgroup-delegation

## Veredito

`no-go` do SPEC.md → **SPECv2 fecha com `go` condicional** para Passo 3, em fatias
(constantes → flock-slots → cgroup wrapper → watchdog-ativo → doctor → runbook).
**Condição:** DT-v2-1 (cgroup) é o que torna o gate real — se a VM não suportar
`systemd-run -p MemoryMax` (doctor reprova), o `admit` degrada honestamente para
limitador-de-contagem com WARN, **sem** prometer segurança de RSS. Calibração de
`HeavyHighMB/MaxMB` fica como medição pós-merge (`mem-watchdog` sob carga),
análoga ao `ScratchBudget` do reclaim. Honestidade de escopo (DT-v2-5): é gate
**cooperativo** + cgroup, não rede box-wide contra jobs não-envelopados.
