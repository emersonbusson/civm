---
slug: runner-memory-admission
title: Admissão de jobs por memória (civmctl admit) — 2 heavy no máximo, light flui
milestone: —
issues: []
---

# PRD — Admissão de jobs por memória (`civmctl admit`)

## Resumo

A VM do runner tem RAM finita (hoje 10 GB, ~7–8 GB úteis após o host). Quando
**dois ou mais jobs pesados** (build Go, `docker compose`, `yarn install`)
rodam ao mesmo tempo, a VM entra em **swap e trava** — exatamente o estado
`critical` que o `mem-watchdog` (`internal/memwatchdog`) já detecta
(`MemAvailable < 8%` ou `swap-in-use > 1536 MB`). Hoje o `mem-watchdog` só
**observa**; não há nenhum controle que **impeça** o thrash antes de começar.

Este PRD adiciona esse controle: um portão de **admissão por memória** que o job
atravessa no início. A regra-alvo (pedido do dono): **no máximo 2 jobs pesados
simultâneos** (menos se a RAM estiver apertada); **jobs leves/smoke fluem** sem
bloquear. Valor operacional: o runner para de morrer por OOM/thrash de forma
silenciosa sob carga concorrente, e a capacidade de RAM vira um contrato
explícito e auditável — o equivalente, para RAM, do guard de headroom que o
reclaim já tem para disco.

## Contexto técnico

Pacotes/comandos/scripts e o que cada um faz na topologia (guest Go ↔ hook de
job ↔ runner):

- **`internal/memwatchdog`** — parseia `/proc/meminfo` e expõe
  `Meminfo{MemTotalMB, MemAvailableMB, AvailPct, SwapTotalMB, SwapUsedMB}` +
  `classify` (OK/Warn/Critical). **Confirmado no codebase**
  (`internal/memwatchdog/memwatchdog.go:43-72`). O sinal autoritativo é
  `MemAvailableMB` (free + cache reclamável), **não** "free".
- **`internal/dockerlock`** — flock advisory (`LOCK_EX|LOCK_NB`, backoff linear)
  com `WaitBudget`→`ErrWaitBudgetExceeded`, `Heartbeat` JSON (`acquired_at`,
  pid), e `HoldBudget` que é **só alarme (`over_budget`), nunca mata o holder**.
  `Release` para o heartbeat, destrava e remove o sidecar. **Confirmado no
  codebase** (`internal/dockerlock/dockerlock.go:62-132`). Hoje serializa
  trabalho docker-heavy em **1 por vez**.
- **`internal/portblock`** — reserva **sticky** persistida como mapa JSON
  `slot→base`, com flock no **ciclo inteiro read→find→write** sobre um sidecar
  (`StatePath + ".lock"`, nunca o `StatePath`, porque `os.Rename` troca o inode)
  e escrita atômica via temp + `os.Rename`. **Confirmado no codebase**
  (`internal/portblock/portblock.go:6-75`). É o padrão de **ledger de reserva**.
- **Hooks de job** — o runner chama `civmctl job-started` / `job-completed` no
  início/fim de cada job (`cmd/civmctl/main.go:28`, via
  `job-started.sh`/`job-completed.sh`). **Confirmado no codebase.**
- **Constantes** (`internal/civm/civm.go`): `DefaultDockerHeavyLockWaitMinutes=75`,
  `DefaultDockerHeavyLockBudgetMinutes=50`, `DefaultDockerHeavyHeartbeatSeconds=30`.
  **Confirmado no codebase.**

**Confirmado na documentação oficial:** `flock(2)` é advisory e liberado no
`close`/morte do processo (libera reserva órfã de job que crashou);
`MemAvailable` do kernel é a estimativa correta de RAM alocável sem swap.

**Sendo proposto:** um subcomando `civmctl admit` + um **ledger de reservas de
memória** (reusando os padrões `dockerlock` + `portblock`) + constantes de
budget. `civmctl admit` ainda **não existe** nos workflows (feature nova).

