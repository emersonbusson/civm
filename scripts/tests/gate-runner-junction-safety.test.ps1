$ErrorActionPreference = 'Stop'

$repoRoot = [System.IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..'))
$sources = @(
    (Join-Path $repoRoot 'deploy\windows\civm-gate-runner-provision.ps1'),
    (Join-Path $repoRoot 'deploy\windows\civm-gate-task-setup.ps1')
)
$expectedVersion = '2.336.0'

function Remove-Junction {
    param([Parameter(Mandatory)][string]$Path)
    if (-not (Test-Path -LiteralPath $Path)) { return }
    $item = Get-Item -LiteralPath $Path -Force
    if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -eq 0) {
        throw "refusing to remove non-junction fixture: $Path"
    }
    & cmd.exe /d /c "rmdir `"$Path`""
    if ($LASTEXITCODE -ne 0) { throw "rmdir failed for junction fixture: $Path" }
}

function Assert-Rejected {
    param(
        [Parameter(Mandatory)][scriptblock]$Operation,
        [Parameter(Mandatory)][string]$Pattern
    )
    try {
        & $Operation
        throw 'unsafe junction fixture was accepted'
    } catch {
        if ($_.Exception.Message -notlike $Pattern) { throw }
    }
}

foreach ($source in $sources) {
    $tokens = $null
    $parseErrors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseFile(
        $source, [ref]$tokens, [ref]$parseErrors)
    if ($parseErrors.Count -ne 0) { throw "parse failed: $source" }
    foreach ($name in @(
            'Initialize-CivmGateNative',
            'Get-FileLinkCount',
            'Assert-SafeOfficialRunnerJunction',
            'Get-SafeTreeItems')) {
        $functionAst = $ast.Find({
            param($node)
            $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
                $node.Name -eq $name
        }, $true)
        if ($null -eq $functionAst) { throw "missing function $name in $source" }
        Invoke-Expression $functionAst.Extent.Text
    }

    $RunnerVersion = $expectedVersion
    $expectedRunnerVersion = $expectedVersion
    $fixture = Join-Path ([System.IO.Path]::GetTempPath()) `
        ('civm-junction-fixture-' + [guid]::NewGuid().ToString('N'))
    $external = Join-Path ([System.IO.Path]::GetTempPath()) `
        ('civm-junction-external-' + [guid]::NewGuid().ToString('N'))
    $link = Join-Path $fixture 'bin'
    $rollbackFixture = "$fixture.rollback"
    $rollbackLink = Join-Path $rollbackFixture 'externals'
    $rollbackTarget = Join-Path $fixture "externals.$expectedVersion"
    $rollbackPhysicalTarget = Join-Path $rollbackFixture `
        "externals.$expectedVersion"
    try {
        New-Item -ItemType Directory -Path $fixture, $external | Out-Null

        $target = Join-Path $fixture "bin.$expectedVersion"
        New-Item -ItemType Directory -Path $target | Out-Null
        New-Item -ItemType File -Path (Join-Path $target 'marker') | Out-Null
        New-Item -ItemType Junction -Path $link -Target $target | Out-Null
        $items = @(Get-SafeTreeItems -Path $fixture `
            -AllowedRunnerRoots @($fixture))
        $unique = @($items.FullName | Sort-Object -Unique)
        if ($items.Count -ne 4 -or $unique.Count -ne 4) {
            throw "safe junction was traversed or duplicated in $source"
        }
        Remove-Junction -Path $link
        Remove-Item -LiteralPath $target -Recurse -Force

        New-Item -ItemType Directory -Path $rollbackFixture | Out-Null
        New-Item -ItemType Directory -Path $rollbackTarget | Out-Null
        New-Item -ItemType Junction -Path $rollbackLink `
            -Target $rollbackTarget | Out-Null
        Remove-Item -LiteralPath $rollbackTarget -Recurse -Force
        New-Item -ItemType Directory -Path $rollbackPhysicalTarget | Out-Null
        $rollbackItems = @(Get-SafeTreeItems -Path $rollbackFixture `
            -AllowedRunnerRoots @($fixture, $rollbackFixture))
        if ($rollbackItems.Count -ne 3) {
            throw "relocated rollback junction item mismatch: $($rollbackItems.Count)"
        }
        Remove-Junction -Path $rollbackLink
        New-Item -ItemType Junction -Path $rollbackLink -Target $external | Out-Null
        Assert-Rejected {
            [void](Get-SafeTreeItems -Path $rollbackFixture `
                -AllowedRunnerRoots @($fixture, $rollbackFixture))
        } '*fora do alvo pinado*'
        Remove-Junction -Path $rollbackLink
        Remove-Item -LiteralPath $rollbackFixture -Recurse -Force

        New-Item -ItemType Junction -Path $link -Target $external | Out-Null
        Assert-Rejected {
            [void](Get-SafeTreeItems -Path $fixture -AllowedRunnerRoots @($fixture))
        } '*fora do alvo pinado*'
        Remove-Junction -Path $link

        $nested = Join-Path $fixture 'nested'
        $nestedTarget = Join-Path $nested "bin.$expectedVersion"
        New-Item -ItemType Directory -Path $nestedTarget -Force | Out-Null
        $nestedLink = Join-Path $nested 'bin'
        New-Item -ItemType Junction -Path $nestedLink -Target $nestedTarget | Out-Null
        Assert-Rejected {
            [void](Get-SafeTreeItems -Path $fixture -AllowedRunnerRoots @($fixture))
        } '*fora da raiz top-level*'
        Remove-Junction -Path $nestedLink
        Remove-Item -LiteralPath $nested -Recurse -Force

        $externalFile = Join-Path $external 'outside.txt'
        New-Item -ItemType File -Path $externalFile | Out-Null
        $hardLink = Join-Path $fixture 'outside-hardlink.txt'
        New-Item -ItemType HardLink -Path $hardLink -Target $externalFile | Out-Null
        Assert-Rejected {
            [void](Get-SafeTreeItems -Path $fixture -AllowedRunnerRoots @($fixture))
        } '*link de filesystem proibido*'
        Remove-Item -LiteralPath $hardLink -Force

        $wrongNameTarget = Join-Path $fixture "tools.$expectedVersion"
        New-Item -ItemType Directory -Path $wrongNameTarget | Out-Null
        $wrongName = Join-Path $fixture 'tools'
        New-Item -ItemType Junction -Path $wrongName -Target $wrongNameTarget | Out-Null
        Assert-Rejected {
            [void](Get-SafeTreeItems -Path $fixture -AllowedRunnerRoots @($fixture))
        } '*reparse point proibido*'
        Remove-Junction -Path $wrongName
        Remove-Item -LiteralPath $wrongNameTarget -Recurse -Force

        $wrongVersionTarget = Join-Path $fixture 'bin.2.335.0'
        New-Item -ItemType Directory -Path $wrongVersionTarget | Out-Null
        New-Item -ItemType Junction -Path $link -Target $wrongVersionTarget | Out-Null
        Assert-Rejected {
            [void](Get-SafeTreeItems -Path $fixture -AllowedRunnerRoots @($fixture))
        } '*fora do alvo pinado*'
        Remove-Junction -Path $link
        Remove-Item -LiteralPath $wrongVersionTarget -Recurse -Force

        $chainedTarget = Join-Path $fixture "bin.$expectedVersion"
        New-Item -ItemType Junction -Path $chainedTarget -Target $external | Out-Null
        New-Item -ItemType Junction -Path $link -Target $chainedTarget | Out-Null
        Assert-Rejected {
            [void](Get-SafeTreeItems -Path $fixture -AllowedRunnerRoots @($fixture))
        } '*reparse point proibido*'
        Remove-Junction -Path $link
        Remove-Junction -Path $chainedTarget

        $cleanupRoot = Join-Path $fixture 'cleanup-contract'
        $cleanupLink = Join-Path $cleanupRoot 'external'
        $cleanupSentinel = Join-Path $external 'sentinel.txt'
        New-Item -ItemType Directory -Path $cleanupRoot | Out-Null
        New-Item -ItemType File -Path $cleanupSentinel | Out-Null
        New-Item -ItemType Junction -Path $cleanupLink -Target $external | Out-Null
        Remove-Item -LiteralPath $cleanupRoot -Recurse -Force
        if (-not (Test-Path -LiteralPath $cleanupSentinel -PathType Leaf)) {
            throw 'recursive cleanup traversed an external junction target'
        }
    } finally {
        foreach ($path in @(
                $link,
                (Join-Path $fixture 'outside-hardlink.txt'),
                (Join-Path $fixture 'cleanup-contract\external'),
                $rollbackLink,
                (Join-Path $fixture 'tools'),
                (Join-Path $fixture 'nested\bin'),
                (Join-Path $fixture "bin.$expectedVersion"))) {
            if (Test-Path -LiteralPath $path) {
                $item = Get-Item -LiteralPath $path -Force
                if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
                    Remove-Junction -Path $path
                }
            }
        }
        if (Test-Path -LiteralPath $fixture) {
            Remove-Item -LiteralPath $fixture -Recurse -Force
        }
        if (Test-Path -LiteralPath $rollbackFixture) {
            Remove-Item -LiteralPath $rollbackFixture -Recurse -Force
        }
        if (Test-Path -LiteralPath $external) {
            Remove-Item -LiteralPath $external -Recurse -Force
        }
    }
    Write-Host "PASS: $(Split-Path $source -Leaf) official junction contract"
}

