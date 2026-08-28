---
slug: runner-memory-admission
title: Admissão de jobs por memória (civmctl admit) — 2 heavy no máximo, light flui
milestone: —
issues: []
---

# SPECv3 — Admissão de jobs por memória (`civmctl admit`)

> Versão melhorada após a 2ª rodada do Passo 2.5 (red-team sobre `SPECv2.md`).
> Baseline: `SPEC.md`; camada anterior: `SPECv2.md`. A auditoria deu **no-go** e
> a camada vinculante abaixo (DT-v3-N) prevalece.
>
> Por que o `no-go` do v2 (registrado pra não reincidir):
> 1. **CRÍTICO — `systemd-run --scope` roda o job como ROOT.** `User=` é
>    propriedade de *service*, **ignorada em `--scope`** — verificado na VM:
>    `sudo systemd-run --scope -p User=emdev -- id -un` retorna `root`. Pior pra
>    ESTE repo: job root grava `_work` root-owned, que o usuário do runner não
>    apaga (EACCES no unlinkat) e **trava o "Complete runner"** — exatamente o
>    desastre que o `civm-safedelete` existe pra mitigar (`internal/civm/civm.go`
>    DefaultSafeDeleteWrapperPath). O v2 tornaria isso o estado permanente.
> 2. **CRÍTICO — lifetime desacoplado.** O `--scope`/unit transiente não é filho
>    do runner; cancel/restart pode **orfanar** o scope com a RAM, e o flock é do
>    *wrapper* não do payload → wrapper morre, slot lê "livre", scope órfão
>    segura 5 GB → admite novo heavy em cima. Bug do v1 ressuscitado.
> 3. **HIGH — DT-v2-2 × DT-v2-3 contradição.** "Relaxar contagem" é incompatível
>    com N slots de flock fixos: ou viola o invariante ou trava pra sempre.
> 4. **HIGH — `MemoryHigh`+swap=0 trava job legítimo.** Job de 3.5–5 GB
>    (anon, irreclamável) acima do `MemoryHigh` sem swap entra em throttle-stall:
>    não morre, não erra, segura o slot. Pior que matar limpo.
> 5. **HIGH — modo degradado reintroduz o CRÍTICO do v1** (limitador-de-contagem
>    cego sem cgroup).
> 6. **MEDIUM — `MemAvailable` gate duplica o cgroup** e recusa admissões que o
>    cgroup já provaria seguras (page cache lido como pressão).
> 7. **HIGH — ordem admit→docker-heavy(75min) starva slot** (slot escasso preso
>    75 min enquanto espera o lock interno).
> 8. **HIGH — limite não-medido vira "kill CI".** `MemoryMax=5000` adivinhado
>    OOM-mata um job que pica legítimo em 5.5 GB — v1 advisory só agendava mal.

## Evidência empírica que ancora o v3 (Número, não adjetivo)

Verificado no runner real (Ubuntu 24.04, cgroup2fs, systemd 255, `emdev` com
`NOPASSWD: ALL`):

| Comando | Resultado | Conclusão |
| --- | --- | --- |
| `sudo systemd-run --scope -p User=emdev -- id -un` | **`root`** | `--scope` ignora `User=` → roda root (F1) |
| `sudo systemd-run --pipe --wait -p User=emdev -p MemoryMax=256M -p MemorySwapMax=0 -- id -un` | **`emdev`** + "Memory peak: 256.0K" | **service `--pipe`** roda como emdev + cgroup enforça |

O mecanismo correto é **service transiente `--pipe --wait`**, não `--scope`.

---

## Resolução dos blockers (decisões fechadas)

