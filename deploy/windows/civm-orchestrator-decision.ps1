# Pure scale-to-zero decision. It never touches the VM: the caller performs the
# returned action after the generation queue has proven its boundary conditions.

function Get-OrchestratorDecision {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)][ValidateSet('Off', 'Running')][string]$VmState,
        [Parameter(Mandatory)][int]$Queued,
        [Parameter(Mandatory)][int]$Running,
        [double]$IdleMinutes = 0,
        [int]$IdleStopMinutes = 10,
        [scriptblock]$HasActiveJobProbe = { $false },
        [int]$VFreeGB = 0,
        [int]$WarnFloorGB = 28,
        # A new generation must have a measured 80 GiB free after the clean
        # boundary. Unknown capacity is not permission to admit work.
        [int]$AdmitFloorGB = 80
    )

    $hasWork = (($Queued + $Running) -gt 0)
    if ($VmState -eq 'Off') {
        if (-not $hasWork) { return 'noop_off' }
        if ($VFreeGB -le 0 -or $VFreeGB -lt $AdmitFloorGB) {
            return 'admission_blocked'
        }
        return 'start'
    }

    # During an active job, disk pressure can trigger online, regenerable-cache
    # cleanup only. It must never power off the guest or terminate the job.
    if ($Running -gt 0) {
        if ($VFreeGB -gt 0 -and $VFreeGB -lt $WarnFloorGB) { return 'warn_clean' }
        return 'mark_busy'
    }

    # Queued work with no running job is a generation admission point. The
    # enforce queue owns the clean boundary; this generic decision may only hold.
    if ($Queued -gt 0) {
        if ($VFreeGB -le 0 -or $VFreeGB -lt $AdmitFloorGB) { return 'admission_blocked' }
        return 'mark_busy'
    }

    if ($IdleMinutes -lt $IdleStopMinutes) { return 'idle_debounce' }
    if (& $HasActiveJobProbe) { return 'stop_aborted_active_job' }
    return 'stop_and_compact'
}
