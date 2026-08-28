<#
.SYNOPSIS
    Serial clean-slate admission controller for a shared GitHub Actions VM.

.DESCRIPTION
    The host polls non-terminal GitHub workflow runs and serializes immutable
    generation IDs (`pr-N@SHA`). A generation context is published to the host
    gate only after a full guest cleanup, graceful shutdown, offline VHDX
    compaction, a measured V: floor of at least 80 GiB, reboot and runner
    restore. Jobs never trigger a timeout-based or forced VM shutdown.

    API, SSH, runner-idle, maintenance, compaction and capacity uncertainty all
    fail closed: the previous context remains authorized and the next one waits.
    A host reboot can resume an already published generation, but cannot publish
    a new one outside this sequence.

.NOTES
    Requires one actions:read token per GitHub owner in
    C:\ProgramData\civm\gh-token-<owner>.txt and must run as SYSTEM so it can
    read the guest SSH key. This is a disabled legacy rollback owner and does
    not publish dynamic generation labels. Never activate it for a dynamic-label
    peer. A supervised rollback must first disable the C# owner, quarantine all
    gate custom labels and coordinate the peer's return to the prior static
    [self-hosted, civm-gate] contract.
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string]$VMName = 'gha-ubuntu-2404',
    [string]$VhdxPath = 'V:\Hyper-V\gha-ubuntu-2404\Virtual Hard Disks\gha-ubuntu-2404.vhdx',
    # Um PAT fine-grained por resource owner (cada um cobre 1 dono).
    # Exemplo: @{ 'myorg' = 'C:\ProgramData\civm\gh-token-myorg.txt' }
    [hashtable]$TokenPaths = @{},
    [string]$GuestSshTarget = 'emdev@gha-ubuntu-2404',
    [string]$SshKeyPath = 'C:\ProgramData\civm\ssh\id_ed25519',
    # Repos vigiados (owner/name). Vazio = orchestrator nao ve fila GitHub ate
    # o operador configurar (fail-safe: sem repo, nao desliga por "idle API").
    [string[]]$Repos = @(),
    [ValidateRange(1, 120)][int]$IdleStopMinutes = 10,
    # Disk safety floor in GiB. Online cleanup is allowed only while a job is
    # active; offline compaction belongs exclusively to a clean boundary.
    [int]$WarnFloorGB = 28,
    [string]$StatePath = 'V:\civm-orchestrator-state.json',
    [string]$LogPath = 'V:\civm-orchestrator.log',
    # Lock canonico de reclaim (SPECv3 DT-v3-3): exclusao mutua com qualquer outro
    # reclaimer do mesmo VHDX. Mesmo path do civm-vhdx-autoreclaim/optimize.
    [string]$ReclaimLockPath = 'V:\civm-reclaim.lock',
    # Estado da fila FIFO por-PR (Phase 1b, observe-mode): contextos em ordem de chegada
    # + o slot simulado. Por enquanto so LOGA (would_grant/would_advance), nao impoe.
    [string]$PrQueuePath = 'V:\civm-pr-queue.json',
    # Caminho HOST do contexto concedido. O gate job (runner Windows do HOST, label
    # civm-gate) le isto e segura os jobs reais Linux ate ser a vez do PR. Fica no HOST
    # de proposito: sobrevive ao Stop-VM do guest no compact de boundary (um gate dentro
    # do guest seria cancelado pelo compact). So e escrito com -EnforceQueue.
    [string]$CurrentContextPath = 'C:\ProgramData\civm\gate\current-context',
    # Liga o ENFORCE da fila por-PR: publica o currentPr no host + limpa+compacta no
    # boundary do contexto. Default OFF (so observe). Ligar SO depois do canario provar
    # o gate (gate-no-host) num PR throwaway — nunca direto nos 7 workflows.
    [switch]$EnforceQueue,
    # Modo observe: loga "would_start"/"would_stop" em vez de agir. Valida a
    # logica contra a box real sem mexer na VM — mais limpo que -WhatIf (que
    # suprime ate o Add-Content do log e os New-Alias do modulo Hyper-V).
    [switch]$Observe
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Write-OrcLog {
    param([string]$Event, [hashtable]$Data = @{}, [string]$Level = 'INFO')
    $rec = [ordered]@{ ts = (Get-Date).ToUniversalTime().ToString('o'); level = $Level; event = $Event }
    foreach ($k in $Data.Keys) { $rec[$k] = $Data[$k] }
    $line = ($rec | ConvertTo-Json -Compress -Depth 5)
    try { Add-Content -LiteralPath $LogPath -Value $line -Encoding UTF8 } catch { }
    Write-Host $line
}

$script:TokenCache = @{}
function Get-GhTokenForOwner {
    param([string]$Owner)
    if ($script:TokenCache.ContainsKey($Owner)) { return $script:TokenCache[$Owner] }
    $path = $TokenPaths[$Owner]
    if ([string]::IsNullOrWhiteSpace($path) -or -not (Test-Path -LiteralPath $path)) {
        throw "token ausente para owner '$Owner' (esperado em $path)"
    }
    $tok = (Get-Content -LiteralPath $path -Raw).Trim()
    $script:TokenCache[$Owner] = $tok
    return $tok
}

