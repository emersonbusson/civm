# SPEC — Host Volume Reclaim Liveness

> **SUPERSEDED-BY (2026-06-17): orchestrator scale-to-zero.** O reclaim do VHDX
> agora pertence ao `civm-vm-orchestrator.ps1` (único dono do stop/compact/
> power-state; tasks `autoreclaim`/`optimize`/`optimize-watchdog` `Disabled`).
> Fonte de verdade viva: `docs/specs/orchestrator-scale-to-zero/`. O conteúdo
> abaixo é preservado como histórico do mecanismo anterior — não o reimplemente.

> SSDV3 PASSO 2. Traduz `PRD.md` em arquivos, predicados Go testáveis, diffs PS1
> e thresholds. Links Kahneman nos passos críticos. Implementa estritamente os
> RF-1..RF-4 / RNF-1..RNF-4 do PRD.

> **Status: baseline pré-auditoria — NÃO é o escopo entregue.** O PASSO 2.5
> deu **NO-GO** na RF-2 (drain-on-pressure): drenar ao WARN interrompe o job em
> curso por ganho marginal (`SPECv2.md` §B1). A versão ativa é `SPECv2.md` e o
> `IMPL.md` entregou apenas **RF-1 + RF-3 + backstop ExecutionTimeLimit**.
> Logo, **RF-2/RF-4 NÃO foram implementados**: `internal/hostdisk/drain.go` e o
> predicado `ShouldDrainForReclaim` descritos abaixo **não existem** no código —
> são o plano original, preservado aqui como baseline histórico. Para o que de
> fato foi feito, ver `SPECv2.md` + `IMPL.md`.

## Princípio de design

As **DECISÕES** (é fantasma? deve drenar?) vão para Go puro e testável em
`internal/hostdisk` (reuso da regra dura SSDV3 — já há
`watchdog_race_test.go`). A **ORQUESTRAÇÃO** (Stop-VM, SSH drain, Optimize)
permanece nos scripts PS1 que já a fazem. Nenhum reclaim novo é criado; estende-se
`civm-vhdx-autoreclaim.ps1` e o watchdog em `register-civm-vhdx-optimize.ps1`.

## RF-1 — Detector de fantasma no watchdog (liveness, ≤5 min)

**Predicado Go** (novo, `internal/hostdisk/phantom.go`):

```go
// IsPhantomReclaim reporta se a task de reclaim está num estado fantasma:
// o Scheduler a marca em execução, mas não há processo vivo do script e o
// lock de reclaim está órfão (não-segurado). Kahneman #13: estado da task
// (existência) ≠ reclaim rodando (função).
func IsPhantomReclaim(taskRunning bool, scriptProcessAlive bool, reclaimLockHeld bool) bool {
    return taskRunning && !scriptProcessAlive && !reclaimLockHeld
}
```

**Orquestração** — corpo do `civm-vhdx-optimize-watchdog` (here-string em
`deploy/windows/register-civm-vhdx-optimize.ps1`), ANTES do check `state -eq
Running` da VM (o fantasma ocorre com a VM Running):

1. `taskRunning` = `(Get-ScheduledTask civm-vhdx-autoreclaim).State -eq 'Running'`.
2. `scriptProcessAlive` = existe `powershell`/`pwsh` com CommandLine
   `*civm-vhdx-autoreclaim.ps1*` (reuso do process-scan já presente no watchdog).
3. `reclaimLockHeld` = `Test-LockHeld 'V:\civm-autoreclaim.lock'` (helper já
   existente no watchdog).
4. Se fantasma → `Stop-ScheduledTask civm-vhdx-autoreclaim` +
   `Remove-Item V:\civm-autoreclaim.lock,V:\civm-reclaim.lock -Force` +
   log `reclaim_liveness_phantom_cleared`. A próxima cadência (30 min) ou um
   trigger imediato roda fresco.

**Habilitar a task** `civm-vhdx-optimize-watchdog` (hoje Disabled) via o register
(estado inicial Enabled) e `Enable-ScheduledTask` no host. Cadência 5 min já
registrada. Kahneman **#16**: a cura (watchdog) não pode estar desligada.

