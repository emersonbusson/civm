$ErrorActionPreference = 'Stop'
$taskName = 'civm-vm-orchestrator'
$arg = '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File C:\civm-deploy\civm-vm-orchestrator.ps1 -Observe'
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument $arg
$trigger = New-ScheduledTaskTrigger -Once -At (Get-Date)
$trigger.Repetition = (New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 2) -RepetitionDuration (New-TimeSpan -Days 3650)).Repetition
$bootTrigger = New-ScheduledTaskTrigger -AtStartup
# Mesmo principal do autoreclaim: SYSTEM / Highest (le a ssh key; faz Hyper-V).
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Hours 2) -MultipleInstances IgnoreNew -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries  # PT2H: cobre o Optimize (SPECv4 §8)
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger @($trigger, $bootTrigger) -Principal $principal -Settings $settings -Force | Out-Null
"registered: $taskName (Observe, SYSTEM/Highest, every 2min)"
# Roda uma vez agora pra validar como SYSTEM (ssh do stop-guard se aplicavel).
Start-ScheduledTask -TaskName $taskName
Start-Sleep 10
"last_result(0=ok): $((Get-ScheduledTask $taskName | Get-ScheduledTaskInfo).LastTaskResult)"
"state: $((Get-ScheduledTask $taskName).State)"