# Get-PrActivity observes every non-terminal workflow run and groups it by the
# immutable generation ID supplied by civm-pr-queue.ps1. A partial observation
# is never treated as an empty queue: the caller must defer admission.
function Get-PrActivity {
    $counts = @{}
    $firstSeen = @{}
    $queued = 0
    $running = 0
    if (-not $Repos -or @($Repos).Count -eq 0) {
        return [pscustomobject]@{ verified = $false; counts = $counts; firstSeen = $firstSeen; queued = $queued; running = $running; errors = @('no repositories configured') }
    }
    foreach ($repo in $Repos) {
        $owner = $repo.Split('/')[0]
        try { $token = Get-GhTokenForOwner -Owner $owner }
        catch {
            return [pscustomobject]@{ verified = $false; counts = $counts; firstSeen = $firstSeen; queued = $queued; running = $running; errors = @("$repo token: $($_.Exception.Message)") }
        }
        $headers = @{ Authorization = "Bearer $token"; 'User-Agent' = 'civm-orchestrator'; Accept = 'application/vnd.github+json' }
        foreach ($status in @('queued', 'in_progress')) {
            $page = 1
            while ($true) {
                $uri = "https://api.github.com/repos/$repo/actions/runs?status=$status&per_page=100&page=$page"
                try { $response = Invoke-RestMethod -Uri $uri -Headers $headers -Method Get -TimeoutSec 20 }
                catch {
                    return [pscustomobject]@{ verified = $false; counts = $counts; firstSeen = $firstSeen; queued = $queued; running = $running; errors = @("${repo} ${status} page ${page}: $($_.Exception.Message)") }
                }
                if ($null -eq $response -or -not ($response.PSObject.Properties.Name -contains 'workflow_runs')) {
                    return [pscustomobject]@{ verified = $false; counts = $counts; firstSeen = $firstSeen; queued = $queued; running = $running; errors = @("${repo} ${status} page ${page}: workflow_runs missing") }
                }
                $runs = @($response.workflow_runs)
                foreach ($run in $runs) {
                    # A completed ghost returned by a stale filtered index does
                    # not represent active work. A non-terminal malformed run
                    # is unknown and must block the boundary.
                    if ("$($run.status)" -ne $status) { continue }
                    $context = Get-RunGenerationContext -Run $run
                    if ([string]::IsNullOrWhiteSpace($context)) {
                        return [pscustomobject]@{ verified = $false; counts = $counts; firstSeen = $firstSeen; queued = $queued; running = $running; errors = @("${repo} ${status} page ${page}: generation identity missing") }
                    }
                    try { $created = [datetime]::Parse([string]$run.created_at).ToUniversalTime() }
                    catch {
                        return [pscustomobject]@{ verified = $false; counts = $counts; firstSeen = $firstSeen; queued = $queued; running = $running; errors = @("${repo} ${status} page ${page}: created_at invalid") }
                    }
                    if (-not $counts.ContainsKey($context)) {
                        $counts[$context] = 0
                        $firstSeen[$context] = $created.ToString('o')
                    }
                    elseif ($created -lt [datetime]::Parse([string]$firstSeen[$context]).ToUniversalTime()) {
                        $firstSeen[$context] = $created.ToString('o')
                    }
                    $counts[$context]++
                    if ($status -eq 'queued') { $queued++ } else { $running++ }
                }
                if ($runs.Count -lt 100) { break }
                $page++
            }
        }
    }
    return [pscustomobject]@{ verified = $true; counts = $counts; firstSeen = $firstSeen; queued = $queued; running = $running; errors = @() }
}

function Get-State {
    $s = $null
    if (Test-Path -LiteralPath $StatePath) {
        try { $s = (Get-Content -LiteralPath $StatePath -Raw | ConvertFrom-Json) } catch { }
    }
    if ($null -eq $s) { $s = [pscustomobject]@{ lastBusyUtc = (Get-Date).ToUniversalTime().ToString('o') } }
    if (-not ($s.PSObject.Properties.Name -contains 'lastBusyUtc')) {
        $s | Add-Member -NotePropertyName lastBusyUtc -NotePropertyValue (Get-Date).ToUniversalTime().ToString('o') -Force
    }
    return $s
}

function Save-State {
    param($State)
    try { ($State | ConvertTo-Json -Compress) | Set-Content -LiteralPath $StatePath -Encoding UTF8 } catch { }
}

# Monta os args de SSH (batch, timeout, chave) para um alvo. Centralizado: as 3
# funcoes que falam com o guest reusam a mesma config em vez de duplicar.
function Get-GuestSshArgs {
    param([Parameter(Mandatory)][string]$Target)
    $a = @('-o', 'BatchMode=yes', '-o', 'ConnectTimeout=20', '-o', 'ServerAliveInterval=15', '-o', 'ServerAliveCountMax=2', '-o', 'StrictHostKeyChecking=accept-new')
    if (-not [string]::IsNullOrWhiteSpace($SshKeyPath)) { $a += @('-o', 'IdentitiesOnly=yes', '-i', $SshKeyPath) }
    $a += $Target
    return $a
}

# Wrap every remote command with a guest-side deadline and an exclusive lock.
# A transport loss cannot leave an orphan command that a later tick overlaps;
# `flock -n` fails closed while the previous operation is still terminating.
function ConvertTo-GuestBoundedCommand {
    param(
        [Parameter(Mandatory)][string]$Command,
        [ValidateRange(6, 3600)][int]$TimeoutSec = 1800
    )
    $encoded = [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($Command))
    $budget = [Math]::Max(1, $TimeoutSec - 5)
    return ("bash -lc 'mkdir -p `$HOME/.cache/civm && printf %s {0} | base64 -d | flock -n `$HOME/.cache/civm/guest.lock timeout --signal=TERM --kill-after=5s {1}s bash'" -f $encoded, $budget)
}

