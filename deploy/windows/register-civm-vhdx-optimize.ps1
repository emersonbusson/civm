<#
.SYNOPSIS
    Idempotent registrar for the civm-vhdx-optimize Scheduled Task and its
    watchdog.

.DESCRIPTION
    Registers the SYSTEM Scheduled Tasks defined in
    docs/specs/host-volume-reclamation/SPECv2.md (ITEM-15, DT-v2-6/7):

      * civm-vhdx-optimize           — runs civm-vhdx-optimize.ps1. Primary
        trigger is manual (`schtasks /run /tn civm-vhdx-optimize`) in a
        supervised maintenance window. A weekly off-hours trigger is added so
        the reclaim runs unattended when desired; the SPECv2 baseline uses
        `/sc onstart`, but a weekly window is the safer default for a runner
        host that is rarely rebooted. Both are SYSTEM / RL HIGHEST.

      * civm-vhdx-optimize-watchdog  — runs every 5 minutes. If the VM is Off
        AND no civm-vhdx-optimize instance is running, it unconditionally
        Start-VM (DT-v2-7). This guarantees a crashed reclaim that left the VM
        Off is recovered within 5 minutes.

    Idempotent: existing tasks of the same name are deleted (`/f`) and
    recreated, so re-running converges to the declared definition. Reversible
    with `schtasks /delete /tn <name> /f`.

    Privilege audit (DT-v2-6): both tasks run as SYSTEM with RL HIGHEST. They
    require only the local Hyper-V management right (Optimize-VHD / Start-VM /
    Get-VM), read/write on V:, and outbound SSH to the guest through the
    dedicated local key under C:\ProgramData\civm\ssh. The private key must be
    owned/readable only by SYSTEM for Windows OpenSSH. No repo secret is
    embedded; key management lives outside this registrar.

.PARAMETER ScriptDir
    Directory containing civm-vhdx-optimize.ps1. Defaults to this script's own
    directory so a checked-out deploy/windows tree is self-registering.

.PARAMETER VMName
    Hyper-V VM name passed to both scripts. Default gha-ubuntu-2404.

.PARAMETER MinHeadroomGB
    Headroom floor passed to civm-vhdx-optimize.ps1 (-MinHeadroomGB). Mirrors
    civm.DefaultHostVolumeHeadroomGB (8).

.PARAMETER WeeklyDay
    Day of week for the unattended weekly trigger. Default SUN.

.PARAMETER WeeklyTime
    HH:mm for the unattended weekly trigger (off-hours, low traffic; never
    during Windows Update — DT-v2-16). Default 03:00.

.EXAMPLE
    powershell -ExecutionPolicy Bypass -File .\register-civm-vhdx-optimize.ps1

.NOTES
    Run elevated (the schtasks /create for SYSTEM requires admin). Verify with:
        schtasks /query /tn civm-vhdx-optimize /v /fo LIST
        schtasks /query /tn civm-vhdx-optimize-watchdog /v /fo LIST
    Trigger manually with:
        schtasks /run /tn civm-vhdx-optimize
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$ScriptDir = '',

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$VMName = 'gha-ubuntu-2404',

    [Parameter()]
    [ValidateRange(1, 4096)]
    [int]$MinHeadroomGB = 8,

    [Parameter()]
    [ValidateSet('MON', 'TUE', 'WED', 'THU', 'FRI', 'SAT', 'SUN')]
    [string]$WeeklyDay = 'SUN',

    [Parameter()]
    [ValidatePattern('^\d{2}:\d{2}$')]
    [string]$WeeklyTime = '03:00',

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$PowerShellPath = "$env:SystemRoot\System32\WindowsPowerShell\v1.0\powershell.exe"
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$scriptRoot = if (-not [string]::IsNullOrWhiteSpace($PSScriptRoot)) {
    $PSScriptRoot
} else {
    Split-Path -Parent $MyInvocation.MyCommand.Path
}
if ([string]::IsNullOrWhiteSpace($ScriptDir)) {
    $ScriptDir = $scriptRoot
}

$OptimizeTaskName = 'civm-vhdx-optimize'
$WatchdogTaskName = 'civm-vhdx-optimize-watchdog'

