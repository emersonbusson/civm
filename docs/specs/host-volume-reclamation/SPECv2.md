---
slug: host-volume-reclamation
title: Reclamação de volume do host (VHDX) — guest-free vira host-free com segurança
milestone: —
issues: []
---

# SPECv2 — Reclamação de volume do host (VHDX): guest-free vira host-free com segurança

> **SUPERSEDED-BY (2026-06-17): orchestrator scale-to-zero.** O reclaim do VHDX
> agora pertence ao `civm-vm-orchestrator.ps1` (único dono do stop/compact/
> power-state; tasks `autoreclaim`/`optimize`/`optimize-watchdog` `Disabled`).
> Fonte de verdade viva: `docs/specs/orchestrator-scale-to-zero/`. Esta versão
> (e o runbook que a cita como source-of-truth) é preservada como histórico —
> não a reimplemente.

> Versão melhorada após auditoria do Passo 2.5.
> Baseline preservado: `SPEC.md`.
> Motivo: a auditoria (4 perspectivas, verificada contra o código) deu `no-go` por blockers de **robustez operacional do host**: o `.ps1` de compactação sem `try/catch/finally` real, sem timeouts concretos, sem lock anti-concorrência e com promessa "VM nunca fica Off" não garantida em falha de `Start-VM`; guard de headroom só point-in-time (corrida com job que consome espaço); entrega SSH de métricas best-effort sem semântica de falha; `maintenance enter/exit` com idempotência e retorno ambíguos; acoplamento `capacity→hostdisk` com risco de import cycle; procedimento SCSI/discard (RF-2) sem cmdlets exatos nem validação de `fstab`/boot; registro das Scheduled Tasks (privilégio SYSTEM) não especificado; cita de linha do `idle.Check` errada. Este v2 **fecha cada blocker com decisão explícita**. Onde houver conflito, **o v2 prevalece** sobre `SPEC.md`.

## Como ler este v2

`SPEC.md` define ITENS e estrutura; este `SPECv2.md` é a **camada vinculante de resolução**. PASSO 3 implementa `SPEC.md` **com os overrides deste v2**.

---

## Resolução dos blockers do Passo 2.5 (decisões fechadas)

