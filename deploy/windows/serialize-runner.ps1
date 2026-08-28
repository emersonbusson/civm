<#
.SYNOPSIS
    Impoe o invariante de serializacao de runner do box: no maximo 1 runner
    civm-labeled por organizacao (o runner ORG), sem runner por-repo redundante.

.DESCRIPTION
    A box (guest gha-ubuntu-2404) hospeda varios runners self-hosted. O acme
    e servido por um runner ORG (civm-acme-org, gitHubUrl https://github.com/acme)
    que atende acme/app E acme/civm num unico processo, serializando a org
    inteira em fila FIFO. Se um runner POR-REPO (civm-app, repo acme/app)
    coexistir, ambos carregam o label civm e um job de acme cai em qualquer um
    -> 2 jobs concorrentes no mesmo disco/daemon Docker -> "concurrent prune on
    shared civm runner" mata o docker pull de um deles (incidente #1184,
    validation.md 2026-06-18).

    Este script e IDEMPOTENTE e OPERADOR-GATED (dry-run por default, igual aos
    subcomandos destrutivos do civmctl). Ele:
      1. roda `civmctl doctor --json` no guest (via SSH) e le o check
         RUNNER_SERIALIZATION + systemd_runners;
      2. para cada runner por-repo redundante, executa a REMOCAO COMPLETA via
         `civmctl runner remove` (svc.sh stop + uninstall + config.sh remove +
         rm -rf) no guest — mantendo o runner org intacto.

    Por que REMOCAO e nao `systemctl disable`: um runner so disabled continua
    loaded; o runner-watchdog (tick de ~2min) o ve inactive/dead e da
    `systemctl restart`, RESSUSCITANDO a colisao. O watchdog ja foi endurecido
    para declinar esse restart (runner-restart-skipped redundant-repo-runner),
    mas o estado duravel correto e o runner por-repo NAO existir como service —
    assim runner.List() nunca mais o ve. Day-0: o runner org torna o por-repo
    pura redundancia, entao remove-se de vez (sem shim de "disable depois deleta").

.NOTES
    Provisionamento real do runner org: registrado MANUALMENTE no nivel da org
    (GitHub Settings > Actions > Runners da organizacao). O `civmctl runner add`
    so cobre runners POR-REPO (--repo=owner/repo). Por isso a serializacao nao
    pode ser garantida so pelo `runner add`; ela e imposta por (a) este script,
    (b) o guard RUNNER_SERIALIZATION no doctor, (c) o watchdog que nao ressuscita,
    e (d) o runbook de adocao (org runner como path primario).

    Idempotencia: sem colisao -> no-op (sai 0). Re-rodar apos remocao -> no-op.
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    # Alvo SSH do guest. Mesmo default do orchestrator (emdev@gha-ubuntu-2404).
    [string]$GuestSshTarget = 'emdev@gha-ubuntu-2404',
    [string]$SshKeyPath = 'C:\ProgramData\civm\ssh\id_ed25519',
    # PAT por owner para mintar o remove-token (gh api .../actions/runners/remove-token).
    # Preencha no host: @{ 'myorg' = 'C:\ProgramData\civm\gh-token-myorg.txt' }
    [hashtable]$TokenPaths = @{},
    # Path do civmctl no guest.
    [string]$CivmctlPath = '/usr/local/bin/civmctl',
    # Aplica de fato (default: dry-run, so reporta o que faria).
    [switch]$Execute
)

$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

function Write-SrLog {
    param([string]$Event, [hashtable]$Data = @{}, [string]$Level = 'INFO')
    $rec = [ordered]@{ ts = (Get-Date).ToUniversalTime().ToString('o'); level = $Level; event = $Event }
    foreach ($k in $Data.Keys) { $rec[$k] = $Data[$k] }
    Write-Host ($rec | ConvertTo-Json -Compress -Depth 6)
}

# Invoca um comando no guest via SSH e devolve stdout cru. Falha (throw) se o
# ssh sair non-zero — o caller decide o que fazer.
function Invoke-Guest {
    param([Parameter(Mandatory)][string]$Command)
    $sshArgs = @(
        '-o', 'BatchMode=yes', '-o', 'ConnectTimeout=20',
        '-o', 'StrictHostKeyChecking=accept-new'
    )
    if (Test-Path -LiteralPath $SshKeyPath) { $sshArgs += @('-i', $SshKeyPath) }
    $sshArgs += @($GuestSshTarget, $Command)
    $out = & ssh @sshArgs 2>&1
    if ($LASTEXITCODE -ne 0) {
        throw "ssh '$Command' saiu $LASTEXITCODE`: $out"
    }
    return ($out -join "`n")
}

function Get-RepoOwner {
    param([string]$Repo)
    $i = $Repo.IndexOf('/')
    if ($i -le 0) { return '' }
    return $Repo.Substring(0, $i)
}

