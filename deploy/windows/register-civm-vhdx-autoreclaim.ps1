<#
.SYNOPSIS
    Registers the civm-vhdx-autoreclaim Scheduled Task.

.DESCRIPTION
    Idempotently registers a frequent SYSTEM task that runs
    civm-vhdx-autoreclaim.ps1. The worker is gated by host free space, guest
    idle-check, sudo fstrim, and a reclaimable-gap estimate before it stops the
    VM, so running it frequently is the prevention path rather than a manual
    emergency-only repair.

    The task uses a dedicated local host->guest SSH key by default:
    C:\ProgramData\civm\ssh\id_ed25519. The private key is host state, not repo
    state, and must be owned/readable only by SYSTEM for Windows OpenSSH.

    Rollback trigger: if the task causes unwanted downtime, delete this task
    and keep the supervised civm-vhdx-optimize path until the idle predicate or
    thresholds are corrected.
#>
[CmdletBinding(SupportsShouldProcess = $true, ConfirmImpact = 'Medium')]
param(
    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$TaskName = 'civm-vhdx-autoreclaim',

    [Parameter()]
    [ValidateNotNullOrEmpty()]
    [string]$ScriptPath = '',

    [Parameter()]
    [ValidateRange(1, 1440)]
    [int]$IntervalMinutes = 30,

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
if ([string]::IsNullOrWhiteSpace($ScriptPath)) {
    $ScriptPath = Join-Path $scriptRoot 'civm-vhdx-autoreclaim.ps1'
}

if (-not (Test-Path -LiteralPath $ScriptPath)) {
    throw "autoreclaim script not found: $ScriptPath"
}

function Test-TaskExists {
    param([string]$Name)
    $oldPreference = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        & schtasks.exe /query /tn $Name > $null 2> $null
        return ($LASTEXITCODE -eq 0)
    } finally {
        $ErrorActionPreference = $oldPreference
    }
}

if (Test-TaskExists -Name $TaskName) {
    if ($PSCmdlet.ShouldProcess($TaskName, 'Unregister existing Scheduled Task')) {
        & schtasks.exe /delete /tn $TaskName /f
        if ($LASTEXITCODE -ne 0) { throw "schtasks /delete failed for '$TaskName' (exit $LASTEXITCODE)" }
        Write-Host "Removed existing task '$TaskName'."
    }
}

$resolvedScript = (Resolve-Path -LiteralPath $ScriptPath).Path
# Keep /tr below schtasks' 261-character limit. The worker carries the Day-0
# defaults for thresholds and SSH paths; pass custom values by editing the
# installed script or by creating a short wrapper path.
$action = '"{0}" -NoProfile -NonInteractive -ExecutionPolicy Bypass -File "{1}"' -f `
    $PowerShellPath, `
    $resolvedScript

$target = "$TaskName (every $IntervalMinutes min, SYSTEM, RL HIGHEST)"
if ($PSCmdlet.ShouldProcess($target, 'Register Scheduled Task')) {
    & schtasks.exe /create `
        /tn $TaskName `
        /tr $action `
        /sc minute `
        /mo $IntervalMinutes `
        /ru SYSTEM `
        /rl HIGHEST `
        /f
    if ($LASTEXITCODE -ne 0) { throw "schtasks /create failed for '$TaskName' (exit $LASTEXITCODE)" }
    # schtasks /create defaults ExecutionTimeLimit to 72h (P3D). Shorten to 2h: a
    # phantom instance (dead process, task stuck in Running) blocks every tick via
    # MultipleInstances=IgnoreNew, so at 72h the stuck instance would only be killed
    # after V: had long hit the critical floor (2026-06-15 incident: ~30h blocked,
    # runner refused all jobs). 2h is a 9x margin over the real Optimize (~13 min);
    # CompactVirtualDisk keeps running natively even if the wrapper is killed, so the
    # VHDX is never left corrupt. Preserves MultipleInstances and the other settings.
    $st = Get-ScheduledTask -TaskName $TaskName
    $st.Settings.ExecutionTimeLimit = 'PT2H'
    Set-ScheduledTask -TaskName $TaskName -Settings $st.Settings | Out-Null
    Write-Host "Registered Scheduled Task '$TaskName' running '$resolvedScript' every $IntervalMinutes min as SYSTEM."
    Write-Host "Verify: schtasks /query /tn $TaskName /v /fo LIST"
    Write-Host "Run now: schtasks /run /tn $TaskName"
    Write-Host "Remove:  schtasks /delete /tn $TaskName /f"
}