$provisionSource = $sources[0]
$tokens = $null
$parseErrors = $null
$ast = [System.Management.Automation.Language.Parser]::ParseFile(
    $provisionSource, [ref]$tokens, [ref]$parseErrors)
foreach ($name in @(
        'Add-FileSystemRule',
        'Resolve-AccountSid',
        'Initialize-CivmGateNative',
        'Get-FileLinkCount',
        'Set-DaclWithoutPropagation',
        'Get-SecurityDescriptorWithoutFollowing',
        'Set-ProtectedAcl',
        'Assert-ProtectedAcl',
        'Grant-AdminTraversal',
        'Assert-SafeTreeRoot',
        'Ensure-ProtectedSharedRoot',
        'Assert-SafeOfficialRunnerJunction',
        'Get-SafeTreeItems',
        'Protect-AdminTree',
        'Move-StagedRunner')) {
    $functionAst = $ast.Find({
        param($node)
        $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
            $node.Name -eq $name
    }, $true)
    if ($null -eq $functionAst) { throw "missing function $name in $provisionSource" }
    Invoke-Expression $functionAst.Extent.Text
}

$sourceText = Get-Content -LiteralPath $provisionSource -Raw
$resumeBranches = @($ast.FindAll({
    param($node)
    $node -is [System.Management.Automation.Language.IfStatementAst] -and
        $node.Extent.Text -match 'if \(-not \$ResumeStaged\)' -and
        $node.Extent.Text -match 'registration-token'
}, $true))
if ($resumeBranches.Count -ne 1) {
    throw "expected one isolated fresh-provision branch; got $($resumeBranches.Count)"
}
$freshBranch = $resumeBranches[0]
$outsideFreshBranch = $sourceText.Remove(
    $freshBranch.Extent.StartOffset,
    $freshBranch.Extent.EndOffset - $freshBranch.Extent.StartOffset)