## Opção recomendada

**Admissão por reserva de memória.** O job declara seu peso e atravessa
`civmctl admit --weight=heavy|light|auto --exec -- <cmd>`, que: lê
`MemAvailableMB`, lê o ledger de reservas vivas, e **admite o job pesado apenas
se** `(heavy_vivos < MaxHeavy)` **E** `(MemAvailableMB − reservado >=
HeavyReserveMB)`. Se não couber, faz backoff (como o `dockerlock`); jobs leves
fluem direto. Ao admitir, grava a reserva + heartbeat, roda o `--exec`, e libera
no fim (ou na morte — flock + heartbeat stale reclamam reserva órfã).

**Motivo:** é **proativo** (impede o thrash antes de começar, ≠ matar job já
thrashando), **reusa infra testada** (flock/heartbeat do `dockerlock`, ledger
atômico do `portblock`, parse do `memwatchdog`), e implementa exatamente o
"2 heavy max, light flui". Generaliza o lock docker-heavy binário (1-por-vez)
para um budget de RAM (≥1, até `MaxHeavy`).

**Alternativas descartadas:**
- **Semáforo de contagem N=2 puro** (estender o docker-heavy lock para 2 slots):
  é **cego à RAM** — 2 jobs gigantes ainda estouram a memória. Reserva por MB
  cobre o caso "2 heavy mas a RAM não aguarda".
- **Reativo via `mem-watchdog` (matar/pausar em `critical`):** o job já está
  thrashando quando dispara; matar desperdiça trabalho e fere o princípio
  `dockerlock` de "alarme, nunca mata".
- **Cgroups/`systemd-run` com `MemoryMax` por job:** isola, mas **não coordena**
  (cada job não sabe dos outros) e `MemoryMax` mata via OOM-killer — pior que
  admitir/esperar. Fica como evolução futura ortogonal.

**Trade-offs aceitos:** um job pesado pode **esperar** (backoff) até a RAM
liberar — latência trocada por estabilidade. Sob pressão sustentada além do
`WaitBudget`, o gate **admite com alarme** (`admit_over_wait`, fail-open) em vez
de bloquear o CI pra sempre — coerente com "alarme, nunca trava o pipeline".

## Requisitos funcionais

- **RF-1 — Subcomando `civmctl admit`.** `civmctl admit --weight=heavy|light|auto
  --exec -- <cmd...>` adquire admissão, roda `<cmd>` e propaga o exit code dele.
  - **Critério de aceite:** com RAM folgada e 0 heavy ativos, um `--weight=heavy
    --exec -- true` admite imediatamente e retorna 0; o exit do `<cmd>` é
    propagado verbatim.
  - **Isolamento/concorrência:** a decisão (ler ledger → decidir → gravar
    reserva) roda **inteira** sob flock do sidecar do ledger.

- **RF-2 — Teto de heavy.** No máximo `MaxHeavy` (default **2**) reservas heavy
  vivas ao mesmo tempo.
  - **Critério de aceite:** com `MaxHeavy=2` e 2 heavy vivos, um 3º
    `--weight=heavy` **espera** (não admite) até um liberar.

- **RF-3 — Guard de RAM.** Um heavy só admite se
  `MemAvailableMB − reservado_vivo >= HeavyReserveMB`.
  - **Critério de aceite:** com RAM apertada (ex.: `MemAvailable` baixo), mesmo
    com `heavy_vivos < MaxHeavy`, o heavy espera até a folga cobrir
    `HeavyReserveMB`.

- **RF-4 — Light flui.** `--weight=light` admite imediatamente, sem ocupar slot
  heavy nem bloquear por RAM (reserva 0 ou mínima só para contabilidade).
  - **Critério de aceite:** com 2 heavy vivos saturando o teto, um
    `--weight=light` admite na hora.