**Backstop:** `ExecutionTimeLimit=PT2H` na `civm-vhdx-autoreclaim` (JÁ aplicado —
commit `6ffbfee`, branch `fix/autoreclaim-exec-time-limit`); o Windows mata uma
presa em ≤2h mesmo se o watchdog falhar. RF-1 é o detector primário (≤5 min); o
PT2H é o segundo nível.

## RF-2 — Drain-on-pressure no autoreclaim (NO-GO na 2.5 — NÃO implementado)

> RF-2 saiu do escopo na auditoria 2.5 (`SPECv2.md` §B1). O `drain.go` e o
> `ShouldDrainForReclaim` abaixo nunca foram criados; o trecho é o plano
> original preservado como baseline.

**Predicado Go** (novo, `internal/hostdisk/drain.go`):

```go
// ShouldDrainForReclaim reporta se o reclaim deve DRENAR o runner para forçar
// uma janela idle: V: está sob pressão (abaixo do WARN) E o guest não ficou
// idle dentro da janela de espera normal. Acima do WARN, espera idle natural
// (não interrompe CI à toa). Kahneman #16: agir ANTES do piso crítico.
func ShouldDrainForReclaim(vFreeGB, warnGB int64, idleWaitTimedOut bool) bool {
    return idleWaitTimedOut && vFreeGB < warnGB
}
```

**Orquestração** — em `civm-vhdx-autoreclaim.ps1`, quando `Wait-GuestIdle`
retorna `$false` (hoje: `autoreclaim_skip_busy` + exit). Novo caminho:

1. Se `ShouldDrainForReclaim($beforeFreeGB, $WarnGB, $true)`:
   - `reclaim_liveness_drain_start` (log).
   - SSH guest: `sudo systemctl stop "actions.runner.*"` (drena — para de
     aceitar novos jobs; jobs em curso terminam ou são salvos no Stop-VM).
   - `Wait-GuestIdle` de novo (agora idle) → segue para Stop-VM/Optimize/Start.
   - No `finally`, **sempre** SSH guest `sudo systemctl start "actions.runner.*"`
     (religa o runner; RNF-1). Cooldown via `V:\civm-reclaim-drain.json`
     (timestamp; não drenar de novo dentro de `DrainCooldownMinutes`, RNF-2).
2. Senão (acima do WARN) → mantém o `autoreclaim_skip_busy` atual (não drena).

`WarnGB` = `civm.DefaultAutoreclaimPressureGB` (25, já existente). `DrainCooldown`
= 20 min (default; anti-flap).

## RF-3 — Prune do guest antes do Optimize

Em `civm-vhdx-autoreclaim.ps1`, ANTES do `fstrim` (hoje só faz fstrim+Optimize):

- SSH guest: `civmctl disk-watchdog --threshold-pct=0 --execute` (prune
  agressivo: docker/cache/work_old). Best-effort (log `reclaim_liveness_guest_prune`
  com bytes liberados; falha não aborta — fstrim/Optimize seguem).
- Só então `fstrim -av` + Stop-VM + Optimize-VHD.