# Descobre o IPv4 do guest direto do Hyper-V (sem DNS). Usado como fallback
# quando o NOME gha-ubuntu-2404 nao resolve no boot. Exige integration services
# reportando IP (poucos segundos pos-Start-VM). Falha -> $null (o caller so usa
# se nao for nulo).
function Get-GuestIPAddress {
    try {
        $ips = (Get-VMNetworkAdapter -VMName $VMName -ErrorAction Stop).IPAddresses
        return ($ips | Where-Object { $_ -match '^\d{1,3}(\.\d{1,3}){3}$' } | Select-Object -First 1)
    }
    catch { return $null }
}

# SSH ao guest com retry/backoff. Pos-reboot (ex.: queda de energia) o nome
# gha-ubuntu-2404 demora a resolver pelo switch Hyper-V -> "Could not resolve
# hostname"/"Connection refused" transitorios faziam o clean+fstrim e o
# stop-guard pularem (a limpeza nao rodava -> o Optimize nao recuperava nada).
# Tenta ate $Retries vezes; se o NOME nao resolve, acrescenta o IP da VM como
# alvo e tenta por IP (remove a dependencia de DNS). $ErrorActionPreference local
# = Continue para o stderr do ssh nao virar throw -> decidimos sucesso pelo
# $LASTEXITCODE. Retorna a ultima linha do stdout; $script:LastGuestSshOk diz se
# algum alvo respondeu com exit 0.
function Invoke-GuestSsh {
    param(
        [Parameter(Mandatory)][string]$Command,
        [int]$Retries = 3,
        [int]$BackoffSeconds = 5,
        [ValidateRange(6, 3600)][int]$TimeoutSec = 1800
    )
    $ErrorActionPreference = 'Continue'
    $script:LastGuestSshOk = $false
    $user = ($GuestSshTarget -split '@')[0]
    $targets = [System.Collections.Generic.List[string]]::new()
    $targets.Add($GuestSshTarget)
    $lastLine = $null
    $remoteCommand = ConvertTo-GuestBoundedCommand -Command $Command -TimeoutSec $TimeoutSec
    for ($attempt = 1; $attempt -le $Retries; $attempt++) {
        for ($i = 0; $i -lt $targets.Count; $i++) {
            $out = (& ssh @(Get-GuestSshArgs $targets[$i]) $remoteCommand 2>&1)
            if ($LASTEXITCODE -eq 0) { $script:LastGuestSshOk = $true; return ($out | Select-Object -Last 1) }
            $lastLine = ($out | Select-Object -Last 1)
            if (($out | Out-String) -match 'resolve hostname' -and $targets.Count -eq 1) {
                $ip = Get-GuestIPAddress
                if ($ip) { $targets.Add("$user@$ip") }
            }
        }
        if ($attempt -lt $Retries) { Start-Sleep -Seconds $BackoffSeconds }
    }
    return $lastLine
}

# The canonical guest reaper owns cancellation of closed or superseded PR runs.
# The generation queue never uses it to force a worker to terminate.
function Invoke-GuestReapRuns {
    $remote = @'
set -euo pipefail
set -a
[ -f /etc/civm/run-reaper.env ] && . /etc/civm/run-reaper.env
set +a
repos="${CIVM_REAPER_REPOS:-}"
civmctl reap-runs --execute --repos="$repos" 2>&1 | tail -8
'@
    $out = Invoke-GuestSsh -Command $remote
    if ($script:LastGuestSshOk) { Write-OrcLog 'guest_reap_runs' @{ out = "$out" } }
    else { Write-OrcLog 'guest_reap_runs_warn' @{ error = "$out" } 'WARN' }
}

# The canonical stop guard delegates to civmctl idle-check. It is broader than a
# Runner.Worker process probe: it also sees PluginHost, _work and Docker builds.
# Any non-zero result or SSH uncertainty is busy.
function Get-GuestHasActiveJob {
    try {
        if ((Get-VM -Name $VMName -ErrorAction Stop).State -eq 'Off') {
            return $false
        }
    }
    catch {
        Write-OrcLog 'guest_active_probe_failed' @{ error = "vm_state: $($_.Exception.Message)" } 'WARN'
        return $true  # cannot prove Off -> fail-safe busy
    }
    $status = Invoke-GuestSsh -Command 'civmctl idle-check >/dev/null 2>&1; status=$?; printf "%s\n" "$status"; exit 0'
    if (-not $script:LastGuestSshOk) {
        Write-OrcLog 'guest_active_probe_failed' @{ error = "$status" } 'WARN'
        return $true
    }
    if ("$status" -eq '0') { return $false }
    $event = if ("$status" -eq '1') { 'guest_idle_busy' } else { 'guest_idle_unknown' }
    Write-OrcLog $event @{ exit_code = "$status" } 'WARN'
    return $true
}

function Wait-GuestSsh {
    param([int]$TimeoutSec = 600, [int]$PollSec = 10)
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        $null = Invoke-GuestSsh -Command 'true' -Retries 1 -BackoffSeconds 0
        if ($script:LastGuestSshOk) { return $true }
        Start-Sleep -Seconds $PollSec
    }
    return $false
}

function Invoke-GuestBoundaryPrepare {
    $capability = Invoke-GuestSsh -Command 'sudo -n /usr/local/bin/civm-generation-boundary --check' -TimeoutSec 30
    if (-not $script:LastGuestSshOk -or "$capability" -ne 'civm-generation-boundary/v1') {
        Write-OrcLog 'generation_capability_failed' @{ output = "$capability" } 'ERROR'
        return $false
    }
    # Root-owned wrapper only accepts this fixed verb. It performs strict drain,
    # full cleanup, fstrim and graceful poweroff as one fail-fast protocol.
    $out = Invoke-GuestSsh -Command 'sudo -n /usr/local/bin/civm-generation-boundary prepare' -TimeoutSec 1800
    if (-not $script:LastGuestSshOk) {
        Write-OrcLog 'generation_prepare_failed' @{ output = "$out" } 'ERROR'
        return $false
    }
    Write-OrcLog 'generation_prepared' @{ output = "$out" }
    return $true
}

