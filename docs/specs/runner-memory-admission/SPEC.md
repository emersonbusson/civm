---
slug: runner-memory-admission
title: Admissão de jobs por memória (civmctl admit) — 2 heavy no máximo, light flui
milestone: —
issues: []
---

# SPEC — Admissão de jobs por memória (`civmctl admit`)

Derivado de `PRD.md` (mesma pasta). Implementação estrita a partir deste SPEC
(Passo 3). Toda decisão nova volta ao SPEC antes de virar código.

## Objetivo do Passo 2

Transformar os RF-1..7 do PRD em ITEMs de código rastreáveis, reusando
`internal/dockerlock` (heartbeat/staleness), `internal/portblock` (ledger JSON
atômico sob flock de sidecar) e `internal/memwatchdog` (leitura de
`MemAvailableMB`). Sem inventar primitivo novo onde já existe padrão testado.

## ITEMs

### ITEM-1 — `internal/civm/civm.go`: constantes (RF-2/3/4/6) — Day-0

No bloco `const`, após o grupo do reclaim:

```go
DefaultAdmitMaxHeavy         = 2     // teto de heavy simultaneo (RF-2)
DefaultAdmitHeavyReserveMB   = 3500  // RAM reservada por heavy (RF-3) — calibrar
DefaultAdmitLightReserveMB   = 0     // light nao reserva (RF-4)
DefaultAdmitWaitMinutes      = 30    // WaitBudget antes do fail-open (RF-6)
DefaultAdmitHeartbeatSeconds = 30    // espelha DefaultDockerHeavyHeartbeatSeconds
DefaultAdmitLedgerPath       = "/run/civm/admit-reservations.json"
```

Invariante: `HeavyReserveMB > LightReserveMB >= 0`; `MaxHeavy >= 1`.

### ITEM-2 — `internal/memwatchdog`: exportar `Sample` (RF-3)

Hoje o parse de `/proc/meminfo` é interno (`parseMeminfo`). Exportar um helper
puro para o `admit` ler RAM ao vivo sem duplicar o parser:

```go
// Sample lê e parseia /proc/meminfo via opts.MeminfoFn (default /proc/meminfo).
func Sample(opts Options) (Meminfo, error)
```

Sem mudar `Check`/thresholds. Teste: `Sample` com `MeminfoFn` injetado retorna o
`Meminfo` esperado (positivo) e propaga erro de leitura (negativo).

### ITEM-3 — `internal/admit/admit.go`: núcleo da admissão (RF-1/2/3/5/6)

Reusa o padrão `portblock` (flock do sidecar para o ciclo read→decide→write,
escrita atômica temp+`os.Rename`) e o padrão `dockerlock` (heartbeat + staleness).

```go
package admit

type Weight string
const ( WeightHeavy Weight = "heavy"; WeightLight Weight = "light" )

type Reservation struct {
    PID         int       `json:"pid"`
    Weight      Weight    `json:"weight"`
    ReservedMB  int64     `json:"reserved_mb"`
    AcquiredAt  time.Time `json:"acquired_at"`
    HeartbeatAt time.Time `json:"heartbeat_at"`
}
type ledger struct { Reservations []Reservation `json:"reservations"` }

type Options struct {
    LedgerPath     string        // default DefaultAdmitLedgerPath
    MaxHeavy       int
    HeavyReserveMB int64
    Weight         Weight
    ReserveMB      int64         // 0 => default por peso
    WaitBudget     time.Duration
    HeartbeatEvery time.Duration
    // injetaveis (testes hermeticos):
    FlockFn  func(path string) (release func(), err error) // flock do sidecar
    MemFn    func() (memwatchdog.Meminfo, error)           // default memwatchdog.Sample
    PidAlive func(pid int) bool                            // default syscall kill -0
    NowFn    func() time.Time
    SleepFn  func(time.Duration)
}

// Admission e uma reserva viva. Release remove a entrada e para o heartbeat.
type Admission struct { /* pid, ledgerPath, stop chan, ... */ }
func (a *Admission) Release() error

// Acquire admite o job conforme as regras de admissao; bloqueia (backoff) ate
// caber ou ate WaitBudget (fail-open). Light retorna na hora.
func Acquire(ctx context.Context, opts Options) (*Admission, error)
```

**Algoritmo de `Acquire` (cada tentativa, sob flock do sidecar):**
1. `release := FlockFn(LedgerPath+".lock")`; `defer release()`.
2. Ler ledger (JSON inválido/ausente → `ledger{}`, log `admit_ledger_reset`).
3. **Reclamar stale:** descartar reservas com `!PidAlive(pid)` OU
   `Now − HeartbeatAt > 2×HeartbeatEvery` (RF-5).
