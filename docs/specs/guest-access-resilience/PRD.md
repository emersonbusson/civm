---
slug: guest-access-resilience
title: Canal de acesso ao guest fora-de-banda — console serial via named pipe Hyper-V quando o sshd morre
milestone: —
issues: []
---
# PRD — Acesso ao guest resiliente: atravessar o hypervisor quando o sshd está em starvation

> SSDV3 PASSO 1. Slug: `guest-access-resilience`. Repo: `civm`.
> Complementa `docs/specs/host-volume-reclaim-liveness/` (mantém o reclaim vivo)
> com a garantia de **acesso**: quando o sshd do guest morre sob carga, ainda
> existe uma rota de manutenção/recovery que não depende da rede nem do sshd.

## Resumo

- **O que é.** Um canal de acesso ao guest `gha-ubuntu-2404` que atravessa o
  **hypervisor** a partir do host EMEDEV (sempre vivo), em vez de atravessar a
  pilha de rede + sshd do guest (que morrem sob carga). Mecanismo: um **console
  serial COM** exposto pelo Hyper-V como **named pipe** no host
  (`\\.\pipe\civm-console`), com um **getty** (login PAM normal) no `ttyS0` do
  guest. O cliente do pipe roda do host via `sudo.exe` não-interativo — o mesmo
  vetor já provado para `Get-VM`/`Stop-VM`/`Optimize-VHD`.
- **Por que existe.** Hoje **todo** canal host→guest do `civm` é SSH:
  `civm-host-metrics.ps1` (`Get-GuestFreeBytes`/`Send-MetricsToGuest`) e
  `civm-vhdx-autoreclaim.ps1` (`Wait-GuestIdle`, `fstrim`, guest-prune) atravessam
  `ssh emdev@gha-ubuntu-2404`. Quando o sshd entra em starvation sob jobs
  testcontainers concorrentes, todo esse caminho dá `Connection timed out` — e a
  manutenção/diagnóstico fica sem rota. Foi o caso que travou a sessão de
  2026-06-15: não foi possível inspecionar o `disk-watchdog` nem limpar cache
  porque o único canal de exec (SSH) estava morto.
- **Problema que resolve.** Dá um degrau de **acesso out-of-band (OOB)**: rodar
  `civmctl disk-watchdog`/`cleanup`/`journalctl` no guest semi-vivo **sem** passar
  pelo sshd. Não tenta fazer caber um working set maior que o disco (isso é
  hardware) nem cura starvation por si — é o **transporte** que sobrevive ao
  recurso morto, fechando o gap da disciplina #15 (o curador não morre com o
  recurso que cura).
- **Valor operacional.** O reclaim e seus watchdogs continuam existindo; o que
  faltava era a mão humana/automação **entrar** quando o SSH está morto. O
  console serial entrega isso no nível do kernel (init/PID 1 respawna o getty),
  então existe quando o userspace de rede já não responde.

## Contexto técnico

### Topologia real (Confirmado no codebase + host)

- Host **EMEDEV** (Windows 11, Hyper-V de cliente). Guest **gha-ubuntu-2404**
  (Ubuntu 24.04, user `emdev`) = runner self-hosted do CI (2 runners:
  um por-repo + um por-org).
- Este ambiente **WSL2 "emdev"** roda no MESMO host Windows e tem **`sudo.exe`
  não-interativo** (admin, UAC off) — já usado para `Get-VM`/`Stop-VM`/`Set-ScheduledTask`/`Optimize-VHD`. `/mnt/v` e `/mnt/c` montados.
- `ssh emdev@gha-ubuntu-2404` alcança o guest **quando o sshd responde**. Sob
  carga pesada (testcontainers concorrentes) o sshd entra em starvation →
  `Connection timed out` → toda manutenção SSH-gated trava.

### Peças `civm` existentes (Confirmado no codebase)