function Invoke-GuestMaintenanceExit {
    $capability = Invoke-GuestSsh -Command 'sudo -n /usr/local/bin/civm-generation-boundary --check' -TimeoutSec 30
    if (-not $script:LastGuestSshOk -or "$capability" -ne 'civm-generation-boundary/v1') {
        Write-OrcLog 'generation_capability_failed' @{ output = "$capability" } 'ERROR'
        return $false
    }
    $out = Invoke-GuestSsh -Command 'sudo -n /usr/local/bin/civm-generation-boundary resume' -TimeoutSec 135
    if (-not $script:LastGuestSshOk) {
        Write-OrcLog 'generation_restore_failed' @{ output = "$out" } 'ERROR'
        return $false
    }
    Write-OrcLog 'generation_restored' @{ output = "$out" }
    return $true
}

function Wait-GuestOff {
    param([int]$TimeoutSec = 180)
    $deadline = (Get-Date).AddSeconds($TimeoutSec)
    while ((Get-Date) -lt $deadline) {
        try {
            if ((Get-VM -Name $VMName -ErrorAction Stop).State -eq 'Off') { return $true }
        }
        catch { break }
        Start-Sleep -Seconds 2
    }
    Write-OrcLog 'generation_shutdown_not_off' @{ timeout_sec = $TimeoutSec } 'ERROR'
    return $false
}

# Mede o V: livre em GB. 0 = medida falhou -> a decisao trata como fail-safe (nao
# entra em panic/warn por uma medida ruim — Kahneman #15).
function Get-VFreeGB {
    try { return [int]((Get-PSDrive V -ErrorAction Stop).Free / 1GB) }
    catch { Write-OrcLog 'vfree_probe_failed' @{ error = $_.Exception.Message } 'WARN'; return 0 }
}

# warn_clean: limpeza SEGURA durante CI ativo. Poda APENAS o cache de build do
# docker (regeneravel; nao toca imagens de runs em andamento -> sem o bug de
# eviction que o age-guard consertou) + fstrim (marca os blocos liberados pra a
# VHDX dinamica reusa-los em vez de crescer). Best-effort.
function Invoke-GuestWarnClean {
    $remote = 'sudo -n /usr/local/bin/civm-generation-boundary warn-clean; df -BG --output=avail / | tail -1 | tr -dc 0-9'
    $free = Invoke-GuestSsh -Command $remote
    if ($script:LastGuestSshOk) { Write-OrcLog 'disk_warn_clean' @{ free_after = "$free" } }
    else { Write-OrcLog 'disk_warn_clean_warn' @{ error = "$free" } 'WARN' }
}

# Compaction is permitted only after the generation boundary has already
# completed graceful shutdown. This function never changes VM power state.
function Invoke-CompactStoppedVm {
    param([string]$Reason, [int]$AdmitFloorGB = 80)
    $reclaimLock = $null
    try {
        $reclaimLock = [System.IO.FileStream]::new($ReclaimLockPath,
            [System.IO.FileMode]::OpenOrCreate, [System.IO.FileAccess]::ReadWrite, [System.IO.FileShare]::None)
    }
    catch {
        Write-OrcLog 'reclaim_skip_locked' @{ reason = $Reason; lock = $ReclaimLockPath } 'WARN'
        return [pscustomobject]@{ succeeded = $false; reason = 'reclaim lock unavailable'; vFreeGB = (Get-VFreeGB) }
    }
    try {
        Write-OrcLog 'reclaim_start' @{ reason = $Reason }
        if ((Get-VM -Name $VMName -ErrorAction Stop).State -ne 'Off') {
            Write-OrcLog 'generation_compact_requires_off' @{ reason = $Reason } 'ERROR'
            return [pscustomobject]@{ succeeded = $false; reason = 'VM is not Off'; vFreeGB = (Get-VFreeGB) }
        }
        Start-Sleep -Seconds 5
        $vBeforeGB = Get-VFreeGB
        Write-OrcLog 'reclaim_post_off_remeasure' @{ reason = $Reason; v_free_after_off_gb = $vBeforeGB; scratch_budget_gb = $ReclaimScratchBudgetGB }
        if (-not (Test-OptimizeSlack -VFreeAfterOffGB $vBeforeGB)) {
            Write-OrcLog 'reclaim_skip_insufficient_slack' @{ reason = $Reason; v_free_after_off_gb = $vBeforeGB; hard_floor_gb = $ReclaimHardFloorGB; scratch_budget_gb = $ReclaimScratchBudgetGB } 'ERROR'
            return [pscustomobject]@{ succeeded = $false; reason = 'insufficient Optimize-VHD scratch'; vFreeGB = $vBeforeGB }
        }
        # Hyper-V race: Get-VM State=Off can precede VHDX release (0x80070020 on
        # Mount-VHD). Settle + retry before treating as hard failure.
        $mounted = $false
        $mountErr = $null
        for ($attempt = 1; $attempt -le 8; $attempt++) {
            Start-Sleep -Seconds ([Math]::Min(2 * $attempt, 15))
            try {
                $vhdInfo = Get-VHD -Path $VhdxPath -ErrorAction Stop
                if ($vhdInfo.Attached) {
                    Write-OrcLog 'reclaim_vhdx_still_attached' @{ reason = $Reason; attempt = $attempt } 'WARN'
                    continue
                }
                Mount-VHD -Path $VhdxPath -ReadOnly -ErrorAction Stop
                $mounted = $true
                break
            }
            catch {
                $mountErr = $_.Exception.Message
                Write-OrcLog 'reclaim_mount_retry' @{ reason = $Reason; attempt = $attempt; error = "$mountErr" } 'WARN'
            }
        }
        if (-not $mounted) {
            Write-OrcLog 'reclaim_mount_failed' @{ reason = $Reason; error = "$mountErr" } 'ERROR'
            return [pscustomobject]@{ succeeded = $false; reason = 'VHDX mount failed'; vFreeGB = (Get-VFreeGB) }
        }
        $dismountError = $null
        try { Optimize-VHD -Path $VhdxPath -Mode Full -ErrorAction Stop }
        catch {
            Write-OrcLog 'reclaim_optimize_failed' @{ reason = $Reason; error = "$($_.Exception.Message)" } 'ERROR'
            return [pscustomobject]@{ succeeded = $false; reason = 'Optimize-VHD failed'; vFreeGB = (Get-VFreeGB) }
        }
        finally {
            try { Dismount-VHD -Path $VhdxPath -ErrorAction Stop }
            catch { $dismountError = $_.Exception.Message }
        }
        if ($null -ne $dismountError) {
            Write-OrcLog 'reclaim_dismount_failed' @{ reason = $Reason; error = "$dismountError" } 'ERROR'
            return [pscustomobject]@{ succeeded = $false; reason = 'VHDX dismount failed'; vFreeGB = (Get-VFreeGB) }
        }
        $vhd = Get-VHD -Path $VhdxPath
        $vAfterGB = Get-VFreeGB
        $recoveredGB = $vAfterGB - $vBeforeGB
        Write-OrcLog 'reclaim_done' @{ reason = $Reason; vhdx_gb = [int]($vhd.FileSize / 1GB); v_free_gb = $vAfterGB; recovered_gb = $recoveredGB }
        if ($vAfterGB -lt $AdmitFloorGB) {
            Write-OrcLog 'generation_capacity_blocked' @{ reason = $Reason; v_free_gb = $vAfterGB; floor_gb = $AdmitFloorGB; recovered_gb = $recoveredGB } 'ERROR'
            return [pscustomobject]@{ succeeded = $false; reason = 'post-compact capacity below floor'; vFreeGB = $vAfterGB }
        }
        return [pscustomobject]@{ succeeded = $true; reason = 'compacted'; vFreeGB = $vAfterGB }
    }
    catch {
        Write-OrcLog 'reclaim_failed' @{ reason = $Reason; error = "$($_.Exception.Message)" } 'ERROR'
        return [pscustomobject]@{ succeeded = $false; reason = 'reclaim exception'; vFreeGB = (Get-VFreeGB) }
    }
    finally {
        if ($null -ne $reclaimLock) {
            $reclaimLock.Close(); $reclaimLock.Dispose()
            Remove-Item -LiteralPath $ReclaimLockPath -Force -ErrorAction SilentlyContinue
        }
    }
}

