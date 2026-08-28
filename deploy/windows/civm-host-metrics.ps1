<#
.SYNOPSIS
    civm host VHDX/volume metrics collector (RF-3, ITEM-10).

.DESCRIPTION
    Reads the Hyper-V host V: volume and the runner VHDX, queries the guest root
    free space over SSH, then writes a JSON snapshot to the host path
    (DefaultHostMetricsFileNameOnHost on V:\) AND delivers a copy into the guest
    at DefaultHostMetricsPath (/var/lib/civm/host-metrics.json) so that
    `civmctl host-disk` can read it guest-side.

    Read-only on the host: never runs Optimize-VHD, Stop-VM, or Start-VM.

    SPEC: docs/specs/host-volume-reclamation/SPECv2.md
      - DT-v2-5  : SSH/scp delivery failure => WARN, host-only snapshot with
                   delivery_status="failed", guest_free_gb=0, gap_gb=null, exit 0.
      - DT-v2-9  : freshness; this task runs every 10 min, hostdisk.MaxAge=30.
      - DT-v2-11 : field semantics (VFreeGB / VSizeGB / VHDXFileSizeGB /
                   VHDXMaxSizeGB / VHDXMinSizeGB).
    JSON contract: internal/hostdisk/hostdisk.go (Metrics struct).
