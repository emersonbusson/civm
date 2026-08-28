$ErrorActionPreference = 'Stop'

. "$PSScriptRoot\configure-civm-vm-memory.ps1"

$pass = 0
$fail = 0

function Check($Name, $Got, $Expected) {
    if ($Got -eq $Expected) {
        $script:pass++
        "PASS  $Name"
        return
    }

    $script:fail++
    "FAIL  $Name (expected=$Expected got=$Got)"
}

$gib = [int64]1GB
$vmOff = { param($Name) [pscustomobject]@{ State = 'Off'; Name = $Name } }
$vmRunning = { param($Name) [pscustomobject]@{ State = 'Running'; Name = $Name } }
$dynamicMemory = {
    param($Name)
    [pscustomobject]@{
        VMName               = $Name
        DynamicMemoryEnabled = $true
        Startup              = [int64](7.5GB)
    }
}
$fixedMemory = {
    param($Name)
    [pscustomobject]@{
        VMName               = $Name
        DynamicMemoryEnabled = $false
        Startup              = [int64](12GB)
    }
}

$script:mutations = 0
$setMemory = {
    param($Name, $Bytes)
    $script:mutations++
    $script:lastMutation = [pscustomobject]@{ Name = $Name; Bytes = $Bytes }
}

$dryRun = Invoke-CivmVmMemoryConfiguration `
    -VMName 'gha-ubuntu-2404' -MemoryGiB 12 `
    -GetVMFn $vmRunning -GetMemoryFn $dynamicMemory -SetMemoryFn $setMemory
Check 'dry-run reports plan' $dryRun.status 'plan'
Check 'dry-run never mutates' $script:mutations 0

$runningFailed = $false
try {
    Invoke-CivmVmMemoryConfiguration `
        -VMName 'gha-ubuntu-2404' -MemoryGiB 12 -Execute `
        -GetVMFn $vmRunning -GetMemoryFn $dynamicMemory -SetMemoryFn $setMemory | Out-Null
}
catch {
    $runningFailed = $_.Exception.Message -match 'Off'
}
Check 'execute refuses a running VM' $runningFailed $true
Check 'running refusal happens before mutation' $script:mutations 0

$noop = Invoke-CivmVmMemoryConfiguration `
    -VMName 'gha-ubuntu-2404' -MemoryGiB 12 -Execute `
    -GetVMFn $vmOff -GetMemoryFn $fixedMemory -SetMemoryFn $setMemory
Check 'matching fixed memory is idempotent' $noop.status 'noop'
Check 'idempotent apply performs zero mutations' $script:mutations 0

$script:memoryReads = 0
$driftThenFixed = {
    param($Name)
    $script:memoryReads++
    if ($script:memoryReads -eq 1) { return (& $dynamicMemory $Name) }
    return (& $fixedMemory $Name)
}
$changed = Invoke-CivmVmMemoryConfiguration `
    -VMName 'gha-ubuntu-2404' -MemoryGiB 12 -Execute `
    -GetVMFn $vmOff -GetMemoryFn $driftThenFixed -SetMemoryFn $setMemory
Check 'off VM with drift is changed' $changed.status 'changed'
Check 'change calls Set-VMMemory exactly once' $script:mutations 1
Check 'change targets 12 GiB' $script:lastMutation.Bytes ([int64](12 * $gib))

$postconditionFailed = $false
try {
    Invoke-CivmVmMemoryConfiguration `
        -VMName 'gha-ubuntu-2404' -MemoryGiB 12 -Execute `
        -GetVMFn $vmOff -GetMemoryFn $dynamicMemory -SetMemoryFn $setMemory | Out-Null
}
catch {
    $postconditionFailed = $_.Exception.Message -match 'post-condition'
}
Check 'divergent post-condition fails closed' $postconditionFailed $true

$source = Get-Content -LiteralPath "$PSScriptRoot\configure-civm-vm-memory.ps1" -Raw
Check 'source never starts the VM' ($source -match '\bStart-VM\b') $false
Check 'source never stops the VM' ($source -match '\bStop-VM\b') $false
Check 'source defaults to 12 GiB' ($source -match '\[int\]\$MemoryGiB\s*=\s*12') $true

''
"RESULT: $pass PASS / $fail FAIL"
if ($fail -gt 0) { exit 1 }