# Carrega a decisao pura + as primitivas de reclaim (gate de 2 fases, cooldown)
# — os MESMOS modulos que os testes exercitam (Kahneman #13: codigo deployado ==
# codigo testado).
. "$PSScriptRoot\civm-orchestrator-decision.ps1"
. "$PSScriptRoot\civm-reclaim-gate.ps1"
. "$PSScriptRoot\civm-pr-queue.ps1"

# The published context is the authorization record read by the host gate. A
# missing file is an empty authorization; an unreadable file blocks progress.
function Read-PublishedContext {
    try {
        if (-not (Test-Path -LiteralPath $CurrentContextPath)) {
            return [pscustomobject]@{ readable = $true; context = '' }
        }
        $context = (Get-Content -LiteralPath $CurrentContextPath -Raw -ErrorAction Stop).Trim()
        if ($context -ne '' -and $context -notmatch '^(pr-\d+|branch-.+)@[0-9a-fA-F]{40}$') {
            Write-OrcLog 'generation_context_legacy_ignored' @{ context = $context } 'WARN'
            $context = ''
        }
        return [pscustomobject]@{ readable = $true; context = $context }
    }
    catch {
        Write-OrcLog 'generation_context_read_failed' @{ error = "$($_.Exception.Message)" } 'ERROR'
        return [pscustomobject]@{ readable = $false; context = '' }
    }
}

# Publication is the last admission step. A temporary file prevents a reader
# from observing a partially written generation ID; an absent value fails closed.
function Publish-CurrentContext {
    param([AllowEmptyString()][string]$ContextId)
    $temporaryPath = ''
    try {
        $temporaryPath = "$CurrentContextPath.$([guid]::NewGuid().ToString('N')).tmp"
        [System.IO.File]::WriteAllText($temporaryPath, $ContextId, [System.Text.Encoding]::ASCII)
        if ([System.IO.File]::Exists($CurrentContextPath)) {
            [System.IO.File]::Replace($temporaryPath, $CurrentContextPath, $null)
        }
        else {
            [System.IO.File]::Move($temporaryPath, $CurrentContextPath)
        }
        Write-OrcLog 'generation_context_published' @{ context = $ContextId }
        return $true
    }
    catch {
        Write-OrcLog 'generation_context_publish_failed' @{ context = $ContextId; error = "$($_.Exception.Message)" } 'ERROR'
        if (-not [string]::IsNullOrWhiteSpace($temporaryPath)) {
            Remove-Item -LiteralPath $temporaryPath -Force -ErrorAction SilentlyContinue
        }
        return $false
    }
}

