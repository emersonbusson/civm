# Pure decision-table test. It has no Hyper-V, SSH, or GitHub dependency.
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot\civm-orchestrator-decision.ps1"

$trueValue = $true; $falseValue = $false
$cases = @(
    @{ vm = 'Off'; q = 0; r = 0; idle = 0; probe = $falseValue; vfree = 80; expected = 'noop_off'; description = 'off and no work stays off' },
    @{ vm = 'Off'; q = 1; r = 0; idle = 0; probe = $falseValue; vfree = 80; expected = 'start'; description = 'off with exactly 80 GiB may start after boundary' },
    @{ vm = 'Off'; q = 1; r = 0; idle = 0; probe = $falseValue; vfree = 79; expected = 'admission_blocked'; description = 'off with 79 GiB never admits' },
    @{ vm = 'Off'; q = 1; r = 0; idle = 0; probe = $falseValue; vfree = 0; expected = 'admission_blocked'; description = 'unknown off capacity never admits' },
    @{ vm = 'Running'; q = 1; r = 2; idle = 0; probe = $falseValue; vfree = 80; expected = 'mark_busy'; description = 'active work stays running' },
    @{ vm = 'Running'; q = 1; r = 2; idle = 0; probe = $falseValue; vfree = 15; expected = 'warn_clean'; description = 'critical disk with active work is online clean only' },
    @{ vm = 'Running'; q = 1; r = 0; idle = 0; probe = $falseValue; vfree = 80; expected = 'mark_busy'; description = 'queued clean generation remains available' },
    @{ vm = 'Running'; q = 1; r = 0; idle = 0; probe = $falseValue; vfree = 79; expected = 'admission_blocked'; description = 'queued generation below 80 waits for boundary' },
    @{ vm = 'Running'; q = 0; r = 0; idle = 9.9; probe = $falseValue; vfree = 80; expected = 'idle_debounce'; description = 'idle debounce holds before stop' },
    @{ vm = 'Running'; q = 0; r = 0; idle = 10; probe = $trueValue; vfree = 80; expected = 'stop_aborted_active_job'; description = 'canonical busy probe aborts idle stop' },
    @{ vm = 'Running'; q = 0; r = 0; idle = 10; probe = $falseValue; vfree = 80; expected = 'stop_and_compact'; description = 'proven idle may enter safe maintenance' }
)

$pass = 0; $fail = 0
foreach ($case in $cases) {
    $probe = if ($case.probe) { { $true } } else { { $false } }
    $got = Get-OrchestratorDecision -VmState $case.vm -Queued $case.q -Running $case.r -IdleMinutes $case.idle -IdleStopMinutes 10 -HasActiveJobProbe $probe -VFreeGB $case.vfree -AdmitFloorGB 80
    if ($got -eq $case.expected) { $pass++; "PASS  [$got] $($case.description)" }
    else { $fail++; "FAIL  expected=$($case.expected) got=$got :: $($case.description)" }
}

''; "RESULT: $pass PASS / $fail FAIL"
if ($fail -gt 0) { exit 1 }