if ($outsideFreshBranch -match 'registration-token' -or
    $outsideFreshBranch -match 'config\.cmd --unattended') {
    throw 'resume path can reach registration POST or config replacement'
}
Write-Host 'PASS: resume path excludes registration POST and replacement'

$setupText = Get-Content -LiteralPath $sources[1] -Raw
if ($setupText -match 'Set-ExactProtectedAcl -Path \$Root') {
    throw 'setup mutates the shared root from one gate'
}
if ($setupText -notmatch 'Assert-ExactProtectedAcl -Path \$Root') {
    throw 'setup does not fail closed on shared-root ACL drift'
}
Write-Host 'PASS: setup validates shared root without mutating it'

$windowsPrincipal = [System.Security.Principal.WindowsPrincipal]::new(
    [System.Security.Principal.WindowsIdentity]::GetCurrent())
if (-not $windowsPrincipal.IsInRole(
        [System.Security.Principal.WindowsBuiltInRole]::Administrator)) {
    throw 'ACL handoff fixture requires elevation before creating its fixture'
}
$RunnerVersion = $expectedVersion
$currentSid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
$systemSid = [System.Security.Principal.SecurityIdentifier]'S-1-5-18'
$administratorsSid = [System.Security.Principal.SecurityIdentifier]'S-1-5-32-544'
$networkServiceSid = [System.Security.Principal.SecurityIdentifier]'S-1-5-20'