| # | Blocker (severidade) | Decisão vinculante (override do baseline) |
| --- | --- | --- |
| DT-v2-1 | **CRÍTICO — `civm-vhdx-optimize.ps1` sem try/catch/finally e "VM nunca fica Off" não garantida** (ITEM-11) | Script tem `try { steps } catch { log; throw } finally { ... }`. **`finally` força `Start-VM` + espera Running**. Se `Start-VM` falhar após **3 tentativas (10 s entre)** → log `CRITICAL vm_left_off`, exit não-zero, **não** chama `maintenance exit` (VM Off vira sinal de intervenção manual). Timeouts concretos: shutdown-wait 120 s, `Optimize-VHD` 1800 s, `Start-VM` 60 s/tentativa, SSH 30 s. |
| DT-v2-2 | **CRÍTICO — execuções concorrentes da task** (ITEM-11) | No início, abre `V:\civm-optimize.lock` com `[System.IO.FileStream]` `FileShare::None`; se falhar (já travado) → log `already-running` + exit 1; libera no `finally`. |
| DT-v2-3 | **CRÍTICO — guard de headroom só point-in-time** (ITEM-11) | Guard **em duas fases**: (1) no início; (2) **após `maintenance enter` e antes do `shutdown`**, relê métricas; se `v_free_gb < DefaultHostVolumeHeadroomGB`, chama `maintenance exit --execute` (restaura) e aborta com `headroom_check_failed_after_drain`. Antes do shutdown, **re-loop `idle-check` até idle** (deixa job em voo terminar). |
| DT-v2-4 | **CRÍTICO — `maintenance enter/exit` idempotência/retorno ambíguos** (ITEM-4) | Ambos: `func Enter(ctx, opts) (State, error)` e `func Exit(ctx, opts) (State, error)`. `Enter` adquire `flock` em `/var/lib/civm/maintenance.lock`, checa `idle.Check`, tenta **parar units E remover label** (`and`), grava `State` **só após** drenar; falha individual de `systemctl`/`gh` → WARN+continua; **erro só se ambos falharem**. `Exit` lê `State` (ausente → no-op idempotente), restaura **apenas o que `State` registra**, deleta `State` (falha de delete → erro). Re-run de `Enter`/`Exit` é no-op (atualiza `drained_at`). |
| DT-v2-5 | **CRÍTICO — entrega SSH de métricas sem semântica de falha** (ITEM-10) | Se `scp`/`ssh` ao guest falhar (exit ≠ 0): log WARN, **escreve métricas só-host** em `V:\civm-host-metrics.json` com `delivery_status:"failed"` e `guest_free_gb:0,gap_gb:null`, **exit 0** (não falha a task). `hostdisk.Check` no guest vê stale → `level=crit` (DT-v2-9). |
| DT-v2-6 | **CRÍTICO — registro das Scheduled Tasks (SYSTEM/Hyper-V) não especificado** (ITEM-10/11) | Novo ITEM-15: `deploy/windows/register-civm-host-metrics.ps1` e `register-civm-vhdx-optimize.ps1` com `schtasks /create` exatos (`/RU SYSTEM /RL HIGHEST`, intervalo). Versionados em `deploy/windows/`. Auditoria de privilégio documentada (apenas direito Hyper-V; sem rede; sem segredo). |
| DT-v2-7 | **CRÍTICO — sem watchdog se a task crashar com VM Off** (ITEM-11) | Task externa `civm-vhdx-optimize-watchdog` (a cada 5 min, SYSTEM): se a VM está `Off` E nenhuma instância de `civm-vhdx-optimize` está rodando → `Start-VM` incondicional + log. |
| DT-v2-8 | **CRÍTICO — risco de import cycle `capacity→hostdisk`** (ITEM-5b) | **`capacity` NÃO importa `hostdisk`.** Remove-se o campo `HostLevel` de `capacity.Report`. Visibilidade do host é **exclusivamente** `civmctl host-disk` (CLI chama `hostdisk.Check`). ITEM-5b cancelado. |
| DT-v2-9 | **HIGH — freshness de métricas** (ITEM-2/5) | `hostdisk.Check`: se `Stale` (timestamp > `DefaultHostMetricsMaxAgeMinutes`) OU `delivery_status:"failed"` → `level=crit` **mesmo que `v_free_gb` pareça ok** (freshness não garantida). Task `civm-host-metrics` roda a cada **10 min**; `MaxAge=30`. |
| DT-v2-10 | **HIGH — `disk-doctor` sem árvore de decisão** (ITEM-3) | `root_cause` por ordem: (1) device não montado em `RootPath` → erro; (2) mount sem `discard` → `discard disabled on mount`; (3) controlador IDE → `IDE controller does not propagate UNMAP`; (4) `DISC-MAX==0` → `TRIM not advertised`; (5) `DISC-MAX>0` → `TRIM supported, online shrink expected`. Device via `findmnt --json RootPath`. Delta-test é **opt-in** `--reference-test` (aloca 100 MB→libera→fstrim→mede); evidência Kahneman usa checks estáticos + (opcional) o delta. |
| DT-v2-11 | **HIGH — semântica dos campos de `hostdisk`** (ITEM-5) | Documentar no struct: `VFreeGB`=livre do `V:`; `VSizeGB`=capacidade do `V:`; `VHDXFileSizeGB`=tamanho atual do arquivo; `VHDXMaxSizeGB`=max configurado (Get-VHD); `VHDXMinSizeGB`=min (Get-VHD). **Dois checks distintos:** `FreeHeadroomViolation = VFreeGB < DefaultHostVolumeHeadroomGB`; `AllocationHeadroomViolation = VSizeGB - VHDXMaxSizeGB < DefaultHostVolumeHeadroomGB`. |
| DT-v2-12 | **HIGH — SCSI/discard (RF-2) sem cmdlets nem validação fstab/boot** (ITEM-12) | Runbook traz procedimento exato (abaixo, §Procedimento SCSI). Pré-requisito: `fstab` por UUID (`blkid`). Pós: boot manual + `lsblk` + `disk-doctor trim_effective=true`. Se device mudar (`sda`→`sdb`), UUID em `fstab` mantém boot OK. |
| DT-v2-13 | **HIGH — exit codes dos 3 subcomandos + sintaxe CLI** (ITEM-6/7/8/9) | `disk-doctor`: sempre 0 (diagnóstico). `maintenance <enter\|exit>` (nested switch como `runner`): 0 ok / 1 erro de aplicar/restaurar. `host-disk`: 0 ok / 1 crit ou headroom violation. Flag inválida → `exitUsage`(64). |
| DT-v2-14 | **HIGH — `enter` "e/ou" ambíguo** (ITEM-4) | Resolvido em DT-v2-4: tenta **ambos** (stop + remove-label), WARN individual, erro só se ambos falham; `State` grava o que de fato ocorreu; `Exit` restaura só isso. |
| DT-v2-15 | **HIGH — `Start-VM` sem timeout** (ITEM-11) | `Start-VM -ErrorAction Stop` + poll até Running com timeout 60 s/tentativa, 3 tentativas; log `vm_state_after_start`+`elapsed_ms`. |
| DT-v2-16 | **HIGH — saúde do Hyper-V não checada** (ITEM-11) | No início: `if ((Get-Service vmms).Status -ne 'Running') { exit 1 }`. Documentar: **não** agendar durante Windows Update; rodar em janela de baixo tráfego com alerta se não completar em 2 h. |
| DT-v2-17 | **HIGH — `$LASTEXITCODE` não checado após SSH** (ITEM-11) | Após cada `ssh ... 'civmctl maintenance enter/exit'`: `if ($LASTEXITCODE -ne 0) { throw }`. Erro de drain → **não desliga** a VM. |
| DT-v2-18 | **HIGH — cite de `idle.Check` errada** | Corrigir: `internal/idle/idle.go:68-98` (não 116-169). Assinatura `func Check(ctx, opts) Result` confirmada. |
| DT-v2-19 | **HIGH — valor de `DefaultHostVolumeHeadroomGB` calibrado por host** | A primeira execução offline (ITEM-12) roda `Optimize-VHD` com `Start-Transcript` e poll de `v_free` a cada 5 s; registra o **low-water mark** de scratch. Em 2026-05-31 o host Day-0 foi calibrado para **8 GB**: `V:`=119 GB, `VHDXMax`=110 GB, `SizeMax` do volume sem expansão útil, e dois ciclos `Optimize-VHD` concluíram com VM reiniciada. Rollback trigger: se o low-water chegar a `<=8 GB`, elevar a constante ou mover/expandir `V:` antes de reabilitar a task. |
| DT-v2-20 | **MEDIUM — rollback trigger "3 medições" vago** | Procedimento exato (abaixo, §Rollback trigger v2). |
| DT-v2-21 | **MEDIUM — `Repos`/`gh` semântica** (ITEM-4) | `Repos []string` filtra repos a drenar; vazio → inferir de `actions-runner-*` (lê `.runner`). Remove label: `gh api --method DELETE repos/{owner}/{repo}/labels/civm`; restaura: `gh api --method POST repos/{owner}/{repo}/labels -f 'labels[]=civm'`. |
| DT-v2-22 | **MEDIUM — Mapa Kahneman: delta-test e ITEM-5b** | Evidência de `disk-doctor` usa checks estáticos (+ `--reference-test` opcional); linha de ITEM-5b removida (DT-v2-8 cancelou ITEM-5b). |
| DT-v2-23 | **MEDIUM — Ordem step 8 (SCSI) sem dono/ambiente** | SCSI re-attach é **operação manual/escriptada de host numa janela**, disparada quando `disk-doctor` aponta IDE/no-discard; valida por boot + `disk-doctor` + 3 medições. |
| DT-v2-24 | **MEDIUM — constantes: âncora e comentários** (ITEM-2) | Inserir **após a linha 62** (`DefaultUpgradeVerifySeconds`), antes de fechar o `const`. Comentário de `DefaultHostVolumeHeadroomGB`: "mínimo de `V:` livre ANTES do `Optimize-VHD`; abaixo disso aborta sem zero-fill (folga p/ crescimento temporário do VHDX na compactação)". Adicionar `DefaultHostMetricsFileNameOnHost = "civm-host-metrics.json"`. |

