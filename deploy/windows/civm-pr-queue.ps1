# civm-pr-queue.ps1 — pure FIFO decision for the shared-box generation queue.
#
# The exclusion unit is an immutable generation: PR+head SHA (`pr-123@abc`) or
# branch+SHA (`branch-main@abc`). A new push becomes a new FIFO item; it never
# changes the SHA of a generation that still has checks. This removes the need
# for push-wave reap/wait/forced compaction.

function Get-RunGenerationContext {
    [CmdletBinding()]
    param([Parameter(Mandatory)]$Run)

    $sha = [string]$Run.head_sha
    if ([string]::IsNullOrWhiteSpace($sha)) { return '' }
    if ($Run.pull_requests -and @($Run.pull_requests).Count -gt 0) {
        $number = $Run.pull_requests[0].number
        if ($null -ne $number -and "${number}" -match '^\d+$') {
            return "pr-$number@$sha"
        }
        return ''
    }
    $branch = [string]$Run.head_branch
    if ([string]::IsNullOrWhiteSpace($branch)) { return '' }
    return "branch-$branch@$sha"
}

function Ensure-PrQueueState {
    param($State)
    if ($null -eq $State) {
        $State = [pscustomobject]@{}
    }
    $defaults = [ordered]@{
        contexts = @()
        currentPr = ''
        currentIdleSinceUtc = ''
    }
    foreach ($entry in $defaults.GetEnumerator()) {
        if (-not ($State.PSObject.Properties.Name -contains $entry.Key)) {
            $State | Add-Member -NotePropertyName $entry.Key -NotePropertyValue $entry.Value -Force
        }
    }
    return $State
}

# Resolve-PrSlot decides the queue action from observed state. It returns an
# action (grant|hold|boundary_advance|idle), currentPr, and the grace clock. It
# is pure: no I/O, GitHub, guest, or Hyper-V.
function Resolve-PrSlot {
    [CmdletBinding()]
    param(
        # FIFO contexts. Each item has number (the exact generation ID) and
        # realJobs (queued+in_progress workflow runs for that generation).
        [object[]]$Prs = @(),
        [string]$CurrentPr = '',
        [string]$CurrentIdleSinceUtc = '',
        [Parameter(Mandatory)][string]$NowUtc,
        # Ten minutes covers workflow propagation without promoting a transient
        # absence to generation completion.
        [int]$DoneGraceMinutes = 10
    )
    $byID = @{}
    foreach ($pr in $Prs) {
        if ($null -ne $pr) { $byID["$($pr.number)"] = [int]$pr.realJobs }
    }

    $next = ''
    foreach ($pr in $Prs) {
        if ($null -ne $pr -and "$($pr.number)" -ne "$CurrentPr") {
            $next = "$($pr.number)"
            break
        }
    }

    if ("$CurrentPr" -ne '') {
        $currentJobs = if ($byID.ContainsKey("$CurrentPr")) { $byID["$CurrentPr"] } else { 0 }
        if ($currentJobs -gt 0) {
            return [pscustomobject]@{ action = 'hold'; currentPr = $CurrentPr; idleSinceUtc = ''; reason = "generation $CurrentPr has $currentJobs active run(s)" }
        }
        if ([string]::IsNullOrWhiteSpace($CurrentIdleSinceUtc)) {
            return [pscustomobject]@{ action = 'hold'; currentPr = $CurrentPr; idleSinceUtc = $NowUtc; reason = "generation $CurrentPr has no active run; grace armed" }
        }
        $idleMinutes = ([datetime]::Parse($NowUtc).ToUniversalTime() - [datetime]::Parse($CurrentIdleSinceUtc).ToUniversalTime()).TotalMinutes
        if ($idleMinutes -lt $DoneGraceMinutes) {
            return [pscustomobject]@{ action = 'hold'; currentPr = $CurrentPr; idleSinceUtc = $CurrentIdleSinceUtc; reason = "generation $CurrentPr remains inside grace ($([math]::Round($idleMinutes,1))<$DoneGraceMinutes)" }
        }
        return [pscustomobject]@{ action = 'boundary_advance'; currentPr = $next; idleSinceUtc = ''; reason = "generation $CurrentPr completed after $([math]::Round($idleMinutes,1)) minutes; clean boundary before '$next'" }
    }

    if ("$next" -ne '') {
        return [pscustomobject]@{ action = 'grant'; currentPr = $next; idleSinceUtc = ''; reason = "grant clean boundary to '$next'" }
    }
    return [pscustomobject]@{ action = 'idle'; currentPr = ''; idleSinceUtc = ''; reason = 'generation queue empty' }
}