$sharedRoot = Join-Path ([System.IO.Path]::GetTempPath()) `
    ('civm-shared-root-' + [guid]::NewGuid().ToString('N'))
$sharedExternal = Join-Path ([System.IO.Path]::GetTempPath()) `
    ('civm-shared-external-' + [guid]::NewGuid().ToString('N') + '.txt')
$sharedAlias = Join-Path $sharedRoot 'outside-hardlink.txt'
$sharedFixtureCreated = $false
try {
    New-Item -ItemType Directory -Path $sharedRoot | Out-Null
    New-Item -ItemType File -Path $sharedExternal | Out-Null
    New-Item -ItemType HardLink -Path $sharedAlias `
        -Target $sharedExternal | Out-Null
    $sharedFixtureCreated = $true
    $legacy = New-Object System.Security.AccessControl.DirectorySecurity
    $legacy.SetOwner($currentSid)
    $legacy.SetAccessRuleProtection($false, $true)
    $legacyRule = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $currentSid,
        [System.Security.AccessControl.FileSystemRights]::FullControl,
        [System.Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit',
        [System.Security.AccessControl.PropagationFlags]::None,
        [System.Security.AccessControl.AccessControlType]::Allow)
    $legacy.AddAccessRule($legacyRule) | Out-Null
    Set-Acl -LiteralPath $sharedRoot -AclObject $legacy
    $sharedSddlBefore = (Get-Acl -LiteralPath $sharedExternal).Sddl
    $Root = $sharedRoot
    Assert-Rejected { Ensure-ProtectedSharedRoot } `
        '*DACL de provisionamento divergente*'
    $sharedSddlAfter = (Get-Acl -LiteralPath $sharedExternal).Sddl
    if ($sharedSddlAfter -ne $sharedSddlBefore) {
        throw 'shared-root drift validation changed an external hard-link DACL'
    }
    Write-Host 'PASS: shared-root drift rejects without hard-link propagation'
    Set-ProtectedAcl -Path $sharedRoot -Directory $true -NetworkRead $true `
        -InheritToChildren $false
    $canonicalSddlBefore = (Get-Acl -LiteralPath $sharedExternal).Sddl
    Ensure-ProtectedSharedRoot
    $canonicalSddlAfter = (Get-Acl -LiteralPath $sharedExternal).Sddl
    if ($canonicalSddlAfter -ne $canonicalSddlBefore) {
        throw 'canonical shared-root validation changed an external hard-link DACL'
    }
    Write-Host 'PASS: canonical shared root validates without mutation'
} finally {
    if ($sharedFixtureCreated) {
        if (Test-Path -LiteralPath $sharedExternal) {
            $fileAcl = New-Object System.Security.AccessControl.FileSecurity
            $fileAcl.SetOwner($currentSid)
            $fileAcl.SetAccessRuleProtection($true, $false)
            $fileRule = [System.Security.AccessControl.FileSystemAccessRule]::new(
                $currentSid,
                [System.Security.AccessControl.FileSystemRights]::FullControl,
                [System.Security.AccessControl.AccessControlType]::Allow)
            $fileAcl.AddAccessRule($fileRule) | Out-Null
            Set-Acl -LiteralPath $sharedExternal -AclObject $fileAcl
        }
        if (Test-Path -LiteralPath $sharedAlias) {
            Remove-Item -LiteralPath $sharedAlias -Force
        }
        if (Test-Path -LiteralPath $sharedRoot) {
            Remove-Item -LiteralPath $sharedRoot -Recurse -Force
        }
        if (Test-Path -LiteralPath $sharedExternal) {
            Remove-Item -LiteralPath $sharedExternal -Force
        }
    }
}

$inheritableRoot = Join-Path ([System.IO.Path]::GetTempPath()) `
    ('civm-acl-inheritable-root-' + [guid]::NewGuid().ToString('N'))
$inheritableExternal = Join-Path ([System.IO.Path]::GetTempPath()) `
    ('civm-acl-inheritable-external-' + [guid]::NewGuid().ToString('N') + '.txt')
$inheritableAlias = Join-Path $inheritableRoot 'outside-hardlink.txt'
$inheritableFixtureCreated = $false
try {
    New-Item -ItemType Directory -Path $inheritableRoot | Out-Null
    New-Item -ItemType File -Path $inheritableExternal | Out-Null
    New-Item -ItemType HardLink -Path $inheritableAlias `
        -Target $inheritableExternal | Out-Null
    $inheritableFixtureCreated = $true
    $inheritableAcl = New-Object System.Security.AccessControl.DirectorySecurity
    $inheritableAcl.SetOwner($currentSid)
    $inheritableAcl.SetAccessRuleProtection($true, $false)
    $inheritableRule = `
        [System.Security.AccessControl.FileSystemAccessRule]::new(
            $systemSid,
            [System.Security.AccessControl.FileSystemRights]::FullControl,
            [System.Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit',
            [System.Security.AccessControl.PropagationFlags]::None,
            [System.Security.AccessControl.AccessControlType]::Allow)
    $inheritableAcl.AddAccessRule($inheritableRule) | Out-Null
    Set-Acl -LiteralPath $inheritableRoot -AclObject $inheritableAcl
    $driftAcl = New-Object System.Security.AccessControl.FileSecurity
    $driftAcl.SetOwner($currentSid)
    $driftAcl.SetAccessRuleProtection($false, $false)
    $driftRule = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $currentSid,
        [System.Security.AccessControl.FileSystemRights]::FullControl,
        [System.Security.AccessControl.AccessControlType]::Allow)
    $driftAcl.AddAccessRule($driftRule) | Out-Null
    Set-Acl -LiteralPath $inheritableExternal -AclObject $driftAcl
    $inheritableSddlBefore = (Get-Acl -LiteralPath $inheritableExternal).Sddl
    Assert-Rejected {
        [void](Get-SafeTreeItems -Path $inheritableRoot `
            -AllowedRunnerRoots @($inheritableRoot) -EnsureAdminTraversal)
    } '*link de filesystem proibido*'
    $inheritableSddlAfter = (Get-Acl -LiteralPath $inheritableExternal).Sddl
    if ($inheritableSddlAfter -ne $inheritableSddlBefore) {
        throw 'admin traversal propagated an inheritable ACE through hardlink'
    }
    Write-Host 'PASS: raw admin insertion does not propagate through hardlink'
} finally {
    if ($inheritableFixtureCreated) {
        if (Test-Path -LiteralPath $inheritableRoot) {
            $rootRecovery = New-Object `
                System.Security.AccessControl.DirectorySecurity
            $rootRecovery.SetOwner($currentSid)
            $rootRecovery.SetAccessRuleProtection($true, $false)
            $rootRecoveryRule = `
                [System.Security.AccessControl.FileSystemAccessRule]::new(
                    $currentSid,
                    [System.Security.AccessControl.FileSystemRights]::FullControl,
                    [System.Security.AccessControl.AccessControlType]::Allow)
            $rootRecovery.AddAccessRule($rootRecoveryRule) | Out-Null
            [CivmGateNative.SecurityDescriptorWriter]::SetDacl(
                $inheritableRoot,
                $rootRecovery.GetSecurityDescriptorBinaryForm())
        }
        if (Test-Path -LiteralPath $inheritableExternal) {
            $fileAcl = New-Object System.Security.AccessControl.FileSecurity
            $fileAcl.SetOwner($currentSid)
            $fileAcl.SetAccessRuleProtection($true, $false)
            $fileRule = [System.Security.AccessControl.FileSystemAccessRule]::new(
                $currentSid,
                [System.Security.AccessControl.FileSystemRights]::FullControl,
                [System.Security.AccessControl.AccessControlType]::Allow)
            $fileAcl.AddAccessRule($fileRule) | Out-Null
            Set-Acl -LiteralPath $inheritableExternal -AclObject $fileAcl
        }
        if (Test-Path -LiteralPath $inheritableAlias) {
            Remove-Item -LiteralPath $inheritableAlias -Force
        }
        if (Test-Path -LiteralPath $inheritableRoot) {
            Remove-Item -LiteralPath $inheritableRoot -Recurse -Force
        }
        if (Test-Path -LiteralPath $inheritableExternal) {
            Remove-Item -LiteralPath $inheritableExternal -Force
        }
    }
}