# Le o remove-token efemero (~1h) do repo. Usa o PAT do owner via GH_TOKEN.
function Get-RemoveToken {
    param([string]$Repo)
    $owner = Get-RepoOwner $Repo
    $tokenPath = $TokenPaths[$owner]
    if ([string]::IsNullOrWhiteSpace($tokenPath) -or -not (Test-Path -LiteralPath $tokenPath)) {
        throw "sem PAT para owner '$owner' (esperado em TokenPaths); nao da pra mintar remove-token de $Repo"
    }
    $pat = (Get-Content -LiteralPath $tokenPath -Raw).Trim()
    # Mint do remove-token NO GUEST (gh ja autenticado la), com GH_TOKEN do owner.
    $cmd = "GH_TOKEN='$pat' gh api -X POST /repos/$Repo/actions/runners/remove-token --jq .token"
    $token = (Invoke-Guest -Command $cmd).Trim()
    if ([string]::IsNullOrWhiteSpace($token)) { throw "remove-token vazio para $Repo" }
    return $token
}

Write-SrLog 'serialize_start' @{ target = $GuestSshTarget; execute = [bool]$Execute }

# 1) Diagnostico: civmctl doctor --json. O check RUNNER_SERIALIZATION ja
#    classifica a colisao; systemd_runners da o snapshot bruto.
$doctorJson = Invoke-Guest -Command "$CivmctlPath doctor --repos=auto --json"
$doctor = $doctorJson | ConvertFrom-Json

$serCheck = $doctor.hook_checks | Where-Object { $_.name -eq 'RUNNER_SERIALIZATION' }
if ($null -eq $serCheck) {
    throw "doctor nao retornou o check RUNNER_SERIALIZATION (civmctl desatualizado no guest?)"
}
Write-SrLog 'serialization_check' @{ severity = $serCheck.severity; detail = $serCheck.detail }

if ($serCheck.severity -eq 'ok') {
    Write-SrLog 'serialize_noop' @{ reason = 'sem runner por-repo redundante (ja serializado)' }
    exit 0
}

# 2) Deriva as units redundantes do snapshot systemd. A regra espelha
#    runner.DetectCollisions (Go): um runner por-repo (name=civm-<x>, repo
#    owner/repo) cujo OWNER tem um runner org (name termina em -org, repo sem
#    barra) e redundante. Mantemos a logica fonte-da-verdade no Go; aqui so
#    reconstruimos a lista de units a remover a partir do mesmo snapshot.
$units = @($doctor.systemd_runners)
$orgOwners = @{}
foreach ($u in $units) {
    if ($u.name -like '*-org' -and $u.repo -and ($u.repo -notlike '*/*')) {
        $orgOwners[$u.repo] = $u.name
    }
}

$redundant = @()
foreach ($u in $units) {
    if ($u.name -like '*-org') { continue }
    $owner = Get-RepoOwner $u.repo
    if ($owner -and $orgOwners.ContainsKey($owner)) {
        $redundant += [pscustomobject]@{
            Unit  = $u.unit_name
            Name  = $u.name
            Repo  = $u.repo
            Org   = $orgOwners[$owner]
            Short = ($u.name -replace '^civm-', '')
        }
    }
}

if ($redundant.Count -eq 0) {
    # O check disse critico mas nao reconstruimos unit — defensivo: nao agir as cegas.
    Write-SrLog 'serialize_mismatch' @{ reason = 'RUNNER_SERIALIZATION critico mas nenhuma unit redundante derivada'; detail = $serCheck.detail } 'WARN'
    exit 1
}

# 3) Enforcement: remove cada runner por-repo redundante (dry-run default).
$failed = 0
foreach ($r in $redundant) {
    if (-not $Execute) {
        Write-SrLog 'would_remove' @{ unit = $r.Unit; name = $r.Name; repo = $r.Repo; keeps_org = $r.Org }
        continue
    }
    if (-not $PSCmdlet.ShouldProcess($r.Unit, "civmctl runner remove (mantem $($r.Org))")) { continue }
    try {
        $token = Get-RemoveToken -Repo $r.Repo
        # civmctl runner remove: svc.sh stop + uninstall + config.sh remove + rm -rf.
        # Aborta fail-closed se houver job/build ativo (idle-gate interno).
        $cmd = "sudo $CivmctlPath runner remove --short='$($r.Short)' --token='$token' --execute"
        $out = Invoke-Guest -Command $cmd
        Write-SrLog 'removed' @{ unit = $r.Unit; repo = $r.Repo; keeps_org = $r.Org; detail = ($out -split "`n" | Select-Object -Last 1) }
    }
    catch {
        $failed++
        Write-SrLog 'remove_failed' @{ unit = $r.Unit; repo = $r.Repo; error = "$_" } 'ERROR'
    }
}

if (-not $Execute) {
    Write-SrLog 'serialize_dryrun_done' @{ redundant = $redundant.Count; hint = 'rode novamente com -Execute para aplicar' }
    exit 0
}

if ($failed -gt 0) {
    Write-SrLog 'serialize_done' @{ removed = ($redundant.Count - $failed); failed = $failed } 'ERROR'
    exit 1
}
Write-SrLog 'serialize_done' @{ removed = $redundant.Count; failed = 0 }
exit 0