function Save-PrQueueState {
    param([Parameter(Mandatory)]$Queue)
    $temporaryPath = ''
    try {
        $temporaryPath = "$PrQueuePath.$([guid]::NewGuid().ToString('N')).tmp"
        $serialized = $Queue | ConvertTo-Json -Depth 5 -Compress
        [System.IO.File]::WriteAllText($temporaryPath, $serialized, [System.Text.Encoding]::UTF8)
        if ([System.IO.File]::Exists($PrQueuePath)) {
            [System.IO.File]::Replace($temporaryPath, $PrQueuePath, $null)
        }
        else {
            [System.IO.File]::Move($temporaryPath, $PrQueuePath)
        }
        return $true
    }
    catch {
        Write-OrcLog 'generation_queue_state_save_failed' @{ error = "$($_.Exception.Message)" } 'ERROR'
        if (-not [string]::IsNullOrWhiteSpace($temporaryPath)) {
            Remove-Item -LiteralPath $temporaryPath -Force -ErrorAction SilentlyContinue
        }
        return $false
    }
}

# A published generation may need recovery after a host reboot. It can only be
# restarted when the same 80 GiB floor is still measured; it never creates a new
# authorization or skips the clean boundary that preceded publication.
function Ensure-PublishedGenerationAvailable {
    param([Parameter(Mandatory)][string]$ContextId, [int]$AdmitFloorGB = 80)
    $vFreeGB = Get-VFreeGB
    if ($vFreeGB -lt $AdmitFloorGB) {
        Write-OrcLog 'generation_recovery_capacity_blocked' @{ context = $ContextId; v_free_gb = $vFreeGB; floor_gb = $AdmitFloorGB } 'ERROR'
        return $false
    }
    try {
        $vm = Get-VM -Name $VMName -ErrorAction Stop
        if ($vm.State -eq 'Off') {
            Start-VM -Name $VMName -ErrorAction Stop
            if (-not (Wait-GuestSsh)) {
                Write-OrcLog 'generation_recovery_boot_failed' @{ context = $ContextId } 'ERROR'
                return $false
            }
            Write-OrcLog 'generation_recovery_booted' @{ context = $ContextId }
        }
        elseif ($vm.State -ne 'Running') {
            Write-OrcLog 'generation_recovery_vm_state_unknown' @{ context = $ContextId; vm = "$($vm.State)" } 'ERROR'
            return $false
        }
        if (Get-GuestHasActiveJob) { return $true }
        if (-not (Invoke-GuestMaintenanceExit)) { return $false }
        if (Get-GuestHasActiveJob) {
            Write-OrcLog 'generation_recovery_concurrent_work' @{ context = $ContextId } 'WARN'
            return $false
        }
        return $true
    }
    catch {
        Write-OrcLog 'generation_recovery_failed' @{ context = $ContextId; error = "$($_.Exception.Message)" } 'ERROR'
        return $false
    }
}

# Prepare a clean-slate boundary without changing the gate authorization. Every
# phase is fail-closed: on failure the old context remains published and no
# timeout can turn into a forced shutdown.
function Invoke-PrepareGeneration {
    param(
        [AllowEmptyString()][string]$ContextId,
        [bool]$ResumeRunner = $true,
        [int]$AdmitFloorGB = 80
    )
    $phase = 'vm_state'
    try {
        $vm = Get-VM -Name $VMName -ErrorAction Stop
        if ($vm.State -eq 'Off') {
            $phase = 'boot_before_drain'
            Start-VM -Name $VMName -ErrorAction Stop
            if (-not (Wait-GuestSsh)) {
                Write-OrcLog 'generation_boundary_failed' @{ context = $ContextId; phase = $phase; reason = 'guest SSH did not become ready' } 'ERROR'
                return [pscustomobject]@{ succeeded = $false; reason = $phase; vFreeGB = (Get-VFreeGB) }
            }
        }
        elseif ($vm.State -ne 'Running') {
            Write-OrcLog 'generation_boundary_failed' @{ context = $ContextId; phase = $phase; reason = "unexpected VM state $($vm.State)" } 'ERROR'
            return [pscustomobject]@{ succeeded = $false; reason = $phase; vFreeGB = (Get-VFreeGB) }
        }

        $phase = 'pre_drain_idle'
        if (Get-GuestHasActiveJob) {
            Write-OrcLog 'generation_boundary_deferred' @{ context = $ContextId; phase = $phase; reason = 'guest not proven idle' } 'WARN'
            return [pscustomobject]@{ succeeded = $false; reason = $phase; vFreeGB = (Get-VFreeGB) }
        }
        $phase = 'prepare_clean_shutdown'
        if (-not (Invoke-GuestBoundaryPrepare)) {
            return [pscustomobject]@{ succeeded = $false; reason = $phase; vFreeGB = (Get-VFreeGB) }
        }
        $phase = 'wait_for_graceful_shutdown'
        if (-not (Wait-GuestOff)) {
            return [pscustomobject]@{ succeeded = $false; reason = $phase; vFreeGB = (Get-VFreeGB) }
        }
        $phase = 'offline_compaction'
        $compact = Invoke-CompactStoppedVm -Reason 'generation_clean_boundary' -AdmitFloorGB $AdmitFloorGB
        if (-not $compact.succeeded) {
            return [pscustomobject]@{ succeeded = $false; reason = $phase; vFreeGB = $compact.vFreeGB }
        }
        if (-not $ResumeRunner) {
            Write-OrcLog 'generation_boundary_ready_off' @{ context = $ContextId; v_free_gb = $compact.vFreeGB }
            return [pscustomobject]@{ succeeded = $true; reason = 'clean boundary ready while off'; vFreeGB = $compact.vFreeGB }
        }

        $phase = 'boot_after_compaction'
        Start-VM -Name $VMName -ErrorAction Stop
        if (-not (Wait-GuestSsh)) {
            Write-OrcLog 'generation_boundary_failed' @{ context = $ContextId; phase = $phase; reason = 'guest SSH did not return after compaction' } 'ERROR'
            return [pscustomobject]@{ succeeded = $false; reason = $phase; vFreeGB = (Get-VFreeGB) }
        }
        $phase = 'restore_runner'
        if (-not (Invoke-GuestMaintenanceExit)) {
            return [pscustomobject]@{ succeeded = $false; reason = $phase; vFreeGB = (Get-VFreeGB) }
        }
        $phase = 'post_restore_idle'
        if (Get-GuestHasActiveJob) {
            Write-OrcLog 'generation_boundary_deferred' @{ context = $ContextId; phase = $phase; reason = 'guest accepted work before publication' } 'WARN'
            return [pscustomobject]@{ succeeded = $false; reason = $phase; vFreeGB = (Get-VFreeGB) }
        }
        Write-OrcLog 'generation_boundary_ready' @{ context = $ContextId; v_free_gb = (Get-VFreeGB) }
        return [pscustomobject]@{ succeeded = $true; reason = 'clean boundary ready'; vFreeGB = (Get-VFreeGB) }
    }
    catch {
        Write-OrcLog 'generation_boundary_failed' @{ context = $ContextId; phase = $phase; error = "$($_.Exception.Message)" } 'ERROR'
        return [pscustomobject]@{ succeeded = $false; reason = $phase; vFreeGB = (Get-VFreeGB) }
    }
}

