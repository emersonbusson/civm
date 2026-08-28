# SPEC — Acesso ao guest resiliente (console serial OOB)

> SSDV3 PASSO 2. Traduz `PRD.md` em arquivos, predicados Go testáveis, esqueletos
> `.ps1` vinculantes e thresholds. Links Kahneman nos passos críticos. Implementa
> os RF-1..RF-5 / RNF-1..RNF-5 do PRD.

## Princípio de design

As **DECISÕES** (classificar o erro tipado, decidir se o artefato está stale, se
o timeout estourou) vão para Go puro e testável em `internal/serialrecover`
(regra dura SSDV3 de reuso — espelha o padrão `internal/hostdisk`). A
**ORQUESTRAÇÃO** (abrir o named pipe, login PAM, enviar bytes) fica no
`.ps1`/cliente de pipe que só o Windows pode fazer. Nenhum canal de acesso novo é
criado dentro do guest — só `serial-getty` (init nativo) + um cliente host-side.
O `sudo.exe` é o mesmo vetor de `Stop-VM`/`Optimize-VHD`.

## Arquitetura de tiers (decisão fechada)

| Tier | Papel | Vive em | Por quê |
| --- | --- | --- | --- |
| **Serial console OOB** (este SPEC) | **PRIMARY** acesso sob sshd morto | getty no guest (init/PID 1) + pipe no VMWP (host) | único caminho cujo ator não é processo de rede do guest (#15) |
| cgroup/slice + cap concorrência + sshd hardening | SECONDARY preventivo | guest | reduz a frequência do OOB; metade já existe (`internal/admit`) — fora deste SPEC |
| power-cycle host-side (`Invoke-GuestUnreachableForcedReboot`) | SECONDARY último recurso | host | martelo quando até o login serial pendura; **já shipado** |
| vsock / Hyper-V Sockets | REJECT | guest userspace | ator morre com o recurso (disco/RAM, não rede) |
| tunnel outbound (Tier 3) | DEFER | guest userspace | morre com o recurso; só ajuda guest-vivo+rede-bloqueada |

## RF-1 — Canal serial OOB existe e é servido pelo host

**Host (one-time, idempotente)** — `deploy/windows/register-civm-serial-console.ps1`:

```powershell
# Set-VMComPort exige a VM Off (config de firmware; nao e hot-reconfiguravel).
# Idempotente no molde register-*.ps1: ler o path atual; so parar/setar/religar
# se divergir, senao no-op. NUNCA recria o pipe a quente.
#   $cur = (Get-VMComPort -VMName $VMName -Number 1).Path
#   if ($cur -eq $PipePath) { log serial_console_already_configured; exit 0 }   # idempotente (#16)
#   $wasRunning = (Get-VM $VMName).State -eq 'Running'
#   if ($wasRunning) { Stop-VM -Name $VMName -ErrorAction Stop; Wait-VMState Off }
#   Set-VMComPort -VMName $VMName -Number 1 -Path $PipePath -ErrorAction Stop
#   if ($wasRunning) { Start-VM -Name $VMName -ErrorAction Stop; Wait-VMState Running }
#   Copy-Item civm-serial-console.ps1 -Destination C:\civm-deploy -Force         # artefato fresco (RF-5)
```

`$PipePath = '\\.\pipe\civm-console'` (cristalizado Day-0). O registro só roda em
janela (VM Off aceitável). Mirror do `register-civm-host-metrics.ps1`
(`Test-TaskExists`/idempotência) e do `register-civm-vhdx-optimize.ps1`
(`Wait-VMState`).

**Guest (imagem/cloud-init)** — `serial-getty@ttyS0`:

```bash
# kernel cmdline: console=ttyS0,115200 console=tty1  (mantem tty1; tty1 por ultimo
#   nao rouba o console primario do serial)
# systemctl enable serial-getty@ttyS0.service   # agetty, login PAM normal
# PROIBIDO: qualquer override com --autologin (RF-2)
```

**Disciplina Kahneman:**
- **Disciplina** #16 Idempotência · **Link** `disciplines/KAHNEMAN-DISCIPLINES.md`
  §16 · **Pergunta** "Re-rodar o register duplica config ou exige estado prévio?"
  · **Evidência** `Get-VMComPort` igual antes/depois do 2º run; nenhum 2º
  Stop/Start quando o path já bate · **Abort trigger** se o register exigir VM
  Off num run em que ela já estava configurada → bug de idempotência, corrigir.

## RF-2 — Acesso sem `--autologin root`

O `serial-getty@ttyS0` usa o template padrão do systemd (agetty → `/bin/login`
→ PAM). Nenhum drop-in `[Service] ExecStart=... --autologin`. A senha do `emdev`
é secret do operador (password manager), nunca no repo.

**Lint guard (novo, `internal/hostdisk/ps1_safety_test.go` estende):** regex
`--autologin` em `deploy/windows/*.ps1` E em qualquer unit/cloud-init versionado
sob `deploy/**` → falha. Mirror do gate Int32 (#17).

**Disciplina Kahneman:**
- **Disciplina** #15 Fail-safe default · **Link** §15 · **Pergunta** "Um root
  shell sempre-ligado atrás do pipe é a default segura?" · **Evidência** grep
  `--autologin` = 0 matches em `deploy/**`; getty apresenta `login:` · **Abort
  trigger** qualquer `--autologin` no diff → bloquear merge.

## RF-3 — Acesso provado por EFEITO, sob starvation

**Cenário de teste (host-side, validação):**

1. Reproduzir o wedge: disparar carga testcontainers concorrente no guest até o
   `ssh emdev@gha-ubuntu-2404 true` dar `Connection timed out` (sshd morto).
2. **Par negativo (#13):** registrar que o SSH falha no mesmo instante.
3. **Par positivo:** `civmctl serial-recover --cmd 'civmctl cleanup --execute'`
   via pipe → ler `df` do guest **antes e depois** (pelo próprio serial) → o
   free SUBIU. Efeito, não "comando enviado".

Existência (`Set-VMComPort` exit 0, pipe presente, getty `enabled`) **não**
conta como aceite. O aceite é o `df` subindo com o SSH provadamente morto.

**Disciplina Kahneman:**
- **Disciplina** #13 Ilusão de validade · **Link** §13 (`internal/safedelete`
  INVARIANTS #9) · **Pergunta** "O verde prova que entrei e mutei o guest, ou só
  que o pipe abre?" · **Evidência** `df` antes/depois via serial + SSH timeout
  pareado no mesmo cenário · **Abort trigger** se o aceite for um probe de
  existência (pipe/getty status) sem efeito medido → não merge (é o #59).

## RF-4 — Fail-fast determinístico e tipado

**Predicado Go puro** (`internal/serialrecover/classify.go`):

```go
package serialrecover

// Outcome classifica o resultado de uma tentativa de acesso serial em erros
// TIPADOS e mutuamente exclusivos. Kahneman #15: o erro tem que ser barulhento
// e distinguivel — "VM off" != "PAM travou" != "shell vivo, comando pendurou".
type Outcome string

const (
	OutcomeOK              Outcome = "ok"
	OutcomeVMOffPipeAbsent Outcome = "vm_off_pipe_absent"  // pipe nao servido (VM Off)
	OutcomePipeConnectTO   Outcome = "pipe_connect_timeout" // Connect() estourou (VMWP travado)
	OutcomePAMLoginHung    Outcome = "pam_login_hung"       // login: apareceu mas nao progride
	OutcomeShellCmdHung    Outcome = "shell_alive_cmd_hung" // logou, comando nao retornou prompt
	OutcomeStaleArtifact   Outcome = "stale_artifact"       // .ps1 instalado != repo (RF-5)
)

// ClassifyAttempt mapeia (estágio alcançado, qual timeout estourou) para um
// Outcome. stage: "connect" | "login" | "cmd". connected/loggedIn marcam
// progresso. Determinístico: sem retry, sem mascarar (corolário de #14).
func ClassifyAttempt(stage string, connected, loggedIn, promptSeen, timedOut bool) Outcome {
	switch {
	case stage == "connect" && !connected && timedOut:
		return OutcomePipeConnectTO
	case stage == "connect" && !connected:
		return OutcomeVMOffPipeAbsent
	case stage == "login" && connected && !loggedIn && timedOut:
		return OutcomePAMLoginHung
	case stage == "cmd" && loggedIn && !promptSeen && timedOut:
		return OutcomeShellCmdHung
	case stage == "cmd" && loggedIn && promptSeen:
		return OutcomeOK
	default:
		return OutcomeShellCmdHung // conservador: trata desconhecido como falha barulhenta
	}
}

// ExitCode mapeia Outcome para o exit code do subcomando (0/1/2; nunca pendura).
func (o Outcome) ExitCode() int {
	switch o {
	case OutcomeOK:
		return 0
	case OutcomeStaleArtifact:
		return 2
	default:
		return 1
	}
}
```

**Orquestração** (`civm-serial-console.ps1`): `NamedPipeClientStream('.',
'civm-console', InOut)`; `Connect($ConnectTimeoutSeconds)` finito; após conectar,
ler até `login:` com `$LoginTimeoutSeconds`; enviar `emdev`+senha; ler até prompt
com `$LoginTimeoutSeconds`; enviar `--cmd`; ler até prompt com `$CmdTimeoutSeconds`.
Cada estágio que estoura → escreve o `Outcome` tipado em
`V:\civm-serial-recover-last.json` e sai com o exit code. **Nunca** `Connect()`
sem timeout (bloqueia indefinido se a VM Off).

**Constantes (novo bloco em `internal/civm/civm.go`):**

```go
// Acesso serial OOB ao guest (docs/specs/guest-access-resilience).
DefaultSerialPipePath           = `\\.\pipe\civm-console` // cristalizado Day-0; mudar exige Stop/Start-VM
DefaultSerialConnectTimeoutSec  = 15  // Connect() ao named pipe; VM Off => falha rapida
DefaultSerialLoginTimeoutSec    = 60  // login: -> prompt; counterfactual de rollback (>60s no PAM em 3 incidentes -> power-cycle)
DefaultSerialCmdTimeoutSec      = 120 // prompt apos --cmd; cleanup/disk-watchdog cabem
DefaultSerialIdleTMOUTSec       = 300 // TMOUT do shell de recovery (RNF-4)
```

**Disciplina Kahneman:**
- **Disciplina** #15 Fail-safe + #14 Retry calibrado · **Link** §15/§14 ·
  **Pergunta** "Sob VM Off ou PAM travado, o cliente falha rápido e tipado ou
  pendura silencioso?" · **Evidência** unit `ClassifyAttempt` table-driven (5
  Outcomes); teste host: VM Off → `vm_off_pipe_absent` em ≤15s · **Abort
  trigger** qualquer caminho sem timeout finito no `Connect()`/espera de prompt →
  no merge.

## RF-5 — O curador não vive dentro do guest moribundo

O `civm-serial-console.ps1` e o `civmctl serial-recover` host-side **nunca**
invocam SSH. O caminho de recovery é 100% host/WSL2 + pipe.

**Gate de stale (#106):** antes de abrir o pipe, `serial-recover` compara o
SHA-256 do `C:\civm-deploy\civm-serial-console.ps1` instalado com o do repo
(`deploy/windows/civm-serial-console.ps1`, embarcado no binário via `go:embed`
hash ou parâmetro de deploy). Divergência → `OutcomeStaleArtifact` exit 2, recusa
rodar. Replica a lição do postmortem #106 (`.ps1` corrigido rodava STALE em
`C:\civm-deploy`).

**Disciplina Kahneman:**
- **Disciplina** #15 (curador não morre/stale) · **Link** §15 · **Pergunta** "O
  `.ps1` que roda é o corrente ou a cópia velha de `C:\civm-deploy`?" ·
  **Evidência** hash instalado == hash repo no log `serial_recover_artifact_ok` ·
  **Abort trigger** hash diverge → `stale_artifact` exit 2, deploy corrige antes.

## Arquivos a CRIAR

**`internal/serialrecover/classify.go`**
- **Propósito:** classificação pura de erro tipado + exit code do acesso serial.
- **Requisitos cobertos:** RF-4.
- **Funções:** `ClassifyAttempt(...) Outcome`, `(Outcome).ExitCode() int` (acima).
- **Dependências:** stdlib apenas.
- **Padrão de referência:** `internal/hostdisk/phantom.go` (predicado puro
  testável, sem I/O).
- **Testes requeridos:** `classify_test.go` table-driven — os 5 Outcomes + o
  default conservador; exit codes 0/1/2.

**`internal/serialrecover/classify_test.go`** — table-driven RED→GREEN.

**`deploy/windows/civm-serial-console.ps1`**
- **Propósito:** cliente do named pipe (Connect+login PAM+`--cmd`) com timeouts
  tipados.
- **Requisitos cobertos:** RF-3, RF-4, RF-5, RNF-2, RNF-4.
- **Esqueleto vinculante:**
  ```powershell
  # param: $PipePath, $User='emdev', $Cmd, $ConnectTimeoutSec, $LoginTimeoutSec,
  #        $CmdTimeoutSec, $LogPath='V:\civm-hyperv-maintenance.log'
  # 1. gate stale (RF-5): comparar hash deste .ps1 vs o esperado; diverge -> stale_artifact exit 2
  # 2. $pipe = [System.IO.Pipes.NamedPipeClientStream]::new('.', 'civm-console',
  #      [System.IO.Pipes.PipeDirection]::InOut)
  #    try { $pipe.Connect($ConnectTimeoutSec*1000) } catch { -> vm_off_pipe_absent/pipe_connect_timeout }
  # 3. ler ate 'login:' (deadline $LoginTimeoutSec); enviar "$User`n"; ler ate 'Password:';
  #    enviar senha (de env CIVM_SERIAL_PASS, NUNCA hardcoded); ler ate prompt -> pam_login_hung se estourar
  # 4. enviar "export TMOUT=$IdleTMOUT`n"; enviar "$Cmd`n"; ler ate prompt (deadline $CmdTimeoutSec)
  #    -> shell_alive_cmd_hung se estourar
  # 5. Write-Json V:\civm-serial-recover-last.json { outcome, cmd, duration_ms }
  # SEM Invoke-Expression de input externo; SEM [math]::Max(0,...) literal (#17)
  ```
- **Padrão de referência:** `civm-host-metrics.ps1` (`Write-JsonAtomic`,
  `Write-Log` estruturado, exit codes).
- **Testes requeridos:** lint `ps1_safety_test.go` (sem `--autologin`, sem
  `Invoke-Expression`, sem clamp Int32); host: VM Off → `vm_off_pipe_absent`.

**`deploy/windows/register-civm-serial-console.ps1`**
- **Propósito:** one-time `Set-VMComPort` idempotente + instala o `.ps1` em
  `C:\civm-deploy`.
- **Requisitos cobertos:** RF-1, RF-5, RNF-1.
- **Padrão de referência:** `register-civm-host-metrics.ps1` (idempotência,
  `SupportsShouldProcess`/`-WhatIf`).
- **Testes requeridos:** lint host; `-WhatIf` não muta; 2º run = no-op
  (`serial_console_already_configured`).

**`cmd/civmctl/serialrecover.go`**
- **Propósito:** subcomando `serial-recover` (host-side) que invoca
  `sudo.exe powershell -File civm-serial-console.ps1` via `exec.CommandContext`
  (sem shell) e classifica o resultado.
- **Requisitos cobertos:** RF-3, RF-4, RF-5.
- **Funções:** `runSerialRecover(args []string) int` — flags `--cmd`,
  `--interactive`, timeouts (defaults das constantes); monta o `sudo.exe`
  CommandLine; lê o `Outcome` e retorna `(Outcome).ExitCode()`.
- **Padrão de referência:** `cmd/civmctl/diskwatchdog.go` (FlagSet, exec
  context, exit codes).
- **Testes requeridos:** `serialrecover_test.go` — parse de flags, montagem do
  CommandLine (sem shell), mapeamento Outcome→exit.

**`runbooks/RUNBOOK-GUEST-SERIAL-RECOVERY.md`**
- **Propósito:** procedimento operacional (quando o SSH dá timeout, como entrar
  pelo serial, comandos, par positivo/negativo, escalonamento para power-cycle).
- **Requisitos cobertos:** RF-3, RNF-2, RNF-3.

## Arquivos a MODIFICAR

**`internal/civm/civm.go`** — adicionar o bloco `const` de serial (acima).
- **Impacto:** novas constantes lidas por `internal/serialrecover`,
  `cmd/civmctl/serialrecover.go`, `civm-serial-console.ps1` (via parâmetro).
- **Sync rule:** se o contrato de acesso virar convenção, atualizar
  README ≡ AGENTS ≡ CODEX ≡ rules no mesmo commit.

**`cmd/civmctl/main.go`** — dispatch `case "serial-recover"` + entrada no
`printHelp` (`serial-recover  Acesso OOB ao guest via console serial (sshd morto)`).
- **Impacto:** contrato CLI; `printHelp` é o contrato visível.

**`internal/hostdisk/ps1_safety_test.go`** — estender o lint com a regex
`--autologin` sobre `deploy/**` (RF-2) e a presença de `Connect(` com argumento
de timeout no `civm-serial-console.ps1` (RF-4: proíbe `Connect()` sem timeout).
- **Impacto:** novo gate de CI (`build-civmctl` roda `go test -race`).

## Observabilidade

**Estado / outcome (host):** `V:\civm-serial-recover-last.json`
(`last_attempt_utc`, `outcome`, `cmd` sanitizado, `duration_ms`).

**Logs estruturados** (`V:\civm-hyperv-maintenance.log`, família `serial_recover_*`):

| Evento | Level | Campos |
| --- | --- | --- |
| `serial_recover_artifact_ok` | Info | `hash` |
| `serial_recover_start` | Info | `cmd`, `connect_timeout_s` |
| `serial_recover_ok` | Info | `cmd`, `duration_ms` |
| `serial_recover_pipe_absent` | Warn | `pipe_path` |
| `serial_recover_connect_timeout` | Error | `connect_timeout_s` |
| `serial_recover_pam_hung` | Error | `login_timeout_s` |
| `serial_recover_cmd_hung` | Error | `cmd`, `cmd_timeout_s` |
| `serial_recover_stale` | Critical | `hash_installed`, `hash_repo` |

Guest = `slog`/JSON; host = log estruturado já existente. Sem PII, sem segredo
(a senha nunca é logada nem persistida; vem de `CIVM_SERIAL_PASS`).

## Contratos e documentação viva

| Documento | Atualização | Motivo |
| --- | --- | --- |
| `cmd/civmctl/main.go` (`printHelp`) | Alterar | subcomando novo `serial-recover` |
| `internal/civm/civm.go` | Alterar | constantes de serial que gateiam timeout/path |
| `deploy/windows/civm-serial-console.ps1` + `register-*.ps1` | Criar | cliente de pipe + registro one-time |
| `runbooks/RUNBOOK-GUEST-SERIAL-RECOVERY.md` | Criar | procedimento operacional OOB |
| `runbooks/MULTI-PROJECT-RUNNER.md` | Alterar / N/A | seção de acesso/recovery se referenciar SSH como único canal |
| `README.md` / `AGENTS.md` / `CODEX.md` / `rules/*.md` | Alterar / N/A | sync rule se o contrato de acesso virar convenção |
| `disciplines/INVARIANTS.md` | Alterar | novo gate "sem `--autologin` + `Connect()` com timeout" |
| `docs/specs/guest-access-resilience/IMPL.md` | Criar | registro do que foi feito |

## Ordem de implementação

1. Slice 0 (sem código): baseline do `ttyS0`/COM port; medir login serial com VM
   saudável (existência, NÃO aceite).
2. Constantes (`internal/civm/civm.go`) + predicado puro
   (`internal/serialrecover/classify.go` + test) — RED→GREEN.
3. Guest: kernel cmdline + `serial-getty@ttyS0` (sem `--autologin`) + drop-in
   TMOUT (imagem/cloud-init).
4. Host: `civm-serial-console.ps1` (cliente de pipe, timeouts tipados, gate
   stale) + `register-civm-serial-console.ps1` (`Set-VMComPort` one-time).
5. `cmd/civmctl/serialrecover.go` + dispatch/`printHelp`.
6. Lint: estender `ps1_safety_test.go` (`--autologin`, `Connect(` com timeout).
7. Validação por efeito (#13): sob carga real, login PAM + `cleanup --execute`
   muta o `df`; par negativo SSH timeout.
8. Docs vivos: runbook + sync rule + INVARIANTS.

## Plano de testes

**Guest (Go):**
- `classify_test.go`: `ClassifyAttempt` table-driven — `("connect",false,...,true)
  -> pipe_connect_timeout`; `("connect",false,...,false) -> vm_off_pipe_absent`;
  `("login",true,false,...,true) -> pam_login_hung`; `("cmd",true,...,false,true)
  -> shell_alive_cmd_hung`; `("cmd",true,...,true) -> ok`; default conservador.
  ExitCode 0/1/2.
- `serialrecover_test.go`: parse de flags; CommandLine do `sudo.exe` montado sem
  shell; Outcome→exit.

**Host (PowerShell, lint + janela):**
- lint `ps1_safety_test.go`: sem `--autologin` em `deploy/**`; `Connect(` sempre
  com argumento de timeout; sem `Invoke-Expression`; sem clamp Int32 (#17).
- janela: `register-civm-serial-console.ps1` em VM Off seta o pipe; 2º run no-op;
  VM Off → `serial-recover` retorna `vm_off_pipe_absent` em ≤15s.

**Validação por efeito (RF-3, #13):**
- sob carga testcontainers concorrente real: `ssh ... true` dá timeout (par
  negativo); `serial-recover --cmd 'civmctl cleanup --execute'` via pipe → `df`
  do guest sobe (par positivo). Colar `df` antes/depois + o timeout do SSH no
  IMPL.

## Checklist de validação

**Guest (Go):**
- [ ] `gofmt -w ./...`
- [ ] `golangci-lint run -c .golangci.yml ./...`
- [ ] `go vet ./...`
- [ ] `go test ./... -race -count=1`
- [ ] `go test -count=1 -cover ./internal/...` (≥80% por package)
- [ ] `govulncheck ./...`

**Host (PowerShell):**
- [ ] lint host: sem `--autologin`; `Connect(` com timeout; sem clamp Int32
- [ ] janela: `Set-VMComPort` seta o pipe; VM Off → `vm_off_pipe_absent` ≤15s

**Validação por efeito:**
- [ ] sob carga real: SSH timeout (negativo) + `cleanup --execute` muta `df`
      (positivo) colados no IMPL

**Docs:**
- [ ] Links locais resolvem (`validate-templates`)
- [ ] Sync rule se contrato de acesso mudou

**Gates cognitivos:**
- [ ] Cada etapa crítica aponta para `disciplines/KAHNEMAN-DISCIPLINES.md`
- [ ] Cada etapa crítica registra pergunta obrigatória, evidência mínima e abort
      trigger
- [ ] Aceite por EFEITO, não por probe de existência (#13)

## Decisões e trade-offs

- **DT-1: serial como PRIMARY, vsock REJECT.** vsock troca o transporte mas o
  ator (sshd/socat/agente) fica no userspace do guest — morre com o disco/RAM, o
  failure mode real. O serial tem o getty respawnado pelo init (kernel) e o pipe
  servido pelo VMWP (host). Counterfactual (#2): se em 3 incidentes de starvation
  o login PAM serial pendurar >60s, o serial NÃO entrega OOB → rebaixa e o
  recovery passa a ser o power-cycle host-side (`Invoke-GuestUnreachableForcedReboot`).
- **DT-2: pipe path cristalizado Day-0 (`\\.\pipe\civm-console`).** `Set-VMComPort`
  exige VM Off; mudar depois custa um Stop/Start-VM. Fixar no Day-0 evita ter de
  reconfigurar num guest wedged (justo o que se quer evitar).
- **DT-3: login PAM normal, sem `--autologin root`.** Um root shell sempre-ligado
  atrás do pipe viola a default fail-safe (#15) e o invariante de segurança. A
  senha fica no password manager; o pipe NT já é restrito a Administrators/SYSTEM
  (mesmo trust boundary do `sudo.exe`).
- **DT-4: erros tipados, não exit-code do texto.** O pipe é byte-stream cru sem
  framing; em vez de inferir sucesso do output (anti-padrão de classificação
  frágil que o repo combate, #14), o `Outcome` é decidido por **qual estágio
  alcançou + qual timeout estourou** — determinístico e testável em Go.
- **DT-5: artefato no host, gate de stale.** O `.ps1` vive em `C:\civm-deploy`;
  o `serial-recover` recusa rodar se o hash divergir do repo (#106). O curador
  não pode ser uma cópia velha.

## Links Kahneman (passos críticos)

- RF-1: **#16** (register idempotente, `Set-VMComPort` no-op no 2º run).
- RF-2, RF-4, RF-5, DT-1: **#15** (fail-safe default + curador independente fora
  do guest; sem `--autologin`; timeouts tipados; sem stale).
- RF-3, DT-4: **#13** (existência ≠ função — aceite por efeito sob starvation,
  par positivo+negativo) e **#14** (sem retry mascarando; erro determinístico).