| # | Blocker (sev) | Decisão vinculante |
| --- | --- | --- |
| DT-v3-1 | **CRÍTICO — rodar como emdev, não root** (F1) | `admit` envelopa em `sudo systemd-run --pipe --wait -p User=emdev -p Group=emdev -p MemoryMax=<MaxMB>M -p MemorySwapMax=0 -- <cmd>` — **service** transiente, não `--scope`. **Verificado:** roda como `emdev` (zero `_work` root-owned, zero escalação) + cgroup enforça o teto. Teste obrigatório: asserir `id -un == emdev` (uid≠0) no payload. |
| DT-v3-2 | **CRÍTICO — co-terminar flock + service + payload** (F2) | O arquivo de slot grava o **nome da unit** (`run-uNNNN.service`). (a) trap de sinal no `admit` → `systemctl stop <unit>` no exit; (b) **reap-on-reuse:** antes de reusar um slot livre, `Acquire` para qualquer unit órfã registrada nele (flock-livre-mas-service-vivo é capturado antes de re-admitir). O flock-é-liveness passa a rastrear o consumidor real. Teste obrigatório: SIGKILL do `admit` → a unit some / é reapada **antes** do slot ser reusado. |
| DT-v3-3 | **HIGH — sem mint de slot; exit tipado** (F3) | Deletar "relaxar contagem" do v2. Após `WaitBudget` → retorna **exit tipado** `exitAdmitWaitTimeout` (como o `exitLockWaitTimeout=75` do `dockerlock`); o **job-timeout do runner** decide. Nunca cria slot N+1, nunca trava. Mantém `MaxHeavy` = nº de slots fixo (invariante intacto). |
| DT-v3-4 | **HIGH — só `MemoryMax`, sem throttle-stall** (F4) | Remover `MemoryHigh` (o footgun do throttle-stall). Só `MemoryMax` + `MemorySwapMax=0`: job que excede é **OOM-morto limpo** (CI vermelho determinístico, slot libera) — melhor que stall silencioso. |
| DT-v3-5 | **HIGH — generoso até medir** (F8) | Até `HeavyMaxMB` ser **medido** sob carga, `MemoryMax` sai **generoso**: `MemTotal − DefaultAdmitHostReserveMB` (pega só runaway real, não pico legítimo). O valor apertado medido entra depois por commit com evidência (análogo ao `ScratchBudget` do reclaim). Safe-by-default: não mata job legítimo antes de calibrar. |
| DT-v3-6 | **HIGH — degradado = watchdog-gated, nunca cego** (F5) | Sem cgroup (`doctor` reprova): degrada para **limitador-de-contagem GUARDADO pelo watchdog** (`memwatchdog.Check()==Critical`→recusa; `Warn`→sem novo heavy), **nunca** contagem-cega. Sem watchdog também → `admit` **recusa heavy** (não finge). |
| DT-v3-7 | **MEDIUM — `MemAvailable` é backpressure, não bound** (F6) | Remover a aritmética `MemAvailable − Σreserva >= reserve` (o cgroup + invariante `MaxHeavy×MaxMB <= MemTotal−host` já garantem o fit). Manter só `memwatchdog.Check()==Critical → backoff` (seguro barato contra jobs não-participantes). Sem veto por leitura instantânea de page cache. |
| DT-v3-8 | **HIGH — subsumir docker como sub-slot** (F7) | `--exclusive=docker` = um **sub-slot count=1 do próprio admit** (`/run/civm/admit-docker.lock`, mesmo modelo de flock), **não** o `dockerlock` legado de 75 min. Sem budget aninhado, sem starvation de slot por 75 min. `civmctl lock --scope docker-heavy` fica **deprecated** para jobs envelopados em `admit`. |

---

## Constantes (override de SPECv2)

```go
DefaultAdmitMaxHeavy          = 2     // teto = nº de slots de flock
DefaultAdmitHostReserveMB     = 2048  // RAM reservada ao host/SO; MemoryMax = MemTotal - isto / MaxHeavy se nao calibrado
DefaultAdmitHeavyMaxMB        = 0     // 0 => generoso (MemTotal-host)/MaxHeavy ate medir (DT-v3-5)
DefaultAdmitWaitMinutes       = 30    // WaitBudget; depois exit tipado (DT-v3-3)
DefaultAdmitSlotPathPrefix    = "/run/civm/admit-heavy-"   // + "{1..MaxHeavy}.lock"
DefaultAdmitDockerSlotPath    = "/run/civm/admit-docker.lock"  // sub-slot count=1 (DT-v3-8)
```

Sem `MemoryHigh` (DT-v3-4). Invariante: `MaxHeavy × MaxMB-efetivo <= MemTotal −
HostReserveMB` (checado no `doctor`/bootstrap).

## ITEMs que substituem o baseline

### ITEM-3 (override) — `internal/admit/admit.go`
- `Acquire`: light → retorna na hora. heavy → loop: (a) `memwatchdog.Check()`
  — `Critical`/`Warn` recusa/backoff (DT-v3-7); (b) **reap órfãs** dos slots
  (DT-v3-2); (c) `flock NB` num slot livre. Sucesso → `Admission` segurando o fd
  do slot + grava `unit_name` no slot. Após `WaitBudget` → `exitAdmitWaitTimeout`
  (DT-v3-3). Sem aritmética de `MemAvailable` (DT-v3-7).
- `Release`: `systemctl stop <unit>` + fecha o fd do slot (idempotente).
- Injetáveis: `FlockFn`, `CheckFn` (memwatchdog), `RunFn` (systemd-run/systemctl),
  `PidAliveFn`, `NowFn` — testes herméticos.