# FIFO control plane for immutable PR/head generations. In enforcement mode
# this is the sole publisher and sole caller of Invoke-PrepareGeneration.
function Invoke-PrGenerationQueue {
    param(
        [Parameter(Mandatory)][string]$NowUtc,
        [int]$AdmitFloorGB = 80,
        [bool]$DoEnforce = $true
    )
    $result = [pscustomobject]@{
        verified = $false
        queued = 0
        running = 0
        current = ''
        action = 'defer'
        prepared = $false
    }
    try {
        $activity = Get-PrActivity
        $result.queued = [int]$activity.queued
        $result.running = [int]$activity.running
        if (-not $activity.verified) {
            Write-OrcLog 'generation_activity_unverified' @{ errors = ($activity.errors -join '; ') } 'ERROR'
            return $result
        }
        $result.verified = $true

        $queue = $null
        if (Test-Path -LiteralPath $PrQueuePath) {
            try { $queue = Get-Content -LiteralPath $PrQueuePath -Raw -ErrorAction Stop | ConvertFrom-Json }
            catch { Write-OrcLog 'generation_queue_state_invalid' @{ error = "$($_.Exception.Message)" } 'ERROR'; return $result }
        }
        $queue = Ensure-PrQueueState $queue
        if ($DoEnforce) {
            $published = Read-PublishedContext
            if (-not $published.readable) { return $result }
            if ("$($queue.currentPr)" -ne "$($published.context)") {
                Write-OrcLog 'generation_queue_reconciled' @{ state_context = "$($queue.currentPr)"; published_context = "$($published.context)" } 'WARN'
                $queue.currentPr = "$($published.context)"
                $queue.currentIdleSinceUtc = ''
            }
        }

        $seen = @{}
        $ordered = @()
        foreach ($entry in @($queue.contexts)) {
            if ($null -eq $entry) { continue }
            $id = "$($entry.id)"
            if ($activity.counts.ContainsKey($id)) {
                $ordered += [pscustomobject]@{ id = $id; firstSeenUtc = "$($activity.firstSeen[$id])" }
                $seen[$id] = $true
            }
        }
        $newContexts = @()
        foreach ($id in $activity.counts.Keys) {
            if (-not $seen.ContainsKey("$id")) {
                $newContexts += [pscustomobject]@{ id = "$id"; firstSeenUtc = "$($activity.firstSeen[$id])" }
            }
        }
        $ordered += @($newContexts | Sort-Object firstSeenUtc, id)

        $prs = @()
        foreach ($entry in $ordered) {
            $prs += [pscustomobject]@{ number = "$($entry.id)"; realJobs = [int]$activity.counts["$($entry.id)"] }
        }
        $slot = Resolve-PrSlot -Prs $prs -CurrentPr "$($queue.currentPr)" -CurrentIdleSinceUtc "$($queue.currentIdleSinceUtc)" -NowUtc $NowUtc
        $result.action = "$($slot.action)"
        $result.current = "$($slot.currentPr)"
        $summaryItems = @()
        foreach ($entry in $ordered) {
            $id = "$($entry.id)"
            $summaryItems += "${id}:$([int]$activity.counts[$id])"
        }
        $contextSummary = $summaryItems -join ' '
        Write-OrcLog "generation_queue_$($slot.action)" @{ current = "$($queue.currentPr)"; target = "$($slot.currentPr)"; contexts = $contextSummary; reason = "$($slot.reason)" }

        if (-not $DoEnforce) { return $result }

        $previousContext = "$($queue.currentPr)"
        $effectiveCurrent = "$($slot.currentPr)"
        $effectiveIdleSince = "$($slot.idleSinceUtc)"
        if ($slot.action -eq 'hold' -and $effectiveCurrent -ne '' -and $activity.counts.ContainsKey($effectiveCurrent)) {
            if (-not (Ensure-PublishedGenerationAvailable -ContextId $effectiveCurrent -AdmitFloorGB $AdmitFloorGB)) {
                Write-OrcLog 'generation_recovery_deferred' @{ context = $effectiveCurrent } 'WARN'
            }
        }
        elseif ($slot.action -eq 'grant' -or $slot.action -eq 'boundary_advance') {
            $resumeRunner = ($effectiveCurrent -ne '')
            $prepared = Invoke-PrepareGeneration -ContextId $effectiveCurrent -ResumeRunner $resumeRunner -AdmitFloorGB $AdmitFloorGB
            if ($prepared.succeeded -and (Publish-CurrentContext -ContextId $effectiveCurrent)) {
                $result.prepared = $true
                Write-OrcLog 'generation_admitted' @{ previous = $previousContext; current = $effectiveCurrent; v_free_gb = $prepared.vFreeGB }
            }
            else {
                $effectiveCurrent = $previousContext
                $effectiveIdleSince = "$($queue.currentIdleSinceUtc)"
                $result.current = $effectiveCurrent
                $result.action = 'defer'
                Write-OrcLog 'generation_admission_deferred' @{ previous = $previousContext; target = "$($slot.currentPr)"; reason = "$($prepared.reason)" } 'WARN'
            }
        }

        $queue.contexts = $ordered
        $queue.currentPr = $effectiveCurrent
        $queue.currentIdleSinceUtc = $effectiveIdleSince
        $null = Save-PrQueueState -Queue $queue
        $result.current = $effectiveCurrent
        return $result
    }
    catch {
        Write-OrcLog 'generation_queue_failed' @{ error = "$($_.Exception.Message)" } 'ERROR'
        return $result
    }
}

