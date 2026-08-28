[CmdletBinding(SupportsShouldProcess)]
param(
    [string]$TaskName = 'civm-watchdog',
    [string]$DeployDir = 'C:\civm-deploy'
)

$ErrorActionPreference = 'Stop'
$source = Join-Path $PSScriptRoot 'civm-watchdog.ps1'
$deployed = Join-Path $DeployDir 'civm-watchdog.ps1'
if (-not (Test-Path -LiteralPath $source)) {
    throw "missing source: $source"
}

$parseErrors = $null
[System.Management.Automation.Language.Parser]::ParseFile(
    $source, [ref]$null, [ref]$parseErrors) | Out-Null
if ($parseErrors) {
    throw "parse error: $($parseErrors -join '; ')"
}

if (-not (Test-Path -LiteralPath $DeployDir)) {
    New-Item -ItemType Directory -Path $DeployDir -Force | Out-Null
}
if ($PSCmdlet.ShouldProcess($deployed, 'Deploy owner-aware watchdog')) {
    Copy-Item -LiteralPath $source -Destination $deployed -Force
}

$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument (
    '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File "{0}"' -f $deployed)
$periodic = New-ScheduledTaskTrigger -Once -At (Get-Date) `
    -RepetitionInterval (New-TimeSpan -Minutes 20) `
    -RepetitionDuration (New-TimeSpan -Days 3650)
$startup = New-ScheduledTaskTrigger -AtStartup
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' `
    -LogonType ServiceAccount -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet `
    -ExecutionTimeLimit (New-TimeSpan -Minutes 5) `
    -MultipleInstances IgnoreNew `
    -StartWhenAvailable `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries

if ($PSCmdlet.ShouldProcess($TaskName, 'Register owner-aware watchdog task')) {
    Register-ScheduledTask -TaskName $TaskName -Action $action `
        -Trigger @($periodic, $startup) -Principal $principal `
        -Settings $settings -Force | Out-Null
}

Write-Output "watchdog registrado: $TaskName -> $deployed"