4. Se `Weight==light`: gravar reserva (ReservedMB=Light), `os.Rename`, retornar
   `Admission` (RF-4) — sem checar teto/RAM.
5. Se `Weight==heavy`: `heavyLive := count(weight==heavy)`;
   `reservedSum := Σ ReservedMB`; `mem := MemFn()`.
   - Admite se `heavyLive < MaxHeavy` **E**
     `mem.MemAvailableMB − reservedSum >= ReserveMB` (RF-2/3) → grava + rename +
     retorna `Admission`.
   - Senão: soltar flock; `SleepFn(backoff)`; se `elapsed > WaitBudget` → admite
     fail-open (grava reserva, log `admit_over_wait` WARN) (RF-6); else re-tenta.
6. `Admission` inicia goroutine de heartbeat (atualiza `HeartbeatAt` a cada
   `HeartbeatEvery`, sob flock). `Release` para a goroutine, remove a entrada
   (sob flock, temp+rename) e é idempotente (chamar 2× é no-op) — espelha
   `dockerlock.Lock.Release`.

Erro de `MemFn` → tratar como fail-open (admite, log `admit_meminfo_error` WARN)
(RF erro). **Nunca** retorna não-zero por "não coube".

### ITEM-4 — `cmd/civmctl/admit.go`: CLI (RF-1/7)

```go
func runAdmit(args []string) int
```
- flags: `--weight` (enum, default `auto`), `--reserve-mb`, `--wait-minutes`,
  `--json`, e `--exec --` separando o comando (obrigatório; ausente → `exitUsage`).
- `auto` → `CIVM_JOB_WEIGHT` env; ausente → `heavy` (RF-7).
- `Acquire(ctx, opts)`; em erro de uso → `64`.
- Roda `<cmd>` com `exec.CommandContext` herdando stdio; **propaga o exit code**.
- `defer admission.Release()`; trap `SIGINT/SIGTERM` → Release + repassa o sinal
  ao filho (RF-5: morte libera).

### ITEM-5 — `cmd/civmctl/main.go` + `capacity` (observabilidade)

- `case "admit": return runAdmit(rest)`.
- `civmctl capacity --json` ganha `admit: {heavy_live, reserved_mb, max_heavy}`
  lendo o ledger read-only (reusa o parser do `admit`, sem flock de escrita).

### ITEM-6 — Adoção (runbook + template) — não-código

- `runbooks/RUNNER-MEMORY-ADMISSION.md`: como o peer envelopa o step pesado
  (`civmctl admit --weight=heavy --exec -- <build>`), calibração de
  `HeavyReserveMB`/`MaxHeavy`, e leitura de `capacity --json`.
- `templates/`: snippet de step para o `ci.yml` do peer.

## Guardrail cognitivo obrigatório

### ITEM-3 — concorrência do ledger (#5 Availability)
- **Pergunta:** dois `admit` simultâneos podem ambos achar que cabe e estourar o teto/RAM?
- **Evidência mínima:** o ciclo read→decide→write roda **inteiro** sob flock do
  sidecar (padrão `portblock`); teste com `FlockFn` serializando 3 heavy
  concorrentes confirma `heavy_live <= MaxHeavy` sempre.
- **Abort trigger:** qualquer caminho que grave reserva fora do flock.

### ITEM-3 — reserva órfã (#5 Availability)
- **Pergunta:** job que morre (SIGKILL) vaza o slot heavy pra sempre?
- **Evidência mínima:** reclamação por `!PidAlive` **e** heartbeat stale; teste
  de morte (PID inexistente + heartbeat velho) libera o slot na próxima decisão.
- **Abort trigger:** `heavy_live` que nunca decrementa após morte do holder.

### ITEM-1/3 — calibração do budget (#3 Número não adjetivo)
- **Pergunta:** por que `HeavyReserveMB=3500` e `MaxHeavy=2`?
- **Evidência mínima:** medir o RSS de pico de um build real (Go/yarn/docker) via
  `mem-watchdog` sob carga; `HeavyReserveMB >= pico medido`. Até medir, é
  estimativa explícita (PRD WYSIATI) — ship com o default + recalibrar por commit.
- **Abort trigger:** baixar `HeavyReserveMB` abaixo do pico medido.

### RF-6 — fail-open vs trava (#2 Counterfactual / #5)
- **Pergunta:** o que acontece sob saturação que dura mais que `WaitBudget`?
- **Evidência mínima:** admite com `admit_over_wait` (WARN), nunca bloqueia o CI;
  o `mem-watchdog` segue como rede observacional.
- **Abort trigger:** `admit` que retorna não-zero / trava o pipeline por "não coube".