foreach ($source in $sources) {
    $sourceTokens = $null
    $sourceErrors = $null
    $sourceAst = [System.Management.Automation.Language.Parser]::ParseFile(
        $source, [ref]$sourceTokens, [ref]$sourceErrors)
    foreach ($functionName in @(
            'Initialize-CivmGateNative',
            'Get-FileLinkCount',
            'Assert-SafeOfficialRunnerJunction',
            'Get-SafeTreeItems')) {
        $sourceFunction = $sourceAst.Find({
            param($node)
            $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
                $node.Name -eq $functionName
        }, $true)
        if ($null -eq $sourceFunction) {
            throw "missing function $functionName in $source"
        }
        Invoke-Expression $sourceFunction.Extent.Text
    }
    $hardLinkRoot = Join-Path ([System.IO.Path]::GetTempPath()) `
        ('civm-acl-hardlink-root-' + [guid]::NewGuid().ToString('N'))
    $hardLinkExternal = Join-Path ([System.IO.Path]::GetTempPath()) `
        ('civm-acl-hardlink-external-' + [guid]::NewGuid().ToString('N') + '.txt')
    $hardLinkAlias = Join-Path $hardLinkRoot 'outside-hardlink.txt'
    $hardLinkFixtureCreated = $false
    try {
        New-Item -ItemType Directory -Path $hardLinkRoot | Out-Null
        New-Item -ItemType File -Path $hardLinkExternal | Out-Null
        New-Item -ItemType HardLink -Path $hardLinkAlias `
            -Target $hardLinkExternal | Out-Null
        $hardLinkFixtureCreated = $true
        $restricted = New-Object System.Security.AccessControl.FileSecurity
        $restricted.SetOwner($currentSid)
        $restricted.SetAccessRuleProtection($true, $false)
        $restrictedRule = [System.Security.AccessControl.FileSystemAccessRule]::new(
            $networkServiceSid,
            [System.Security.AccessControl.FileSystemRights]::FullControl,
            [System.Security.AccessControl.AccessControlType]::Allow)
        $restricted.AddAccessRule($restrictedRule) | Out-Null
        Set-Acl -LiteralPath $hardLinkExternal -AclObject $restricted
        $externalSddlBefore = (Get-Acl -LiteralPath $hardLinkExternal).Sddl
        if ((Get-FileLinkCount -Path $hardLinkAlias) -ne 2) {
            throw 'native hard-link count did not return two'
        }
        Assert-Rejected {
            [void](Get-SafeTreeItems -Path $hardLinkRoot `
                -AllowedRunnerRoots @($hardLinkRoot))
        } '*link de filesystem proibido*'
        Assert-Rejected {
            [void](Get-FileLinkCount -Path (Join-Path $hardLinkRoot 'missing'))
        } '*falha validando hardlinks*'
        $externalSddlAfter = (Get-Acl -LiteralPath $hardLinkExternal).Sddl
        if ($externalSddlAfter -ne $externalSddlBefore) {
            throw 'native hard-link validation changed the external DACL'
        }
        Write-Host "PASS: $(Split-Path $source -Leaf) rejects restricted hardlink"
    } finally {
        if ($hardLinkFixtureCreated) {
            if (Test-Path -LiteralPath $hardLinkExternal) {
                $fileAcl = New-Object System.Security.AccessControl.FileSecurity
                $fileAcl.SetOwner($currentSid)
                $fileAcl.SetAccessRuleProtection($true, $false)
                $fileRule = [System.Security.AccessControl.FileSystemAccessRule]::new(
                    $currentSid,
                    [System.Security.AccessControl.FileSystemRights]::FullControl,
                    [System.Security.AccessControl.AccessControlType]::Allow)
                $fileAcl.AddAccessRule($fileRule) | Out-Null
                Set-Acl -LiteralPath $hardLinkExternal -AclObject $fileAcl
            }
            if (Test-Path -LiteralPath $hardLinkAlias) {
                Remove-Item -LiteralPath $hardLinkAlias -Force
            }
            if (Test-Path -LiteralPath $hardLinkRoot) {
                Remove-Item -LiteralPath $hardLinkRoot -Recurse -Force
            }
            if (Test-Path -LiteralPath $hardLinkExternal) {
                Remove-Item -LiteralPath $hardLinkExternal -Force
            }
        }
    }
}