$OptimizeScript = Join-Path $ScriptDir 'civm-vhdx-optimize.ps1'
$WatchdogScript = Join-Path $ScriptDir 'civm-vhdx-optimize-watchdog.ps1'

if (-not (Test-Path -LiteralPath $OptimizeScript)) {
    throw "optimize script not found: $OptimizeScript"
}

# The watchdog body is small and self-contained; materialize it next to the
# optimize script so a single deploy/windows checkout fully registers. It is
# the operational form of DT-v2-7.
$watchdogBody = @"
<#
.SYNOPSIS
    civm-vhdx-optimize-watchdog: religa a VM se ela ficou Off sem reclaim ativo.
.DESCRIPTION
    SPECv2 DT-v2-7. A cada 5 min (SYSTEM): se a VM esta Off E nenhuma manutencao
    de VHDX esta ativa, faz Start-VM e loga. Isso recupera um reclaim que crashou
    deixando a VM Off (invariante "VM nunca fica Off").

    "Manutencao ativa" e checada pelos DOIS locks de reclaim — civm-vhdx-optimize
    (civm-optimize.lock) E civm-vhdx-autoreclaim (civm-autoreclaim.lock) —, nao
    so pela Scheduled Task civm-vhdx-optimize. O autoreclaim e quem dispara a cada
    ciclo: ele para a VM pra Optimize-VHD e a religa no proprio finally. Religar a
    VM no meio desse Optimize batia em 0x80070020 ("arquivo ja esta sendo usado
    por outro processo") e era logado CRITICAL toda janela de reclaim. Um lock so
    conta como vivo se abrir FileShare::None lanca; um arquivo orfao (reclaim que
    morreu) abre livre e NAO impede a recuperacao da VM.
#>
[CmdletBinding()]
param(
    [string]`$VMName = '$VMName'
)
`$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
`$LogPath = 'V:\civm-hyperv-maintenance.log'

# Locks de manutencao de VHDX. Mirror das constantes em civm-vhdx-optimize.ps1
# (LockPath) e civm-vhdx-autoreclaim.ps1 (LockPath). Qualquer um vivo => um
# reclaim e dono do estado de energia da VM; o watchdog nao interfere.
`$MaintenanceLocks = @('V:\civm-reclaim.lock', 'V:\civm-optimize.lock', 'V:\civm-autoreclaim.lock')

function Write-WatchdogLog {
    param([string]`$Event, [hashtable]`$Data = @{}, [string]`$Level = 'INFO')
    `$record = [ordered]@{
        timestamp = (Get-Date).ToUniversalTime().ToString('o')
        level     = `$Level
        event     = `$Event
        vm        = `$VMName
    }
    foreach (`$k in `$Data.Keys) { `$record[`$k] = `$Data[`$k] }
    `$line = (`$record | ConvertTo-Json -Compress -Depth 6)
    try { Add-Content -LiteralPath `$LogPath -Value `$line -Encoding UTF8 -ErrorAction Stop } catch {}
    Write-Host "`$Event `$line"
}

# Vivo == abrir FileShare::None lanca (alguem segura). Ausente ou aberto livre
# (orfao de um reclaim que crashou) == nao impede a recuperacao da VM.
function Test-LockHeld {
    param([string]`$Path)
    if (-not (Test-Path -LiteralPath `$Path)) { return `$false }
    try {
        `$fs = [System.IO.FileStream]::new(`$Path, [System.IO.FileMode]::Open, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
        `$fs.Close(); `$fs.Dispose()
        return `$false
    } catch {
        return `$true
    }
}

function Get-HeldMaintenanceLock {
    foreach (`$lock in `$MaintenanceLocks) {
        if (Test-LockHeld -Path `$lock) { return `$lock }
    }
    return `$null
}

try {
    `$state = (Get-VM -Name `$VMName -ErrorAction Stop).State
} catch {
    Write-WatchdogLog -Event 'watchdog_get_vm_failed' -Level 'WARN' -Data @{ error = `$_.Exception.Message }
    exit 1
}

# RF-1 (SPEC docs/specs/host-volume-reclaim-liveness/SPECv2.md): limpa um
# autoreclaim FANTASMA antes da logica de VM-state — o fantasma ocorre com a VM
# Running. Task em Running, SEM processo vivo do script E com o lock orfao =>
# fantasma; o Stop-ScheduledTask libera o MultipleInstances=IgnoreNew para a
# proxima cadencia. Incidente 2026-06-15: ~30h bloqueado. Kahneman #13 (Running
# != funcao) e #16 (a cura nao pode morrer e auto-bloquear a ressurreicao).
# Exigir AMBOS (sem processo E sem lock) evita matar uma instancia recem-iniciada.
try {
    `$arTask = Get-ScheduledTask -TaskName 'civm-vhdx-autoreclaim' -ErrorAction Stop
    if (`$arTask.State -eq 'Running') {
        `$arProcAlive = `$null -ne (Get-CimInstance Win32_Process -ErrorAction SilentlyContinue |
            Where-Object { `$_.CommandLine -like '*civm-vhdx-autoreclaim.ps1*' })
        `$arLockHeld = Test-LockHeld 'V:\civm-autoreclaim.lock'
        if ((-not `$arProcAlive) -and (-not `$arLockHeld)) {
            Stop-ScheduledTask -TaskName 'civm-vhdx-autoreclaim' -ErrorAction SilentlyContinue
            Remove-Item -LiteralPath 'V:\civm-autoreclaim.lock', 'V:\civm-reclaim.lock' -Force -ErrorAction SilentlyContinue
            Write-WatchdogLog -Event 'reclaim_liveness_phantom_cleared' -Level 'WARN' -Data @{
                proc_alive = `$arProcAlive
                lock_held  = `$arLockHeld
            }
        }
    }
} catch {
    Write-WatchdogLog -Event 'reclaim_liveness_phantom_check_failed' -Level 'WARN' -Data @{ error = `$_.Exception.Message }
}

if (`$state -eq 'Running') { exit 0 }

# Manutencao viva (civm-optimize.lock OU civm-autoreclaim.lock): o reclaim religa
# a VM no proprio finally. Recuar.
`$heldLock = Get-HeldMaintenanceLock
if (`$null -ne `$heldLock) {
    Write-WatchdogLog -Event 'watchdog_skip_reclaim_active' -Data @{ vm_state = "`$state"; lock = `$heldLock }
    exit 0
}

# Defesa em profundidade: honrar tambem a Scheduled Task civm-vhdx-optimize
# rodando, caso o lock ainda nao tenha sido materializado.
`$reclaimRunning = `$false
try {
    `$task = Get-ScheduledTask -TaskName 'civm-vhdx-optimize' -ErrorAction Stop
    if ((`$task | Get-ScheduledTaskInfo).LastTaskResult -eq 267009) { `$reclaimRunning = `$true }  # 0x41301 = currently running
    if (`$task.State -eq 'Running') { `$reclaimRunning = `$true }
} catch {
    # Fall back to a process scan if the scheduled task is not queryable.
    `$reclaimRunning = `$null -ne (Get-Process -Name powershell -ErrorAction SilentlyContinue |
        Where-Object { `$_.CommandLine -like '*civm-vhdx-optimize.ps1*' -or `$_.CommandLine -like '*civm-vhdx-autoreclaim.ps1*' })
}

if (`$reclaimRunning) {
    Write-WatchdogLog -Event 'watchdog_skip_reclaim_active' -Data @{ vm_state = "`$state"; reason = 'optimize-task' }
    exit 0
}

Write-WatchdogLog -Event 'watchdog_vm_off_starting' -Level 'WARN' -Data @{ vm_state = "`$state" }
try {
    Start-VM -Name `$VMName -ErrorAction Stop
    Write-WatchdogLog -Event 'watchdog_start_vm_issued'
} catch {
    # 0x80070020 (ERROR_SHARING_VIOLATION): um reclaim agarrou o VHDX na janela
    # TOCTOU entre o check de lock e o Start-VM. Transiente — um tick de 5 min
    # depois recupera quando o lock soltar. NAO escalar pra CRITICAL.
    `$msg = `$_.Exception.Message
    if (`$msg -match '0x80070020' -or `$msg -match 'sendo usado por outro processo' -or `$msg -match 'being used by another process') {
        Write-WatchdogLog -Event 'watchdog_start_vm_skipped_busy' -Level 'WARN' -Data @{ error = `$msg }
        exit 0
    }
    Write-WatchdogLog -Event 'watchdog_start_vm_failed' -Level 'CRITICAL' -Data @{ error = `$msg }
    exit 1
}
exit 0
"@

if ($PSCmdlet.ShouldProcess($WatchdogScript, "Write watchdog script")) {
    Set-Content -LiteralPath $WatchdogScript -Value $watchdogBody -Encoding UTF8 -Force
    Write-Host "wrote watchdog script: $WatchdogScript"
}

# --- schtasks helpers ---------------------------------------------------------
function Remove-TaskIfPresent {
    param([string]$Name)
    $oldPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & schtasks.exe /query /tn $Name > $null 2> $null
        $exists = ($LASTEXITCODE -eq 0)
    } finally {
        $ErrorActionPreference = $oldPreference
    }
    if ($exists) {
        if ($PSCmdlet.ShouldProcess($Name, 'schtasks /delete')) {
            & schtasks.exe /delete /tn $Name /f | Out-Null
            if ($LASTEXITCODE -ne 0) { throw "schtasks /delete failed for $Name (exit $LASTEXITCODE)" }
            Write-Host "deleted existing task: $Name"
        }
    }
}

function New-Task {
    param(
        [string]$Name,
        [string]$Command,
        [string[]]$ScheduleArgs
    )
    $create = @(
        '/create',
        '/tn', $Name,
        '/tr', $Command,
        '/ru', 'SYSTEM',
        '/rl', 'HIGHEST',
        '/f'
    ) + $ScheduleArgs
    if ($PSCmdlet.ShouldProcess($Name, 'schtasks /create /ru SYSTEM /rl HIGHEST')) {
        & schtasks.exe @create
        if ($LASTEXITCODE -ne 0) {
            throw "schtasks /create failed for $Name (exit $LASTEXITCODE)"
        }
        Write-Host "registered task: $Name"
    }
}

# PowerShell command lines for schtasks /tr. -NonInteractive / -NoProfile keep
# the SYSTEM run deterministic; -ExecutionPolicy Bypass avoids policy blocks.
$resolvedOptimize = (Resolve-Path -LiteralPath $OptimizeScript).Path
# Under -WhatIf the watchdog file is not written, so fall back to its declared
# path when Resolve-Path cannot see it yet.
$resolvedWatchdog = if (Test-Path -LiteralPath $WatchdogScript) {
    (Resolve-Path -LiteralPath $WatchdogScript).Path
} else {
    $WatchdogScript
}
$optimizeCmd = '"{0}" -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "{1}" -MinHeadroomGB {2}' -f $PowerShellPath, $resolvedOptimize, $MinHeadroomGB
$watchdogCmd = '"{0}" -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "{1}" -VMName "{2}"' -f $PowerShellPath, $resolvedWatchdog, $VMName

# --- Register civm-vhdx-optimize (weekly + manual-trigger friendly) -----------
Remove-TaskIfPresent -Name $OptimizeTaskName
New-Task -Name $OptimizeTaskName -Command $optimizeCmd -ScheduleArgs @(
    '/sc', 'weekly',
    '/d', $WeeklyDay,
    '/st', $WeeklyTime
)
# Manual trigger remains the primary supervised path:
#   schtasks /run /tn civm-vhdx-optimize

# --- Register civm-vhdx-optimize-watchdog (every 5 minutes) -------------------
Remove-TaskIfPresent -Name $WatchdogTaskName
New-Task -Name $WatchdogTaskName -Command $watchdogCmd -ScheduleArgs @(
    '/sc', 'minute',
    '/mo', '5'
)

Write-Host ''
Write-Host 'Done. Verify with:'
Write-Host "  schtasks /query /tn $OptimizeTaskName /v /fo LIST"
Write-Host "  schtasks /query /tn $WatchdogTaskName /v /fo LIST"
Write-Host "Trigger reclaim manually in a maintenance window with:"
Write-Host "  schtasks /run /tn $OptimizeTaskName"
exit 0
