$ErrorActionPreference = 'Stop'
$taskName = 'civm-vm-orchestrator'
# Nunca remova a ultima task valida antes de copiar/validar o deploy novo. O
# Register-ScheduledTask -Force abaixo substitui a definicao em uma operacao.
if (Test-Path 'V:\civm-reclaim.lock') { throw 'reclaim em curso (V:\civm-reclaim.lock); abortar deploy' }
$existingOrchestrator = Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue
if ($existingOrchestrator -and $existingOrchestrator.State -eq 'Running') {
    throw 'orchestrator em execucao; aguardar o tick terminar antes de trocar os artefatos'
}
$hostTask = Get-ScheduledTask -TaskName 'civm-host-orchestrator' -ErrorAction SilentlyContinue
if ($hostTask -and $hostTask.State -ne 'Disabled') {
    throw 'dual-owner recusado: civm-host-orchestrator deve estar Disabled antes do rollback PowerShell'
}
$toCopy = @(
    'civm-orchestrator-decision.ps1',
    'civm-reclaim-gate.ps1',
    'civm-pr-queue.ps1',
    'civm-vm-orchestrator.ps1',
    'civm-host-metrics.ps1'
)
# Valide todos os sources antes de fechar o owner anterior. Uma falha de source
# nao pode transformar um deploy que nem começou em indisponibilidade.
foreach ($f in $toCopy) {
    $src = Join-Path $PSScriptRoot $f
    if (-not (Test-Path -LiteralPath $src)) { throw "missing source: $src" }
    $perr = $null
    [System.Management.Automation.Language.Parser]::ParseFile($src, [ref]$null, [ref]$perr) | Out-Null
    if ($perr) { throw "parse error no source ${f}: $($perr -join '; ')" }
}
# Desabilite todos os owners anteriores antes de publicar a task nova. Se um
# deles ainda estiver Running, nao ha prova de quiescencia para um cutover.
$legacy = @($taskName, 'civm-vhdx-autoreclaim', 'civm-vhdx-optimize', 'civm-vhdx-optimize-watchdog')
foreach ($t in $legacy) {
    $task = Get-ScheduledTask -TaskName $t -ErrorAction SilentlyContinue
    if ($task -and $task.State -eq 'Running') {
        throw "owner anterior em execucao: $t; aguardar conclusao antes do cutover"
    }
    if ($task -and $task.State -ne 'Disabled') {
        Disable-ScheduledTask -TaskName $t -ErrorAction Stop | Out-Null
    }
}
$dst = 'C:\civm-deploy'
if (-not (Test-Path $dst)) { New-Item -ItemType Directory -Path $dst -Force | Out-Null }
# The controller dot-sources these companion modules. The scheduled task stays
# disabled while they are copied and parsed so no tick can observe a mixed set.
foreach ($f in $toCopy) {
    $src = Join-Path $PSScriptRoot $f
    $destFile = Join-Path $dst $f
    # Skip no-op when ja estamos em C:\civm-deploy (re-run in-place).
    if ((Resolve-Path -LiteralPath $src).Path -eq (Resolve-Path -LiteralPath (Split-Path $destFile -Parent)).Path + "\$f") {
        if ((Test-Path -LiteralPath $destFile) -and ((Get-FileHash $src).Hash -eq (Get-FileHash $destFile).Hash)) { continue }
    }
    if ((Test-Path -LiteralPath $destFile) -and ((Resolve-Path $src).Path -eq (Resolve-Path $destFile).Path)) { continue }
    Copy-Item $src $destFile -Force
}
foreach ($f in $toCopy) {
    $perr = $null
    [System.Management.Automation.Language.Parser]::ParseFile((Join-Path $dst $f), [ref]$null, [ref]$perr) | Out-Null
    if ($perr) { throw "parse error no artefato deployado ${f}: $($perr -join '; ')" }
}
. (Join-Path $dst 'civm-orchestrator-decision.ps1')
. (Join-Path $dst 'civm-reclaim-gate.ps1')
. (Join-Path $dst 'civm-pr-queue.ps1')
foreach ($requiredFunction in @('Get-OrchestratorDecision', 'Get-RunGenerationContext', 'Resolve-PrSlot')) {
    if (-not (Get-Command $requiredFunction -ErrorAction SilentlyContinue)) {
        throw "funcao obrigatoria ausente no deploy: $requiredFunction"
    }
}
"deploy: $($toCopy.Count) .ps1 copiados + validados por AST"
# -EnforceQueue is mandatory: it is the only admission path and publishes an
# exact generation only after the clean 80 GiB boundary.
$arg = '-NoProfile -NonInteractive -ExecutionPolicy Bypass -File C:\civm-deploy\civm-vm-orchestrator.ps1 -EnforceQueue'
$action = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument $arg
$trigger = New-ScheduledTaskTrigger -Once -At (Get-Date)
$trigger.Repetition = (New-ScheduledTaskTrigger -Once -At (Get-Date) -RepetitionInterval (New-TimeSpan -Minutes 2) -RepetitionDuration (New-TimeSpan -Days 3650)).Repetition
# Boot trigger + StartWhenAvailable: a task religa sozinha apos um restart do
# Windows. Sem isso, o gatilho -Once com StartBoundary no passado nao re-dispara
# de forma confiavel pos-reboot (o orchestrator ficaria morto ate intervencao).
$bootTrigger = New-ScheduledTaskTrigger -AtStartup
$principal = New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest
# PT2H (nao 30min): o Optimize-VHD do gate de admissao e o passo longo; PT2H da margem
# (espelha register-civm-vhdx-autoreclaim). Trade-off: um tick pendurado fica ate 2h, mas
# IgnoreNew so engole ticks (sem efeito) e o CompactVirtualDisk e nativo (VHDX nao
# corrompe). (SPECv4 §8 / M-2)
$settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit (New-TimeSpan -Hours 2) -MultipleInstances IgnoreNew -StartWhenAvailable -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
Register-ScheduledTask -TaskName $taskName -Action $action -Trigger @($trigger, $bootTrigger) -Principal $principal -Settings $settings -Force | Out-Null
'orchestrator ATIVO (sem -Observe)'
# Um dono so do power/compact da VM (fail-safe #15). Os curadores legados ja
# estavam Disabled antes do Register; confira a pos-condicao sem abrir janela
# de dual-owner durante a substituicao.
$legacyWithoutOwner = @('civm-vhdx-autoreclaim', 'civm-vhdx-optimize', 'civm-vhdx-optimize-watchdog')
foreach ($t in $legacyWithoutOwner) {
    $task = Get-ScheduledTask -TaskName $t -ErrorAction SilentlyContinue
    if ($task -and $task.State -ne 'Disabled') {
        throw "pos-condicao de owner unico falhou: $t=$($task.State)"
    }
}
$states = ($legacyWithoutOwner | ForEach-Object { "$_=" + (Get-ScheduledTask $_ -ErrorAction SilentlyContinue).State }) -join ' '
'orch_state=' + (Get-ScheduledTask $taskName).State + ' | legacy: ' + $states