# ---- decisao principal ----
try {
    $state = Get-State
    $now = (Get-Date).ToUniversalTime()
    $nowUtc = $now.ToString('o')
    $AdmitFloorGB = 80

    if (-not $EnforceQueue -and -not $Observe) {
        Write-OrcLog 'enforce_queue_required' @{ note = 'refusing unsafe admission without the generation gate' } 'ERROR'
        exit 64
    }

    try {
        $last = [datetime]::Parse("$($state.lastBusyUtc)").ToUniversalTime()
    }
    catch {
        $last = $now
        Write-OrcLog 'generation_state_time_invalid' @{ value = "$($state.lastBusyUtc)" } 'WARN'
    }
    $idleMin = ($now - $last).TotalMinutes

    $queue = Invoke-PrGenerationQueue -NowUtc $nowUtc -AdmitFloorGB $AdmitFloorGB -DoEnforce:($EnforceQueue -and -not $Observe)
    if (-not $queue.verified) {
        throw 'generation activity is not verified; admission and idle shutdown are deferred'
    }
    $queued = [int]$queue.queued
    $running = [int]$queue.running
    if (($queued + $running) -gt 0) {
        $state.lastBusyUtc = $nowUtc
        if (-not $Observe) { Save-State $state }
        $idleMin = 0
    }
    $vm = Get-VM -Name $VMName -ErrorAction Stop
    $vfree = Get-VFreeGB
    Write-OrcLog 'tick' @{
        vm = "$($vm.State)"
        queued = $queued
        running = $running
        current = "$($queue.current)"
        queue_action = "$($queue.action)"
        idle_min = [math]::Round($idleMin, 1)
        v_free_gb = $vfree
        admit_floor_gb = $AdmitFloorGB
    }

    $decision = Get-OrchestratorDecision -VmState "$($vm.State)" -Queued $queued -Running $running -IdleMinutes $idleMin -IdleStopMinutes $IdleStopMinutes -HasActiveJobProbe { Get-GuestHasActiveJob } -VFreeGB $vfree -WarnFloorGB $WarnFloorGB -AdmitFloorGB $AdmitFloorGB
    switch ($decision) {
        'noop_off' { }
        'start' {
            Write-OrcLog 'generation_queue_owns_start' @{ current = "$($queue.current)"; observed = $Observe }
        }
        'admission_blocked' {
            Write-OrcLog 'generation_admission_capacity_blocked' @{ current = "$($queue.current)"; v_free_gb = $vfree; floor_gb = $AdmitFloorGB } 'ERROR'
        }
        'mark_busy' {
            if (-not $Observe) { $state.lastBusyUtc = $nowUtc; Save-State $state }
        }
        'idle_debounce' { Write-OrcLog 'idle_debounce' @{ idle_min = [math]::Round($idleMin, 1); need = $IdleStopMinutes } }
        'stop_aborted_active_job' {
            Write-OrcLog 'stop_aborted_active_job' @{ note = 'canonical idle-check found active or unknown guest work' }
            if (-not $Observe) { $state.lastBusyUtc = $nowUtc; Save-State $state }
        }
        'warn_clean' {
            if ($Observe) { Write-OrcLog 'would_warn_clean' @{ v_free_gb = $vfree; floor = $WarnFloorGB } }
            else { Write-OrcLog 'disk_warn' @{ v_free_gb = $vfree; floor = $WarnFloorGB }; Invoke-GuestWarnClean }
        }
        'stop_and_compact' {
            if ("$($queue.current)" -ne '') {
                Write-OrcLog 'generation_idle_boundary_deferred' @{ current = "$($queue.current)"; reason = 'generation grace or recovery is still active' }
            }
            elseif ($Observe) {
                Write-OrcLog 'would_prepare_idle_boundary' @{ idle_min = [math]::Round($idleMin, 1) }
            }
            else {
                $idleBoundary = Invoke-PrepareGeneration -ContextId '' -ResumeRunner:$false -AdmitFloorGB $AdmitFloorGB
                if (-not $idleBoundary.succeeded) {
                    Write-OrcLog 'generation_idle_boundary_failed' @{ reason = $idleBoundary.reason; v_free_gb = $idleBoundary.vFreeGB } 'ERROR'
                }
            }
        }
    }
}
catch {
    Write-OrcLog 'orchestrator_error' @{ error = $_.Exception.Message } 'ERROR'
    exit 1
}