**Confirmado ao vivo (2026-06-15):** sem o prune, `autoreclaim_skip_low_gap
gap=2.44GB`; com `docker_prune` (17.5GB liberados), o gap virou ~28GB e o
Optimize procedeu. RF-3 é o que torna o reclaim EFETIVO sob carga real (Kahneman
#13: validar por efeito — o gap reclamável).

## RF-4 — Degradação graciosa (fora de escopo na 2.5 — NÃO implementado)

> RF-4 também saiu do escopo na 2.5 (`SPECv2.md`): o caso que ele cobria é F3
> (working set ativo > capacidade do disco), que é limite de hardware. O piso
> crítico de admissão existente já é o fail-safe correto; nada novo foi feito.

Sem mudança no piso crítico de admissão (`hook.go` / `hostdisk.Blocks()` exit
75). RF-2 age ANTES (no WARN, 25GB > piso 10GB), então a recusa vira último
recurso. Quando F3 (working set > disco) torna o piso inevitável, a recusa
permanece o fail-safe correto (RNF-3, Kahneman **#15/#16**).

## Arquivos tocados

| Arquivo | Mudança | RF |
| --- | --- | --- |
| `internal/hostdisk/phantom.go` (novo) | `IsPhantomReclaim` puro | RF-1 |
| `internal/hostdisk/phantom_test.go` (novo) | table-driven (fantasma vs vivo vs idle-recém-iniciado) | RF-1 |
| `internal/hostdisk/drain.go` (novo) | `ShouldDrainForReclaim` puro — **NÃO implementado (NO-GO 2.5)** | RF-2 |
| `internal/hostdisk/drain_test.go` (novo) | table-driven (WARN×idle-timeout) — **NÃO implementado (NO-GO 2.5)** | RF-2 |
| `deploy/windows/register-civm-vhdx-optimize.ps1` | watchdog: detector de fantasma + estado inicial Enabled | RF-1 |
| `deploy/windows/civm-vhdx-autoreclaim.ps1` | guest-prune (RF-3) entregue; drain-on-pressure (RF-2) **NÃO** (NO-GO 2.5) | RF-3 |
| `deploy/windows/register-civm-vhdx-autoreclaim.ps1` | `ExecutionTimeLimit=PT2H` (JÁ feito, `6ffbfee`) | RF-1 |

## Validação (critérios de sucesso do PRD → testes)

1. **RF-1 unit (Go):** `IsPhantomReclaim(true,false,false)=true`;
   `(true,true,false)=false` (vivo); `(true,false,true)=false` (lock segurado =
   recém-iniciado, ainda não soltou). RED→GREEN.
2. **RF-2 unit (Go):** `ShouldDrainForReclaim(5,25,true)=true`;
   `(30,25,true)=false` (acima do WARN); `(5,25,false)=false` (idle natural OK).
3. **RF-1 host (efeito):** injetar fantasma (`Start` + matar o processo, ou
   forçar `State=Running` órfão) → watchdog limpa ≤5 min (evento
   `reclaim_liveness_phantom_cleared`). Pareado (#13): com processo VIVO, o
   watchdog NÃO limpa (não mata um reclaim legítimo).
4. **RF-2/RF-3 host (efeito):** dirigir `V:` ao WARN sob carga → o reclaim drena,
   pruna o guest, compacta, religa o runner; `V:` volta acima do WARN; nenhum
   job recusado por exit 75.
5. **Regressão:** `go test ./... -race` (civm) verde; re-run CI acme #1155 sem
   falha de disco (slack reclamável suficiente).

## Links Kahneman (passos críticos)

- RF-1, RF-2: **#16** (`docs/methodology` no acme / `disciplines/` no civm) — a
  cura não pode morrer (fantasma) nem ser starvada (load) com o recurso.
- RF-1, RF-3, validação por efeito: **#13** — task Running ≠ reclaim funcionando;
  gap reclamável é a prova.
- RF-4: **#15** — o piso crítico fail-fast/fail-safe é correto e não se relaxa.

## Decisões e trade-offs

- **DT-1: WARN=25GB (reuso de `DefaultAutoreclaimPressureGB`).** Acima dele,
  espera idle natural (não interrompe CI). Abaixo + busy → drena. Trade-off:
  drenar pausa novos jobs ~13-15 min (idle+Stop-VM+Optimize+Start); aceitável vs
  recusar TODOS por horas. Counterfactual (#2): se 25GB der drains frequentes
  demais, subir o WARN só agrava (drena antes); a alavanca real é disco/conc.
- **DT-2: drain via `systemctl stop actions.runner.*`, não `Stop-VM -TurnOff`.**
  Preserva jobs em curso (terminam ou salvam no Stop-VM normal). `-TurnOff` fica
  só no backstop guest-wedge já existente. RNF-1.
- **DT-3: cooldown 20 min.** Evita laço drena-religa-drena se o working set for
  estruturalmente grande (F3) — nesse caso degrada para a recusa (RF-4), não
  para flap infinito. RNF-2.