- **RF-5 — Liberação garantida.** Ao terminar (ou morrer) o `<cmd>`, a reserva é
  liberada: remoção da entrada + parada do heartbeat; uma reserva **órfã**
  (heartbeat stale > `2× HeartbeatSeconds` OU flock liberado por morte) é
  reclamada pela próxima decisão.
  - **Critério de aceite:** matar `civmctl admit` (SIGKILL) no meio libera o
    slot para a próxima decisão dentro de `2× HeartbeatSeconds`.

- **RF-6 — Backoff com fail-open.** Se não couber, faz backoff linear até
  `WaitBudget`; ao exceder, **admite mesmo assim** logando `admit_over_wait`
  (fail-open) — nunca trava o CI indefinidamente.
  - **Critério de aceite:** sob saturação que dura mais que `WaitBudget`, o job
    eventualmente roda, com `admit_over_wait` no log.

- **RF-7 — `auto`.** `--weight=auto` resolve o peso por `CIVM_JOB_WEIGHT` (env
  setado pelo workflow do peer); ausente → default **heavy** (conservador).
  - **Critério de aceite:** `CIVM_JOB_WEIGHT=light civmctl admit --weight=auto`
    trata como light; sem a env, trata como heavy.

## Requisitos não-funcionais

- **Performance:** a decisão de admissão é O(n) sobre o ledger (n = jobs vivos,
  ≤ dezena) + 1 leitura de `/proc/meminfo`; overhead esperado < 50 ms quando
  admite na hora. O poll de backoff usa o intervalo do `dockerlock` (sem busy-wait).
- **Segurança:** o ledger fica em `/run/civm/` (tmpfs, só root/runner-user);
  nenhum secret. Não executa comando de input externo sem validação;
  `--weight` é enum fechado. Anti-skynet: **nunca mata** um job — só admite/espera.
- **Observabilidade:** eventos estruturados (`slog`/JSON) em `admit_acquired`,
  `admit_wait`, `admit_over_wait`, `admit_released`, `admit_reclaimed_stale`,
  com `weight`, `reserved_mb`, `mem_available_mb`, `heavy_live`. `civmctl
  capacity --json` expõe `heavy_live`/`reserved_mb` para decisão humana.
- **Escalabilidade:** comportamento com N peers no mesmo box: o ledger é
  **box-wide** (um por VM), então a admissão coordena todos os runners da VM —
  é o ponto. `MaxHeavy`/`HeavyReserveMB` calibram pelo tamanho real da VM.
- **Resiliência:** job que crasha → flock liberado + heartbeat stale → reserva
  reclamada (sem vazar slot). `/proc/meminfo` ilegível → fail-open com alarme
  (não trava o job por falha de leitura).

## Fluxos

**Happy path (heavy admitido):**
1. Workflow do peer roda `civmctl admit --weight=heavy --exec -- <build>`.
2. `admit` adquire flock do sidecar do ledger; reclama reservas stale; lê
   `MemAvailableMB`.
3. `heavy_vivos < MaxHeavy` E folga cobre `HeavyReserveMB` → grava reserva
   `{pid, weight=heavy, reserved_mb, acquired_at}`, solta o flock, inicia
   heartbeat. Log `admit_acquired`.
4. Roda `<build>`; no exit, remove a reserva + para heartbeat. Log
   `admit_released`. Propaga o exit code.

**Fluxos alternativos:**
- **Heavy precisa esperar:** teto ou RAM não cobrem → solta o flock, backoff
  linear, re-tenta. Log `admit_wait` (1ª vez). Admite quando couber.
- **Light:** admite imediatamente, reserva mínima, sem ocupar slot heavy.

**Fluxos de erro:**
- **`WaitBudget` excedido:** admite fail-open, log `admit_over_wait` (level WARN).
  Impacto: pode haver pressão de RAM momentânea — o `mem-watchdog` segue como
  rede de segurança observacional.
