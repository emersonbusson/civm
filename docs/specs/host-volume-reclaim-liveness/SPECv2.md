# SPECv2 — Host Volume Reclaim Liveness

> SSDV3 PASSO 2.5 deu **NO-GO** num ponto do `SPEC.md` (preservado como
> baseline). Esta versão é a ativa para o IMPL.

## Auditoria 2.5 — achados

- **B1 (NO-GO) — RF-2 (drain-on-pressure) interrompe o job em curso por
  benefício marginal.** Todo reclaim exige `Stop-VM` por ~13 min (Optimize-VHD).
  A pausa quebra a conexão runner↔GitHub → o job in-flight **falha** (não
  "termina graciosamente", como o `SPEC.md` otimistamente afirmava; ver
  `[[project_civm_footprint_fix]]`: "firing mid-run KILLS a long CI job"). Drenar
  ao WARN=25GB é prematuro. E o caso que RF-2 ataca — carga concorrente contínua
  sem janela idle — é, na prática, o **F3 (working set ativo > capacidade do
  disco = hardware)**: nenhum software compacta dados em uso ativo. Logo RF-2
  troca interrupção certa de job por ganho incerto.
  → **Resolução:** RF-2 sai do escopo. A starvation sob carga NORMAL (F2) é
  resolvida por RF-1+RF-3 (o reclaim fica vivo e efetivo, agindo nas janelas
  idle entre workflows); sob carga PATOLÓGICA é F3 (hardware, fora de escopo).

- **B2 (aceito) — falso-positivo de fantasma em instância recém-iniciada.**
  Mitigado: `IsPhantomReclaim` exige `!scriptProcessAlive` (um reclaim
  recém-disparado TEM processo) E `!reclaimLockHeld`. O process-scan é o guarda.
  Sem mudança.

- **B3 (aceito) — habilitar um watchdog deliberadamente Disabled.** O
  `civm-vhdx-optimize-watchdog` foi desabilitado operacionalmente (motivo não
  registrado em commit). Sua lógica é fail-safe (religa VM Off, gated por
  `Test-LockHeld`); habilitá-lo com o detector de fantasma é ganho líquido.
  Registrado aqui como decisão explícita (#16: a cura não pode ficar desligada).

## Escopo ativo (pós-auditoria)

Apenas **RF-1**, **RF-3** e o backstop `ExecutionTimeLimit=PT2H`. RF-2 e RF-4
ficam documentados no PRD como fora de escopo (F3 = hardware/concorrência).

## RF-1 — Detector de fantasma no watchdog (≤5 min) — INALTERADO do SPEC

`internal/hostdisk/phantom.go`:

```go
// IsPhantomReclaim: task marcada em execução, sem processo vivo do script e com
// o lock de reclaim órfão (não-segurado) => fantasma. Kahneman #13.
func IsPhantomReclaim(taskRunning, scriptProcessAlive, reclaimLockHeld bool) bool {
    return taskRunning && !scriptProcessAlive && !reclaimLockHeld
}
```

Orquestração no corpo do `civm-vhdx-optimize-watchdog` (here-string em
`register-civm-vhdx-optimize.ps1`), ANTES do `if state -eq Running { exit 0 }`
(o fantasma ocorre com a VM Running):

1. `taskRunning` = `(Get-ScheduledTask civm-vhdx-autoreclaim).State -eq 'Running'`.
2. `scriptProcessAlive` = `Get-CimInstance Win32_Process` com CommandLine
   `*civm-vhdx-autoreclaim.ps1*` (a query confiável; o `Get-Process|CommandLine`
   do watchdog atual é best-effort).
3. `reclaimLockHeld` = `Test-LockHeld 'V:\civm-autoreclaim.lock'` (helper já
   presente).
4. Fantasma → `Stop-ScheduledTask civm-vhdx-autoreclaim` +
   `Remove-Item V:\civm-autoreclaim.lock,V:\civm-reclaim.lock -Force` +
   log `reclaim_liveness_phantom_cleared` (Level WARN, com taskRunning/lock/proc).
   Não dispara o reclaim aqui (a cadência de 30 min ou o disk-watchdog o fazem);
   só REMOVE o bloqueio.

Habilitar a task `civm-vhdx-optimize-watchdog` (register: estado inicial
Enabled; host: `Enable-ScheduledTask`). #16: a cura não pode ficar desligada.

Backstop já aplicado: `ExecutionTimeLimit=PT2H` na `civm-vhdx-autoreclaim`
(`6ffbfee`). Detector ≤5 min é o primário; PT2H é o segundo nível.

## RF-3 — Prune do guest antes do Optimize — INALTERADO do SPEC

Em `civm-vhdx-autoreclaim.ps1`, ANTES do `fstrim`: SSH guest
`civmctl disk-watchdog --threshold-pct=0 --execute` (prune docker/cache/work).
Best-effort: log `reclaim_liveness_guest_prune {freed_gb}`; falha NÃO aborta
(fstrim/Optimize seguem — o `CompactVirtualDisk` compacta o que houver).
Confirmado ao vivo: sem prune, `skip_low_gap gap=2.4GB`; com prune (17.5GB
docker), gap ~28GB e Optimize procedeu. #13: o gap reclamável é a prova do efeito.

> **Nota de segurança (RNF-1):** o guest-prune roda só quando o reclaim JÁ vai
> agir (guest idle, dentro do fluxo normal pós-idle), então não interrompe job.

## Arquivos tocados (pós-auditoria)

| Arquivo | Mudança | RF |
| --- | --- | --- |
| `internal/hostdisk/phantom.go` (novo) | `IsPhantomReclaim` puro | RF-1 |
| `internal/hostdisk/phantom_test.go` (novo) | table-driven, RED→GREEN | RF-1 |
| `deploy/windows/register-civm-vhdx-optimize.ps1` | watchdog: detector de fantasma + estado Enabled | RF-1 |
| `deploy/windows/civm-vhdx-autoreclaim.ps1` | guest-prune antes do fstrim | RF-3 |
| `deploy/windows/register-civm-vhdx-autoreclaim.ps1` | `ExecutionTimeLimit=PT2H` (JÁ feito) | RF-1 |

(RF-2 `drain.go` REMOVIDO do escopo.)

## Validação (pós-auditoria)

1. **RF-1 unit (Go):** `IsPhantomReclaim(true,false,false)=true`;
   `(true,true,false)=false`; `(true,false,true)=false`;
   `(false,false,false)=false`. RED→GREEN (prova que falha no comportamento
   antigo é N/A — função nova; o teste prova a tabela).
2. **RF-1 host (efeito + par #13):** injetar fantasma → watchdog limpa ≤5 min;
   com processo VIVO, NÃO limpa (não mata reclaim legítimo).
3. **RF-3 host (efeito):** com guest cheio + gap baixo, o reclaim pruna →
   gap sobe → Optimize libera V:. (JÁ demonstrado no firefight 2026-06-15.)
4. **Regressão:** `go test ./... -race` (civm) verde; lint; re-run CI acme
   #1155 sem falha de disco.

## Rastreabilidade

RF-1 → phantom.go + watchdog body + register Enabled + ExecTimeLimit.
RF-3 → civm-vhdx-autoreclaim.ps1 guest-prune.
RF-2/RF-4 → fora de escopo (F3 hardware), documentado no PRD.

## Links Kahneman

- RF-1: **#16** (cura não morre) + **#13** (task Running ≠ função; validar por
  efeito).
- RF-3: **#13** (gap reclamável = prova).
- F3/RF-4 (fora de escopo): **#15** (piso fail-safe correto, não relaxar).