| Peça | Papel | Onde |
| --- | --- | --- |
| `civm-host-metrics.ps1` + Scheduled Task SYSTEM | escreve `V:\civm-host-metrics.json` a cada 10 min, entrega cópia ao guest **via SSH/scp**; `delivery_status=failed` quando o guest df por SSH falha (`DT-v2-5`) | `deploy/windows/`, `internal/hostdisk` |
| `civm-vhdx-autoreclaim.ps1` + Scheduled Task SYSTEM | reclaim host-side; `Invoke-GuestUnreachableForcedReboot` (streak `GuestUnreachableLimit=3`, cooldown `ForcedRebootCooldownHours=6.0`) é o **único** canal através-do-hypervisor hoje: `Stop-VM -TurnOff` + `Start-VM` | `deploy/windows/` |
| `civm-vhdx-optimize-watchdog` | detector de fantasma + religa a VM Off (gated por `Test-LockHeld`) | `register-civm-vhdx-optimize.ps1` |
| `civmctl` (guest) | `disk-watchdog`, `cleanup`, `idle-check`, `host-disk`, `disk-doctor` | `/usr/local/bin/civmctl`, `cmd/civmctl/` |
| chave dedicada host→guest | `C:\ProgramData\civm\ssh\id_ed25519` — **local host state, never repo state** | host |

### Diagnóstico de causa (Confirmado no host)

A starvation do sshd é **disk-wedge + memory/CPU pressure no guest**: jobs
testcontainers (postgres/redis/minio/clamav concorrentes) saturam IO/RAM/CPU; o
kernel sob memory pressure atrasa/mata o sshd (que precisa de fork+pty+escrita de
utmp). A **rede nunca foi o gargalo** — o userspace do guest é. Logo:

