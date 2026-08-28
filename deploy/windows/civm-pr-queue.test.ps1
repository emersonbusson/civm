# Pure FIFO tests for generation identities. No GitHub, guest, or Hyper-V.
$ErrorActionPreference = 'Stop'
. "$PSScriptRoot\civm-pr-queue.ps1"

$pass = 0; $fail = 0
function Test-Slot($label, $got, $expectedAction, $expectedCurrent) {
    if ($got.action -eq $expectedAction -and "$($got.currentPr)" -eq "$expectedCurrent") {
        $script:pass++; "PASS  [$($got.action) -> $($got.currentPr)]  $label"
    }
    else {
        $script:fail++; "FAIL  expected=($expectedAction -> $expectedCurrent) got=($($got.action) -> $($got.currentPr)) :: $label"
    }
}
function Test-Eq($label, $got, $expected) {
    if ("$got" -eq "$expected") { $script:pass++; "PASS  [eq] $label" }
    else { $script:fail++; "FAIL  [eq] expected='$expected' got='$got' :: $label" }
}
function Pr($id, $jobs) { [pscustomobject]@{ number = $id; realJobs = $jobs } }
function PRRun($number, $sha) { [pscustomobject]@{ head_sha = $sha; head_branch = 'feature/demo'; pull_requests = @([pscustomobject]@{ number = $number }) } }
function BranchRun($branch, $sha) { [pscustomobject]@{ head_sha = $sha; head_branch = $branch; pull_requests = @() } }

# Exact, immutable generation identity: a new SHA is a new queue item.
Test-Eq 'PR generation includes number and SHA' (Get-RunGenerationContext (PRRun 10 'aaa')) 'pr-10@aaa'
Test-Eq 'branch generation includes ref and SHA' (Get-RunGenerationContext (BranchRun 'main' 'bbb')) 'branch-main@bbb'
Test-Eq 'missing SHA is rejected' (Get-RunGenerationContext (PRRun 10 '')) ''
Test-Eq 'two SHA values are distinct generations' ((Get-RunGenerationContext (PRRun 10 'aaa')) -ne (Get-RunGenerationContext (PRRun 10 'bbb'))) $true
Test-Eq 'legacy push-wave resolver is absent' ([bool](Get-Command Resolve-PushWaveCompact -ErrorAction SilentlyContinue)) $false

$migrated = Ensure-PrQueueState ([pscustomobject]@{ currentPr = 'pr-42@aaa'; currentIdleSinceUtc = '' })
Test-Eq 'state migration has contexts' @($migrated.contexts).Count 0
Test-Eq 'state migration preserves current generation' "$($migrated.currentPr)" 'pr-42@aaa'

$now = '2026-08-04T00:10:00Z'
$ago9 = '2026-08-04T00:01:00Z'
$ago10 = '2026-08-04T00:00:00Z'

Test-Slot 'empty queue stays idle' (Resolve-PrSlot -Prs @() -CurrentPr '' -NowUtc $now) 'idle' ''
Test-Slot 'grant first exact generation' (Resolve-PrSlot -Prs @((Pr 'pr-10@aaa' 0), (Pr 'pr-10@bbb' 0)) -CurrentPr '' -NowUtc $now) 'grant' 'pr-10@aaa'
Test-Slot 'same PR newer SHA stays behind current generation' (Resolve-PrSlot -Prs @((Pr 'pr-10@aaa' 2), (Pr 'pr-10@bbb' 1)) -CurrentPr 'pr-10@aaa' -NowUtc $now) 'hold' 'pr-10@aaa'

$active = Resolve-PrSlot -Prs @((Pr 'pr-10@aaa' 2), (Pr 'pr-10@bbb' 1)) -CurrentPr 'pr-10@aaa' -CurrentIdleSinceUtc $ago10 -NowUtc $now
Test-Slot 'active run holds current generation' $active 'hold' 'pr-10@aaa'
Test-Eq 'active run clears old grace' "$($active.idleSinceUtc)" ''

$armed = Resolve-PrSlot -Prs @((Pr 'pr-10@aaa' 0), (Pr 'pr-10@bbb' 1)) -CurrentPr 'pr-10@aaa' -CurrentIdleSinceUtc '' -NowUtc $now
Test-Slot 'first empty observation arms ten-minute grace' $armed 'hold' 'pr-10@aaa'
Test-Eq 'grace starts at observation time' "$($armed.idleSinceUtc)" $now
Test-Slot 'nine minutes remains inside grace' (Resolve-PrSlot -Prs @((Pr 'pr-10@aaa' 0), (Pr 'pr-10@bbb' 1)) -CurrentPr 'pr-10@aaa' -CurrentIdleSinceUtc $ago9 -NowUtc $now) 'hold' 'pr-10@aaa'
Test-Slot 'ten minutes advances to next exact generation' (Resolve-PrSlot -Prs @((Pr 'pr-10@aaa' 0), (Pr 'pr-10@bbb' 1)) -CurrentPr 'pr-10@aaa' -CurrentIdleSinceUtc $ago10 -NowUtc $now) 'boundary_advance' 'pr-10@bbb'
Test-Slot 'last generation advances to empty only after grace' (Resolve-PrSlot -Prs @((Pr 'pr-10@aaa' 0)) -CurrentPr 'pr-10@aaa' -CurrentIdleSinceUtc $ago10 -NowUtc $now) 'boundary_advance' ''

''; "RESULT: $pass PASS / $fail FAIL"
if ($fail -gt 0) { exit 1 }
