# Runbook — Manutenção do VHDX do host (reclamação de volume)

> **Quando usar:** o volume `V:` do host Hyper-V mostra pouco espaço
> livre enquanto o guest (VM `gha-ubuntu-2404`) reporta espaço livre
> abundante. O guest libera blocos (`fstrim`/`TRIM`) mas o **arquivo
> VHDX no host não encolhe**, então `V:` continua enchendo até derrubar
> os jobs de CI.
>
> **Modelo conceitual:** o disco do guest é um arquivo `.vhdx` no `V:`.
> Quando o guest apaga arquivos, o `TRIM`/`UNMAP` só funciona end-to-end
> se o controlador de disco propaga o comando (SCSI sim, IDE não) e o
> mount do guest tem `discard`. Mesmo com tudo certo, o VHDX dinâmico
> só **devolve** os blocos ao host num `Optimize-VHD` offline (VM Off).
> Este runbook é a árvore de decisão entre as três alavancas:
> `fstrim` online, re-attach SCSI e `Optimize-VHD` offline.
>
> **Fonte de verdade:** `docs/specs/host-volume-reclamation/SPECv2.md`.
> Onde este runbook citar uma decisão `DT-v2-N`, ela é vinculante e
> sobrepõe qualquer leitura do `SPEC.md` baseline.
>
> **Companion runbooks:**
> - [`MULTI-PROJECT-RUNNER.md`](./MULTI-PROJECT-RUNNER.md) — runtime de
>   isolamento multi-projeto + o `civmctl maintenance` usado aqui como
>   handshake de drain.
> - [`VM-CREDENTIALS.md`](./VM-CREDENTIALS.md) — como acessar a VM/host
>   sem vazar secret no repo.

## Pré-condição operacional (leia antes de tudo)

- Estas operações de **host** (`Optimize-VHD`, re-attach SCSI, registro
  de Scheduled Task) rodam no **host Hyper-V** com PowerShell elevado,
  **não** no guest. O guest é a VM `gha-ubuntu-2404`.
- **Nunca** agendar/rodar `civm-vhdx-optimize` durante Windows Update.
  Rodar em janela de baixo tráfego e alertar se não completar em **2 h**
  (SPECv2 DT-v2-16).
- No host, o serviço Hyper-V precisa estar de pé:
  `(Get-Service vmms).Status -eq 'Running'` (SPECv2 DT-v2-16). Se não
  estiver, **aborte** — não tente compactar.
- O guest precisa de SSH funcional a partir do host. As tasks SYSTEM não
  herdam o `~/.ssh` do usuário interativo; use chave dedicada em
  `C:\ProgramData\civm\ssh\id_ed25519` com owner/ACL só para SYSTEM
  (o OpenSSH do Windows rejeita a chave privada se Administrators ou o
  usuário interativo tiverem acesso), e autorize a `.pub` no
  `emdev@gha-ubuntu-2404`
  (entrega de métricas, `fstrim` e handshake de drain dependem disso —
  SPECv2 DT-v2-5/17).

## Sintomas (como reconhecer o caso)

| Observação | Significado |
| --- | --- |
| `V:` com pouco livre **e** guest com muito livre | VHDX inchado: blocos liberados no guest não voltaram ao host |
| `civmctl host-disk` retorna `level=crit` por `VFreeGB` baixo | Headroom do `V:` violado (`FreeHeadroomViolation`, SPECv2 DT-v2-11) |
| `civmctl host-disk` retorna `level=crit` por **stale**/`delivery_status:"failed"` | Métricas do host não chegaram; freshness não garantida (SPECv2 DT-v2-9) |
| `civmctl disk-doctor` aponta `IDE controller does not propagate UNMAP` | Controlador IDE não propaga `TRIM` → re-attach SCSI necessário (SPECv2 DT-v2-10) |
| `fstrim -av` libera poucos MiB (ex.: 737 MiB) num guest com dezenas de GB livres | `TRIM` não chega ao host (controlador/mount) — diagnosticar com `disk-doctor` |

## Como o civm expõe o estado

Três superfícies de CLI, todas com contrato estável (não parsear journal
nem inferir pelo layout de arquivos):

