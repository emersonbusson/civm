<#
.SYNOPSIS
    Guarded, non-interactive offline compaction of the civm runner VHDX.

.DESCRIPTION
    Host-side reclaim for the Hyper-V VHDX backing the civm self-hosted runner
    guest. Implements the binding flow of
    docs/specs/host-volume-reclamation/SPECv2.md (ITEM-11 override, blockers
    DT-v2-1/2/3/7/15/16/17) and the §Procedimento SCSI re-attach.

    Flow (abort-safe):
      0.  Acquire an anti-concurrency lock on V:\civm-optimize.lock with
          FileShare::None; bail out (exit 1) if another instance holds it
          (DT-v2-2).
      0b. Refuse to run when the Hyper-V Management Service (vmms) is not
          Running (DT-v2-16).
      1.  Phase-1 headroom guard: read host metrics; if v_free_gb <
          MinHeadroomGB, log abort_headroom and exit 2 WITHOUT ever
          zero-filling (DT-v2-3, DT-3). The temporary VHDX growth during
          Optimize-VHD needs that slack on V:.
      2.  Drain the guest: ssh '... sudo -n civmctl maintenance enter --execute'.
          (sudo: /var/lib/civm/maintenance.lock is root-owned.) A
          drain failure ($LASTEXITCODE != 0) throws and MUST NOT power the VM
          off (DT-v2-17).
      2b. Phase-2 headroom guard: re-loop the guest idle-check until idle, then
          re-read metrics; if v_free_gb dropped below MinHeadroomGB after the
          drain, run 'maintenance exit --execute' to restore and throw
          headroom_check_failed_after_drain (DT-v2-3).
      3.  ssh 'sudo fstrim -av' so the discard map is flushed before compaction.
      4.  ssh 'sudo shutdown -h now'; wait for Get-VM State=Off (120 s timeout).
      5.  If the VHDX is NOT on a SCSI controller, re-attach it to SCSI so
          discard/TRIM (UNMAP) propagates, then Optimize-VHD -Mode Full
          (1800 s timeout). Re-attach uses the exact §Procedimento SCSI
          cmdlets.
      6.  FINALLY (always, even on error): Start-VM with up to 3 attempts
          (60 s wait each, 10 s between). When Running, run
          'maintenance exit --execute'. When NOT Running after 3 attempts,
          log CRITICAL vm_left_off and exit 1 WITHOUT calling maintenance exit
          (a powered-off VM is a manual-intervention signal — DT-v2-1). The
          lock is always released.

    A crash must never leave the VM powered off: the FINALLY block always
    attempts Start-VM (SPECv2 §Mapa Kahneman #5, abort trigger "VM ficar Off").

.PARAMETER VMName
    Hyper-V VM name of the runner guest. Default gha-ubuntu-2404.

.PARAMETER VhdxPath
    Absolute path to the VHDX file to compact (on V:).

.PARAMETER MinHeadroomGB
    Minimum free GB required on V: BEFORE Optimize-VHD (both guard phases).
    Mirrors civm.DefaultHostVolumeHeadroomGB (8). Below this the script aborts
    without zero-fill.

.PARAMETER GuestSshTarget
    SSH destination for the guest. Default = emdev@gha-ubuntu-2404 so SYSTEM
    tasks do not depend on an interactive user's SSH config.

.NOTES
    NON-DISRUPTIVE ALTERNATIVE: when the VHDX already sits on a SCSI controller
    with discard enabled in the guest (RF-2 done), online `sudo fstrim -av` in
    the guest shrinks the VHDX with no downtime via the existing hook. This
    offline Optimize-VHD path is the fallback used only when online discard is
    not yet effective or to reclaim residual slack. Run it ONLY in a supervised
    maintenance window, never during Windows Update, and alert if it does not
    complete within 2 h (DT-v2-16).

    Privilege: registered as a SYSTEM Scheduled Task with the Hyper-V right,
    V: access, and outbound SSH through the dedicated local key under
    C:\ProgramData\civm\ssh; no repo secret, no interactive trigger (see
    register-civm-vhdx-optimize.ps1).
#>
[CmdletBinding(SupportsShouldProcess)]
param(
    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$VMName = 'gha-ubuntu-2404',

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$VhdxPath = 'V:\Hyper-V\gha-ubuntu-2404\Virtual Hard Disks\gha-ubuntu-2404.vhdx',

    [Parameter()]
    [ValidateRange(1, 4096)]
    [int]$MinHeadroomGB = 8,

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$GuestSshTarget = 'emdev@gha-ubuntu-2404',

    [Parameter()]
    [string]$SshKeyPath = 'C:\ProgramData\civm\ssh\id_ed25519',

    [Parameter()]
    [string]$KnownHostsPath = 'C:\ProgramData\civm\ssh\known_hosts'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

# --- Constants (mirror SPECv2 DT-v2-1 concrete timeouts and host paths) -------
$LockPath        = 'V:\civm-optimize.lock'
$ReclaimLockPath = 'V:\civm-reclaim.lock'         # SPECv3 DT-v3-3 exclusao mutua
$LogPath         = 'V:\civm-hyperv-maintenance.log'
$MetricsPath     = 'V:\civm-host-metrics.json'   # civm.DefaultHostMetricsFileNameOnHost
$ShutdownWaitSec = 120                            # DT-v2-1 shutdown-wait
$OptimizeWaitSec = 1800                           # DT-v2-1 Optimize-VHD
$StartWaitSec    = 60                             # DT-v2-15 Start-VM per attempt
$StartAttempts   = 3                              # DT-v2-1 / DT-v2-15
$StartSleepSec   = 10                             # DT-v2-1 sleep between attempts
$IdleWaitSec     = 600                            # re-loop idle-check ceiling
$SshTimeoutSec   = 30                             # DT-v2-1 SSH

# --- Structured logging to V:\civm-hyperv-maintenance.log + stdout -----------
# Events match SPEC.md §Eventos/log: optimize_start, optimize_end,
# abort_headroom, vm_restarted_on_error, drain_enter, drain_exit, plus the
# DT-v2 signals vm_left_off / already-running / vmms_down.
function Write-CivmLog {
    param(
        [Parameter(Mandatory = $true)][string]$Event,
        [Parameter()][hashtable]$Data = @{},
        [Parameter()][ValidateSet('INFO', 'WARN', 'ERROR', 'CRITICAL')][string]$Level = 'INFO'
    )
    $record = [ordered]@{
        timestamp = (Get-Date).ToUniversalTime().ToString('o')
        level     = $Level
        event     = $Event
        vm        = $VMName
    }
    foreach ($key in $Data.Keys) { $record[$key] = $Data[$key] }
    $line = ($record | ConvertTo-Json -Compress -Depth 6)
    try {
        Add-Content -LiteralPath $LogPath -Value $line -Encoding UTF8 -ErrorAction Stop
    } catch {
        # Never let logging failure mask the operation; surface to stderr only.
        Write-Error "log write failed: $($_.Exception.Message)"
    }
    if ($Level -eq 'ERROR' -or $Level -eq 'CRITICAL') {
        Write-Error "$Event $line"
    } else {
        Write-Host "$Event $line"
    }
}

# --- Guest SSH helper: bounded, fail-loud --------------------------------------
# Runs `ssh -o ConnectTimeout=... <target> <remoteCommand>` and returns
# stdout. Caller decides what a non-zero exit means; SPECv2 DT-v2-17 requires
# the *drain* path to throw on $LASTEXITCODE != 0, but never power the VM off.
function Invoke-GuestSsh {
    param(
        [Parameter(Mandatory = $true)][string]$RemoteCommand
    )
    $sshDir = Split-Path -Parent $KnownHostsPath
    if (-not [string]::IsNullOrWhiteSpace($sshDir) -and -not (Test-Path -LiteralPath $sshDir)) {
        New-Item -ItemType Directory -Path $sshDir -Force | Out-Null
    }
    $sshArgs = @(
        '-o', "ConnectTimeout=$SshTimeoutSec",
        '-o', 'BatchMode=yes',
        '-o', 'StrictHostKeyChecking=accept-new',
        '-o', "UserKnownHostsFile=$KnownHostsPath"
    )
    if (-not [string]::IsNullOrWhiteSpace($SshKeyPath)) {
        $sshArgs += @('-o', 'IdentitiesOnly=yes', '-i', $SshKeyPath)
    }
    $sshArgs += @($GuestSshTarget, $RemoteCommand)
    # OpenSSH maps remote stderr to PowerShell's error stream. With the script's
    # global ErrorActionPreference=Stop, an informational civmctl warning would
    # otherwise terminate ssh mid-command before we can inspect LASTEXITCODE.
    $oldPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    $output = $null
    $exitCode = 255
    try {
        $output = & ssh @sshArgs 2>&1
        $exitCode = $LASTEXITCODE
    } finally {
        $ErrorActionPreference = $oldPreference
    }
    return [pscustomobject]@{
        ExitCode = $exitCode
        Output   = ($output | Out-String).Trim()
    }
}

# --- Read host metrics JSON (phase-1 / phase-2 headroom guard input) ----------
function Get-VFreeGB {
    if (-not (Test-Path -LiteralPath $MetricsPath)) {
        throw "host metrics file absent: $MetricsPath"
    }
    $json = Get-Content -LiteralPath $MetricsPath -Raw -ErrorAction Stop | ConvertFrom-Json
    if ($null -eq $json.v_free_gb) {
        throw "host metrics missing v_free_gb: $MetricsPath"
    }
    return [int64]$json.v_free_gb
}

# --- Wait for the VM to reach a target power state (bounded) -------------------
function Wait-VMState {
    param(
        [Parameter(Mandatory = $true)][string]$State,
        [Parameter(Mandatory = $true)][int]$TimeoutSec
    )
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        $current = (Get-VM -Name $VMName -ErrorAction Stop).State
        if ($current -eq $State) { return $true }
        Start-Sleep -Seconds 2
    }
    return $false
}

# --- Re-attach the VHDX to a SCSI controller if it is not already there -------
# Exact §Procedimento SCSI cmdlets: Remove the current (IDE) drive, ensure a
# SCSI controller exists, Add the VHDX on SCSI 0/0. UNMAP/TRIM only propagates
# through SCSI, so discard-based online shrink requires this layout.
function Convert-VhdxToScsi {
    $current = Get-VMHardDiskDrive -VMName $VMName -ErrorAction Stop |
        Where-Object { $_.Path -eq $VhdxPath }
    if ($null -eq $current) {
        throw "VHDX $VhdxPath not attached to VM $VMName"
    }
    if ($current.ControllerType -eq 'SCSI') {
        Write-CivmLog -Event 'scsi_already' -Data @{ controller = 'SCSI' }
        return
    }

    Write-CivmLog -Event 'scsi_reattach_start' -Data @{
        from_controller = "$($current.ControllerType)"
        from_number     = $current.ControllerNumber
        from_location   = $current.ControllerLocation
    } -Level 'WARN'

    if ($PSCmdlet.ShouldProcess($VhdxPath, "Re-attach to SCSI controller")) {
        # 1. Remove from its current (IDE) controller.
        Remove-VMHardDiskDrive `
            -VMName $VMName `
            -ControllerType $current.ControllerType `
            -ControllerNumber $current.ControllerNumber `
            -ControllerLocation $current.ControllerLocation `
            -ErrorAction Stop

        # 2. Ensure a SCSI controller exists (idempotent: add only if none).
        $scsi = @(Get-VMScsiController -VMName $VMName -ErrorAction SilentlyContinue)
        if ($scsi.Count -eq 0) {
            Add-VMScsiController -VMName $VMName -ErrorAction Stop
        }

        # 3. Add the VHDX back on SCSI 0/0.
        Add-VMHardDiskDrive `
            -VMName $VMName `
            -ControllerType SCSI `
            -ControllerNumber 0 `
            -ControllerLocation 0 `
            -Path $VhdxPath `
            -ErrorAction Stop
    }

    Write-CivmLog -Event 'scsi_reattach_done' -Data @{ controller = 'SCSI' }
}

# =============================================================================
#  Entry point
# =============================================================================

# 0b. Hyper-V Management Service must be up (DT-v2-16).
$vmms = Get-Service -Name vmms -ErrorAction SilentlyContinue
if ($null -eq $vmms -or $vmms.Status -ne 'Running') {
    Write-CivmLog -Event 'vmms_down' -Level 'ERROR' -Data @{
        status = if ($null -eq $vmms) { 'absent' } else { "$($vmms.Status)" }
    }
    exit 1
}

# 0. Anti-concurrency lock (FileShare::None) — DT-v2-2.
$lockStream = $null
try {
    $lockStream = [System.IO.FileStream]::new(
        $LockPath,
        [System.IO.FileMode]::OpenOrCreate,
        [System.IO.FileAccess]::ReadWrite,
        [System.IO.FileShare]::None)
} catch {
    Write-CivmLog -Event 'already-running' -Level 'WARN' -Data @{ lock = $LockPath }
    exit 1
}

# 0c. Canonical shared reclaim lock (SPECv3 DT-v3-3): mutual exclusion with
# civm-vhdx-autoreclaim so the two reclaimers never Stop-VM / Optimize the same
# VHDX concurrently. Held FileShare::None; released in finally.
$reclaimLockStream = $null
try {
    $reclaimLockStream = [System.IO.FileStream]::new(
        $ReclaimLockPath,
        [System.IO.FileMode]::OpenOrCreate,
        [System.IO.FileAccess]::ReadWrite,
        [System.IO.FileShare]::None)
} catch {
    Write-CivmLog -Event 'reclaim_skip_other_active' -Level 'WARN' -Data @{ lock = $ReclaimLockPath }
    $lockStream.Close(); $lockStream.Dispose()
    Remove-Item -LiteralPath $LockPath -Force -ErrorAction SilentlyContinue
    exit 0
}

# vmLeftOff escalates the FINALLY exit code: when true we must NOT run
# maintenance exit and must exit non-zero (DT-v2-1).
$vmLeftOff = $false
$drained   = $false

try {
    # 1. Phase-1 headroom guard. NEVER zero-fill below headroom (DT-v2-3).
    $vFreeBefore = Get-VFreeGB
    if ($vFreeBefore -lt $MinHeadroomGB) {
        Write-CivmLog -Event 'abort_headroom' -Level 'WARN' -Data @{
            phase        = 1
            v_free_gb    = $vFreeBefore
            headroom_gb  = $MinHeadroomGB
        }
        # Release the lock via finally; explicit exit 2 = headroom abort.
        exit 2
    }

    # 2. Drain the guest. A drain failure must NOT power off the VM (DT-v2-17).
    Write-CivmLog -Event 'drain_enter' -Data @{ target = $GuestSshTarget }
    $enter = Invoke-GuestSsh -RemoteCommand 'sudo -n civmctl maintenance enter --execute'
    if ($enter.ExitCode -ne 0) {
        throw "maintenance enter failed (exit $($enter.ExitCode)): $($enter.Output)"
    }
    $drained = $true

    # 2b. Re-loop idle-check until idle, then phase-2 headroom guard.
    $idleDeadline = (Get-Date).AddSeconds($IdleWaitSec)
    $isIdle = $false
    while ((Get-Date) -lt $idleDeadline) {
        $idle = Invoke-GuestSsh -RemoteCommand 'civmctl idle-check'
        if ($idle.ExitCode -eq 0) { $isIdle = $true; break }
        Start-Sleep -Seconds 10
    }
    if (-not $isIdle) {
        throw 'guest did not reach idle before timeout; refusing to shut down a busy runner'
    }

    $vFreeAfterDrain = Get-VFreeGB
    if ($vFreeAfterDrain -lt $MinHeadroomGB) {
        Write-CivmLog -Event 'abort_headroom' -Level 'WARN' -Data @{
            phase       = 2
            v_free_gb   = $vFreeAfterDrain
            headroom_gb = $MinHeadroomGB
        }
        throw 'headroom_check_failed_after_drain'
    }

    # 3. Flush the discard map before compaction.
    Write-CivmLog -Event 'fstrim_start'
    $trim = Invoke-GuestSsh -RemoteCommand 'sudo fstrim -av'
    if ($trim.ExitCode -ne 0) {
        Write-CivmLog -Event 'fstrim_warn' -Level 'WARN' -Data @{ output = $trim.Output }
    }

    # 4. Graceful shutdown, then wait for Off (bounded).
    Write-CivmLog -Event 'shutdown_start'
    $shutdown = Invoke-GuestSsh -RemoteCommand 'sudo shutdown -h now'
    # shutdown -h drops the SSH connection; a non-zero exit here is expected.
    if (-not (Wait-VMState -State 'Off' -TimeoutSec $ShutdownWaitSec)) {
        throw "VM did not reach Off within ${ShutdownWaitSec}s"
    }
    Write-CivmLog -Event 'vm_off'

    # 5. Ensure SCSI layout (UNMAP path), then Optimize-VHD -Mode Full.
    Convert-VhdxToScsi

    $vhd = Get-VHD -Path $VhdxPath -ErrorAction Stop
    Write-CivmLog -Event 'optimize_start' -Data @{
        vhdx              = $VhdxPath
        file_size_gb      = [math]::Round($vhd.FileSize / 1GB, 2)
        v_free_gb_before  = $vFreeBefore
    }

    # SPECv3 DT-v3-2: campanha de medicao. O scratch high-water deste run alimenta
    # o ScratchBudget que admite (ou nao) o caminho de emergencia do autoreclaim.
    $scratchHighWaterGB = $null
    if ($PSCmdlet.ShouldProcess($VhdxPath, "Optimize-VHD -Mode Full")) {
        # Baseline ao vivo (Get-PSDrive, NUNCA o JSON de 10 min): a medicao de
        # scratch exige o numero ao vivo (red-team Finding 3).
        $liveFreeBeforeGB = [math]::Round((Get-PSDrive V -ErrorAction Stop).Free / 1GB, 2)
        $lowWaterGB = $liveFreeBeforeGB

        $optJob = Start-Job -ScriptBlock {
            param($path)
            Optimize-VHD -Path $path -Mode Full -ErrorAction Stop
        } -ArgumentList $VhdxPath

        # Poll de 1s: amostra o MENOR V: livre durante a compactacao. NAO aborta
        # nada (telemetria, nao controle de seguranca — DT-v3-5); Optimize-VHD e
        # ininterruptivel. So registra o scratch high-water para recalibrar o
        # ScratchBudget. O timeout segue como antes.
        $optDeadline = (Get-Date).AddSeconds($OptimizeWaitSec)
        while ($null -eq (Wait-Job -Job $optJob -Timeout 1)) {
            $nowFreeGB = [math]::Round((Get-PSDrive V -ErrorAction SilentlyContinue).Free / 1GB, 2)
            if ($nowFreeGB -gt 0 -and $nowFreeGB -lt $lowWaterGB) { $lowWaterGB = $nowFreeGB }
            if ((Get-Date) -ge $optDeadline) {
                Stop-Job -Job $optJob -ErrorAction SilentlyContinue
                Remove-Job -Job $optJob -Force -ErrorAction SilentlyContinue
                throw "Optimize-VHD exceeded ${OptimizeWaitSec}s"
            }
        }
        Receive-Job -Job $optJob -ErrorAction Stop | Out-Null
        Remove-Job -Job $optJob -Force -ErrorAction SilentlyContinue
        $scratchHighWaterGB = [math]::Round($liveFreeBeforeGB - $lowWaterGB, 2)
    }

    $vhdAfter = Get-VHD -Path $VhdxPath -ErrorAction Stop
    Write-CivmLog -Event 'optimize_end' -Data @{
        vhdx                  = $VhdxPath
        file_size_gb_after    = [math]::Round($vhdAfter.FileSize / 1GB, 2)
        reclaimed_gb          = [math]::Round(($vhd.FileSize - $vhdAfter.FileSize) / 1GB, 2)
        scratch_high_water_gb = $scratchHighWaterGB
    }
} catch {
    # Log the failure; the FINALLY block still guarantees Start-VM.
    Write-CivmLog -Event 'optimize_error' -Level 'ERROR' -Data @{
        error = $_.Exception.Message
    }
} finally {
    # ---- GUARANTEED Start-VM (DT-v2-1 / DT-v2-15): a crash must never leave
    # ---- the VM powered off. Up to 3 attempts, 60 s wait each, 10 s between.
    $started = $false
    try {
        $state = (Get-VM -Name $VMName -ErrorAction Stop).State
    } catch {
        $state = 'Unknown'
    }

    if ($state -eq 'Running') {
        $started = $true
    } else {
        for ($attempt = 1; $attempt -le $StartAttempts -and -not $started; $attempt++) {
            $sw = [System.Diagnostics.Stopwatch]::StartNew()
            try {
                Start-VM -Name $VMName -ErrorAction Stop
                if (Wait-VMState -State 'Running' -TimeoutSec $StartWaitSec) {
                    $started = $true
                }
            } catch {
                Write-CivmLog -Event 'vm_restarted_on_error' -Level 'WARN' -Data @{
                    attempt = $attempt
                    error   = $_.Exception.Message
                }
            }
            $sw.Stop()
            Write-CivmLog -Event 'vm_state_after_start' -Data @{
                attempt    = $attempt
                running    = $started
                elapsed_ms = $sw.ElapsedMilliseconds
            }
            if (-not $started -and $attempt -lt $StartAttempts) {
                Start-Sleep -Seconds $StartSleepSec
            }
        }
    }

    if (-not $started) {
        # VM is Off after all attempts: manual-intervention signal. Do NOT call
        # maintenance exit (the guest is down). Exit non-zero (DT-v2-1).
        $vmLeftOff = $true
        Write-CivmLog -Event 'vm_left_off' -Level 'CRITICAL' -Data @{
            attempts = $StartAttempts
        }
    } elseif ($drained) {
        # VM is Running: lift the drain so the runner accepts jobs again.
        Write-CivmLog -Event 'drain_exit' -Data @{ target = $GuestSshTarget }
        $exit = Invoke-GuestSsh -RemoteCommand 'sudo -n civmctl maintenance exit --execute'
        if ($exit.ExitCode -ne 0) {
            Write-CivmLog -Event 'drain_exit_warn' -Level 'WARN' -Data @{
                exit_code = $exit.ExitCode
                output    = $exit.Output
            }
        }
    }

    # Always release the anti-concurrency lock.
    if ($null -ne $lockStream) {
        $lockStream.Close()
        $lockStream.Dispose()
        Remove-Item -LiteralPath $LockPath -Force -ErrorAction SilentlyContinue
    }
    # Release the canonical shared reclaim lock (SPECv3 DT-v3-3).
    if ($null -ne $reclaimLockStream) {
        $reclaimLockStream.Close()
        $reclaimLockStream.Dispose()
        Remove-Item -LiteralPath $ReclaimLockPath -Force -ErrorAction SilentlyContinue
    }
}

if ($vmLeftOff) {
    exit 1
}
exit 0
