[CmdletBinding()]
param(
    [ValidateNotNullOrEmpty()]
    [string]$VMName = 'gha-ubuntu-2404',

    [ValidateRange(4, 64)]
    [int]$MemoryGiB = 12,

    [switch]$Execute
)

$ErrorActionPreference = 'Stop'

function Invoke-CivmVmMemoryConfiguration {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [ValidateNotNullOrEmpty()]
        [string]$VMName,

        [Parameter(Mandatory)]
        [ValidateRange(4, 64)]
        [int]$MemoryGiB,

        [switch]$Execute,

        [scriptblock]$GetVMFn = {
            param($Name)
            Get-VM -Name $Name -ErrorAction Stop
        },

        [scriptblock]$GetMemoryFn = {
            param($Name)
            Get-VMMemory -VMName $Name -ErrorAction Stop
        },

        [scriptblock]$SetMemoryFn = {
            param($Name, $Bytes)
            Set-VMMemory -VMName $Name `
                -DynamicMemoryEnabled $false `
                -StartupBytes $Bytes `
                -ErrorAction Stop
        }
    )

    $targetBytes = [int64]$MemoryGiB * [int64]1GB
    $vm = & $GetVMFn $VMName
    $memory = & $GetMemoryFn $VMName
    $vmState = "$($vm.State)"
    $isTarget = (-not [bool]$memory.DynamicMemoryEnabled) -and
        ([int64]$memory.Startup -eq $targetBytes)

    $result = [ordered]@{
        status                 = 'plan'
        vm                     = $VMName
        vm_state               = $vmState
        execute                = [bool]$Execute
        target_memory_gib      = $MemoryGiB
        dynamic_memory_before  = [bool]$memory.DynamicMemoryEnabled
        startup_memory_gib_old = [math]::Round(([int64]$memory.Startup / 1GB), 2)
    }

    if (-not $Execute) {
        return [pscustomobject]$result
    }

    if ($vmState -ne 'Off') {
        throw "VM '$VMName' precisa estar Off antes de configurar memoria; estado atual: $vmState"
    }

    if ($isTarget) {
        $result.status = 'noop'
        return [pscustomobject]$result
    }

    & $SetMemoryFn $VMName $targetBytes

    $afterVM = & $GetVMFn $VMName
    $afterMemory = & $GetMemoryFn $VMName
    $afterState = "$($afterVM.State)"
    $postConditionOK = ($afterState -eq 'Off') -and
        (-not [bool]$afterMemory.DynamicMemoryEnabled) -and
        ([int64]$afterMemory.Startup -eq $targetBytes)
    if (-not $postConditionOK) {
        throw "memory post-condition failed for VM '$VMName'"
    }

    $result.status = 'changed'
    $result.vm_state = $afterState
    $result.dynamic_memory_after = [bool]$afterMemory.DynamicMemoryEnabled
    $result.startup_memory_gib_new = [math]::Round(([int64]$afterMemory.Startup / 1GB), 2)
    return [pscustomobject]$result
}

if ($MyInvocation.InvocationName -ne '.') {
    Invoke-CivmVmMemoryConfiguration `
        -VMName $VMName `
        -MemoryGiB $MemoryGiB `
        -Execute:$Execute | ConvertTo-Json -Compress
}