---

## Exit codes (subcomandos novos) — fecha DT-v2-13

| Subcomando | 0 | 1 | 64 |
| --- | --- | --- | --- |
| `disk-doctor` | sempre (diagnóstico) | — | flag inválida |
| `maintenance enter\|exit` | sucesso | erro ao aplicar/restaurar | flag inválida / sub não-`enter\|exit` |
| `host-disk` | `level=ok/warn` | `level=crit` OU headroom violation OU stale/delivery-failed | flag inválida |

## Procedimento SCSI/discard (RF-2) — fecha DT-v2-12 (vai no runbook)

```text
1. Guest: sudo blkid && cat /etc/fstab   # confirmar que / usa UUID= (não /dev/sdX). Se não, trocar p/ UUID ANTES.
2. Guest: civmctl disk-doctor --json      # registrar controller/discard/DISC-MAX (baseline)
3. Drenar: civmctl maintenance enter --execute; aguardar idle; sudo shutdown -h now
4. Host (elevado): aguardar Get-VM Off; então:
   Remove-VMHardDiskDrive -VMName gha-ubuntu-2404 -ControllerType IDE -ControllerNumber 0 -ControllerLocation 0
   Add-VMScsiController   -VMName gha-ubuntu-2404
   Add-VMHardDiskDrive    -VMName gha-ubuntu-2404 -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 0 -Path <vhdx>
   Start-VM -Name gha-ubuntu-2404
5. Guest (boot manual): lsblk; civmctl disk-doctor --json  # trim_effective deve ser true; device pode ter mudado (sda→sdb) — fstab por UUID mantém boot.
6. civmctl maintenance exit --execute
7. Validar auto-shrink: §Rollback trigger v2 (3 medições).
ROLLBACK: se boot falhar, reverter para IDE (Remove SCSI + Add IDE) em janela.
```