#>
[CmdletBinding()]
param(
    [string]$VMName = 'gha-ubuntu-2404',
    [string]$DriveLetter = 'V',
    # Optional explicit VHDX path; when empty it is resolved from the VM.
    [string]$VhdxPath = '',
    # Host snapshot path (DefaultHostMetricsFileNameOnHost on V:\).
    [string]$HostMetricsPath = 'V:\civm-host-metrics.json',
    # Guest destination (DefaultHostMetricsPath).
    [string]$GuestMetricsPath = '/var/lib/civm/host-metrics.json',
    # SSH target used to read guest free space and deliver the copy.
    [string]$GuestSshTarget = 'emdev@gha-ubuntu-2404',
    # Dedicated host->guest key for SYSTEM scheduled tasks. The private key is
    # local host state, never repo state.
    [string]$SshKeyPath = 'C:\ProgramData\civm\ssh\id_ed25519',
    [string]$KnownHostsPath = 'C:\ProgramData\civm\ssh\known_hosts',
    [int]$SshTimeoutSeconds = 30,
    # Timeout do Get-VHD: ele pendura se o Optimize-VHD do orchestrator esta com o
    # lock do disco (dois donos host-side disputam o VHDX). Desiste apos isto e
    # escreve o snapshot com vhdx nulo + v_free critico, em vez de pendurar e cegar
    # o gate host-aware (incidente 2026-06-18: hang de 6h, panic matou jobs).
    [int]$VhdReadTimeoutSeconds = 20,
    [string]$LogPath = 'V:\civm-hyperv-maintenance.log'
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$GiB = [math]::Pow(1024, 3)

function Write-Log {
    param([string]$Level, [string]$Message)
    $line = ('{0} [{1}] host-metrics: {2}' -f (Get-Date).ToUniversalTime().ToString('o'), $Level, $Message)
    try { Add-Content -Path $LogPath -Value $line -ErrorAction Stop } catch { }
    Write-Host $line
}

function ConvertTo-GiB { param([double]$Bytes) return [int64][math]::Round($Bytes / $GiB) }

function Get-SshArgs {
    param([string]$Target, [int]$TimeoutSeconds)
    $sshDir = Split-Path -Parent $KnownHostsPath
    if (-not (Test-Path -LiteralPath $sshDir)) {
        New-Item -ItemType Directory -Path $sshDir -Force | Out-Null
    }
    $args = @(
        '-o', 'BatchMode=yes',
        '-o', "ConnectTimeout=$TimeoutSeconds",
        '-o', 'StrictHostKeyChecking=accept-new',
        '-o', "UserKnownHostsFile=$KnownHostsPath"
    )
    if (-not [string]::IsNullOrWhiteSpace($SshKeyPath)) {
        $args += @('-o', 'IdentitiesOnly=yes', '-i', $SshKeyPath)
    }
    $args += $Target
    return $args
}

# Resolve the VHDX path from the VM when not provided explicitly.
function Resolve-VhdxPath {
    param([string]$Name, [string]$Explicit)
    if (-not [string]::IsNullOrWhiteSpace($Explicit)) { return $Explicit }
    $drive = Get-VMHardDiskDrive -VMName $Name | Select-Object -First 1
    if ($null -eq $drive) { throw "no VHDX attached to VM '$Name'" }
    return $drive.Path
}

# Le os tamanhos do VHDX sob timeout, num job descartavel. Get-VHD pendura quando
# o Optimize-VHD do orchestrator esta com o lock do disco (dois donos host-side
# disputam o VHDX). Desiste apos TimeoutSeconds e devolve $null — o caller escreve
# o snapshot so com o v_free critico (de Get-Volume, que nao trava). Sem isto o
# script pendura e o gate host-aware fica cego (incidente 2026-06-18: hang de 6h).
function Get-VhdxSizesWithTimeout {
    param([string]$Path, [int]$TimeoutSeconds)
    $job = $null
    try {
        $job = Start-Job -ScriptBlock {
            param($p)
            $v = Get-VHD -Path $p -ErrorAction Stop
            [pscustomobject]@{
                FileSize    = [double]$v.FileSize
                MinimumSize = [double]$v.MinimumSize
                Size        = [double]$v.Size
                BlockSize   = [int64]$v.BlockSize
            }
        } -ArgumentList $Path
        if (Wait-Job -Job $job -Timeout $TimeoutSeconds) { return Receive-Job -Job $job }
        return $null
    } catch { return $null }
    finally {
        if ($job) {
            Stop-Job -Job $job -ErrorAction SilentlyContinue
            Remove-Job -Job $job -Force -ErrorAction SilentlyContinue
        }
    }
}

# Query guest root free bytes over SSH. Returns $null on any failure (DT-v2-5).
function Get-GuestFreeBytes {
    param([string]$Target, [int]$TimeoutSeconds)
    try {
        $remote = "df -B1 --output=avail / | tail -n1 | tr -d '[:space:]'"
        $sshArgs = Get-SshArgs -Target $Target -TimeoutSeconds $TimeoutSeconds
        $out = & ssh @sshArgs $remote 2>$null
        if ($LASTEXITCODE -ne 0) { return $null }
        $val = ($out | Out-String).Trim()
        $parsed = 0L
        if ([int64]::TryParse($val, [ref]$parsed) -and $parsed -gt 0) { return $parsed }
        return $null
    } catch { return $null }
}

# Atomic write: temp file in the same directory + Move-Item -Force.
function Write-JsonAtomic {
    param([string]$Path, [string]$Json)
    $dir = Split-Path -Parent $Path
    $tmp = Join-Path $dir ('.{0}.tmp' -f [System.IO.Path]::GetFileName($Path))
    [System.IO.File]::WriteAllText($tmp, $Json, (New-Object System.Text.UTF8Encoding($false)))
    Move-Item -LiteralPath $tmp -Destination $Path -Force
}

# Deliver the host snapshot into the guest atomically via SSH. The destination
# lives under /var/lib/civm (root-owned), so the host user must have passwordless
# sudo for this bounded write. Returns $true on success, $false on any failure
# (caller marks delivery_status=failed, DT-v2-5).
function Send-MetricsToGuest {
    param([string]$Target, [string]$DestPath, [string]$Json, [int]$TimeoutSeconds)
    try {
        $tmpDest = "$DestPath.tmp"
        $remote = "sudo -n sh -c 'cat > $tmpDest && mv -f $tmpDest $DestPath && chmod 0644 $DestPath'"
        $sshArgs = Get-SshArgs -Target $Target -TimeoutSeconds $TimeoutSeconds
        $Json | & ssh @sshArgs $remote 2>$null
        return ($LASTEXITCODE -eq 0)
    } catch { return $false }
}

try {
    # 1. Host volume V: free/total (DT-v2-11: VFreeGB / VSizeGB).
    $vol = Get-Volume -DriveLetter $DriveLetter -ErrorAction Stop
    $vFreeGB = ConvertTo-GiB -Bytes ([double]$vol.SizeRemaining)
    $vSizeGB = ConvertTo-GiB -Bytes ([double]$vol.Size)

    # 2. VHDX dynamic file size + configured min/max (DT-v2-11). Lido sob timeout:
    # se o Optimize-VHD esta com o lock do disco, Get-VHD penduraria e cegaria o
    # gate (incidente 2026-06-18). $null => snapshot sai com vhdx nulo + v_free ok.
    $resolvedVhdx = Resolve-VhdxPath -Name $VMName -Explicit $VhdxPath
    $vhd = Get-VhdxSizesWithTimeout -Path $resolvedVhdx -TimeoutSeconds $VhdReadTimeoutSeconds
    if ($null -ne $vhd) {
        $vhdxFileGB = ConvertTo-GiB -Bytes $vhd.FileSize
        $vhdxMinGB = ConvertTo-GiB -Bytes $vhd.MinimumSize
        $vhdxMaxGB = ConvertTo-GiB -Bytes $vhd.Size
        $vhdxBlockBytes = $vhd.BlockSize
    } else {
        Write-Log -Level 'WARN' -Message "Get-VHD timed out after ${VhdReadTimeoutSeconds}s (VHDX locked, Optimize-VHD likely running); snapshot with null vhdx sizes, v_free preserved"
        $vhdxFileGB = $null; $vhdxMinGB = $null; $vhdxMaxGB = $null; $vhdxBlockBytes = $null
    }

    # 3. VM power state (observability; not a gate here).
    $vm = Get-VM -Name $VMName -ErrorAction Stop
    $vmState = [string]$vm.State
    $timestamp = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')

    # 4. Guest free via SSH only when Running — Off has no listener (timeout only
    # poisons the metrics task and stamps guest_free=0 as if measured).
    if ($vmState -ne 'Running') {
        Write-Log -Level 'INFO' -Message ("VM state={0}: skip guest SSH; host-only snapshot" -f $vmState)
        $metrics = [ordered]@{
            v_free_gb         = $vFreeGB
            v_size_gb         = $vSizeGB
            vhdx_file_size_gb = $vhdxFileGB
            vhdx_min_size_gb  = $vhdxMinGB
            vhdx_max_size_gb  = $vhdxMaxGB
            vhdx_block_size_bytes = $vhdxBlockBytes
            guest_free_gb     = 0
            gap_gb            = $null
            vm_state          = $vmState
            timestamp         = $timestamp
            delivery_status   = 'skipped_vm_not_running'
        }
        $json = $metrics | ConvertTo-Json -Depth 4
        Write-JsonAtomic -Path $HostMetricsPath -Json $json
        Write-Log -Level 'INFO' -Message ("host-only snapshot written to {0} (vm={1})" -f $HostMetricsPath, $vmState)
        exit 0
    }

    # Guest root free space over SSH; null => delivery failed (DT-v2-5).
    $guestFreeBytes = Get-GuestFreeBytes -Target $GuestSshTarget -TimeoutSeconds $SshTimeoutSeconds

    if ($null -eq $guestFreeBytes) {
        Write-Log -Level 'WARN' -Message "guest df over SSH failed; writing host-only snapshot (delivery_status=failed)"
        $metrics = [ordered]@{
            v_free_gb         = $vFreeGB
            v_size_gb         = $vSizeGB
            vhdx_file_size_gb = $vhdxFileGB
            vhdx_min_size_gb  = $vhdxMinGB
            vhdx_max_size_gb  = $vhdxMaxGB
            vhdx_block_size_bytes = $vhdxBlockBytes
            guest_free_gb     = 0
            gap_gb            = $null
            vm_state          = $vmState
            timestamp         = $timestamp
            delivery_status   = 'failed'
        }
        $json = $metrics | ConvertTo-Json -Depth 4
        Write-JsonAtomic -Path $HostMetricsPath -Json $json
        Write-Log -Level 'INFO' -Message "host-only snapshot written to $HostMetricsPath"
        exit 0
    }

    # gap_gb = VHDX file size minus what the guest actually consumes (reclaimable).
    # So calculavel com os tamanhos do VHDX; se o Get-VHD deu timeout (null), o gap
    # fica null — o gate nao precisa dele, so do v_free.
    $guestFreeGB = ConvertTo-GiB -Bytes ([double]$guestFreeBytes)
    if ($null -ne $vhdxFileGB -and $null -ne $vhdxMaxGB) {
        $guestUsedGB = $vhdxMaxGB - $guestFreeGB
        if ($guestUsedGB -lt 0) { $guestUsedGB = 0 }
        $gapGB = $vhdxFileGB - $guestUsedGB
        if ($gapGB -lt 0) { $gapGB = 0 }
    } else {
        $gapGB = $null
    }

    $metrics = [ordered]@{
        v_free_gb         = $vFreeGB
        v_size_gb         = $vSizeGB
        vhdx_file_size_gb = $vhdxFileGB
        vhdx_min_size_gb  = $vhdxMinGB
        vhdx_max_size_gb  = $vhdxMaxGB
        vhdx_block_size_bytes = $vhdxBlockBytes
        guest_free_gb     = $guestFreeGB
        gap_gb            = $gapGB
        vm_state          = $vmState
        timestamp         = $timestamp
    }
    $json = $metrics | ConvertTo-Json -Depth 4

    # 5. Write host snapshot atomically (V:\), then deliver a copy to the guest.
    Write-JsonAtomic -Path $HostMetricsPath -Json $json
    Write-Log -Level 'INFO' -Message "snapshot written to $HostMetricsPath (v_free=${vFreeGB}GB vhdx_file=${vhdxFileGB}GB guest_free=${guestFreeGB}GB gap=${gapGB}GB vm=$vmState)"

    if (Send-MetricsToGuest -Target $GuestSshTarget -DestPath $GuestMetricsPath -Json $json -TimeoutSeconds $SshTimeoutSeconds) {
        Write-Log -Level 'INFO' -Message "delivered snapshot to guest:$GuestMetricsPath"
        exit 0
    }

    # Delivery failed after a successful host read: re-stamp host-only snapshot
    # with delivery_status=failed so the guest sees crit (DT-v2-5/9).
    Write-Log -Level 'WARN' -Message "guest delivery failed; re-stamping host snapshot with delivery_status=failed"
    $metrics['guest_free_gb'] = 0
    $metrics['gap_gb'] = $null
    $metrics['delivery_status'] = 'failed'
    Write-JsonAtomic -Path $HostMetricsPath -Json ($metrics | ConvertTo-Json -Depth 4)
    exit 0
} catch {
    # Host-side read failure (Get-Volume/Get-VHD/Get-VM): cannot trust any value.
    Write-Log -Level 'ERROR' -Message ("host metrics collection failed: {0}" -f $_.Exception.Message)
    exit 1
}