## Mapa Kahneman

| ITEM | Disciplina | Pergunta | Evidência | Abort trigger |
| --- | --- | --- | --- | --- |
| ITEM-3 ledger | #5 Availability | dois admit estouram o teto? | flock no ciclo todo; teste 3-concorrentes | gravar fora do flock |
| ITEM-3 órfã | #5 | morte vaza slot? | PidAlive + heartbeat stale; teste de morte | heavy_live não decrementa |
| ITEM-1 budget | #3 Número | por que 3500/2? | RSS de pico medido | budget < pico medido |
| RF-6 fail-open | #2 Counterfactual | satura além do WaitBudget? | admite com alarme | travar o CI |

## Matriz de rastreabilidade

| RF | ITEM | Teste |
| --- | --- | --- |
| RF-1 admit/exec | ITEM-3, ITEM-4 | `Acquire` admite + propaga exit do `<cmd>` |
| RF-2 teto heavy | ITEM-1, ITEM-3 | 3º heavy espera com 2 vivos |
| RF-3 guard RAM | ITEM-2, ITEM-3 | heavy espera com `MemFn` baixo mesmo sob teto |
| RF-4 light flui | ITEM-3 | light admite com teto saturado |
| RF-5 liberação/órfã | ITEM-3 | Release idempotente; morte → reclaim |
| RF-6 fail-open | ITEM-3, ITEM-4 | `> WaitBudget` admite + `admit_over_wait` |
| RF-7 auto | ITEM-4 | `CIVM_JOB_WEIGHT` resolve; ausente → heavy |

## Fronteira de atomicidade e rollback

- **Fatias ortogonais (micro-slicing):** (1) constantes (ITEM-1); (2)
  `memwatchdog.Sample` (ITEM-2); (3) núcleo `internal/admit` (ITEM-3); (4) CLI
  (ITEM-4); (5) capacity/observabilidade (ITEM-5); (6) runbook/template (ITEM-6).
  Cada uma é commit atômico, validada antes da próxima.
- **Rollback:** é **forward-only de adoção** — enquanto nenhum workflow chama
  `civmctl admit`, o subcomando é inerte (zero efeito no CI). Reverter = não
  envelopar os steps; o binário com `admit` não muda comportamento sozinho.
  Rollback de código = reverter o commit da fatia ofensora.

## Ordem de implementação

1→6 na ordem acima. ITEM-3 é o núcleo (maior risco) e leva os testes de
concorrência/órfã. ITEM-6 (adoção) só depois do binário validado.

## Plano de testes

- **`internal/admit` (unit, hermético via `FlockFn`/`MemFn`/`PidAlive`/`NowFn`):**
  light flui sob teto; heavy espera por teto; heavy espera por RAM; boundary
  exato (`MemAvailable − reserved == ReserveMB` admite); fail-open após
  `WaitBudget`; Release idempotente; reclaim de órfã (PID morto + heartbeat
  stale); 3 heavy concorrentes nunca passam de `MaxHeavy` (serialização por flock).
- **`internal/memwatchdog`:** `Sample` positivo/negativo.
- **`cmd/civmctl`:** `runAdmit` propaga exit do `<cmd>`; `--weight` inválido →
  64; `auto`+env; `--exec` ausente → usage.
- **Cobertura ≥80%** por package (`internal/admit`, `internal/memwatchdog`).

## Checklist de validação

- [ ] `go test ./... -race -count=1`
- [ ] `golangci-lint run -c .golangci.yml ./...`
- [ ] `go test -cover ./internal/admit/ ./internal/memwatchdog/` (≥80%)
- [ ] `govulncheck ./...`
- [ ] `civmctl admit --weight=light --exec -- true` retorna 0 (smoke)
- [ ] `civmctl capacity --json` mostra `admit.heavy_live`
- [ ] sync rule: README/AGENTS/CODEX/rules citam `civmctl admit` no mesmo commit
  da fatia CLI

## Veredito

`go` para Passo 3 (implementação), com **risco operacional contido**: o
subcomando é **inerte até um workflow chamá-lo** (rollback forward-only). Os dois
pontos que exigem disciplina explícita no Passo 3 estão fechados acima
(concorrência do ledger sob flock; reclamação de órfã por PidAlive+heartbeat). A
**calibração de `HeavyReserveMB`/`MaxHeavy`** fica como medição pós-merge
(`mem-watchdog` sob carga) antes de os peers adotarem em volume — análogo ao
`ScratchBudget` do reclaim. Risco estrutural baixo o suficiente para dispensar
Passo 2.5 (red-team) obrigatório; opcional se a calibração de budget for
contestada.
