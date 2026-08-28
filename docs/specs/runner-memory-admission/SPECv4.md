---
slug: runner-memory-admission
title: Admissão de jobs por memória (civmctl admit) — camada pós-implementação
milestone: —
issues: []
---

# SPECv4 — Admissão de jobs por memória (`civmctl admit`)

> Camada vinculante após o **Passo 3** (implementação + E2E na VM real + auditoria
> multi-perspectiva: correção/concorrência, segurança, fidelidade). O Passo 3
> revelou furos que o `SPECv3` não previa; esta camada (DT-v4-N) prevalece sobre o
> `SPECv3` onde houver conflito. Baseline: `SPECv3.md` (DT-v3-1..8 seguem válidos
> exceto onde revisados abaixo).
>
> **Por que o v3 precisou de v4** (registrado pra não reincidir): o mecanismo do
> SPECv3 estava certo (service `--pipe` como emdev + cgroup), mas a *captura da
> unit por scraping de stderr* (DT-v3-2 / ITEM-4) tinha um **race fatal**, e a
> auditoria encontrou um cluster de furos no mesmo modo de falha — "a unit vaza
> enquanto o slot é reusado", exatamente o invariante de memória que o gate existe
> pra proteger.

## Evidência empírica que ancora o v4 (na VM real)

| Cenário | SPECv3 | Causa | Fix (DT-v4) |
| --- | --- | --- | --- |
| SIGKILL do `admit` no meio do job | record fica `{"unit":"","pid":N}` (provado: `parseUnitName` corre antes do systemd-run imprimir) → órfã **não reapada**, slot reusado em cima | scrape de stderr é racey | DT-v4-1 |
| PID do holder morto é **reciclado** | `kill -0` acha o PID "vivo" → órfã não reapada | falta `PIDStartTicks` (o `dockerlock` tem) | DT-v4-2 |
| `MemTotal` do host ilegível (==0) | admite cap "generoso" de 512MB/slot num host não-medido (fail-open) | floor sem checagem agregada | DT-v4-3 |
| SIGTERM no `admit` | `cancel()` (=SIGKILL via CommandContext) corre com o stop gracioso | ordem no `forwardSignal` | DT-v4-4 |
| release de 2 goroutines | data-race em `done` + `close` de canal já fechado (panic) | guard `bool` sem sync | DT-v4-5 |
| unit name → `sudo systemctl stop` | argument-injection (`--signal=`, `-M`…) se o record for forjado/corrompido | sem validação + sem `--` | DT-v4-6 |

A prova do fix (mesma VM): SIGKILL do `admit` → `admit_reaped unit=civm-admit-heavy-1-<pid>.service` → **órfã `inactive` após o reuse** (antes seguia `active`).

## Decisões (resolução do Passo 3)

| # | Decisão vinculante |
| --- | --- |
| **DT-v4-1** | **Unit determinística, não scraping.** `Acquire` reserva o nome `civm-admit-<slot>-<pid>.service` e o grava no slot record **sob o flock, antes de qualquer start**; o CLI passa `systemd-run --unit=<nome>`. Substitui "captura da saída" do DT-v3-2/ITEM-4. Um `admit` morto a qualquer instante deixa um record reapável. O scraping de stderr (`parseUnitName`/`unitScanner`) foi **removido**. |
| **DT-v4-2** | **Defesa de PID-reuse.** O slot record passa a gravar `pid_start_ticks` (campo 22 de `/proc/<pid>/stat`, portado de `internal/dockerlock`). `reapOrphan` só considera o holder vivo se `kill -0` **E** start-ticks casam; PID reciclado → reapa. Start-ticks ilegíveis → reapa (fail-safe). |
| **DT-v4-3** | **`effectiveMemMB` fail-closed.** `MemTotal` ilegível (0) ou host pequeno demais (floor × MaxHeavy > MemTotal−reserva) → **recusa heavy** (erro), nunca admite cap não-enforçável. O quociente inteiro já garante `eff×MaxHeavy ≤ MemTotal−reserva`. |
| **DT-v4-4** | **Co-terminação graciosa.** `forwardSignal`: `release` (stop da unit + libera slot) → repassa o sinal ao `systemd-run`; **não** chama `cancel()` (que mandaria SIGKILL e correria com o stop). O `defer cancel()` do `admitAndRun` é o last-resort. |
| **DT-v4-5** | **Release goroutine-safe.** O guard de release no CLI vira `sync.Once` (chamado do fluxo principal **e** da goroutine de sinal): sem data-race em `done`, sem `close` de canal já fechado. |
| **DT-v4-6** | **Unit validada + `--` guard.** Todo nome de unit passa por `civm.ValidateServiceUnit` antes de chegar a um `systemctl` privilegiado, e o argv é `sudo systemctl stop -- <unit>` (token nunca vira opção). Slot files abrem com `O_NOFOLLOW`. |
| **DT-v4-7** | **Fail-closed em erro não-contenção.** `grabFreeSlot` só pula um slot em `errSlotBusy`; qualquer outro erro de flock (EACCES/ENOSPC) **falha fechado** em vez de virar timeout silencioso. `reapOrphan` diferencia "sem record" de erro de leitura real (loga, não mascara). `ensureAdmitRunDir` aceita um dir pré-existente só se for **gravável** pelo runner. |

