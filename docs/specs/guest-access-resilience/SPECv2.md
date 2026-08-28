# SPECv2 — Acesso ao guest resiliente (console serial OOB)

> Versão melhorada após auditoria do Passo 2.5 (red-team).
> Baseline preservado: `SPEC.md`.
> Motivo: o `SPEC.md` vende um aceite por efeito mas deixa 4 brechas onde o
> verde codificaria a premissa errada (#13), o curador poderia degradar com o
> recurso (#15) ou o gate de stale ficar implícito (#16). Esta versão fecha o
> escopo do IMPL e torna cada brecha um abort trigger objetivo.
> Onde houver conflito, **esta versão prevalece**.

## Auditoria 2.5 — achados (ordenados por severidade)

### B1 (NO-GO → resolvido) — RF-3: "login serial entra sob starvation" é a premissa não-medida (#13/#1)

**Seção afetada:** `SPEC.md` §RF-3, DT-1.

O serial console é **transporte, não cura**. O getty é respawnado pelo init
(kernel), mas o **login PAM** (agetty → `/bin/login` → PAM) é userspace do guest:
precisa de fork, escrita de utmp/wtmp e schedule de CPU — os mesmos recursos
escassos que mataram o sshd. O `SPEC.md` afirma "o serial entra" sem medir o
login sob 90% de pressão real. Um aceite que mede o login com a VM **saudável**
(Slice 0) é exatamente o falso-verde #59: prova a existência do canal, não a
função sob o failure mode.

→ **Resolução (vinculante):** o aceite RF-3 é **dois números medidos sob o wedge
real**, não um boolean:

1. `serial_login_latency_ms` sob carga testcontainers concorrente que derrube o
   sshd (≥3 incidentes reproduzidos), com o **par negativo** registrado no mesmo
   instante (`ssh ... true` = `Connection timed out`).
2. **Counterfactual numérico de rollback (#2):** se em 3 dos incidentes o login
   PAM serial pendurar **>`DefaultSerialLoginTimeoutSec` (60s)**, o tier NÃO
   entrega OOB → o problema é reclassificado como "starvation do guest" (não
   "transporte") e o recovery PRIMARY rebaixa para o power-cycle host-side
   (`Invoke-GuestUnreachableForcedReboot`, já shipado). O serial vira secundário.

WYSIATI (#1) declarado no IMPL: "sem medir o login serial sob 90% RAM real,
estimo sucesso de entrada ALTO (tira o overhead de crypto+rede do sshd) mas NÃO
garantido". O verde só conta com os dois números colados.

### B2 (NO-GO → resolvido) — o cliente de pipe pode "parecer vivo" e pendurar no 1º comando que aloca (#15)

**Seção afetada:** `SPEC.md` §RF-4 (esqueleto `civm-serial-console.ps1`).

Sob OOM o getty pode entregar um prompt que **trava no primeiro comando que faz
fork/malloc/disk** (`civmctl cleanup` aloca). Sem timeout no estágio `cmd`, isso
é pior que falhar rápido: parece vivo (`OutcomeOK` falso) e mascara a falha — o
oposto de fail-fast (#15). O `ClassifyAttempt` do `SPEC.md` cobre o caso, mas o
esqueleto `.ps1` não exige o timeout no estágio `cmd` de forma imposta por lint.

→ **Resolução (vinculante):**
- O `civm-serial-console.ps1` DEVE ter deadline finito em **todos** os três
  estágios (`connect`/`login`/`cmd`), cada um mapeando para o `Outcome` tipado.
  `OutcomeOK` só é emitido quando o prompt **pós-comando** é lido dentro de
  `DefaultSerialCmdTimeoutSec` — "prompt visto", não "comando enviado".
- O lint `ps1_safety_test.go` ganha uma asserção: o `civm-serial-console.ps1`
  contém `Connect(` com argumento numérico de timeout **e** uma leitura com
  deadline no estágio `cmd` (regex de `ReadTimeout`/loop com `[datetime]` deadline).
  Ausência → falha o gate (mesmo nível do clamp Int32 #17).
- **Validação por efeito é por leitura subsequente, não por "comando voltou":**
  após o `--cmd`, o aceite lê o `df` (pelo próprio serial) e compara; um shell
  pendurado NÃO produz o `df` posterior → `shell_alive_cmd_hung`, nunca `ok`.

### B3 (NO-GO → resolvido) — gate de stale do `.ps1` ficou descrito como "comparar hash" sem fonte canônica (#16/#106)

**Seção afetada:** `SPEC.md` §RF-5.

O `SPEC.md` diz "compara o SHA-256 do instalado com o do repo (`go:embed` hash ou
parâmetro)" — ambíguo. Sem fonte canônica do hash, o gate de stale pode comparar
contra a cópia errada e repetir o #106 (curador stale rodando em `C:\civm-deploy`).

→ **Resolução (vinculante):** o hash esperado é **embarcado no binário `civmctl`
via `go:embed`** do `deploy/windows/civm-serial-console.ps1` no momento do build
(`//go:embed civm-serial-console.ps1` num pacote `internal/serialrecover/embed.go`,
SHA-256 computado em init). O `register-civm-serial-console.ps1` copia o `.ps1`
para `C:\civm-deploy` e o `serial-recover` compara o hash do arquivo instalado
com o hash embarcado **no mesmo binário** que está rodando. Assim o civmctl e o
`.ps1` que ele dispara são **a mesma versão por construção** — não há janela de
divergência entre "binário novo, `.ps1` velho". Idempotência (#16): re-deploy
reconcilia (copia → hash bate → `serial_recover_artifact_ok`).

### B4 (aceito, sem mudança) — segredo da senha no env, nunca no repo nem no log

**Seção afetada:** `SPEC.md` §RF-2, Observabilidade.

A senha do PAM vem de `CIVM_SERIAL_PASS` (env do processo `sudo.exe`, setado pelo
operador/automação a partir do password manager). NUNCA é hardcoded, persistida
em `civm-serial-recover-last.json`, nem logada. O `cmd` é sanitizado antes de ir
ao log. Mantido como está — só registrado aqui como decisão explícita de
segurança.

### B5 (aceito) — `Set-VMComPort` exige VM Off; janela one-time, não hot path

O registro só roda em janela (VM Off aceitável); fora do caminho quente de
reclaim. Cristalizar `\\.\pipe\civm-console` no Day-0 (DT-2) evita reconfigurar
num guest wedged. Sem mudança.

## Escopo ativo (pós-auditoria)

**Entra no IMPL:** RF-1, RF-2, RF-3 (com aceite de dois números medidos),
RF-4 (timeout nos 3 estágios + lint), RF-5 (gate de stale via `go:embed`).

**Fica explicitamente fora agora:**
- **Automação não-supervisionada sobre o serial** — o byte-stream cru sem framing
  reintroduz a classificação frágil de output (#14); o serial é recovery
  manual/semi-auto. Watchdog que dispare `serial-recover` automaticamente em
  `delivery_status=failed` por K ciclos é um follow-up, NÃO este IMPL.
- **Tier SECONDARY preventivo (cgroup/slice/cap)** — reduz a frequência do OOB;
  metade já existe (`internal/admit`); PRD próprio.
- **vsock** — REJECT (ator no userspace do guest).
- **Tier 3 (tunnel outbound)** — DEFER (só guest-vivo + rede-bloqueada).

**Dependências assumidas prontas:** `sudo.exe` não-interativo no WSL2;
`Invoke-GuestUnreachableForcedReboot` (power-cycle last-resort) já shipado;
molde `register-*.ps1`; `internal/hostdisk/ps1_safety_test.go` (lint host).

## Matriz de rastreabilidade PRD → SPECv2

| PRD | Implementação |
| --- | --- |
| RF-1 | `register-civm-serial-console.ps1` (`Set-VMComPort` idempotente) + `serial-getty@ttyS0` |
| RF-2 | sem `--autologin` + lint `ps1_safety_test.go`; senha no env (B4) |
| RF-3 | aceite de 2 números medidos sob wedge real + par negativo SSH (B1) |
| RF-4 | `ClassifyAttempt` (Go) + timeout nos 3 estágios + lint (B2) |
| RF-5 | gate de stale via `go:embed` hash no civmctl (B3) |
| RNF-1 | senha no env, pipe NT ACL-restrito, sem secret no repo |
| RNF-2 | família `serial_recover_*` + `wtmp` |
| RNF-3 | worst-case tipado + power-cycle last-resort (counterfactual B1) |
| RNF-4 | `TMOUT` no shell de recovery |

## Fronteira de atomicidade e política de rollback

- **Atômico nesta issue:** cada `Set-Content`/`os.WriteFile` de estado
  (`civm-serial-recover-last.json`); cada `Set-VMComPort` é uma operação Hyper-V
  única.
- **Fora da atomicidade:** o ciclo `Connect→login→cmd` (3 estágios, cada um com
  seu timeout); uma sessão pode terminar parcial (logou, comando pendurou →
  `shell_alive_cmd_hung`, `TMOUT` fecha). Estado parcial aceito: pipe configurado
  mas login falhando sob starvation → cai para power-cycle (B1).
- **Rollback de app:** subcomando `serial-recover` vira no-op (binário anterior).
- **Rollback de host:** `Set-VMComPort -VMName ... -Number 1 -Path ''` (em janela,
  VM Off) + remover `C:\civm-deploy\civm-serial-console.ps1`.
- **Rollback de estado:** N/A — Day-0, arquivos efêmeros.
- **PROIBIDO:** `--autologin root` em qualquer caminho; deixar a VM Off ao fim do
  register (sempre religa se estava Running). `forward-only`: a mudança de
  `serial-getty` na imagem é forward-only (re-imagear), mas o canal é aditivo —
  reverter é só remover o COM port.

## Mapa Kahneman por etapa crítica

| Etapa / ITEM | Disciplina | Link | Pergunta obrigatória | Evidência mínima | Abort trigger |
| --- | --- | --- | --- | --- | --- |
| RF-3 aceite (B1) | #13 + #1 + #2 | §13/§1/§2 | "O verde prova que entrei e mutei o guest sob sshd morto, ou só que o pipe abre?" | `df` antes/depois via serial + SSH timeout pareado, ≥3 incidentes; `serial_login_latency_ms` | login PAM serial >60s em 3 incidentes → rebaixa serial, PRIMARY vira power-cycle |
| RF-4 (B2) | #15 + #14 | §15/§14 | "Sob OOM, o cliente pendura parecendo vivo ou falha tipado?" | unit `ClassifyAttempt` 5 outcomes; lint exige `Connect(`+deadline `cmd`; `df` posterior prova efeito (não "comando voltou") | qualquer estágio sem deadline finito → no merge |
| RF-5 (B3) | #16 + #15 | §16/§15 | "O `.ps1` que roda é o corrente (mesma versão do binário) ou cópia velha?" | hash `go:embed` == hash instalado no log `serial_recover_artifact_ok` | hash diverge → `stale_artifact` exit 2 |
| RF-1 register (B5) | #16 | §16 | "Re-rodar o register duplica ou exige estado prévio?" | `Get-VMComPort` igual no 2º run; sem 2º Stop/Start quando já configurado | register exige VM Off num run já configurado → bug |
| RF-2 (B4) | #15 | §15 | "Há root shell sempre-ligado ou senha no repo/log?" | grep `--autologin`=0 em `deploy/**`; senha só em `CIVM_SERIAL_PASS`; ausente do JSON/log | `--autologin` no diff OU senha persistida → bloquear |

## Checklist de segurança (pré-implementação)

- [ ] Exec safety: `cmd/civmctl/serialrecover.go` usa `exec.CommandContext` sem
      shell; `civm-serial-console.ps1` sem `Invoke-Expression` de input externo
- [ ] Sem `--autologin` em `deploy/**` (lint); login PAM normal
- [ ] Senha só em `CIVM_SERIAL_PASS`; nunca hardcoded, persistida ou logada (B4)
- [ ] Pipe NT `\\.\pipe\civm-console` herda ACL default (Administrators/SYSTEM);
      não exposto à rede nem ao guest
- [ ] Fail-closed: VM Off / PAM travado / comando pendura → `Outcome` tipado +
      exit ≠ 0, nunca pendura (B2)
- [ ] Gate de stale: hash `go:embed` == instalado, senão `stale_artifact` (B3)
- [ ] Int32 clamp: nenhum `[math]::Max(0, …)` literal no `.ps1` novo (#17)
- [ ] `Connect(` sempre com timeout (lint)

## Arquivos a CRIAR (pós-auditoria)

| Arquivo | Mudança | RF |
| --- | --- | --- |
| `internal/serialrecover/classify.go` | `ClassifyAttempt` + `Outcome.ExitCode` puros | RF-4 |
| `internal/serialrecover/classify_test.go` | table-driven RED→GREEN (5 outcomes) | RF-4 |
| `internal/serialrecover/embed.go` | `//go:embed civm-serial-console.ps1` + SHA-256 do artefato (B3) | RF-5 |
| `deploy/windows/civm-serial-console.ps1` | cliente de pipe, timeout nos 3 estágios, gate stale | RF-3/4/5 |
| `deploy/windows/register-civm-serial-console.ps1` | `Set-VMComPort` one-time idempotente + copia o `.ps1` | RF-1/5 |
| `cmd/civmctl/serialrecover.go` | subcomando host-side `serial-recover` | RF-3/4/5 |
| `runbooks/RUNBOOK-GUEST-SERIAL-RECOVERY.md` | procedimento OOB + escalonamento power-cycle | RF-3 |

## Arquivos a MODIFICAR (pós-auditoria)

| Arquivo | Mudança | RF |
| --- | --- | --- |
| `internal/civm/civm.go` | bloco `const` de serial (path/timeouts/TMOUT) | RF-1/4 |
| `cmd/civmctl/main.go` | dispatch + `printHelp` `serial-recover` | RF-3 |
| `internal/hostdisk/ps1_safety_test.go` | lint: `--autologin`=0 em `deploy/**`; `Connect(`+deadline `cmd` (B2) | RF-2/4 |
| `disciplines/INVARIANTS.md` | novo gate "serial: sem `--autologin` + `Connect()` com timeout" | RF-2/4 |

## Validação (pós-auditoria)

1. **RF-4 unit (Go):** `ClassifyAttempt` os 5 outcomes + default conservador;
   `ExitCode` 0/1/2. RED→GREEN.
2. **RF-5 unit (Go):** hash `go:embed` bate com o `.ps1` do repo; arquivo
   instalado adulterado → `stale_artifact`.
3. **RF-1/RF-2 host (janela):** `Set-VMComPort` seta o pipe; 2º run no-op;
   `serial-getty@ttyS0` `active`; grep `--autologin`=0; lint host verde.
4. **RF-3 efeito (B1, sob wedge real):** ≥3 incidentes — par negativo (`ssh true`
   timeout) + par positivo (`serial-recover --cmd 'civmctl cleanup --execute'` →
   `df` sobe), com `serial_login_latency_ms` colado. Se 3/3 >60s no PAM → aborta
   o tier como PRIMARY (counterfactual #2).
5. **Regressão:** `go test ./... -race` (civm) verde; lint host; `govulncheck`.

## Rastreabilidade

RF-1 → register `Set-VMComPort` + `serial-getty`. RF-2 → sem `--autologin` +
lint + env. RF-3 → aceite de 2 números (B1). RF-4 → `ClassifyAttempt` + timeout
3 estágios + lint (B2). RF-5 → `go:embed` hash gate (B3).
SECONDARY preventivo / vsock / Tier 3 → fora de escopo, documentado no PRD.

## Decisão final de escopo do IMPL

**GO** para RF-1..RF-5 com as três resoluções vinculantes:

- **B1:** RF-3 só fecha com **dois números medidos sob o wedge real** (latência
  do login serial + SSH timeout pareado), ≥3 incidentes, e o **counterfactual de
  rollback** (PAM >60s em 3/3 → serial deixa de ser PRIMARY, vira power-cycle).
  Sem isso, é teatro de existência (#13).
- **B2:** timeout finito nos **três** estágios, imposto por lint; `OutcomeOK` só
  com prompt pós-comando lido; efeito provado por `df` subsequente, não por
  "comando voltou" (#15/#14).
- **B3:** hash do `.ps1` **`go:embed`** no mesmo binário civmctl — civmctl e o
  `.ps1` que ele dispara são a mesma versão por construção; sem janela de stale
  (#16/#106).

**PRIMARY confirmado:** console serial OOB via named pipe Hyper-V. É o único tier
cujo ator do lado do guest é o getty respawnado pelo init (kernel), com o pipe
servido pelo VMWP no host sempre-vivo — fecha o gap #15 que nenhum canal in-band
(SSH/vsock/tunnel) fecha. **SECONDARY:** cgroup/slice preventivo (reduz
frequência) + power-cycle host-side (último recurso, já shipado). **REJECT:**
vsock. **DEFER:** Tier 3 tunnel.

## Links Kahneman

- RF-3 (B1): **#13** (existência ≠ função; aceite por efeito) + **#1** (WYSIATI:
  declarar o login não-medido) + **#2** (counterfactual de rollback numérico).
- RF-4 (B2): **#15** (fail-fast tipado, não pendura parecendo vivo) + **#14**
  (sem mascarar; erro determinístico).
- RF-5 (B3): **#16** (idempotência + mesma versão por construção) + **#15**
  (curador não roda stale).
- RF-1 (B5): **#16** (register idempotente).
- RF-2 (B4): **#15** (fail-safe default: sem `--autologin`, sem secret no repo).
