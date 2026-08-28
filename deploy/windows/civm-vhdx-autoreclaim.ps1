<#
.SYNOPSIS
    Frequent, guarded host V: autoreclaim for the civm runner VHDX.

.DESCRIPTION
    Runs as a SYSTEM Scheduled Task and keeps the Hyper-V host V: volume from
    silently drifting back into low-space territory. This is intentionally
    smaller than civm-vhdx-optimize.ps1: it does not enter maintenance mode or
    alter VM controllers. It only runs when there is host pressure and a real
    reclaimable VHDX gap, then waits for the guest to be idle, flushes discard
    with fstrim, performs offline Optimize-VHD, and always tries to start the
    VM again in finally.

    Abort-safe gates:
      - single-instance lock on V:\civm-autoreclaim.lock;
      - V: must be below ThresholdGB but above MinHeadroomGB;
      - VM must be Running before the operation starts;
      - guest SSH and idle-check must succeed before Stop-VM; fstrim is
        best-effort (discard is opportunistic — Optimize-VHD -Mode Full compacts
        free blocks offline regardless; a fstrim failure logs autoreclaim_fstrim_warn
        and proceeds, never blocks the #106 post-Off reclaim);
      - estimated VHDX slack must be >= MinReclaimableGB;
      - Start-VM is attempted up to 3 times in finally.

    Rollback trigger: if this task interrupts active CI, disable the scheduled
    task and keep only the supervised civm-vhdx-optimize maintenance path until
    the idle predicate is fixed.
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$VMName = 'gha-ubuntu-2404',

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$VhdxPath = 'V:\Hyper-V\gha-ubuntu-2404\Virtual Hard Disks\gha-ubuntu-2404.vhdx',

    [Parameter()]
    [ValidateRange(1, 4096)]
    [int]$ThresholdGB = 50,

    [Parameter()]
    [ValidateRange(1, 4096)]
    [int]$MinHeadroomGB = 8,

    [Parameter()]
    [ValidateRange(1, 4096)]
    [int]$HardFloorGB = 1,

    [Parameter()]
    [ValidateRange(0, 4096)]
    # Day-0 default = DefaultHostVolumeScratchBudgetGB (internal/civm/civm.go): p100
    # scratch high-water observado (10) + 1. Emergencia HABILITADA; segura via gate
    # de duas fases (re-medicao pos-Stop-VM, RF-2/DT-1, #106). 0 desabilitaria.
    [int]$ScratchBudgetGB = 11,

    [Parameter()]
    [ValidateRange(1, 4096)]
    [int]$MinReclaimableGB = 8,

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$GuestSshTarget = 'emdev@gha-ubuntu-2404',

    [Parameter()]
    [string]$SshKeyPath = 'C:\ProgramData\civm\ssh\id_ed25519',

    [Parameter()]
    [string]$KnownHostsPath = 'C:\ProgramData\civm\ssh\known_hosts',

    [Parameter()]
    [ValidateRange(1, 600)]
    [int]$SshTimeoutSeconds = 30,

    [Parameter()]
    [ValidateRange(10, 86400)]
    [int]$IdleWaitSeconds = 600,

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$LockPath = 'V:\civm-autoreclaim.lock',

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$ReclaimLockPath = 'V:\civm-reclaim.lock',

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$LogPath = 'V:\civm-hyperv-maintenance.log',

    # Escalacao guest-unreachable (incidente 2026-06-10): com o guest wedgeado
    # (disco 0% livre, sshd sem banner) o autoreclaim falhava o guest df em TODO
    # tick e nunca agia — a pre-condicao de SSH virou self-deadlock e os runners
    # ficaram offline por dias. Apos GuestUnreachableLimit falhas CONSECUTIVAS
    # de guest df, o host forca Stop-VM -TurnOff + Start-VM, limitado pelo
    # cooldown (anti boot-loop). 0 desabilita a escalacao.
    [Parameter()]
    [ValidateRange(0, 100)]
    [int]$GuestUnreachableLimit = 3,

    [Parameter()]
    [ValidateRange(0.5, 168)]
    [double]$ForcedRebootCooldownHours = 6.0,

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$GuestFailStatePath = 'V:\civm-autoreclaim-guest-fail.json'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$GiB = [math]::Pow(1024, 3)
$StartAttempts = 3
$StartWaitSeconds = 90

function ConvertTo-GiB {
    param([double]$Bytes)
    return [math]::Round($Bytes / $GiB, 2)
}

function Write-ReclaimLog {
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
    try { Add-Content -LiteralPath $LogPath -Value $line -Encoding UTF8 -ErrorAction Stop } catch { }
    Write-Host "$Event $line"
}

function Get-VFreeGB {
    $drive = Split-Path -Qualifier $VhdxPath
    if ([string]::IsNullOrWhiteSpace($drive)) { $drive = 'V:' }
    $letter = $drive.TrimEnd(':')
    $psDrive = Get-PSDrive -Name $letter -ErrorAction Stop
    return (ConvertTo-GiB -Bytes ([double]$psDrive.Free))
}

function Get-SshArgs {
    param([string]$Target)
    $sshDir = Split-Path -Parent $KnownHostsPath
    if (-not [string]::IsNullOrWhiteSpace($sshDir) -and -not (Test-Path -LiteralPath $sshDir)) {
        New-Item -ItemType Directory -Path $sshDir -Force | Out-Null
    }
    $args = @(
        '-o', 'BatchMode=yes',
        '-o', "ConnectTimeout=$SshTimeoutSeconds",
        '-o', 'StrictHostKeyChecking=accept-new',
        '-o', "UserKnownHostsFile=$KnownHostsPath"
    )
    if (-not [string]::IsNullOrWhiteSpace($SshKeyPath)) {
        $args += @('-o', 'IdentitiesOnly=yes', '-i', $SshKeyPath)
    }
    $args += $Target
    return $args
}

function Invoke-Guest {
    param([Parameter(Mandatory = $true)][string]$RemoteCommand)
    try {
        $sshArgs = Get-SshArgs -Target $GuestSshTarget
        $output = & ssh @sshArgs $RemoteCommand 2>&1
        return [pscustomobject]@{
            ExitCode = $LASTEXITCODE
            Output   = ($output | Out-String).Trim()
        }
    } catch {
        return [pscustomobject]@{
            ExitCode = 255
            Output   = $_.Exception.Message
        }
    }
}

function Get-GuestFreeBytes {
    $result = Invoke-Guest -RemoteCommand "df -B1 --output=avail / | tail -n1 | tr -d '[:space:]'"
    if ($result.ExitCode -ne 0) {
        throw "guest df failed (exit $($result.ExitCode)): $($result.Output)"
    }
    $parsed = 0L
    if (-not [int64]::TryParse($result.Output, [ref]$parsed) -or $parsed -le 0) {
        throw "guest df returned an invalid value: $($result.Output)"
    }
    return $parsed
}

function Get-GuestFailState {
    $state = @{ consecutiveFailures = 0; lastForcedRebootUtc = '' }
    if (Test-Path -LiteralPath $GuestFailStatePath) {
        try {
            $raw = Get-Content -LiteralPath $GuestFailStatePath -Raw | ConvertFrom-Json
            if ($raw.PSObject.Properties['consecutiveFailures']) {
                $state.consecutiveFailures = [int]$raw.consecutiveFailures
            }
            if ($raw.PSObject.Properties['lastForcedRebootUtc']) {
                $state.lastForcedRebootUtc = [string]$raw.lastForcedRebootUtc
            }
        } catch { }
    }
    return $state
}

function Save-GuestFailState {
    param([Parameter(Mandatory = $true)][hashtable]$State)
    try {
        ($State | ConvertTo-Json -Compress) | Set-Content -LiteralPath $GuestFailStatePath -Encoding UTF8
    } catch { }
}

function Reset-GuestFailStreak {
    $state = Get-GuestFailState
    if ([int]$state.consecutiveFailures -ne 0) {
        $state.consecutiveFailures = 0
        Save-GuestFailState -State $state
    }
}

function Add-GuestFailStreak {
    $state = Get-GuestFailState
    $state.consecutiveFailures = [int]$state.consecutiveFailures + 1
    Save-GuestFailState -State $state
    return [int]$state.consecutiveFailures
}

# Invoke-GuestUnreachableForcedReboot is the host-side backstop for a wedged
# guest: in-guest watchdogs cannot run on a dead OS and every SSH-gated path
# here fail-closes forever (the 2026-06-10 self-deadlock). A hard power-cycle
# releases the VMRS, lets the guest boot clean (in-guest cleanup timers +
# runner services auto-start) and restores SSH. Bounded: only after a
# consecutive-failure streak and never more than once per cooldown window
# (anti boot-loop).
function Invoke-GuestUnreachableForcedReboot {
    param([Parameter(Mandatory = $true)][int]$Streak)
    $state = Get-GuestFailState
    if (-not [string]::IsNullOrWhiteSpace([string]$state.lastForcedRebootUtc)) {
        $last = [datetime]::MinValue
        if ([datetime]::TryParse([string]$state.lastForcedRebootUtc, [ref]$last)) {
            $sinceHours = ((Get-Date).ToUniversalTime() - $last.ToUniversalTime()).TotalHours
            if ($sinceHours -lt $ForcedRebootCooldownHours) {
                Write-ReclaimLog -Event 'autoreclaim_forced_reboot_capped' -Level 'CRITICAL' -Data @{
                    streak           = $Streak
                    since_last_hours = [math]::Round($sinceHours, 2)
                    cooldown_hours   = $ForcedRebootCooldownHours
                }
                return $false
            }
        }
    }
    Write-ReclaimLog -Event 'autoreclaim_forced_reboot' -Level 'WARN' -Data @{
        streak = $Streak
        reason = 'guest ssh unreachable; wedged guest cannot honor the SSH precondition'
    }
    Stop-VM -Name $VMName -TurnOff -Force -ErrorAction Stop
    if (-not (Wait-VMState -State 'Off' -TimeoutSeconds 120)) {
        throw 'forced reboot: VM did not reach Off state'
    }
    Start-VM -Name $VMName -ErrorAction Stop
    $state.consecutiveFailures = 0
    $state.lastForcedRebootUtc = (Get-Date).ToUniversalTime().ToString('o')
    Save-GuestFailState -State $state
    Write-ReclaimLog -Event 'autoreclaim_forced_reboot_done' -Data @{
        v_free_gb = (Get-VFreeGB)
    }
    return $true
}

function Wait-GuestIdle {
    $deadline = (Get-Date).AddSeconds($IdleWaitSeconds)
    while ((Get-Date) -lt $deadline) {
        $idle = Invoke-Guest -RemoteCommand 'civmctl idle-check'
        if ($idle.ExitCode -eq 0) { return $true }
        Start-Sleep -Seconds 15
    }
    return $false
}

function Wait-VMState {
    param(
        [Parameter(Mandatory = $true)][string]$State,
        [Parameter(Mandatory = $true)][int]$TimeoutSeconds
    )
    $deadline = (Get-Date).AddSeconds($TimeoutSeconds)
    while ((Get-Date) -lt $deadline) {
        $current = (Get-VM -Name $VMName -ErrorAction Stop).State
        if ($current -eq $State) { return $true }
        Start-Sleep -Seconds 2
    }
    return $false
}

$lockStream = $null
try {
    $lockStream = [System.IO.FileStream]::new(
        $LockPath,
        [System.IO.FileMode]::OpenOrCreate,
        [System.IO.FileAccess]::ReadWrite,
        [System.IO.FileShare]::None)
} catch {
    Write-ReclaimLog -Event 'autoreclaim_already_running' -Level 'WARN' -Data @{ lock = $LockPath }
    exit 0
}

# Canonical shared reclaim lock (SPECv3 DT-v3-3): mutual exclusion with
# civm-vhdx-optimize so the two reclaimers never Stop-VM / Optimize the same
# VHDX concurrently. Held FileShare::None; released in finally.
$reclaimLockStream = $null
try {
    $reclaimLockStream = [System.IO.FileStream]::new(
        $ReclaimLockPath,
        [System.IO.FileMode]::OpenOrCreate,
        [System.IO.FileAccess]::ReadWrite,
        [System.IO.FileShare]::None)
} catch {
    Write-ReclaimLog -Event 'reclaim_skip_other_active' -Level 'WARN' -Data @{ lock = $ReclaimLockPath }
    $lockStream.Close(); $lockStream.Dispose()
    Remove-Item -LiteralPath $LockPath -Force -ErrorAction SilentlyContinue
    exit 0
}

$mounted = $false
$exitCode = 0
$operationStarted = $false

try {
    $vmms = Get-Service -Name vmms -ErrorAction Stop
    if ($vmms.Status -ne 'Running') {
        Write-ReclaimLog -Event 'autoreclaim_skip_vmms_down' -Level 'WARN' -Data @{ status = "$($vmms.Status)" }
        exit 0
    }

    $beforeFreeGB = Get-VFreeGB
    if ($beforeFreeGB -ge $ThresholdGB) {
        Write-ReclaimLog -Event 'autoreclaim_skip_threshold' -Data @{
            v_free_gb    = $beforeFreeGB
            threshold_gb = $ThresholdGB
        }
        exit 0
    }
    # RF-2 / DT-1 (issue #106): admission de emergencia em DUAS FASES.
    # Fase 1 (aqui, pre-stop): decide apenas se a emergencia esta HABILITADA
    # (ScratchBudget>0). NAO faz o gate de folga com $beforeFreeGB porque o VMRS
    # (~8GB de saved-state) so e liberado no Stop-VM — medir a folga ANTES do Off
    # a SUBESTIMA, e foi exatamente isso que travou a espiral a 6.6GB
    # (5.6 < 11 => abort eterno 'autoreclaim_abort_insufficient_slack').
    # ScratchBudget=0 => emergencia DESABILITADA (sem realocar o deadlock para um
    # piso adivinhado). A folga real e re-medida na Fase 2, pos-Wait-VMState Off.
    $emergency = $false
    if ($beforeFreeGB -lt $MinHeadroomGB) {
        if ($ScratchBudgetGB -le 0) {
            Write-ReclaimLog -Event 'autoreclaim_abort_headroom' -Level 'ERROR' -Data @{
                v_free_gb   = $beforeFreeGB
                headroom_gb = $MinHeadroomGB
                reason      = 'emergency_disabled_no_budget'
            }
            $exitCode = 2
            exit 2
        }
        $emergency = $true
    }

    $vm = Get-VM -Name $VMName -ErrorAction Stop
    if ($vm.State -ne 'Running') {
        Write-ReclaimLog -Event 'autoreclaim_skip_vm_not_running' -Level 'WARN' -Data @{ vm_state = "$($vm.State)" }
        exit 0
    }

    $vhd = Get-VHD -Path $VhdxPath -ErrorAction Stop
    $guestFreeBytes = $null
    try {
        $guestFreeBytes = Get-GuestFreeBytes
        Reset-GuestFailStreak
    } catch {
        # Guest SSH down: every SSH-gated step below would fail-close forever
        # (the 2026-06-10 self-deadlock: wedged guest, runners offline for
        # days). Track the consecutive streak host-side and escalate to a
        # bounded forced power-cycle once it crosses the limit.
        $streak = Add-GuestFailStreak
        Write-ReclaimLog -Event 'autoreclaim_guest_unreachable' -Level 'WARN' -Data @{
            streak = $streak
            limit  = $GuestUnreachableLimit
            error  = $_.Exception.Message
        }
        if ($GuestUnreachableLimit -gt 0 -and $streak -ge $GuestUnreachableLimit) {
            if (Invoke-GuestUnreachableForcedReboot -Streak $streak) {
                exit 0
            }
        }
        throw
    }
    # Force the Max(long, long) overload: a bare 0 is Int32, which pins both
    # args to Max(int, int) and overflows on any byte value > 2 GiB
    # (Int32.MaxValue). That was throwing every run and aborting the reclaim.
    $guestUsedBytes = [math]::Max([int64]0, ([int64]$vhd.Size - $guestFreeBytes))
    $gapBytes = [math]::Max([int64]0, ([int64]$vhd.FileSize - $guestUsedBytes))
    $gapGB = ConvertTo-GiB -Bytes ([double]$gapBytes)
    if ($gapGB -lt $MinReclaimableGB) {
        # RF-3 (SPEC host-volume-reclaim-liveness): gap baixo => o guest pode estar
        # cheio de docker/cache PRUNAVEL (leftover de jobs). Pruna e RE-computa o
        # gap ANTES de desistir. Confirmado 2026-06-15: o prune liberou ~13-17GB de
        # docker, elevando o gap de <1GB para >20GB, e o Optimize entao liberou o V:.
        # Roda antes do Wait-GuestIdle: o prune de docker UNUSED nao interrompe job.
        $prune = Invoke-Guest -RemoteCommand 'civmctl disk-watchdog --threshold-pct=0 --execute'
        Write-ReclaimLog -Event 'reclaim_liveness_guest_prune' -Data @{
            exit_code     = $prune.ExitCode
            gap_gb_before = $gapGB
        }
        try {
            $guestFreeBytes = Get-GuestFreeBytes
            $guestUsedBytes = [math]::Max([int64]0, ([int64]$vhd.Size - $guestFreeBytes))
            $gapBytes = [math]::Max([int64]0, ([int64]$vhd.FileSize - $guestUsedBytes))
            $gapGB = ConvertTo-GiB -Bytes ([double]$gapBytes)
        } catch {
            Write-ReclaimLog -Event 'reclaim_liveness_prune_remeasure_failed' -Level 'WARN' -Data @{ error = $_.Exception.Message }
        }
    }
    if ($gapGB -lt $MinReclaimableGB) {
        # Ainda baixo apos o prune => guest cheio de dados ATIVOS (F3, working set
        # > disco = hardware). Pular e correto: nao ha slack reclamavel (Kahneman
        # #15 — fail-fast no determinístico; o piso critico segue como fail-safe).
        Write-ReclaimLog -Event 'autoreclaim_skip_low_gap' -Data @{
            v_free_gb          = $beforeFreeGB
            gap_gb             = $gapGB
            min_reclaimable_gb = $MinReclaimableGB
            after_prune        = $true
        }
        exit 0
    }

    if (-not (Wait-GuestIdle)) {
        Write-ReclaimLog -Event 'autoreclaim_skip_busy' -Level 'WARN' -Data @{
            waited_seconds = $IdleWaitSeconds
        }
        exit 0
    }

    Write-ReclaimLog -Event 'autoreclaim_fstrim_start' -Data @{ target = $GuestSshTarget }
    $trim = Invoke-Guest -RemoteCommand 'sudo -n fstrim -av'
    if ($trim.ExitCode -ne 0) {
        # RF-4 / red-team BLOQUEANTE-1 (Passo 2.5): fstrim e DISCARD OPORTUNISTICO, nao
        # pre-requisito do reclaim. Optimize-VHD -Mode Full compacta os blocos livres
        # offline por conta propria, independente do trim online. Um fstrim fail-hard
        # aqui (EPERM/EOPNOTSUPP, controlador sem UNMAP, ou falta de NOPASSWD no sudoers)
        # abortava ANTES do Stop-VM e deixava a Fase 2 (o fix #106) SEM rodar — espiral
        # silenciosa (Kahneman #13: parece deployado, nao funciona). Best-effort como em
        # civm-vhdx-optimize.ps1 (fstrim_warn): registra WARN e segue para Stop-VM/Optimize.
        Write-ReclaimLog -Event 'autoreclaim_fstrim_warn' -Level 'WARN' -Data @{
            exit_code = $trim.ExitCode
            output    = $trim.Output
        }
    }

    $startEvent = if ($emergency) { 'emergency_reclaim_start' } else { 'autoreclaim_start' }
    Write-ReclaimLog -Event $startEvent -Data @{
        v_free_gb_before  = $beforeFreeGB
        gap_gb            = $gapGB
        vhdx              = $VhdxPath
        emergency         = $emergency
        scratch_budget_gb = $ScratchBudgetGB
    }

    if ($PSCmdlet.ShouldProcess($VMName, 'Stop-VM for offline Optimize-VHD')) {
        $operationStarted = $true
        Stop-VM -Name $VMName -ErrorAction Stop
        if (-not (Wait-VMState -State 'Off' -TimeoutSeconds 180)) {
            throw 'VM did not reach Off within 180s'
        }
    }

    # RF-2 / DT-1 (issue #106): gate AUTORITATIVO pos-Off. O VMRS (~8GB) so e
    # liberado quando a VM chega a Off; a folga real para o Optimize e medida
    # AGORA (nao com $beforeFreeGB pre-stop). vmrs_release_gb registra
    # empiricamente quanto o Off liberou (valida a premissa do DT-2, sem
    # adivinhar). Se nem com o VMRS liberado a folga cobre ScratchBudget, pula o
    # Optimize ininterruptivel; o bloco finally religa a VM (operationStarted=true).
    if ($operationStarted) {
        $liveFreeAfterOff = Get-VFreeGB
        Write-ReclaimLog -Event 'autoreclaim_post_off_remeasure' -Data @{
            v_free_gb_before_stop  = $beforeFreeGB
            live_free_after_off_gb = $liveFreeAfterOff
            vmrs_release_gb        = [math]::Round($liveFreeAfterOff - $beforeFreeGB, 2)
            hard_floor_gb          = $HardFloorGB
            scratch_budget_gb      = $ScratchBudgetGB
        }
        if ($emergency -and (($liveFreeAfterOff - $HardFloorGB) -lt $ScratchBudgetGB)) {
            Write-ReclaimLog -Event 'autoreclaim_skip_insufficient_slack_post_off' -Level 'WARN' -Data @{
                live_free_after_off_gb = $liveFreeAfterOff
                hard_floor_gb          = $HardFloorGB
                scratch_budget_gb      = $ScratchBudgetGB
            }
            $exitCode = 0
            exit 0
        }
    }

    try {
        Mount-VHD -Path $VhdxPath -ReadOnly -ErrorAction Stop
        $mounted = $true
    } catch {
        Write-ReclaimLog -Event 'autoreclaim_mount_ro_skipped' -Level 'WARN' -Data @{ error = $_.Exception.Message }
    }

    $vhdBefore = Get-VHD -Path $VhdxPath -ErrorAction Stop
    $scratchHighWaterGB = $null
    if ($PSCmdlet.ShouldProcess($VhdxPath, 'Optimize-VHD -Mode Full')) {
        # SPECv3 DT-v3-2: mede o scratch high-water (poll de V: ao vivo a cada 1s
        # durante a compactacao). Telemetria PURA — Optimize-VHD e ininterruptivel,
        # o poll NAO aborta nada (sem Stop-Job); so alimenta o ScratchBudget da
        # admissao de emergencia. Sem timeout-abort: igual ao comportamento
        # sincrono anterior, que tambem bloqueava ate o fim.
        $liveFreeBeforeGB = ConvertTo-GiB -Bytes ([double](Get-PSDrive V -ErrorAction Stop).Free)
        $lowWaterGB = $liveFreeBeforeGB
        $optJob = Start-Job -ScriptBlock {
            param($path)
            Optimize-VHD -Path $path -Mode Full -ErrorAction Stop
        } -ArgumentList $VhdxPath
        while ($null -eq (Wait-Job -Job $optJob -Timeout 1)) {
            $nowFreeGB = ConvertTo-GiB -Bytes ([double](Get-PSDrive V -ErrorAction SilentlyContinue).Free)
            if ($nowFreeGB -gt 0 -and $nowFreeGB -lt $lowWaterGB) { $lowWaterGB = $nowFreeGB }
        }
        Receive-Job -Job $optJob -ErrorAction Stop | Out-Null
        Remove-Job -Job $optJob -Force -ErrorAction SilentlyContinue
        $scratchHighWaterGB = [math]::Round($liveFreeBeforeGB - $lowWaterGB, 2)
    }
    $vhdAfter = Get-VHD -Path $VhdxPath -ErrorAction Stop
    Write-ReclaimLog -Event 'autoreclaim_optimized' -Data @{
        file_size_gb_before   = (ConvertTo-GiB -Bytes ([double]$vhdBefore.FileSize))
        file_size_gb_after    = (ConvertTo-GiB -Bytes ([double]$vhdAfter.FileSize))
        reclaimed_gb          = (ConvertTo-GiB -Bytes ([double]($vhdBefore.FileSize - $vhdAfter.FileSize)))
        v_free_gb_after       = (Get-VFreeGB)
        scratch_high_water_gb = $scratchHighWaterGB
    }
} catch {
    $exitCode = 1
    Write-ReclaimLog -Event 'autoreclaim_error' -Level 'ERROR' -Data @{ error = $_.Exception.Message }
} finally {
    if ($mounted) {
        try { Dismount-VHD -Path $VhdxPath -ErrorAction Stop } catch { }
    }

    $started = $false
    try {
        $state = (Get-VM -Name $VMName -ErrorAction Stop).State
        if ($state -eq 'Running') {
            $started = $true
        } elseif ($operationStarted) {
            for ($attempt = 1; $attempt -le $StartAttempts -and -not $started; $attempt++) {
                try {
                    Start-VM -Name $VMName -ErrorAction Stop
                    if (Wait-VMState -State 'Running' -TimeoutSeconds $StartWaitSeconds) {
                        $started = $true
                    }
                } catch {
                    Write-ReclaimLog -Event 'autoreclaim_start_vm_retry' -Level 'WARN' -Data @{
                        attempt = $attempt
                        error   = $_.Exception.Message
                    }
                }
                if (-not $started -and $attempt -lt $StartAttempts) {
                    Start-Sleep -Seconds 5
                }
            }
        }
    } catch {
        Write-ReclaimLog -Event 'autoreclaim_start_vm_unknown' -Level 'ERROR' -Data @{ error = $_.Exception.Message }
    }

    if ($operationStarted -and -not $started) {
        $exitCode = 1
        Write-ReclaimLog -Event 'autoreclaim_vm_left_off' -Level 'CRITICAL' -Data @{ attempts = $StartAttempts }
    }

    if ($null -ne $lockStream) {
        $lockStream.Close()
        $lockStream.Dispose()
        Remove-Item -LiteralPath $LockPath -Force -ErrorAction SilentlyContinue
    }
    if ($null -ne $reclaimLockStream) {
        $reclaimLockStream.Close()
        $reclaimLockStream.Dispose()
        Remove-Item -LiteralPath $ReclaimLockPath -Force -ErrorAction SilentlyContinue
    }

    Write-ReclaimLog -Event 'autoreclaim_done' -Data @{
        vm_started = $started
        v_final_gb = (Get-VFreeGB)
        exit_code  = $exitCode
    }
}

exit $exitCode