### Revisão do DT-v3-6 (modo degradado)

O SPECv3 pedia um *count-limiter watchdog-gated* quando o cgroup está ausente. A
implementação adota a forma **mais segura e simples**: sem controller `memory`
enforçável, `admit` **recusa heavy** (fail-closed) — um limitador de contagem sem
bound de RAM é justamente o "contagem-cega" que o DT-v3-6 proíbe. O `doctor`
(`ADMIT_CGROUP`) reporta a condição antes. cgroup v2 é universal nos runners
(Ubuntu 24.04 verificado), então o caminho degradado nunca dispara na prática.

## Spec-sync (decisões de implementação que o SPECv3 não registrava)

1. **`--collect`** no `systemd-run`: garbage-collect da unit transiente ao terminar
   (mesmo em exit≠0) — nenhuma unit "failed" residual.
2. **Provisionamento de `/run/civm`** via `sudo install -d -o <runner>` antes do
   `Acquire` (`/run` é root-owned; o flock seam não pode `mkdir`). Falha alto
   (`exitAdmitInternal`) em vez de timeout silencioso.
3. **`stop` best-effort**: `Release`/`reap` nunca retornam erro — após `--wait`+
   `--collect` a unit já sumiu ("not loaded" = sucesso); o stop só age de fato na
   co-terminação por sinal. A liberação do slot (flock) é o efeito que sempre roda.
4. **Eventos de observabilidade** (stderr/slog): `admit_acquired`, `admit_wait`,
   `admit_wait_timeout`, `admit_released`, `admit_reaped`, `admit_stop_besteffort`,
   `admit_reap_read_error`, `admit_stop_invalid_unit`.

## Checklist de validação (fechado no Passo 3)

- [x] `go test ./... -race` + `golangci-lint` (0 issues) + `govulncheck` (No
      vulnerabilities) + cobertura `internal/admit` 83.6% (≥80%).
- [x] **VM:** payload roda como `emdev` (uid≠0) sob cgroup `MemoryMax=~3946M` — DT-v3-1.
- [x] **VM:** job > MemoryMax **OOM-morto** limpo (`result: oom-kill`), slot libera — DT-v3-4.
- [x] **VM:** **SIGKILL** do `admit` com unit viva → `admit_reaped` → órfã `inactive`
      antes do reuse — DT-v3-2 / DT-v4-1 / DT-v4-2.
- [x] **VM:** 2 heavy = 2 slots; light flui; 3º heavy → exit 78; docker sub-slot
      serializa 1 (2º → 78) — DT-v3-3 / DT-v3-8.
- [x] `civmctl doctor` reporta `ADMIT_CGROUP` / `ADMIT_RUN_AS_USER` / `ADMIT_RAM_INVARIANT`;
      `civmctl capacity` expõe `admit.heavy_live`.
- [x] sync rule: `deploy/systemd/README.md` documenta `civmctl admit` (uso + wiring).

## Pós-merge (autorizado, data-gated)

Calibração de `HeavyMaxMB` após medir o pico RSS real de jobs heavy sob carga
(DT-v3-5: "generoso até medir"). Até lá, `MemoryMax = (MemTotal−reserva)/MaxHeavy`.
Análogo ao `ScratchBudget` do reclaim (issue de medição própria).

## Veredito

SPECv3 fechava com `go` condicional; o Passo 3 cumpriu as condições (DT-v3-2/4
com testes de integração na VM) **e** corrigiu um cluster de furos que só a
implementação + E2E + auditoria exporiam. Com DT-v4-1..7 + a revisão do DT-v3-6,
a feature é **spec-completa e validada de ponta a ponta**. Escopo honesto mantido:
gate cooperativo + cgroup, inerte até um workflow chamar `civmctl admit`.
