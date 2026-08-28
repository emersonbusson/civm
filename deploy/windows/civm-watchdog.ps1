[CmdletBinding()]
param(
    [string]$VMName = 'gha-ubuntu-2404',
    [int]$HeartbeatMaxAgeMinutes = 45,
    [string]$StatusPath = 'V:\civm-watchdog-status.txt',
    [string]$LogPath = 'V:\civm-watchdog.log',
    [string]$HostHeartbeatPath = 'V:\civm-host-shadow.jsonl',
    [string]$QueueStatePath = 'V:\civm-pr-queue.json'
)

$ErrorActionPreference = 'Stop'

function Get-TaskSnapshot {
    param([string]$Name)

    $task = Get-ScheduledTask -TaskName $Name -ErrorAction SilentlyContinue
    if (-not $task) {
        return [pscustomobject]@{
            Name = $Name
            State = 'MISSING'
            Arguments = ''
            LastResult = $null
        }
    }

    $info = Get-ScheduledTaskInfo -TaskName $Name -ErrorAction SilentlyContinue
    [pscustomobject]@{
        Name = $Name
        State = "$($task.State)"
        Arguments = (($task.Actions | ForEach-Object { $_.Arguments }) -join ' ')
        LastResult = if ($info) { $info.LastTaskResult } else { $null }
    }
}

function Test-ActiveTaskState {
    param([string]$State)
    return $State -in @('Ready', 'Running')
}

$timestamp = (Get-Date).ToString('s')
$alerts = [System.Collections.Generic.List[string]]::new()
$hostOwner = Get-TaskSnapshot -Name 'civm-host-orchestrator'
$legacyOwner = Get-TaskSnapshot -Name 'civm-vm-orchestrator'
$autoreclaim = Get-TaskSnapshot -Name 'civm-vhdx-autoreclaim'
$hostActive = Test-ActiveTaskState -State $hostOwner.State
$legacyActive = Test-ActiveTaskState -State $legacyOwner.State
$activeOwnerCount = [int]$hostActive + [int]$legacyActive

if ($activeOwnerCount -ne 1) {
    $alerts.Add("owner count invalido ($activeOwnerCount): host=$($hostOwner.State) legacy=$($legacyOwner.State)")
}

$owner = if ($hostActive -and -not $legacyActive) {
    'host-csharp'
} elseif ($legacyActive -and -not $hostActive) {
    'legacy-powershell'
} elseif ($hostActive) {
    'dual'
} else {
    'none'
}

if ($hostActive) {
    if ($hostOwner.Arguments -notmatch '(?i)(--active|active\.cmd)') {
        $alerts.Add('owner C# sem modo active')
    }
    if ($hostOwner.State -eq 'Ready' -and $null -ne $hostOwner.LastResult -and $hostOwner.LastResult -ne 0) {
        $alerts.Add("owner C# lastResult=$($hostOwner.LastResult)")
    }
    if (-not (Test-Path -LiteralPath $HostHeartbeatPath)) {
        $alerts.Add('heartbeat do owner C# ausente')
    } else {
        $heartbeatAge = ((Get-Date).ToUniversalTime() - (Get-Item -LiteralPath $HostHeartbeatPath).LastWriteTimeUtc).TotalMinutes
        if ($heartbeatAge -gt $HeartbeatMaxAgeMinutes) {
            $alerts.Add(('heartbeat do owner C# stale ({0:N1} min > {1} min)' -f $heartbeatAge, $HeartbeatMaxAgeMinutes))
        }
    }

    if (Test-Path -LiteralPath $QueueStatePath) {
        try {
            $queueState = Get-Content -LiteralPath $QueueStatePath -Raw | ConvertFrom-Json
            if (-not [string]::IsNullOrWhiteSpace("$($queueState.processBlockedReason)")) {
                $alerts.Add("process latch ativo: $($queueState.processBlockedReason)")
            }
        } catch {
            $alerts.Add("estado da fila ilegivel: $($_.Exception.Message)")
        }
    }
}

if ($legacyActive) {
    if ($legacyOwner.Arguments -match '(?i)-Observe') {
        $alerts.Add('owner PowerShell em Observe')
    }
    if ($legacyOwner.Arguments -notmatch '(?i)-EnforceQueue') {
        $alerts.Add('owner PowerShell sem EnforceQueue')
    }
}

if ($autoreclaim.State -in @('Ready', 'Running')) {
    $alerts.Add('autoreclaim legado ativo; desabilitacao manual obrigatoria')
}

$vFree = try { [int]((Get-PSDrive V).Free / 1GB) } catch { -1 }
$vmState = try { "$(Get-VM $VMName -ErrorAction Stop | Select-Object -ExpandProperty State)" } catch { 'UNKNOWN' }
if ($vFree -lt 0) {
    $alerts.Add('V: indisponivel')
} elseif ($vFree -lt 20) {
    $alerts.Add("V: baixo ($vFree GB) possivel death-spiral")
}

$verdict = if ($alerts.Count -eq 0) { 'OK' } else { 'DRIFT' }
$line = "$timestamp | $verdict | V=${vFree}GB VM=$vmState owner=$owner host=$($hostOwner.State) legacy=$($legacyOwner.State) autoreclaim=$($autoreclaim.State)"
if ($alerts.Count -gt 0) {
    $line += ' | ' + ($alerts -join '; ')
}

Add-Content -LiteralPath $LogPath -Value $line
Set-Content -LiteralPath $StatusPath -Value $line
Write-Output $line