## Rollback trigger v2 (auto-shrink) — fecha DT-v2-20

Após RF-2, repetir **3 rodadas**: (1) liberar 50 GB no guest (`dd`/arquivo de teste removido); (2) `Get-VHD` FileSize no host; (3) `sudo fstrim -av`; (4) esperar 10 s; (5) `Get-VHD` FileSize de novo. Se o FileSize **não** cair ≈50 GB (±10%) nas 3 rodadas → reverter RF-2 (SCSI→IDE) e investigar. Outros gatilhos: a task de compactação deixar a VM `Off` ≥1 vez (CRITICAL); `v_free_gb` cruzar 10 GB sem alarme prévio de 30 GB.

---

## Especificações que substituem o baseline (código-nível)

### ITEM-4 `internal/maintenance/maintenance.go` (override) — DT-v2-4/14/21

- `type RunnerState struct { Name string `json:"name"`; Repo string `json:"repo,omitempty"`; Stopped bool `json:"stopped"`; LabelRemoved bool `json:"label_removed"` }`
- `type State struct { DrainedAt string `json:"drained_at"`; Runners []RunnerState `json:"runners"` }`
- `func Enter(ctx, opts) (State, error)`: `flock(/var/lib/civm/maintenance.lock)`; `idle.Check` (se busy e não forçado → erro); para units + remove label por runner (WARN individual; erro só se ambos falharem em **todos**); grava `State`. Re-run com `State` existente → no-op (atualiza `DrainedAt`).
- `func Exit(ctx, opts) (State, error)`: lê `State` (ausente → no-op); restaura só o registrado (`Stopped`→`systemctl start`, `LabelRemoved`→`gh ... POST`); WARN individual; deleta `State` (erro de delete → erro). Idempotente.

### ITEM-5 `internal/hostdisk/hostdisk.go` (override) — DT-v2-9/11

- `Metrics` ganha `DeliveryStatus string `json:"delivery_status,omitempty"``. `Report` ganha `FreeHeadroomViolation`, `AllocationHeadroomViolation`. `Check`: `Stale` OU `DeliveryStatus=="failed"` → `Level="crit"`. Campos documentados conforme DT-v2-11.

### ITEM-3 `internal/diskdoctor/diskdoctor.go` (override) — DT-v2-10

- Árvore de decisão de `root_cause` conforme DT-v2-10; device via `findmnt --json`; flag `--reference-test` (default off) que faz o delta de 100 MB.

### ITEM-11 `deploy/windows/civm-vhdx-optimize.ps1` (override) — DT-v2-1/2/3/7/15/16/17

Esqueleto vinculante:

```powershell
# 0. lock anti-concorrência (FileShare::None) em V:\civm-optimize.lock; senão exit 1
# 0b. if ((Get-Service vmms).Status -ne 'Running') { exit 1 }
try {
  # 1. ler métricas; if v_free_gb < headroom { log abort_headroom; exit 2 }   # NUNCA zero-fill
  # 2. ssh '... maintenance enter --execute'; if ($LASTEXITCODE) { throw }
  # 2b. re-loop ssh '... idle-check' até idle (timeout); reler métricas; if v_free_gb<headroom { ssh maintenance exit; throw 'headroom_after_drain' }
  # 3. ssh 'sudo fstrim -av'
  # 4. ssh 'sudo shutdown -h now'; aguardar Get-VM Off (timeout 120s)
  # 5. Optimize-VHD -Path <vhdx> -Mode Full -ErrorAction Stop   # timeout 1800s
} catch { Write-Host "ERROR $_"; }
finally {
  # Start-VM com 3 tentativas x (timeout 60s, sleep 10s); se Running -> ssh 'maintenance exit --execute'
  # se NÃO Running após 3 -> Write-Host 'CRITICAL vm_left_off'; exit 1  (sem maintenance exit)
  # liberar V:\civm-optimize.lock
}
```

### ITEM-15 (novo) `deploy/windows/register-*.ps1` — DT-v2-6/7