| Comando | O que reporta | Exit codes (SPECv2 §Exit codes / DT-v2-13) |
| --- | --- | --- |
| `civmctl host-disk` | `VFreeGB`, `VSizeGB`, `VHDXFileSizeGB`, `VHDXMaxSizeGB`, `VHDXMinSizeGB`, `FreeHeadroomViolation`, `AllocationHeadroomViolation`, freshness das métricas | `0` = ok/warn · `1` = crit OU headroom violation OU stale/delivery-failed · `64` = flag inválida |
| `civmctl disk-doctor` | `root_cause` (árvore de decisão), controller, `discard`, `DISC-MAX`, `trim_effective` | `0` sempre (é diagnóstico) · `64` = flag inválida |
| `civmctl maintenance enter\|exit` | drain/undrain do guest (para units + remove label `civm`) | `0` = sucesso · `1` = erro ao aplicar/restaurar · `64` = flag inválida / sub não-`enter\|exit` |

Semântica dos campos de `host-disk` (SPECv2 DT-v2-11):

- `VFreeGB` — livre do `V:`.
- `VSizeGB` — capacidade do `V:`.
- `VHDXFileSizeGB` — tamanho atual do arquivo `.vhdx`.
- `VHDXMaxSizeGB` / `VHDXMinSizeGB` — max/min configurado (`Get-VHD`).
- `FreeHeadroomViolation` = `VFreeGB < DefaultHostVolumeHeadroomGB`.
- `AllocationHeadroomViolation` = `VSizeGB - VHDXMaxSizeGB < DefaultHostVolumeHeadroomGB`.

Freshness é tratada como saúde de primeira classe: se as métricas estão
`Stale` (timestamp acima de `DefaultHostMetricsMaxAgeMinutes`, default
`MaxAge=30`) **ou** com `delivery_status:"failed"`, `host-disk` retorna
`level=crit` **mesmo que `VFreeGB` pareça ok** (SPECv2 DT-v2-9). A task
`civm-host-metrics` roda a cada **10 min**.

## Árvore de decisão (qual alavanca usar)

```text
1. civmctl host-disk  ->  level=crit?
   |
   +-- crit por STALE / delivery_status=failed
   |     -> NÃO é problema de espaço; é entrega de métricas.
   |        Investigar SSH host->guest + task civm-host-metrics.
   |        (Fail-safe: crit por freshness, SPECv2 DT-v2-9.)
   |
   +-- crit por FreeHeadroomViolation (V: cheio, guest com livre)
         -> civmctl disk-doctor --json  (achar root_cause)
            |
            +-- root_cause = "TRIM supported, online shrink expected"
            |     (DISC-MAX>0, SCSI, mount com discard)
            |     -> ALAVANCA A: fstrim online (sem desligar a VM).
            |
            +-- root_cause = "discard disabled on mount"
            |     -> corrigir mount (adicionar discard) e repetir disk-doctor.
            |
            +-- root_cause = "IDE controller does not propagate UNMAP"
            |   OU "TRIM not advertised" (DISC-MAX==0)
            |     -> ALAVANCA B: re-attach SCSI (janela, host).
            |
            +-- fstrim/SCSI ok porém VHDX continua grande
                  -> ALAVANCA C: Optimize-VHD offline (task civm-vhdx-optimize).
```

Tradução das alavancas:

- **A — `fstrim` online:** quando o `disk-doctor` já confirma
  `trim_effective=true` (`DISC-MAX>0`, SCSI, mount com `discard`). Não
  desliga a VM; o shrink dinâmico é esperado online.
- **B — re-attach SCSI:** quando o `disk-doctor` aponta IDE ou
  `TRIM not advertised`. Operação **manual/escriptada de host numa
  janela** (SPECv2 DT-v2-23). Ver §Procedimento SCSI.
- **C — `Optimize-VHD` offline:** quando o `TRIM` já funciona mas o VHDX
  dinâmico ainda segura blocos. Executada pela task `civm-vhdx-optimize`
  com drain + headroom guard + watchdog. Ver §Compactação offline.

## civm-host-metrics.ps1 (entrega de métricas)