foreach ($functionName in @(
        'Get-FileLinkCount',
        'Set-DaclWithoutPropagation',
        'Get-SecurityDescriptorWithoutFollowing',
        'Grant-AdminTraversal',
        'Assert-SafeOfficialRunnerJunction',
        'Get-SafeTreeItems')) {
    $provisionFunction = $ast.Find({
        param($node)
        $node -is [System.Management.Automation.Language.FunctionDefinitionAst] -and
            $node.Name -eq $functionName
    }, $true)
    Invoke-Expression $provisionFunction.Extent.Text
}

$reparseAclRoot = Join-Path ([System.IO.Path]::GetTempPath()) `
    ('civm-reparse-acl-handoff-' + [guid]::NewGuid().ToString('N'))
$reparseAclLink = Join-Path $reparseAclRoot 'externals'
$reparseAclTarget = Join-Path $reparseAclRoot "externals.$expectedVersion"
$reparseAclCreated = $false
try {
    New-Item -ItemType Directory -Path $reparseAclTarget -Force | Out-Null
    New-Item -ItemType File -Path (
        Join-Path $reparseAclTarget 'marker') | Out-Null
    New-Item -ItemType Junction -Path $reparseAclLink `
        -Target $reparseAclTarget | Out-Null
    $reparseAclCreated = $true
    $targetSddlBefore = (Get-Acl -LiteralPath $reparseAclTarget).Sddl

    $restrictedLink = New-Object `
        System.Security.AccessControl.DirectorySecurity
    $restrictedLink.SetOwner($systemSid)
    $restrictedLink.SetAccessRuleProtection($true, $false)
    $restrictedLinkRule = `
        [System.Security.AccessControl.FileSystemAccessRule]::new(
            $networkServiceSid,
            [System.Security.AccessControl.FileSystemRights]::FullControl,
            [System.Security.AccessControl.AccessControlType]::Allow)
    $restrictedLink.AddAccessRule($restrictedLinkRule) | Out-Null
    Set-DaclWithoutPropagation -Path $reparseAclLink `
        -SecurityDescriptor $restrictedLink.GetSecurityDescriptorBinaryForm()

    $RunnerVersion = $expectedVersion
    $items = @(Get-SafeTreeItems -Path $reparseAclRoot `
        -AllowedRunnerRoots @($reparseAclRoot) -EnsureAdminTraversal)
    if ($items.Count -ne 4) {
        throw "restricted junction handoff item mismatch: $($items.Count)"
    }
    $targetSddlAfter = (Get-Acl -LiteralPath $reparseAclTarget).Sddl
    if ($targetSddlAfter -ne $targetSddlBefore) {
        throw 'restricted junction handoff changed target DACL'
    }
    Write-Host 'PASS: restricted official junction regains admin traversal'
} finally {
    if ($reparseAclCreated) {
        $recoveryLink = New-Object `
            System.Security.AccessControl.DirectorySecurity
        $recoveryLink.SetOwner($currentSid)
        $recoveryLink.SetAccessRuleProtection($true, $false)
        $recoveryLinkRule = `
            [System.Security.AccessControl.FileSystemAccessRule]::new(
                $currentSid,
                [System.Security.AccessControl.FileSystemRights]::FullControl,
                [System.Security.AccessControl.AccessControlType]::Allow)
        $recoveryLink.AddAccessRule($recoveryLinkRule) | Out-Null
        Set-DaclWithoutPropagation -Path $reparseAclLink `
            -SecurityDescriptor $recoveryLink.GetSecurityDescriptorBinaryForm()
        Remove-Junction -Path $reparseAclLink
        Remove-Item -LiteralPath $reparseAclRoot -Recurse -Force
    }
}

$swapRoot = Join-Path ([System.IO.Path]::GetTempPath()) `
    ('civm-resume-swap-' + [guid]::NewGuid().ToString('N'))
$dir = Join-Path $swapRoot 'runner-1'
$stage = "$dir.new"
$rollback = "$dir.rollback"
$allowedRunnerRoots = @($dir, $stage, $rollback)
try {
    New-Item -ItemType Directory -Path $dir -Force | Out-Null
    New-Item -ItemType File -Path (Join-Path $dir 'old-marker') | Out-Null
    $swapRejected = $false
    try {
        Move-StagedRunner
    } catch {
        $swapRejected = $true
    }
    if (-not $swapRejected) {
        throw 'missing staging did not reject the swap'
    }
    if (-not (Test-Path -LiteralPath (Join-Path $dir 'old-marker') -PathType Leaf) -or
        (Test-Path -LiteralPath $rollback)) {
        throw 'failed second move did not restore the old directory'
    }
    New-Item -ItemType Directory -Path $stage | Out-Null
    New-Item -ItemType File -Path (Join-Path $stage 'new-marker') | Out-Null
    Move-StagedRunner
    if (-not (Test-Path -LiteralPath (Join-Path $dir 'new-marker') -PathType Leaf) -or
        -not (Test-Path -LiteralPath (Join-Path $rollback 'old-marker') -PathType Leaf)) {
        throw 'successful staged swap lost new or rollback content'
    }
    Write-Host 'PASS: staged swap compensates failure and preserves rollback'
} finally {
    if (Test-Path -LiteralPath $swapRoot) {
        Remove-Item -LiteralPath $swapRoot -Recurse -Force
    }
}

$aclFixture = Join-Path ([System.IO.Path]::GetTempPath()) `
    ('civm-acl-handoff-' + [guid]::NewGuid().ToString('N'))
$aclFixtureCreated = $false
try {
    $child = Join-Path $aclFixture 'child'
    New-Item -ItemType Directory -Path $child -Force | Out-Null
    $aclFixtureCreated = $true
    New-Item -ItemType File -Path (Join-Path $child 'marker') | Out-Null
    $restricted = New-Object System.Security.AccessControl.DirectorySecurity
    # Reproduce legacy runner descendants that are not owned by the operator.
    # An operator-owned fixture grants implicit WRITE_DAC and hides the live
    # handoff failure this contract is meant to catch.
    $restricted.SetOwner($systemSid)
    $restricted.SetAccessRuleProtection($true, $false)
    $restrictedRule = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $networkServiceSid,
        [System.Security.AccessControl.FileSystemRights]::FullControl,
        [System.Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit',
        [System.Security.AccessControl.PropagationFlags]::None,
        [System.Security.AccessControl.AccessControlType]::Allow)
    $restricted.AddAccessRule($restrictedRule) | Out-Null
    Set-Acl -LiteralPath $aclFixture -AclObject $restricted
    try {
        [void](Get-ChildItem -LiteralPath $aclFixture -Force -ErrorAction Stop)
        throw 'restricted legacy ACL remained traversable'
    } catch [System.UnauthorizedAccessException] {
        # Expected: owner can repair the DACL but cannot enumerate the tree.
    }
    $items = @(Get-SafeTreeItems -Path $aclFixture `
        -AllowedRunnerRoots @($aclFixture) -EnsureAdminTraversal)
    if ($items.Count -ne 3) { throw "admin handoff item mismatch: $($items.Count)" }
    Write-Host 'PASS: audited legacy ACL regains protected admin traversal'
} finally {
    if ($aclFixtureCreated) {
        foreach ($directory in @($aclFixture, (Join-Path $aclFixture 'child'))) {
            $recovery = New-Object System.Security.AccessControl.DirectorySecurity
            $recovery.SetOwner($currentSid)
            $recovery.SetAccessRuleProtection($true, $false)
            $currentRule = [System.Security.AccessControl.FileSystemAccessRule]::new(
                $currentSid,
                [System.Security.AccessControl.FileSystemRights]::FullControl,
                [System.Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit',
                [System.Security.AccessControl.PropagationFlags]::None,
                [System.Security.AccessControl.AccessControlType]::Allow)
            $recovery.AddAccessRule($currentRule) | Out-Null
            Set-Acl -LiteralPath $directory -AclObject $recovery
        }
        Remove-Item -LiteralPath $aclFixture -Recurse -Force
    }
}