- **`/proc/meminfo` ilegível:** admite fail-open, log `admit_meminfo_error`
  (WARN). Não bloqueia o job por falha de leitura.
- **Ledger corrompido (JSON inválido):** trata como vazio, reinicia o ledger,
  log `admit_ledger_reset` (WARN) — fail-open.

## Modelo de estado

**Ledger de reservas** — JSON em `/run/civm/admit-reservations.json`, protegido
pelo sidecar `/run/civm/admit-reservations.json.lock` (padrão `portblock`):

```json
{
  "reservations": [
    {"pid": 12345, "weight": "heavy", "reserved_mb": 3500,
     "acquired_at": "2026-06-05T03:00:00Z", "heartbeat_at": "2026-06-05T03:00:30Z"}
  ]
}
```

Reserva viva = pid existe E `heartbeat_at` dentro de `2× HeartbeatSeconds`.
Escrita atômica via temp + `os.Rename` (sidecar é o lock, nunca o arquivo).

**Constantes novas** (`internal/civm/civm.go`):

```go
DefaultAdmitMaxHeavy        = 2     // teto de jobs pesados simultaneos
DefaultAdmitHeavyReserveMB  = 3500  // RAM reservada por job heavy
DefaultAdmitLightReserveMB  = 0     // light nao reserva
DefaultAdmitWaitMinutes     = 30    // WaitBudget antes do fail-open
DefaultAdmitHeartbeatSeconds = 30   // espelha DockerHeavyHeartbeatSeconds
DefaultAdmitLedgerPath      = "/run/civm/admit-reservations.json"
```

## API / Interfaces

`civmctl admit` (subcomando novo em `cmd/civmctl/`):

| Flag | Valor |
| --- | --- |
| `--weight` | `heavy` \| `light` \| `auto` (default `auto`) |
| `--reserve-mb` | override do reserve do peso (default pela constante) |
| `--exec -- <cmd...>` | comando a rodar sob admissão (obrigatório) |
| `--json` | logs estruturados em stdout |
| `--wait-minutes` | override do `WaitBudget` |

**Exit codes:** propaga o exit do `<cmd>`. Erros do próprio `admit`: `64`
(flag/uso inválido). `admit` em si nunca retorna não-zero por "não coube" —
ele espera ou faz fail-open.

## WYSIATI — o que NÃO foi visto

- **Calibração real de `HeavyReserveMB` e `MaxHeavy`:** 3500 MB e 2 são estimados
  pelo tamanho da VM (10 GB) sem medir o pico real de um build Go/yarn/docker.
  Sem ter medido o RSS de pico de um job real, estimo `HeavyReserveMB≈3500` com
  confiança ~60% — calibrar com `mem-watchdog` sob carga (disciplina #3).
- **Adoção pelos peers:** exige o workflow do peer **envelopar** o step pesado em
  `civmctl admit` — fora do controle do civm; precisa de runbook + template.
- **Interação com o docker-heavy lock existente:** `admit` e o lock docker-heavy
  coexistem; se ambos guardarem o mesmo job pode haver dupla espera. A decisão
  de subsumir o lock docker-heavy no `admit` fica para o SPEC (risco de contrato).

## Disciplinas (Kahneman) antecipadas para o SPEC

- **Calibração de budget** (#3 Número não adjetivo): `HeavyReserveMB`/`MaxHeavy`
  precisam de medição (RSS de pico via `mem-watchdog`), não de adjetivo.
- **Fail-open vs fail-closed** (#5 Availability): a escolha de admitir-com-alarme
  após `WaitBudget` é o counterfactual — nunca travar o CI; o `mem-watchdog`
  permanece como rede observacional.
- **Reclamação de reserva órfã** (#5): flock + heartbeat stale; abort trigger =
  slot vazado (heavy_live nunca decrementa) — teste de morte (SIGKILL) obrigatório.