- Trocar o **transporte** de rede→VMBus (vsock) **não cura** nada se o **ator**
  continuar no userspace do guest (vsock daemon, runner Worker, tunnel) — morre
  com o recurso (#15).
- O serial console também é só **transporte**, mas seu ator do lado do guest é o
  **getty respawnado pelo init (PID 1, prioridade de kernel)**, não um daemon de
  rede — e o servidor do pipe é o **VMWP no host**, sempre vivo. É o único tier
  avaliado em que nenhuma ponta do caminho de acesso é um processo de rede do
  guest.

### Confirmado na documentação oficial

- Hyper-V `Set-VMComPort -VMName <vm> -Number 1 -Path \\.\pipe\<name>` expõe um
  **named pipe server** servido pelo VMWP enquanto a VM estiver ligada. Exige a
  VM **Off** para alterar o path (config de firmware da VM; não é
  hot-reconfigurável).
- Linux: `console=ttyS0,115200 console=tty1` no cmdline +
  `serial-getty@ttyS0.service` (agetty, login PAM normal). O `serial-getty` é
  respawnado pelo systemd/init independentemente de rede/sshd.
- `System.IO.Pipes.NamedPipeClientStream('.', '<name>', InOut)` abre o pipe do
  lado Windows. WSL2 **não** lê o named pipe Win32 nativamente (objeto do NT
  Object Manager, não arquivo em `/mnt/c`) — por isso o canal é sempre
  `sudo.exe powershell -NonInteractive -File <client.ps1>`.

### Sendo proposto

1. **(PRIMARY) Console serial OOB** — `Set-VMComPort` (host, one-time, idempotente
   no molde `register-*.ps1`) + `serial-getty@ttyS0` no guest (cloud-init/imagem) +
   um **cliente de pipe** self-contained no host/WSL2 (`civm-serial-console.ps1`)
   acionado por `sudo.exe`. Subcomando `civmctl serial-recover` (host-side) que
   abre o pipe, faz login PAM e roda um comando, com **timeout finito tipado**.
2. **(SECONDARY preventivo)** isolamento cgroup/slice + cap de concorrência +
   hardening sshd — **reduz a frequência** com que o OOB precisa ser acionado.
   Metade já existe (`internal/admit`, `systemd-run -p MemoryMax`).
3. **(SECONDARY last-resort)** o power-cycle host-side **já shipado**
   (`Invoke-GuestUnreachableForcedReboot`) permanece como martelo quando até o
   login serial pendura (guest realmente travado, não só sshd faminto).

## Opção recomendada

**PRIMARY = Tier 1, console serial OOB via named pipe Hyper-V.** É o único tier
que atravessa o hypervisor sem tocar rede nem sshd: o `serial-getty` é
respawnado pelo init (PID 1, prioridade de kernel), e o pipe é servido pelo VMWP
no host, sempre vivo. Reaproveita 100% a topologia "WSL2 atravessa o hypervisor
via `sudo.exe`" já provada (Stop-VM/Optimize-VHD) e o molde idempotente
`register-*.ps1`.

**Motivo da escolha.** O repo PROVA que esse é o gap real: tanto
`civm-host-metrics.ps1` quanto `civm-vhdx-autoreclaim.ps1` degradam exatamente
sob a starvation de sshd que travou a manutenção (o `delivery_status=failed` é o
sintoma); nenhum mecanismo OOB de **exec** existe — só o martelo
`Stop-VM`/`Start-VM`, que destrói estado e jobs em voo. O serial é o degrau
intermediário entre "SSH vivo" e "puxar o cabo".

**Alternativas descartadas:**

- **vsock (sshd-over-vsock / Hyper-V Sockets)** — REJEITADO. Troca o transporte
  (rede→VMBus) mas mantém o **ator** no userspace do guest (sshd/socat/agente Go),
  a mesma classe de processo que morre quando `/` enche / a RAM acaba. Não
  sobrevive ao failure mode real (disco/memória, não rede). Custo de cliente
  host ALTO (registro de GUID + socket `AF_HYPERV` custom, que o WSL2 não fala).
- **Tier 2 "fino" (host mata jobs / reinicia sshd via canal Tier 1)** —
  REJEITADO como escopado. As ações finas exigem **exec dentro do guest**, que
  só o canal serial entrega — então dependem deste PRD, não o substituem. A ação
  coarse (power-cycle) **já existe** (`Invoke-GuestUnreachableForcedReboot`);
  recriá-la é redundância.
- **Tier 3 (tunnel outbound Tailscale/WG/Cloudflare/SSM + workflow_dispatch)** —
  DEFERIDO a redundância-secundária só para o caso **guest vivo + rede/NAT
  bloqueada**. Ambos os sub-mecanismos vivem no guest e morrem com ele sob
  starvation; o Tailscale conhecido está DOWN; tratar "tunnel up" como acesso é o
  falso-verde de #13.

**Trade-offs aceitos:**

- O serial é **transporte, não cura**: sob OOM/CPU-thrash severo o próprio login
  PAM pode degradar. Ele eleva muito a probabilidade de entrar (tira o overhead
  de crypto+rede do sshd) mas **não a garante** — declarado como WYSIATI (#1) e
  validado por efeito sob carga real (#13), com counterfactual numérico de
  rollback (login PAM serial >60s em 3 incidentes → cai para o power-cycle).
- Login serial é uma **superfície de login local permanente** no host — mitigado
  por login PAM normal (sem `--autologin root`), senha fora do repo, ACL default
  do named pipe NT (só Administrators/SYSTEM) e logging de abertura de sessão.

## Requisitos funcionais

- **RF-1 — Canal serial OOB existe e é servido pelo host.** O guest expõe um
  console serial em `ttyS0` com `serial-getty@ttyS0` ativo; o host expõe
  `\\.\pipe\civm-console` via `Set-VMComPort`. O registro é **idempotente**
  (re-rodar = mesmos bytes, molde `register-*.ps1`).
  - **Critério de aceite:** `(Get-VMComPort -VMName gha-ubuntu-2404 -Number 1).Path
    -eq '\\.\pipe\civm-console'`; no guest `systemctl is-active serial-getty@ttyS0`
    = `active`; e o **teste por efeito** do RF-3 passa (não basta existir).
  - **Concorrência:** `Set-VMComPort` exige VM Off; é setup one-time na
    imagem/registração, fora do caminho quente de reclaim.

- **RF-2 — Acesso sem `--autologin root`.** A sessão serial entra por **login PAM
  normal** (`emdev` + senha; `sudo` dentro da sessão para privilégio, igual ao
  resto). É PROIBIDO `serial-getty@ttyS0 --autologin root`.
  - **Critério de aceite:** o getty apresenta `login:`; `id` após autenticar
    retorna `uid=...(emdev)`; nenhum unit/override contém `--autologin`.
  - **Segurança:** a senha do `emdev` é secret do operador — nunca no repo
    (invariante absoluta), fica no password manager.

- **RF-3 — Acesso provado por EFEITO, sob starvation, não por probe.** A partir do
  host/WSL2, abrir o pipe, autenticar e rodar um comando que **muta estado
  observável** do guest **com o sshd de fato morto** sob carga testcontainers
  concorrente reproduzindo o wedge.
  - **Critério de aceite:** `civmctl serial-recover --cmd 'civmctl cleanup --execute'`
    via pipe → o `df` do guest sobe (efeito), **enquanto** `ssh emdev@gha-ubuntu-2404`
    no mesmo cenário dá timeout (par positivo+negativo, #13). Probe de existência
    (`Set-VMComPort` retornou 0, pipe presente, getty `enabled`) **não** conta.

- **RF-4 — Fail-fast determinístico e tipado.** O cliente de pipe tem **timeout
  finito** em `Connect()` e na espera do prompt; erros são **barulhentos e
  tipados**: `vm_off_pipe_absent` ≠ `pam_login_hung` ≠ `shell_alive_cmd_hung`.
  Nunca pendura silencioso.
  - **Critério de aceite:** com a VM Off, `serial-recover` retorna
    `vm_off_pipe_absent` exit ≠ 0 em ≤ `ConnectTimeoutSeconds`, não bloqueia;
    com login PAM que não progride em `LoginTimeoutSeconds`, retorna
    `pam_login_hung` exit ≠ 0.

- **RF-5 — O curador não vive dentro do guest moribundo.** O artefato de recovery
  (`civm-serial-console.ps1` + `civmctl serial-recover` host-side) é
  self-contained no **host/WSL2**, nunca um `ssh guest civmctl ...`. O `.ps1`
  corrente DEVE ser o que está de fato instalado em `C:\civm-deploy`, não a cópia
  stale do repo (postmortem #106).
  - **Critério de aceite:** o caminho de recovery não invoca SSH em nenhum passo;
    o deploy verifica que o hash do `.ps1` instalado == o do repo.

## Requisitos não-funcionais

- **RNF-1 — Sem novo secret no repo.** O cliente `.ps1` é código público; a
  credencial é a senha do PAM (password manager). O named pipe `\\.\pipe\civm-console`
  é objeto NT com ACL default restrita a Administrators/SYSTEM — mesmo trust
  boundary do `sudo.exe`/UAC-off já presente. Nenhuma porta exposta à rede nem
  ao guest.

- **RNF-2 — Observabilidade.** Cada decisão do `serial-recover` emite evento no
  `V:\civm-hyperv-maintenance.log` (família `serial_recover_*`) com o erro tipado.
  Abertura de sessão serial é registrada no audit (`wtmp` já grava `ttyS0`);
  paridade com o logging de acesso SSH.

- **RNF-3 — Resiliência (worst-case, #5).** VM Off (pipe não servido), VMWP
  travado, login PAM pendurado sob 90% RAM, shell vivo mas comando pendurado —
  cada um com erro tipado e timeout; o último recurso é o power-cycle host-side
  já existente.

- **RNF-4 — Sessão sem keepalive não vira login fantasma.** Se o WSL2 cair no meio,
  o shell de recovery tem `TMOUT` (logout idle) para não deixar uma sessão
  logada exposta no console (o serial não tem o teardown limpo do sshd).

- **RNF-5 — Performance.** O caminho OOB é manual/eventual (recovery), não no hot
  path. `Set-VMComPort` é one-time. O cliente de pipe é byte-stream cru — para
  recovery interativo é suficiente; para automação não-supervisionada o output
  é frágil (sem exit-code estruturado), declarado em Fora de escopo.

## Fluxos

### Happy path (recovery sob sshd morto)

1. **Host/WSL2:** operador/automação detecta `delivery_status=failed` por ≥K
   ciclos em `V:\civm-host-metrics.json` (sshd morto), ou um SSH manual deu
   timeout.
2. **Host/WSL2:** `sudo.exe powershell -NonInteractive -File C:\civm-deploy\civm-serial-console.ps1`
   (ou `civmctl serial-recover --cmd '...'`) abre
   `NamedPipeClientStream('.', 'civm-console', InOut)` com `Connect(ConnectTimeoutSeconds)`.
3. **Hyper-V/VMWP (host):** serve o pipe; bytes fluem para o `ttyS0` do guest.
4. **Guest (init/PID 1):** o `serial-getty@ttyS0` (respawnado pelo kernel)
   apresenta `login:`. O cliente envia `emdev` + senha (login PAM).
5. **Guest:** shell de recovery (`TMOUT` setado). O cliente envia
   `civmctl cleanup --execute` (ou `disk-watchdog`/`journalctl`) e espera o prompt
   com `LoginTimeoutSeconds`/`CmdTimeoutSeconds`.
6. **Efeito:** o `df` do guest sobe / o estado muta — provado por leitura
   subsequente, não por "comando enviado".

### Fluxos alternativos

- **Guest vivo, rede/NAT bloqueada** (não é o wedge): o Tier 3 deferido (tunnel
  outbound) ajudaria; fora de escopo deste PRD.
- **sshd vivo:** o caminho normal (SSH) segue sendo o primário para manutenção
  rotineira; o serial é só para quando o SSH falha.

### Fluxos de erro

| Condição | Resultado / erro tipado | Log | Impacto |
| --- | --- | --- | --- |
| VM Off / pipe não servido | `vm_off_pipe_absent`, exit ≠ 0 em ≤ `ConnectTimeoutSeconds` | `serial_recover_pipe_absent` WARN | nenhuma mutação; sobe para power-cycle se streak |
| VMWP travado / Connect pendura | `pipe_connect_timeout`, exit ≠ 0 | `serial_recover_connect_timeout` ERROR | nenhuma mutação |
| login PAM não progride (RAM thrash) | `pam_login_hung`, exit ≠ 0 em `LoginTimeoutSeconds` | `serial_recover_pam_hung` ERROR | guest realmente starvado → power-cycle |
| shell vivo, comando pendura | `shell_alive_cmd_hung`, exit ≠ 0 em `CmdTimeoutSeconds` | `serial_recover_cmd_hung` ERROR | sessão fechada; `TMOUT` garante logout |
| `.ps1` stale (hash ≠ repo) | recusa rodar, `stale_artifact` | `serial_recover_stale` CRITICAL | deploy corrige antes de usar |

## Modelo de dados

> **N/A — sem banco.** Estado em arquivos efêmeros.

**Estado novo (host):**

```text
V:\civm-serial-recover-last.json (host):
  { "last_attempt_utc": "<iso>", "outcome": "ok|vm_off_pipe_absent|pipe_connect_timeout|
    pam_login_hung|shell_alive_cmd_hung|stale_artifact", "cmd": "<sanitized>",
    "duration_ms": N }
  Escrita: os.WriteFile / Set-Content atômico (temp + Move-Item -Force).
```

**Alterações em estado/constantes existentes:** ver §API / Interfaces (bloco
`const` em `internal/civm/civm.go`). Backfill = **N/A — Day-0** (estado efêmero).

## API / Interfaces

> **Sem endpoint HTTP.** Interfaces = CLI `civmctl` (host-side) + componente host
> (`.ps1` + `Set-VMComPort`) + guest unit (`serial-getty@ttyS0`).

**Subcomando `civmctl serial-recover` (host-side, roda no WSL2/host):**

| Campo | Valor |
| --- | --- |
| Subcomando | `serial-recover --cmd '<remote>' [--interactive]` |
| Read-only? | não (executa `--cmd` no guest); `--interactive` abre sessão |
| Exit codes | `0` ok / `1` erro tipado / `2` stale artifact / `64` flag inválida |
| Privilégio | invoca `sudo.exe powershell -File civm-serial-console.ps1` (admin host) |
| Idempotência | sim — re-rodar `--cmd 'civmctl cleanup --execute'` é seguro (cleanup é idempotente por efeito) |

**Componente host (Windows, `deploy/windows/`):**

| Artefato | Função |
| --- | --- |
| `civm-serial-console.ps1` | cliente do named pipe: `Connect(timeout)`, login PAM, envia `--cmd`, espera prompt com timeout, erros tipados |
| `register-civm-serial-console.ps1` | one-time: `Set-VMComPort -Number 1 -Path \\.\pipe\civm-console` com a VM Off (idempotente); instala o `.ps1` em `C:\civm-deploy` |

**Guest (Ubuntu 24.04, imagem/cloud-init):**

| Artefato | Função |
| --- | --- |
| kernel cmdline | `console=ttyS0,115200 console=tty1` (mantém tty1) |
| `serial-getty@ttyS0.service` | `systemctl enable serial-getty@ttyS0` — agetty, **login PAM normal**, nunca `--autologin` |
| drop-in TMOUT (RNF-4) | `/etc/profile.d/civm-serial-tmout.sh` exporta `TMOUT` no shell de recovery serial |

**Erros / abort triggers:** ver §Fluxos de erro.

**Impacto em contrato / docs:** subcomando novo no `printHelp`
(`cmd/civmctl/main.go`); constantes novas em `internal/civm/civm.go`; runbook
novo (`runbooks/RUNBOOK-GUEST-SERIAL-RECOVERY.md`); sync rule
(README ≡ AGENTS ≡ CODEX ≡ rules) se o contrato de acesso mudar.

## Dependências e riscos

- **Pré-requisitos:** `sudo.exe` não-interativo no WSL2 (já existe); a VM precisa
  ser parada **uma vez** para `Set-VMComPort` (janela one-time).
- **Riscos técnicos + mitigação:**
  - *Login serial pendura sob OOM real (#13/#1).* Validar por efeito sob carga;
    counterfactual numérico de rollback (>60s no PAM em 3 incidentes → power-cycle).
  - *`Set-VMComPort` exige VM Off, não é hot-reconfigurável.* Cristalizar o pipe
    path no Day-0 (`\\.\pipe\civm-console`); mudar depois exige Stop/Start-VM.
  - *Named pipe é byte-stream cru sem framing.* Parse de output é frágil
    (prompts ANSI, eco) — ok para recovery manual, declarado fora de escopo para
    automação não-supervisionada.
  - *`.ps1` stale em `C:\civm-deploy` (#106).* Gate de hash no deploy (RF-5).
- **Impacto em componentes existentes:** nenhum no hot path do reclaim. O serial
  é canal paralelo; `civm-host-metrics.ps1`/`autoreclaim` seguem por SSH.
- **Breaking changes:** nenhuma (canal aditivo).
- **Rollout:** janela one-time para `Set-VMComPort` (VM Off); depois o pipe é
  servido continuamente. O subcomando e o `.ps1` são deploy normal.
- **Rollback:** app (subcomando vira no-op) / host (`Set-VMComPort -Path ''` +
  `schtasks`/arquivo removido, em janela) / estado (N/A — efêmero). É PROIBIDO
  `--autologin root` em qualquer caminho de rollback.
- **Hipóteses que exigem disciplina no SPEC:** #13 (existência ≠ função, teste
  por efeito), #15 (fail-fast + curador fora do guest), #16 (registro idempotente).

## Estratégia de implementação

1. **Slice 0 (sem código):** documentar o estado atual do `ttyS0`/COM port da VM
   (baseline); medir o login serial com a VM **saudável** (existência) — NÃO conta
   como aceite, só baseline.
2. **Slice 1 (guest):** kernel cmdline + `serial-getty@ttyS0` na imagem/cloud-init
   (sem `--autologin`); drop-in TMOUT.
3. **Slice 2 (host):** `register-civm-serial-console.ps1` (`Set-VMComPort`,
   one-time, VM Off) + `civm-serial-console.ps1` (cliente de pipe, timeouts
   tipados); molde `register-*.ps1`.
4. **Slice 3 (Go):** `internal/serialrecover` (decisões puras: classificação de
   erro tipado, gate de timeout) + `civmctl serial-recover` + `printHelp`.
5. **Slice 4 (validação por efeito, #13):** sob carga testcontainers concorrente
   real (sshd morto), provar login PAM + `cleanup --execute` muta o `df`; par
   negativo (SSH dá timeout no mesmo cenário).
6. **Slice 5 (docs vivos):** runbook + sync rule.

## Fora de escopo

- **Cura da starvation** (cgroup/slice/cap de concorrência) — é o tier
  SECONDARY preventivo, não o objetivo deste PRD (acesso, não prevenção). Metade
  já existe (`internal/admit`). Pode virar PRD próprio.
- **Automação não-supervisionada sobre o serial** (parse de output sem framing) —
  frágil sem exit-code estruturado; o serial é para recovery manual/semi-auto.
- **vsock / Hyper-V Sockets** — rejeitado (ator no userspace do guest).
- **Tunnel outbound / workflow_dispatch (Tier 3)** — deferido a redundância só
  para guest-vivo + rede-bloqueada.
- **Fazer caber working set > disco** — hardware (F3 do reclaim-liveness).

## Disciplinas Kahneman

- **#13 (existência ≠ função):** maior risco. `Set-VMComPort` verde + pipe
  presente + getty `enabled` é a armadilha clássica (#59/safedelete). Aceite por
  EFEITO sob starvation real, com par positivo+negativo.
- **#15 (fail-safe + curador independente):** o curador (cliente de pipe) vive no
  host/WSL2, nunca dentro do guest moribundo; timeouts finitos e tipados; sem
  `--autologin root`. Último recurso = power-cycle host-side já existente.
- **#16 (idempotência):** `register-civm-serial-console.ps1` re-rodável sem
  duplicar (mesmos bytes, molde `register-*.ps1`); `--cmd` idempotente por efeito.