- `register-civm-host-metrics.ps1`: `schtasks /create /tn civm-host-metrics /tr "...host-metrics.ps1" /sc minute /mo 10 /ru SYSTEM /rl HIGHEST /f`.
- `register-civm-vhdx-optimize.ps1`: `schtasks /create /tn civm-vhdx-optimize /tr "...vhdx-optimize.ps1" /sc onstart /ru SYSTEM /rl HIGHEST /f` (acionada por `schtasks /run`, não por intervalo) + `civm-vhdx-optimize-watchdog` `/sc minute /mo 5`.
- Auditoria de privilégio: tasks rodam SYSTEM só com direito Hyper-V; sem rede; sem segredo. Reversível por `schtasks /delete`.

### ITEM-5b — CANCELADO (DT-v2-8). `capacity.Report` não muda; host só via `civmctl host-disk`.

---

## Mapa Kahneman v2 (overrides)

| Etapa / ITEM | Disciplina | Link | Pergunta obrigatória | Evidência mínima | Abort trigger |
| --- | --- | --- | --- | --- | --- |
| **ITEM-1/ITEM-3 (diagnóstico)** | #3 Número não adjetivo | `disciplines/KAHNEMAN-DISCIPLINES.md` #3 | Por que `fstrim` libera só 737 MiB? | `disk-doctor --json` (controller/discard/DISC-MAX) + opcional `--reference-test` delta | afirmar causa sem medir |
| **ITEM-11/ITEM-12 (compactação/SCSI)** | #5 Availability | idem #5 | A task pode deixar a VM Off ou estourar o `V:`? | `finally` força `Start-VM` (3×); watchdog 5 min; headroom 2-fases; transcript do scratch low-water mark | VM ficar Off ao fim; zero-fill sob baixo headroom; `Start-VM` falhar 3× |
| **ITEM-2/ITEM-5 (headroom/obs)** | #2 Counterfactual | idem #2 | Qual número dispara alarme/rollback? | `host-disk` crit a 10 GB / warn a 30 GB; stale/delivery-failed → crit | `v_free_gb` cruzar 10 GB sem alarme prévio |
| **ITEM-4 (maintenance)** | #5 Availability | idem #5 | Drain mata job vivo ou deixa estado órfão? | `idle.Check`+re-loop antes do shutdown; `State` idempotente; teste enter/exit re-run no-op | drain matar job ativo; `Exit` não restaurar `State` |

ITEM-5b removido do Mapa (DT-v2-8/22).

## Ordem de implementação v2 (override)

Inalterada (1→11), com: passo 6 inclui `maintenance` com `flock`; **ITEM-15 (register-*.ps1)** entra junto de ITEM-10/11; passo 8 (SCSI) é operação de host em janela (DT-v2-23); ITEM-5b removido.

## Plano de testes v2 (adições)

- `maintenance`: enter/exit idempotentes (re-run no-op), `flock` serializa, partial-failure (só stop OU só label) restaura correto, busy bloqueia enter.
- `hostdisk`: stale→crit, delivery_status=failed→crit, FreeHeadroom vs AllocationHeadroom.
- `disk-doctor`: árvore de decisão por caso (ide/no-discard/DISC-MAX 0/>0); `findmnt --json` mock.
- **Host (janela):** `civm-vhdx-optimize` — lock anti-concorrência; headroom 2-fases abortando após drain; `Start-VM` falha simulada → 3 tentativas → CRITICAL exit; watchdog religa VM Off; `$LASTEXITCODE` de SSH abortando sem desligar.

## Checklist de validação v2 (adições)

- [ ] `go test ./... -race -count=1` (maintenance/hostdisk/diskdoctor)
- [ ] `golangci-lint run -c .golangci.yml ./...`
- [ ] Import-cycle: `capacity` **não** importa `hostdisk` (`go list -deps ./internal/capacity | grep -q internal/hostdisk` vazio)
- [ ] PSScriptAnalyzer nos `.ps1` (se disponível)
- [ ] `schtasks /query /tn civm-vhdx-optimize` + watchdog registrados (SYSTEM, RL HIGHEST)
- [ ] Janela RF-2: boot OK pós-SCSI + `disk-doctor trim_effective=true` + 3 medições de auto-shrink
- [ ] `npm run docs:index` + `npm run docs:check`

## Veredito

`go` **condicional**: pronto para PASSO 3. A operação de host **RF-2 (SCSI)** e a primeira execução de **`civm-vhdx-optimize`** exigem janela de manutenção supervisionada (não rodar durante Windows Update); todos os `*DECISION*` do Passo 2.5 estão fechados acima.