### ITEM-4 (override) — `cmd/civmctl/admit.go`
- Roda `sudo systemd-run --pipe --wait -p User=$SUDO_USER||emdev -p Group=... -p
  MemoryMax=<eff>M -p MemorySwapMax=0 -- <cmd>` (DT-v3-1); captura o `unit_name`
  da saída ("Running as unit: …") e grava no slot (DT-v3-2). Propaga exit + stdio
  (`--pipe`). Trap de sinal → Release + repassa ao filho.
- `--exclusive=docker` adquire o sub-slot docker antes do cmd (DT-v3-8).

### ITEM-7 (override) — `civmctl doctor`
- Reporta: cgroup v2 + `sudo systemd-run --pipe -p MemoryMax` funciona (roda como
  emdev, não root) + `MaxHeavy×MaxMB <= MemTotal−host`. Se cgroup ausente →
  `admit` opera em modo degradado watchdog-gated (DT-v3-6), reportado como WARN.

---

## Mapa Kahneman v3

| DT | Disciplina | Pergunta | Evidência | Abort trigger |
| --- | --- | --- | --- | --- |
| DT-v3-1 user | #5 Availability | o job roda como root? | teste `id -un==emdev`; verificado na VM | payload uid==0 (volta o _work root) |
| DT-v3-2 lifetime | #5 | morte do admit orfana o cgroup? | reap-on-reuse + trap; teste SIGKILL→unit some | slot reusado com unit órfã viva |
| DT-v3-4 MaxMB | #5 | job legítimo trava ou morre limpo? | só MemoryMax (sem High); teste job>Max é morto, slot libera | High<Max com swap=0 |
| DT-v3-5 calibração | #3 Número | por que MaxMB=generoso? | medir pico RSS antes de apertar | apertar MaxMB sem medir |
| DT-v3-6 degradado | #2 Counterfactual | sem cgroup, finge seguro? | watchdog-gated; sem watchdog recusa | contagem-cega "com WARN" |

## Matriz de rastreabilidade v3 (adições)

| Requisito | ITEM | Teste |
| --- | --- | --- |
| roda como emdev (DT-v3-1) | ITEM-4 | payload `id -un==emdev`, uid≠0 |
| co-termina (DT-v3-2) | ITEM-3/4 | SIGKILL admit → unit reapada antes do reuse do slot |
| exit tipado (DT-v3-3) | ITEM-3 | `>WaitBudget` → `exitAdmitWaitTimeout`, sem mint, sem hang |
| MaxMax limpo (DT-v3-4) | ITEM-4 | job>MaxMB OOM-morto isolado, VM estável, slot libera |
| degradado seguro (DT-v3-6) | ITEM-3/7 | sem cgroup: Critical recusa; sem watchdog recusa heavy |
| docker sub-slot (DT-v3-8) | ITEM-3/4 | 2 docker jobs não prendem slot heavy 75 min |

## Plano de testes v3

- **`internal/admit` (hermético):** flock-slot via `FlockFn`; `CheckFn` Critical
  recusa / Warn sem-novo-heavy; após WaitBudget exit tipado (não hang, não mint);
  Release idempotente + `systemctl stop`; reap-on-reuse de unit órfã; docker
  sub-slot serializa 1.
- **Integração (na VM):** `id -un==emdev` no payload (DT-v3-1); job que aloca >
  MaxMB é OOM-morto isolado e o slot libera (DT-v3-4); SIGKILL do admit → unit
  some/reapada antes do reuse (DT-v3-2).
- **`doctor`:** detecta cgroup + run-as-user; modo degradado watchdog-gated.

## Checklist de validação v3

- [ ] `go test ./... -race` + `golangci-lint` + `govulncheck` + cobertura ≥80%
- [ ] **VM:** payload roda como `emdev` (uid≠0) — prova DT-v3-1
- [ ] **VM:** job > MaxMB OOM-morto isolado, VM não vai a swap — DT-v3-4
- [ ] **VM:** SIGKILL admit → unit reapada antes do reuse do slot — DT-v3-2
- [ ] `civmctl doctor` reporta cgroup + run-as-user + invariante de RAM
- [ ] sync rule: README/AGENTS/CODEX/rules citam `civmctl admit`

## Veredito

`no-go` do v2 (rodava root; lifetime desacoplado; contradição interna) →
**SPECv3 fecha com `go` condicional** para Passo 3, em fatias (constantes →
flock-slots + reap → systemd-run-pipe-as-user wrapper → docker sub-slot →
doctor/degradado → runbook). O mecanismo central (DT-v3-1) está **verificado no
runner real** (service `--pipe` roda como emdev + cgroup enforça). Condições:
DT-v3-2 (co-termination) e DT-v3-4 (só MaxMax, generoso até medir) têm testes de
integração como gate; calibração do MaxMB fica como medição pós-merge. Escopo
honesto mantido (DT-v2-5): gate cooperativo + cgroup, não rede box-wide contra
jobs não-envelopados.