Script de host registrado como task SYSTEM rodando a cada **10 min**
(SPECv2 DT-v2-6/9). Coleta `V:` (free/size) + `Get-VHD`, faz `scp`/`ssh`
ao guest para somar o lado guest, e escreve
`V:\civm-host-metrics.json` (`DefaultHostMetricsFileNameOnHost`).

Contrato de SSH:

- destino padrão: `emdev@gha-ubuntu-2404`;
- chave padrão: `C:\ProgramData\civm\ssh\id_ed25519`;
- `known_hosts` padrão: `C:\ProgramData\civm\ssh\known_hosts`;
- a cópia guest-local em `/var/lib/civm/host-metrics.json` usa
  `sudo -n` porque `/var/lib/civm` é root-owned. Se o sudo sem senha
  falhar, a task deve marcar `delivery_status:"failed"` em vez de abrir
  permissão no diretório.

Semântica de falha de entrega (SPECv2 DT-v2-5):

- Se `scp`/`ssh` ao guest falhar (exit ≠ 0): log **WARN**, escreve
  métricas **só-host** com `delivery_status:"failed"` e
  `guest_free_gb:0, gap_gb:null`, e **sai com exit 0** (não falha a
  task).
- `civmctl host-disk` no guest então vê o JSON stale/failed e retorna
  `level=crit` (fail-safe, DT-v2-9).

Inspeção manual no host:

```powershell
# Ler o último snapshot de métricas
Get-Content V:\civm-host-metrics.json | ConvertFrom-Json

# Confirmar que a task está registrada (SYSTEM, RL HIGHEST, 10 min)
schtasks /query /tn civm-host-metrics /v /fo LIST
```

## civm-vhdx-autoreclaim.ps1 (prevenção automática)

> **SUPERSEDED (2026-06-17) pelo orchestrator scale-to-zero.** O
> `civm-vm-orchestrator.ps1` passou a ser o **único dono** do
> stop+compact da VM (um curador só, sem disputa de lock/power-state).
> A task `civm-vhdx-autoreclaim` está **Disabled por design** — não a
> registre nem a reabilite enquanto o orchestrator estiver ativo, sob
> pena de dois reclaimers brigarem pelo mesmo `V:\civm-reclaim.lock`.
> A prevenção automática agora é o tick do orchestrator (idle ≥ N min →
> stop+compact; pisos de disco panic 18 GB / warn 28 GB). Fonte de
> verdade: `docs/specs/orchestrator-scale-to-zero/SPEC.md`. A seção
> abaixo é preservada como histórico do mecanismo anterior — **não a
> execute**.

> **SUPERSEDED novamente em 2026-08-02:** o owner vivo agora é a task C#
> `civm-host-orchestrator`; `civm-vm-orchestrator` permanece `Disabled` como
> rollback. Esta seção preserva o corte histórico de 2026-06-17.

Script de host registrado como task SYSTEM a cada **30 min**. Ele é o
caminho preventivo para o caso recorrente: `V:` vai enchendo durante o
dia, mas o guest ainda tem espaço livre. Diferente do
`civm-vhdx-optimize`, ele não altera controlador nem entra em maintenance
mode; só compacta quando as guardas abaixo passam.

Guardas obrigatórias antes de desligar a VM:

1. lock `V:\civm-autoreclaim.lock` adquirido;
2. `V:` abaixo de `ThresholdGB` (default 50 GB) e acima de
   `MinHeadroomGB` (default 8 GB);
3. VM `gha-ubuntu-2404` está `Running`;
4. SSH host→guest funciona com a chave de `C:\ProgramData\civm\ssh`;
5. gap estimado do VHDX ≥ `MinReclaimableGB` (default 8 GB);
6. `civmctl idle-check` fica verde dentro de 10 min;
7. `sudo -n fstrim -av` completa com exit 0.

Só depois disso a task executa `Stop-VM -> Optimize-VHD -Mode Full ->
Start-VM`, com 3 tentativas de `Start-VM` no `finally`. Rollback trigger:
se a task interromper CI ativo, delete `civm-vhdx-autoreclaim` e mantenha
apenas a compactação supervisionada até corrigir o predicado de idle.

Registro (**SUPERSEDED — não registrar**; o orchestrator é o dono do
stop+compact desde 2026-06-17). Mantido só para leitura histórica e para
o caso break-glass de desativar o orchestrator e religar o mecanismo
antigo:

```powershell
# NÃO rode com o orchestrator ativo — dois reclaimers colidem no lock.
powershell -ExecutionPolicy Bypass -File C:\civm-deploy\register-civm-vhdx-autoreclaim.ps1
schtasks /query /tn civm-vhdx-autoreclaim /v /fo LIST
```

## civm-vhdx-optimize.ps1 (compactação offline)

> **BREAK-GLASS ONLY (desde 2026-06-17).** A compactação offline de
> rotina migrou para o orchestrator scale-to-zero, que faz Stop-VM +
> `Optimize-VHD` na fronteira de cada PR (ocioso) e no panic de disco.
> Este `civm-vhdx-optimize` deixou de ser a alavanca de rotina — só rode
> manualmente como recurso de emergência **depois de pausar/desabilitar
> o orchestrator**, senão os dois disputam o `V:\civm-reclaim.lock` e o
> power-state da VM (fail-safe Kahneman #15: um dono só):
>
> ```powershell
> # 1. Pausar o orchestrator (desabilita a Scheduled Task ~1min)
> schtasks /change /tn civm-vm-orchestrator /disable
> # 2. ... rodar a compactação break-glass abaixo ...
> # 3. Religar o orchestrator quando terminar
> schtasks /change /tn civm-vm-orchestrator /enable
> ```
>
> Em operação normal, **não** dispare esta task — deixe o orchestrator
> compactar. Fonte de verdade do fluxo vivo:
> `docs/specs/orchestrator-scale-to-zero/SPEC.md`.

Script de host (`deploy/windows/civm-vhdx-optimize.ps1`, SPECv2 ITEM-11
override / DT-v2-1/2/3/7/15/16/17). Acionada **sob demanda** via
`schtasks /run` (registrada `/sc onstart`, não por intervalo — SPECv2
DT-v2-6), numa janela supervisionada. Sequência vinculante:

1. **Lock anti-concorrência:** abre `V:\civm-optimize.lock` com
   `FileShare::None`. Se já travado → log `already-running` + exit 1
   (DT-v2-2).
2. **Saúde Hyper-V:** `if ((Get-Service vmms).Status -ne 'Running') { exit 1 }`
   (DT-v2-16).
3. **Headroom guard — fase 1:** lê métricas; se
   `v_free_gb < DefaultHostVolumeHeadroomGB` → log `abort_headroom` +
   `exit 2`. **Nunca** faz zero-fill (DT-v2-3).
4. **Drain:** `ssh '... civmctl maintenance enter --execute'`; se
   `$LASTEXITCODE -ne 0` → `throw` (não desliga a VM — DT-v2-17).
5. **Headroom guard — fase 2 (pós-drain, pré-shutdown):** re-loop
   `idle-check` até idle (deixa job em voo terminar), relê métricas; se
   `v_free_gb < headroom` → `ssh '... maintenance exit --execute'`
   (restaura) e aborta com `headroom_check_failed_after_drain`
   (DT-v2-3).
6. **`fstrim` + shutdown:** `ssh 'sudo fstrim -av'` → `ssh 'sudo shutdown -h now'`;
   aguarda `Get-VM Off` (timeout 120 s).
7. **Compactação:** `Optimize-VHD -Path <vhdx> -Mode Full -ErrorAction Stop`
   (timeout 1800 s).
8. **`finally` (sempre executa):** `Start-VM` com **3 tentativas ×
   (timeout 60 s, sleep 10 s)**. Se a VM ficar Running → `ssh
   '... maintenance exit --execute'`. Se **não** ficar Running após 3
   tentativas → log `CRITICAL vm_left_off` + `exit 1`, **sem** chamar
   `maintenance exit` (VM Off vira sinal de intervenção manual —
   DT-v2-1/15). Libera `V:\civm-optimize.lock`.

Timeouts concretos (DT-v2-1): shutdown-wait 120 s · `Optimize-VHD`
1800 s · `Start-VM` 60 s/tentativa · SSH 30 s.

Watchdog independente (`civm-vhdx-optimize-watchdog`, a cada 5 min,
SYSTEM — DT-v2-7): se a VM está `Off` **e** nenhuma instância de
`civm-vhdx-optimize` está rodando → `Start-VM` incondicional + log. É a
rede de segurança caso a task principal seja morta (`SIGKILL`) com a VM
desligada.

> **SUPERSEDED (2026-06-17).** Este watchdog está **Disabled por design**:
> ele relançaria a VM que o orchestrator deliberadamente mantém `Off`
> entre rajadas, brigando com o dono do power-state. O próprio
> orchestrator já religa a VM sob demanda (job na fila no próximo tick),
> então a rede de segurança "VM Off órfã → Start-VM" agora é dele. Só
> reabilite junto com o `civm-vhdx-optimize` no fluxo break-glass acima
> (com o orchestrator pausado).

Disparo manual numa janela:

```powershell
# 1. Confirmar saúde e headroom ANTES
(Get-Service vmms).Status                       # deve ser Running
ssh gha-ubuntu-2404 'civmctl host-disk --json'  # ver VFreeGB / FreeHeadroomViolation

# 2. Disparar a task (ela faz drain + headroom 2-fases + Optimize-VHD + Start-VM)
schtasks /run /tn civm-vhdx-optimize

# 3. Acompanhar
schtasks /query /tn civm-vhdx-optimize /v /fo LIST
Get-VM gha-ubuntu-2404 | Select-Object Name, State, Status

# 4. Confirmar resultado
Get-VHD -Path <vhdx> | Select-Object FileSize, Size, MinimumSize
ssh gha-ubuntu-2404 'civmctl host-disk --json'
```

> **Primeira execução (calibração do headroom):** rode com
> `Start-Transcript` e poll de `v_free` a cada 5 s, registrando o
> **low-water mark** do scratch durante o `Optimize-VHD`. Em 2026-05-31,
> o host Day-0 foi calibrado com `DefaultHostVolumeHeadroomGB=8`, porque
> `V:` tem `119,24 GiB`; desde 30/07/2026, o VHDX max tem `40 GiB` e o
> volume não pode ser expandido
> (`Get-PartitionSupportedSize -DriveLetter V` mostrou `SizeMax` igual ao
> tamanho atual). Rollback trigger: se o low-water real chegar a `<=8 GB`,
> eleve a constante ou mova/expanda o volume antes de reabilitar a task
> (SPECv2 DT-v2-19).

## Maintenance drain handshake

`civmctl maintenance enter|exit` é o handshake que esvazia o guest antes
de desligá-lo e o reativa depois (SPECv2 ITEM-4 override / DT-v2-4/14).
Ambos são **idempotentes** e retornam `(State, error)`:

- `enter` adquire `flock(/var/lib/civm/maintenance.lock)`, checa
  `idle.Check` (se busy e não forçado → erro), e para **ambos**: as
  units `systemctl` dos runners **e** remove o label `civm` dos repos
  (`gh api --method DELETE .../labels/civm`). Falha individual de
  `systemctl`/`gh` → **WARN + continua**; erro só se **ambos** falharem
  em **todos** os runners. Grava `State` só após drenar. Re-run com
  `State` existente → no-op (atualiza `drained_at`).
- `exit` lê `State` (ausente → no-op idempotente), restaura **apenas o
  que `State` registra** (`Stopped`→`systemctl start`,
  `LabelRemoved`→`gh ... POST labels`), e deleta `State` (falha de delete
  → erro).

Uso direto no guest (fora da task, ex.: manutenção manual):

```bash
# Drenar (parar de aceitar jobs + remover label civm)
ssh gha-ubuntu-2404 'civmctl maintenance enter --execute'

# Confirmar ocioso antes de desligar
ssh gha-ubuntu-2404 'civmctl idle-check'   # exit 0 = idle

# ... fazer a manutenção (ex.: shutdown + Optimize-VHD no host) ...

# Reativar (re-subir units + re-criar label civm)
ssh gha-ubuntu-2404 'civmctl maintenance exit --execute'
```

> A VM **só** é desligada após `idle-check` retornar idle. Drain nunca
> mata job vivo: `enter` checa `idle.Check`
> (`internal/idle/idle.go:68-98`, SPECv2 DT-v2-18) e a task faz re-loop
> de `idle-check` antes do `shutdown` (DT-v2-3).

## Procedimento SCSI / discard (ALAVANCA B)

Operação **manual/escriptada de host numa janela** (SPECv2 §Procedimento
SCSI / DT-v2-12/23), disparada quando `disk-doctor` aponta IDE ou
`TRIM not advertised`. **Pré-requisito:** `/etc/fstab` do guest por
`UUID=` (não `/dev/sdX`), porque o re-attach pode mudar o device
(`sda`→`sdb`); o UUID mantém o boot.

```text
1. Guest: sudo blkid && cat /etc/fstab   # confirmar que / usa UUID= (não /dev/sdX). Se não, trocar p/ UUID ANTES.
2. Guest: civmctl disk-doctor --json      # registrar controller/discard/DISC-MAX (baseline)
3. Drenar: civmctl maintenance enter --execute; aguardar idle; sudo shutdown -h now
4. Host (elevado): aguardar Get-VM Off; então:
   Remove-VMHardDiskDrive -VMName gha-ubuntu-2404 -ControllerType IDE -ControllerNumber 0 -ControllerLocation 0
   Add-VMScsiController   -VMName gha-ubuntu-2404
   Add-VMHardDiskDrive    -VMName gha-ubuntu-2404 -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 0 -Path <vhdx>
   Start-VM -Name gha-ubuntu-2404
5. Guest (boot manual): lsblk; civmctl disk-doctor --json  # trim_effective deve ser true; device pode ter mudado (sda->sdb) — fstab por UUID mantém boot.
6. civmctl maintenance exit --execute
7. Validar auto-shrink: §Rollback trigger (3 medições).
ROLLBACK: se boot falhar, reverter para IDE (Remove SCSI + Add IDE) em janela.
```

`disk-doctor` resolve o device via `findmnt --json` no `RootPath` e
classifica `root_cause` nesta ordem (SPECv2 DT-v2-10): (1) device não
montado em `RootPath` → erro; (2) mount sem `discard` →
`discard disabled on mount`; (3) controlador IDE →
`IDE controller does not propagate UNMAP`; (4) `DISC-MAX==0` →
`TRIM not advertised`; (5) `DISC-MAX>0` →
`TRIM supported, online shrink expected`. O delta-test (alocar 100 MB →
liberar → `fstrim` → medir) é **opt-in** via `--reference-test`.

## Headroom guard (por que existe)

`DefaultHostVolumeHeadroomGB` é o mínimo de `V:` livre exigido **antes**
do `Optimize-VHD`; abaixo disso a task aborta sem zero-fill (folga para o
crescimento temporário do VHDX durante a compactação — SPECv2 DT-v2-24).
Valor Day-0 atual: **8 GB**.
O guard é de **duas fases** (DT-v2-3): uma no início e outra **após o
drain e antes do shutdown**, porque um job pode ter consumido `V:` no
intervalo. Se a fase 2 falhar, a task restaura o guest
(`maintenance exit --execute`) e aborta — nunca segue para o shutdown
com headroom insuficiente.

## Rollback / sinais de reverter

Sempre que algo der errado, o invariante é: **a VM volta a rodar**. O
`finally` da task força `Start-VM` (3 tentativas) e o watchdog religa
VM `Off` órfã a cada 5 min (SPECv2 DT-v2-7).

### Reverter o re-attach SCSI

Se o boot pós-SCSI falhar, reverter para IDE numa janela:

```powershell
# Guest desligado (Get-VM Off):
Remove-VMHardDiskDrive -VMName gha-ubuntu-2404 -ControllerType SCSI -ControllerNumber 0 -ControllerLocation 0
Add-VMHardDiskDrive    -VMName gha-ubuntu-2404 -ControllerType IDE  -ControllerNumber 0 -ControllerLocation 0 -Path <vhdx>
Start-VM -Name gha-ubuntu-2404
```

### Validar auto-shrink (3 medições) — fecha DT-v2-20

Após a ALAVANCA B, repetir **3 rodadas**:

```text
1. Guest: liberar 50 GB (criar arquivo de teste e remover, ou apagar artefatos)
2. Host:  Get-VHD -Path <vhdx> | Select-Object FileSize   # baseline
3. Guest: sudo fstrim -av
4.        esperar 10 s
5. Host:  Get-VHD -Path <vhdx> | Select-Object FileSize   # de novo
```

Se o `FileSize` **não** cair ≈50 GB (±10%) nas 3 rodadas → reverter a
ALAVANCA B (SCSI→IDE) e investigar.

### Gatilhos de rollback (SPECv2 §Rollback trigger v2)

> **Nota (2026-06-17):** estes gatilhos descrevem o mecanismo antigo
> (`civm-vhdx-optimize`/autoreclaim). Os pisos vivos hoje são os do
> orchestrator — **panic 18 GB** (compacta mesmo com job ativo) e
> **warn 28 GB** (limpeza online segura), não os `10/30 GB` de
> observabilidade abaixo. Para os gatilhos de rollback em vigor, ver
> `docs/specs/orchestrator-scale-to-zero/SPEC.md`. Os itens abaixo só
> valem no fluxo break-glass com o orchestrator pausado.

- A task `civm-vhdx-optimize` deixar a VM `Off` ≥1 vez (`CRITICAL
  vm_left_off`).
- `v_free_gb` cruzar **10 GB** sem alarme prévio de **30 GB** (a
  observabilidade deveria ter avisado em `warn` a 30 GB e `crit` a
  10 GB). _(Mecanismo antigo; o orchestrator usa warn 28 / panic 18.)_
- `Start-VM` falhar nas 3 tentativas (intervenção manual imediata).

## Sequência copy-paste (caso comum: V: cheio, guest com livre)

```bash
# --- No GUEST (via SSH a partir do host ou do laptop do operador) ---

# 1. Diagnosticar: por que o V: está cheio?
ssh gha-ubuntu-2404 'civmctl host-disk --json'      # crit por FreeHeadroomViolation?
ssh gha-ubuntu-2404 'civmctl disk-doctor --json'    # root_cause?

# 2a. Se root_cause = "TRIM supported, online shrink expected" -> ALAVANCA A (online):
ssh gha-ubuntu-2404 'sudo fstrim -av'
ssh gha-ubuntu-2404 'civmctl host-disk --json'      # VFreeGB subiu? então acabou.

# 2b. Se root_cause aponta IDE / "TRIM not advertised" -> ALAVANCA B (janela, host): ver §Procedimento SCSI.

# 2c. Se TRIM ok mas VHDX continua grande -> ALAVANCA C (offline):
```

```powershell
# --- No HOST (PowerShell elevado), ALAVANCA C ---

# 3. Pré-checagem
(Get-Service vmms).Status                            # Running?
Get-Content V:\civm-host-metrics.json | ConvertFrom-Json

# 4. Disparar a compactação supervisionada (drain + headroom + Optimize-VHD + Start-VM no finally)
schtasks /run /tn civm-vhdx-optimize

# 5. Acompanhar até a VM voltar Running
Get-VM gha-ubuntu-2404 | Select-Object Name, State, Status

# 6. Confirmar shrink e headroom restaurado
Get-VHD -Path <vhdx> | Select-Object FileSize, Size
ssh gha-ubuntu-2404 'civmctl host-disk --json'       # level=ok esperado
```

```bash
# 7. Confirmar que o guest voltou a aceitar jobs (a task chama maintenance exit no finally)
ssh gha-ubuntu-2404 'civmctl maintenance exit --execute'   # idempotente (no-op se já restaurado)
ssh gha-ubuntu-2404 'civmctl capacity --json'              # accepting_jobs=true esperado
```

## Auditoria de privilégio das Scheduled Tasks

Todas as tasks rodam como **SYSTEM** com direito Hyper-V e SSH outbound
para o guest usando chave local em `C:\ProgramData\civm\ssh`. Nenhum
segredo fica no repo (SPECv2 DT-v2-6). Registradas por
`deploy/windows/register-*.ps1` com `schtasks /create ... /ru SYSTEM
/rl HIGHEST /f`. Reversível por `schtasks /delete`.

**Estado esperado das tasks (2026-08-02, após cutover F4):**

| Task | Estado esperado | Papel |
| --- | --- | --- |
| `civm-host-orchestrator` | **Ready/Running** (~2 min) | **Dono C# ativo** do power-state, fila e compactação |
| `civm-vm-orchestrator` | **Disabled** | Owner PowerShell mantido somente para rollback |
| `civm-host-metrics` | Ready (10 min) | Entrega de métricas do host (`V:` + `Get-VHD`) ao guest |
| `civm-watchdog` | Ready (20 min + startup) | Detecta owner zero/dual, heartbeat C# stale e latch de processo |
| `civm-vhdx-autoreclaim` | **Disabled** (superseded) | Reclaim antigo — subsumido pelo orchestrator |
| `civm-vhdx-optimize` | **Disabled** (break-glass) | Compactação offline manual — só com o orchestrator pausado |
| `civm-vhdx-optimize-watchdog` | **Disabled** (superseded) | Religaria a VM que o orchestrator mantém Off de propósito |

A task ativa de reclaim é o `civm-host-orchestrator` — o PowerShell,
`autoreclaim`,
`optimize` e `optimize-watchdog` estão **Disabled por design** (um dono
só do stop/compact/power-state, fail-safe Kahneman #15). Uma auditoria
saudável vê o owner C# `Ready`/rodando, as quatro tasks antigas `Disabled` e
`civm-watchdog` em `OK`; ver `civm-host-metrics` `Ready` é esperado.

```powershell
# Verificar registro/saúde das tasks
schtasks /query /tn civm-host-orchestrator       /v /fo LIST   # dono ativo
schtasks /query /tn civm-vm-orchestrator         /v /fo LIST   # esperado: Disabled
schtasks /query /tn civm-host-metrics            /v /fo LIST
schtasks /query /tn civm-watchdog                /v /fo LIST
Get-Content V:\civm-watchdog-status.txt
schtasks /query /tn civm-vhdx-autoreclaim        /v /fo LIST   # esperado: Disabled
schtasks /query /tn civm-vhdx-optimize           /v /fo LIST   # esperado: Disabled
schtasks /query /tn civm-vhdx-optimize-watchdog  /v /fo LIST   # esperado: Disabled

# Remover (rollback de registro)
schtasks /delete /tn civm-vhdx-optimize /f
```

## Histórico

- **2026-08-02** — `civm-host-orchestrator` vira o owner ativo. O watchdog
  passa a validar owner único, heartbeat C# e latch de processo sem mutar tasks.
  A admissão ocorre por geração `<contexto>@<head_sha>` após cleanup guest,
  TRIM, compactação e broker-ready.
- **2026-06-17** — Orchestrator scale-to-zero subsume o stop+compact.
  O `civm-vm-orchestrator.ps1` virou o **único dono** do
  power-state da VM (liga sob demanda na fila; full clean + Stop-VM +
  `Optimize-VHD` na fronteira de PR ocioso e no panic de disco). As tasks
  `civm-vhdx-autoreclaim` e `civm-vhdx-optimize-watchdog` foram
  **desabilitadas** (um dono só, sem curadores em conflito disputando o
  `V:\civm-reclaim.lock`/power-state — fail-safe Kahneman #15); o
  `civm-vhdx-optimize` virou **break-glass-only** (rodar só com o
  orchestrator pausado). Pisos vivos: warn 28 GB / panic 18 GB. Fonte de
  verdade: `docs/specs/orchestrator-scale-to-zero/SPEC.md` +
  `deploy/windows/civm-vm-orchestrator.ps1`.
- **2026-05-31** — Autoreclaim promovido a prevenção automática: task a
  cada 30 min, chave SSH dedicada com private key exclusiva de SYSTEM em
  `C:\ProgramData\civm\ssh`,
  entrega de métricas via `sudo -n` em `/var/lib/civm` e guardas de
  threshold/gap/idle/fstrim antes de desligar a VM.
- **2026-05-29** — Primeira versão. Autoria a partir de
  `docs/specs/host-volume-reclamation/SPECv2.md` (PASSO 3 pendente):
  árvore de decisão fstrim/SCSI/Optimize-VHD, handshake de drain via
  `civmctl maintenance`, headroom guard de 2 fases, watchdog e rollback
  com `Start-VM` sempre garantido no `finally`.
