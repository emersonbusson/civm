# civm-gate-task-setup.ps1 — activate a least-privilege Windows gate runner.
param(
    [Parameter(Mandatory)][int]$Index,
    [Parameter(Mandatory)][System.Security.SecureString]$GitHubToken,
    [string]$Root = 'C:\civm-gate',
    [string]$ContextPath = 'C:\ProgramData\civm\gate\current-context'
)
$ErrorActionPreference = 'Stop'
if ($Index -lt 1 -or $Index -gt 99) { throw 'Index fora de 1..99' }
$Root = [System.IO.Path]::GetFullPath($Root).TrimEnd('\')
$ContextPath = [System.IO.Path]::GetFullPath($ContextPath)
if (-not $Root.Equals('C:\civm-gate', [System.StringComparison]::OrdinalIgnoreCase) -or
    -not $ContextPath.Equals(
        'C:\ProgramData\civm\gate\current-context',
        [System.StringComparison]::OrdinalIgnoreCase)) {
    throw 'Root ou ContextPath fora do escopo canônico'
}
$dir = Join-Path $Root "runner-$Index"
$rollback = "$dir.rollback"
$task = "civm-gate-runner-$Index"
$publisherTask = 'civm-host-orchestrator'
$networkServiceSid = [System.Security.Principal.SecurityIdentifier]'S-1-5-20'
$systemSid = [System.Security.Principal.SecurityIdentifier]'S-1-5-18'
$administratorsSid = [System.Security.Principal.SecurityIdentifier]'S-1-5-32-544'
$listenerStartTimeoutSeconds = 120
$remoteOnlineTimeoutSeconds = 120
$runnerConfigPath = Join-Path $dir '.runner'
$runCmdPath = Join-Path $dir 'run.cmd'
$listenerPath = Join-Path $dir 'bin\Runner.Listener.exe'
$workDir = Join-Path $dir '_work'
$diagDir = Join-Path $dir '_diag'
$contextDir = Split-Path -Parent $ContextPath
$expectedRunnerVersion = '2.336.0'
$allowedRunnerRoots = @($dir, $rollback)

function Add-FileSystemRule {
    param(
        [Parameter(Mandatory)]$Acl,
        [Parameter(Mandatory)][System.Security.Principal.SecurityIdentifier]$Sid,
        [Parameter(Mandatory)][System.Security.AccessControl.FileSystemRights]$Rights,
        [System.Security.AccessControl.InheritanceFlags]$Inheritance =
            [System.Security.AccessControl.InheritanceFlags]::None
    )
    $rule = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $Sid, $Rights, $Inheritance,
        [System.Security.AccessControl.PropagationFlags]::None,
        [System.Security.AccessControl.AccessControlType]::Allow)
    $Acl.AddAccessRule($rule) | Out-Null
}

function Resolve-AccountSid {
    param([Parameter(Mandatory)][string]$Account)
    try {
        return ([System.Security.Principal.SecurityIdentifier]$Account).Value
    } catch {
        return (([System.Security.Principal.NTAccount]$Account).Translate(
            [System.Security.Principal.SecurityIdentifier])).Value
    }
}

function Initialize-CivmGateNative {
    if ($null -eq ('CivmGateNative.LinkInspector' -as [type])) {
        Add-Type -TypeDefinition @'
using System;
using System.ComponentModel;
using System.Diagnostics;
using System.Runtime.InteropServices;
using Microsoft.Win32.SafeHandles;

namespace CivmGateNative {
    [StructLayout(LayoutKind.Sequential)]
    internal struct Luid { public uint LowPart; public int HighPart; }

    [StructLayout(LayoutKind.Sequential)]
    internal struct TokenPrivileges {
        public uint PrivilegeCount;
        public Luid Luid;
        public uint Attributes;
    }

    [StructLayout(LayoutKind.Sequential)]
    internal struct ByHandleFileInformation {
        public uint FileAttributes;
        public System.Runtime.InteropServices.ComTypes.FILETIME CreationTime;
        public System.Runtime.InteropServices.ComTypes.FILETIME LastAccessTime;
        public System.Runtime.InteropServices.ComTypes.FILETIME LastWriteTime;
        public uint VolumeSerialNumber;
        public uint FileSizeHigh;
        public uint FileSizeLow;
        public uint NumberOfLinks;
        public uint FileIndexHigh;
        public uint FileIndexLow;
    }

    public static class LinkInspector {
        const uint TokenAdjustPrivileges = 0x20;
        const uint TokenQuery = 0x8;
        const uint PrivilegeEnabled = 0x2;
        const uint MetadataOnly = 0;
        const uint ShareAll = 0x7;
        const uint OpenExisting = 3;
        const uint OpenReparsePoint = 0x00200000;
        const uint BackupSemantics = 0x02000000;
        const int AccessDenied = 5;

        [DllImport("advapi32.dll", SetLastError = true)]
        static extern bool OpenProcessToken(IntPtr process, uint access,
            out SafeFileHandle token);

        [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        static extern bool LookupPrivilegeValue(string system, string name,
            out Luid luid);

        [DllImport("advapi32.dll", SetLastError = true)]
        static extern bool AdjustTokenPrivileges(SafeFileHandle token,
            bool disableAll, ref TokenPrivileges desired, uint bufferLength,
            out TokenPrivileges previous, out uint returnedLength);

        [DllImport("advapi32.dll", EntryPoint = "AdjustTokenPrivileges",
            SetLastError = true)]
        static extern bool RestoreTokenPrivileges(SafeFileHandle token,
            bool disableAll, ref TokenPrivileges desired, uint bufferLength,
            IntPtr previous, IntPtr returnedLength);

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        static extern SafeFileHandle CreateFile(string name, uint access,
            uint share, IntPtr security, uint creation, uint flags,
            IntPtr template);

        [DllImport("kernel32.dll", SetLastError = true)]
        static extern bool GetFileInformationByHandle(SafeFileHandle handle,
            out ByHandleFileInformation information);

        static SafeFileHandle Open(string path) {
            return CreateFile(path, MetadataOnly, ShareAll, IntPtr.Zero,
                OpenExisting, OpenReparsePoint | BackupSemantics, IntPtr.Zero);
        }

        public static uint GetLinkCount(string path) {
            SafeFileHandle handle = Open(path);
            SafeFileHandle token = null;
            TokenPrivileges previous = new TokenPrivileges();
            bool restorePrivilege = false;
            try {
                if (handle.IsInvalid) {
                    int openError = Marshal.GetLastWin32Error();
                    handle.Dispose();
                    if (openError != AccessDenied) {
                        throw new Win32Exception(openError);
                    }
                    if (!OpenProcessToken(Process.GetCurrentProcess().Handle,
                            TokenAdjustPrivileges | TokenQuery, out token)) {
                        throw new Win32Exception(Marshal.GetLastWin32Error());
                    }
                    Luid luid;
                    if (!LookupPrivilegeValue(null, "SeBackupPrivilege", out luid)) {
                        throw new Win32Exception(Marshal.GetLastWin32Error());
                    }
                    TokenPrivileges desired = new TokenPrivileges {
                        PrivilegeCount = 1, Luid = luid,
                        Attributes = PrivilegeEnabled
                    };
                    uint returnedLength;
                    if (!AdjustTokenPrivileges(token, false, ref desired,
                            (uint)Marshal.SizeOf(typeof(TokenPrivileges)),
                            out previous, out returnedLength)) {
                        throw new Win32Exception(Marshal.GetLastWin32Error());
                    }
                    restorePrivilege = true;
                    int privilegeError = Marshal.GetLastWin32Error();
                    if (privilegeError != 0) {
                        throw new Win32Exception(privilegeError);
                    }
                    handle = Open(path);
                    if (handle.IsInvalid) {
                        throw new Win32Exception(Marshal.GetLastWin32Error());
                    }
                }
                ByHandleFileInformation information;
                if (!GetFileInformationByHandle(handle, out information)) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
                return information.NumberOfLinks;
            } finally {
                if (handle != null) { handle.Dispose(); }
                int restoreError = 0;
                if (restorePrivilege && token != null && !token.IsInvalid) {
                    if (!RestoreTokenPrivileges(token, false, ref previous, 0,
                            IntPtr.Zero, IntPtr.Zero)) {
                        restoreError = Marshal.GetLastWin32Error();
                    }
                }
                if (token != null) { token.Dispose(); }
                if (restoreError != 0) { throw new Win32Exception(restoreError); }
            }
        }
    }

    public static class SecurityDescriptorWriter {
        const uint OwnerSecurityInformation = 0x00000001;
        const uint DaclSecurityInformation = 0x00000004;
        const uint TokenAdjustPrivileges = 0x20;
        const uint TokenQuery = 0x8;
        const uint PrivilegeEnabled = 0x2;
        const uint WriteDac = 0x00040000;
        const uint ReadControl = 0x00020000;
        const uint ShareAll = 0x7;
        const uint OpenExisting = 3;
        const uint OpenReparsePoint = 0x00200000;
        const uint BackupSemantics = 0x02000000;

        [DllImport("advapi32.dll", SetLastError = true)]
        static extern bool OpenProcessToken(IntPtr process, uint access,
            out SafeFileHandle token);

        [DllImport("advapi32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        static extern bool LookupPrivilegeValue(string system, string name,
            out Luid luid);

        [DllImport("advapi32.dll", SetLastError = true)]
        static extern bool AdjustTokenPrivileges(SafeFileHandle token,
            bool disableAll, ref TokenPrivileges desired, uint bufferLength,
            out TokenPrivileges previous, out uint returnedLength);

        [DllImport("advapi32.dll", EntryPoint = "AdjustTokenPrivileges",
            SetLastError = true)]
        static extern bool RestoreTokenPrivileges(SafeFileHandle token,
            bool disableAll, ref TokenPrivileges desired, uint bufferLength,
            IntPtr previous, IntPtr returnedLength);

        [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
        static extern SafeFileHandle CreateFile(string name, uint access,
            uint share, IntPtr security, uint creation, uint flags,
            IntPtr template);

        [DllImport("advapi32.dll", SetLastError = true)]
        static extern bool SetKernelObjectSecurity(SafeFileHandle handle,
            uint securityInformation, IntPtr securityDescriptor);

        [DllImport("advapi32.dll", SetLastError = true)]
        static extern bool GetKernelObjectSecurity(SafeFileHandle handle,
            uint securityInformation, byte[] securityDescriptor,
            uint length, out uint needed);

        public static byte[] GetOwnerAndDacl(string path) {
            SafeFileHandle token = null;
            SafeFileHandle target = null;
            TokenPrivileges previous = new TokenPrivileges();
            bool restorePrivilege = false;
            try {
                if (!OpenProcessToken(Process.GetCurrentProcess().Handle,
                        TokenAdjustPrivileges | TokenQuery, out token)) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
                Luid luid;
                if (!LookupPrivilegeValue(null, "SeBackupPrivilege", out luid)) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
                TokenPrivileges desired = new TokenPrivileges {
                    PrivilegeCount = 1,
                    Luid = luid,
                    Attributes = PrivilegeEnabled
                };
                uint returnedLength;
                if (!AdjustTokenPrivileges(token, false, ref desired,
                        (uint)Marshal.SizeOf<TokenPrivileges>(), out previous,
                        out returnedLength)) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
                restorePrivilege = true;
                int privilegeError = Marshal.GetLastWin32Error();
                if (privilegeError != 0) {
                    throw new Win32Exception(privilegeError);
                }
                target = CreateFile(path, ReadControl, ShareAll, IntPtr.Zero,
                    OpenExisting, OpenReparsePoint | BackupSemantics,
                    IntPtr.Zero);
                if (target.IsInvalid) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
                uint needed;
                bool first = GetKernelObjectSecurity(target,
                    OwnerSecurityInformation | DaclSecurityInformation,
                    null, 0, out needed);
                int sizeError = Marshal.GetLastWin32Error();
                if (first || sizeError != 122 || needed == 0) {
                    throw new Win32Exception(first ? 87 : sizeError);
                }
                byte[] descriptor = new byte[needed];
                if (!GetKernelObjectSecurity(target,
                        OwnerSecurityInformation | DaclSecurityInformation,
                        descriptor, needed, out needed)) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
                return descriptor;
            } finally {
                if (target != null) { target.Dispose(); }
                int restoreError = 0;
                if (restorePrivilege && token != null && !token.IsInvalid) {
                    if (!RestoreTokenPrivileges(token, false, ref previous, 0,
                            IntPtr.Zero, IntPtr.Zero)) {
                        restoreError = Marshal.GetLastWin32Error();
                    }
                }
                if (token != null) { token.Dispose(); }
                if (restoreError != 0) { throw new Win32Exception(restoreError); }
            }
        }

        public static void SetDacl(string path, byte[] securityDescriptor) {
            if (securityDescriptor == null || securityDescriptor.Length == 0) {
                throw new ArgumentException("security descriptor is empty");
            }
            SafeFileHandle token = null;
            SafeFileHandle target = null;
            TokenPrivileges previous = new TokenPrivileges();
            bool restorePrivilege = false;
            GCHandle pinned = GCHandle.Alloc(
                securityDescriptor, GCHandleType.Pinned);
            try {
                if (!OpenProcessToken(Process.GetCurrentProcess().Handle,
                        TokenAdjustPrivileges | TokenQuery, out token)) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
                Luid luid;
                if (!LookupPrivilegeValue(null, "SeRestorePrivilege", out luid)) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
                TokenPrivileges desired = new TokenPrivileges {
                    PrivilegeCount = 1,
                    Luid = luid,
                    Attributes = PrivilegeEnabled
                };
                uint returnedLength;
                if (!AdjustTokenPrivileges(token, false, ref desired,
                        (uint)Marshal.SizeOf<TokenPrivileges>(), out previous,
                        out returnedLength)) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
                restorePrivilege = true;
                int privilegeError = Marshal.GetLastWin32Error();
                if (privilegeError != 0) {
                    throw new Win32Exception(privilegeError);
                }
                target = CreateFile(path, WriteDac, ShareAll, IntPtr.Zero,
                    OpenExisting, OpenReparsePoint | BackupSemantics,
                    IntPtr.Zero);
                if (target.IsInvalid) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
                if (!SetKernelObjectSecurity(target, DaclSecurityInformation,
                        pinned.AddrOfPinnedObject())) {
                    throw new Win32Exception(Marshal.GetLastWin32Error());
                }
            } finally {
                pinned.Free();
                if (target != null) { target.Dispose(); }
                int restoreError = 0;
                if (restorePrivilege && token != null && !token.IsInvalid) {
                    if (!RestoreTokenPrivileges(token, false, ref previous, 0,
                            IntPtr.Zero, IntPtr.Zero)) {
                        restoreError = Marshal.GetLastWin32Error();
                    }
                }
                if (token != null) { token.Dispose(); }
                if (restoreError != 0) { throw new Win32Exception(restoreError); }
            }
        }
    }
}
'@
    }
}

function Get-FileLinkCount {
    param([Parameter(Mandatory)][string]$Path)
    Initialize-CivmGateNative
    try {
        return [CivmGateNative.LinkInspector]::GetLinkCount($Path)
    } catch {
        throw "falha validando hardlinks em ${Path}: $($_.Exception.GetBaseException().Message)"
    }
}

function Assert-NotReparsePoint {
    param([Parameter(Mandatory)][string]$Path)
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue
    if ($null -eq $item) { return }
    if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        throw "reparse point proibido no escopo do gate: $Path"
    }
}

function Get-SafeTreeItems {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string[]]$AllowedRunnerRoots
    )
    $pending = [System.Collections.Stack]::new()
    $pending.Push((Get-Item -LiteralPath $Path -Force))
    $items = [System.Collections.Generic.List[object]]::new()
    while ($pending.Count -ne 0) {
        $item = $pending.Pop()
        if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            Assert-SafeOfficialRunnerJunction -Item $item `
                -ExpectedRunnerVersion $expectedRunnerVersion `
                -AllowedRunnerRoots $AllowedRunnerRoots
            $items.Add($item)
            continue
        }
        if (-not [string]::IsNullOrEmpty([string]$item.LinkType)) {
            throw "link de filesystem proibido no runner: $($item.FullName)"
        }
        if (-not $item.PSIsContainer -and
            (Get-FileLinkCount -Path $item.FullName) -ne 1) {
            throw "link de filesystem proibido no runner: $($item.FullName)"
        }
        $items.Add($item)
        if ($item.PSIsContainer) {
            foreach ($child in (Get-ChildItem -LiteralPath $item.FullName -Force)) {
                $pending.Push($child)
            }
        }
    }
    return $items.ToArray()
}

function Assert-SafeOfficialRunnerJunction {
    param(
        [Parameter(Mandatory)]$Item,
        [Parameter(Mandatory)][string]$ExpectedRunnerVersion,
        [Parameter(Mandatory)][string[]]$AllowedRunnerRoots
    )
    if ($Item.LinkType -ne 'Junction' -or
        $Item.Name -notin @('bin', 'externals') -or
        $null -eq $Item.Parent) {
        throw "reparse point proibido no runner: $($Item.FullName)"
    }
    $targets = @($Item.Target)
    if ($targets.Count -ne 1 -or [string]::IsNullOrWhiteSpace($targets[0])) {
        throw "junction oficial sem alvo unico: $($Item.FullName)"
    }
    $fullName = [System.IO.Path]::GetFullPath($Item.FullName)
    $matchingRoots = @($AllowedRunnerRoots | Where-Object {
        $candidate = [System.IO.Path]::GetFullPath((Join-Path $_ $Item.Name))
        $fullName.Equals($candidate, [System.StringComparison]::OrdinalIgnoreCase)
    })
    if ($matchingRoots.Count -ne 1) {
        throw "junction oficial fora da raiz top-level do runner: $fullName"
    }
    $runnerRoot = [System.IO.Path]::GetFullPath($matchingRoots[0])
    if (-not $Item.Parent.FullName.Equals(
            $runnerRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "junction oficial fora da raiz top-level do runner: $fullName"
    }
    $logicalRoot = $runnerRoot
    if ($runnerRoot.EndsWith(
            '.rollback', [System.StringComparison]::OrdinalIgnoreCase)) {
        $rollbackBase = $runnerRoot.Substring(
            0, $runnerRoot.Length - '.rollback'.Length)
        $matchingBases = @($AllowedRunnerRoots | Where-Object {
            [System.IO.Path]::GetFullPath($_).Equals(
                $rollbackBase, [System.StringComparison]::OrdinalIgnoreCase)
        })
        if ($matchingBases.Count -ne 1) {
            throw "rollback sem raiz logica autorizada: $runnerRoot"
        }
        $logicalRoot = $rollbackBase
    }
    $expected = [System.IO.Path]::GetFullPath((Join-Path $logicalRoot `
        "$($Item.Name).$ExpectedRunnerVersion"))
    $physicalTarget = [System.IO.Path]::GetFullPath((Join-Path $runnerRoot `
        "$($Item.Name).$ExpectedRunnerVersion"))
    $actual = [System.IO.Path]::GetFullPath([string]$targets[0])
    if (-not $actual.Equals($expected, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "junction oficial fora do alvo pinado: $($Item.FullName) -> $actual"
    }
    $targetItem = Get-Item -LiteralPath $physicalTarget -Force -ErrorAction Stop
    if (-not $targetItem.PSIsContainer -or
        ($targetItem.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0 -or
        -not $targetItem.Parent.FullName.Equals(
            $runnerRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "alvo fisico da junction nao e sibling real: $physicalTarget"
    }
}

function Set-ExactProtectedAcl {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][bool]$Directory,
        [bool]$InheritToChildren = $true,
        [Parameter(Mandatory)][System.Security.AccessControl.FileSystemRights]$NetworkServiceRights
    )
    $acl = if ($Directory) {
        New-Object System.Security.AccessControl.DirectorySecurity
    } else {
        New-Object System.Security.AccessControl.FileSecurity
    }
    $acl.SetOwner($systemSid)
    $acl.SetAccessRuleProtection($true, $false)
    $inheritance = if ($Directory -and $InheritToChildren) {
        [System.Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit'
    } else { [System.Security.AccessControl.InheritanceFlags]::None }
    Add-FileSystemRule -Acl $acl -Sid $systemSid `
        -Rights ([System.Security.AccessControl.FileSystemRights]::FullControl) `
        -Inheritance $inheritance
    Add-FileSystemRule -Acl $acl -Sid $administratorsSid `
        -Rights ([System.Security.AccessControl.FileSystemRights]::FullControl) `
        -Inheritance $inheritance
    Add-FileSystemRule -Acl $acl -Sid $networkServiceSid `
        -Rights $NetworkServiceRights -Inheritance $inheritance
    Set-Acl -LiteralPath $Path -AclObject $acl
    Assert-ExactProtectedAcl -Path $Path `
        -Directory $Directory -InheritToChildren $InheritToChildren `
        -NetworkServiceRights $NetworkServiceRights
}

function Assert-ExactProtectedAcl {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][bool]$Directory,
        [bool]$InheritToChildren = $true,
        [Parameter(Mandatory)][System.Security.AccessControl.FileSystemRights]$NetworkServiceRights
    )
    $acl = Get-Acl -LiteralPath $Path
    if (-not $acl.AreAccessRulesProtected -or
        (Resolve-AccountSid -Account $acl.Owner) -ne $systemSid.Value) {
        throw "ACL sem protecao/owner SYSTEM: $Path"
    }
    $normalizedFull = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $systemSid, [System.Security.AccessControl.FileSystemRights]::FullControl,
        [System.Security.AccessControl.AccessControlType]::Allow).FileSystemRights
    $normalizedNetwork = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $networkServiceSid, $NetworkServiceRights,
        [System.Security.AccessControl.AccessControlType]::Allow).FileSystemRights
    $expected = @{}
    $expected[$systemSid.Value] = [int64]$normalizedFull
    $expected[$administratorsSid.Value] = [int64]$normalizedFull
    $expected[$networkServiceSid.Value] = [int64]$normalizedNetwork
    $expectedInheritance = if ($Directory -and $InheritToChildren) {
        [System.Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit'
    } else { [System.Security.AccessControl.InheritanceFlags]::None }
    $rules = @($acl.GetAccessRules(
        $true, $true, [System.Security.Principal.SecurityIdentifier]))
    if ($rules.Count -ne 3) { throw "ACL nao tem exatamente tres ACEs: $Path" }
    $seen = @{}
    foreach ($rule in $rules) {
        $sid = $rule.IdentityReference.Value
        if ($rule.IsInherited -or
            $rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow -or
            $rule.InheritanceFlags -ne $expectedInheritance -or
            $rule.PropagationFlags -ne [System.Security.AccessControl.PropagationFlags]::None -or
            -not $expected.ContainsKey($sid) -or $seen.ContainsKey($sid) -or
            [int64]$rule.FileSystemRights -ne $expected[$sid]) {
            throw "ACL contem ACE inesperada: $Path principal=$sid"
        }
        $seen[$sid] = $true
    }
}

function Get-RunnerProcesses {
    param([Parameter(Mandatory)][string]$ProcessName)
    return ,@(Get-CimInstance Win32_Process -Filter "Name = '$ProcessName'" |
        Where-Object {
            $_.ExecutablePath -and $_.ExecutablePath.StartsWith(
                $dir + '\', [System.StringComparison]::OrdinalIgnoreCase)
        })
}

function Invoke-GitHubApi {
    param(
        [Parameter(Mandatory)][ValidateSet('GET', 'DELETE')][string]$Method,
        [Parameter(Mandatory)][string]$Path
    )
    $plainToken = [System.Net.NetworkCredential]::new('', $GitHubToken).Password
    try {
        return Invoke-RestMethod -Method $Method -Uri "https://api.github.com/$Path" `
            -Headers @{
                Accept = 'application/vnd.github+json'
                Authorization = "Bearer $plainToken"
                'X-GitHub-Api-Version' = '2022-11-28'
                'User-Agent' = 'civm-gate-setup'
            } -ErrorAction Stop
    } finally {
        $plainToken = $null
    }
}

function Get-RemoteRunner {
    param(
        [Parameter(Mandatory)][string]$Owner,
        [Parameter(Mandatory)][string]$Name
    )
    $allRunners = [System.Collections.Generic.List[object]]::new()
    $page = 1
    do {
        $response = Invoke-GitHubApi -Method GET `
            -Path "orgs/$Owner/actions/runners?per_page=100&page=$page"
        $batch = @($response.runners)
        foreach ($runner in $batch) { $allRunners.Add($runner) }
        $page++
    } while ($batch.Count -eq 100)
    $matches = @($allRunners | Where-Object { $_.name -eq $Name })
    if ($matches.Count -ne 1) {
        throw "runner remoto ausente ou duplicado: $Name"
    }
    return $matches[0]
}

function Quarantine-RemoteRunner {
    param(
        [Parameter(Mandatory)][string]$Owner,
        [Parameter(Mandatory)][string]$Name
    )
    $remote = Get-RemoteRunner -Owner $Owner -Name $Name
    $customLabels = @($remote.labels | Where-Object { $_.type -eq 'custom' })
    if ($customLabels.Count -ne 0) {
        Invoke-GitHubApi -Method DELETE `
            -Path "orgs/$Owner/actions/runners/$($remote.id)/labels" | Out-Null
    }
    $stable = 0
    $deadline = (Get-Date).AddSeconds(15)
    do {
        Start-Sleep -Milliseconds 500
        $remote = Get-RemoteRunner -Owner $Owner -Name $Name
        $customLabels = @($remote.labels | Where-Object { $_.type -eq 'custom' })
        $locallyIdle = (Get-RunnerProcesses -ProcessName 'Runner.Worker.exe').Count -eq 0
        if (-not $remote.busy -and $customLabels.Count -eq 0 -and $locallyIdle) {
            $stable++
        } else { $stable = 0 }
    } while ($stable -lt 6 -and (Get-Date) -lt $deadline)
    if ($stable -lt 6) { throw "runner nao esta em quarentena: $Name" }
}

function Wait-RemoteRunnerOnline {
    param(
        [Parameter(Mandatory)][string]$Owner,
        [Parameter(Mandatory)][string]$Name
    )
    $deadline = (Get-Date).AddSeconds($remoteOnlineTimeoutSeconds)
    do {
        Start-Sleep -Milliseconds 500
        $remote = Get-RemoteRunner -Owner $Owner -Name $Name
        $customLabels = @($remote.labels | Where-Object { $_.type -eq 'custom' })
    } while (($remote.status -ne 'online' -or $remote.busy -or
            $customLabels.Count -ne 0) -and
        (Get-Date) -lt $deadline)
    if ($remote.status -ne 'online' -or $remote.busy -or
        $customLabels.Count -ne 0) {
        throw "runner nao ficou online em quarentena: $Name"
    }
}

function Stop-RunnerProcesses {
    if ((Get-RunnerProcesses -ProcessName 'Runner.Worker.exe').Count -ne 0) {
        throw "gate possui Runner.Worker ativo; processo preservado: $dir"
    }
    foreach ($process in (Get-RunnerProcesses -ProcessName 'Runner.Listener.exe')) {
        if ((Get-RunnerProcesses -ProcessName 'Runner.Worker.exe').Count -ne 0) {
            throw "gate iniciou Runner.Worker durante quiescencia; processo preservado: $dir"
        }
        Stop-Process -Id $process.ProcessId -Force -ErrorAction Stop
    }
    $deadline = (Get-Date).AddSeconds(30)
    do {
        Start-Sleep -Milliseconds 250
        $workers = Get-RunnerProcesses -ProcessName 'Runner.Worker.exe'
        if ($workers.Count -ne 0) {
            throw "gate iniciou Runner.Worker durante quiescencia; processo preservado: $dir"
        }
        $remaining = Get-RunnerProcesses -ProcessName 'Runner.Listener.exe'
    } while ($remaining.Count -ne 0 -and (Get-Date) -lt $deadline)
    if ($remaining.Count -ne 0) {
        throw "listener do gate nao encerrou: $dir"
    }
}

function Disable-And-UnregisterTask {
    $existing = Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue
    $errors = [System.Collections.Generic.List[string]]::new()
    if ($null -ne $existing) {
        try {
            Disable-ScheduledTask -TaskName $task -ErrorAction Stop | Out-Null
        } catch { $errors.Add($_.Exception.Message) }
        try {
            Unregister-ScheduledTask -TaskName $task -Confirm:$false `
                -ErrorAction Stop
        } catch { $errors.Add($_.Exception.Message) }
    }
    if ($null -ne (Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue)) {
        $errors.Add("task permaneceu registrada: $task")
    }
    if ($errors.Count -ne 0) {
        throw ($errors -join '; ')
    }
}

function Invoke-NetworkServiceAclProbe {
    $probeTask = "civm-gate-acl-probe-$Index"
    $probeScript = Join-Path $diagDir "$probeTask.ps1"
    $probeResult = Join-Path $diagDir "$probeTask.result"
    $deleteProbe = Join-Path $contextDir "$probeTask.delete"
    $moveProbe = "$deleteProbe.moved"
    $createProbe = Join-Path $contextDir "$probeTask.create"
    $staleProbeTask = Get-ScheduledTask -TaskName $probeTask -ErrorAction SilentlyContinue
    if ($null -ne $staleProbeTask) {
        Stop-ScheduledTask -TaskName $probeTask -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $probeTask -Confirm:$false -ErrorAction Stop
    }
    Remove-Item -LiteralPath $probeScript, $probeResult, $deleteProbe, `
        $moveProbe, $createProbe -Force -ErrorAction SilentlyContinue
    [System.IO.File]::WriteAllText($deleteProbe, 'probe')
    Set-ExactProtectedAcl -Path $deleteProbe -Directory $false `
        -NetworkServiceRights ([System.Security.AccessControl.FileSystemRights]::Read
        )
    @'
param(
    [string]$ContextPath, [string]$ContextDir, [string]$DeleteProbe,
    [string]$MoveProbe, [string]$CreateProbe, [string]$RunnerPaths,
    [string]$ResultPath
)
function Denied([scriptblock]$Operation) {
    try {
        & $Operation
        return $false
    } catch [System.UnauthorizedAccessException] {
        return $true
    } catch [System.Security.SecurityException] {
        return $true
    }
}
$readOk = $false
try { [void][IO.File]::ReadAllText($ContextPath); $readOk = $true } catch {}
$contextWriteDenied = Denied {
    $s = [IO.File]::Open($ContextPath, 'Open', 'Write', 'ReadWrite'); $s.Dispose()
}
$createDenied = Denied {
    $s = [IO.File]::Create($CreateProbe); $s.Dispose()
}
$deleteDenied = Denied { [IO.File]::Delete($DeleteProbe) }
$moveDenied = Denied { [IO.File]::Move($DeleteProbe, $MoveProbe) }
$aclDenied = Denied {
    $a = Get-Acl -LiteralPath $DeleteProbe
    $aclMutationRule = [System.Security.AccessControl.FileSystemAccessRule]::new(
        [System.Security.Principal.SecurityIdentifier]'S-1-1-0',
        [System.Security.AccessControl.FileSystemRights]::Read,
        [System.Security.AccessControl.AccessControlType]::Allow)
    $a.AddAccessRule($aclMutationRule) | Out-Null
    Set-Acl -LiteralPath $DeleteProbe -AclObject $a -ErrorAction Stop
}
$runnerWriteDenied = $true
foreach ($path in $RunnerPaths.Split('|')) {
    if (-not (Denied {
        $s = [IO.File]::Open($path, 'Open', 'Write', 'ReadWrite'); $s.Dispose()
    })) { $runnerWriteDenied = $false }
}
$result = "read=$readOk;context_write_denied=$contextWriteDenied;" +
    "create_denied=$createDenied;delete_denied=$deleteDenied;" +
    "move_denied=$moveDenied;acl_denied=$aclDenied;" +
    "runner_write_denied=$runnerWriteDenied"
[IO.File]::WriteAllText($ResultPath, $result)
if ($result -notmatch '=False' -and $readOk) { exit 0 } else { exit 1 }
'@ | Set-Content -LiteralPath $probeScript -Encoding UTF8
    $requiredRunnerPaths = @(
        $runCmdPath,
        $runnerConfigPath,
        (Join-Path $dir '.credentials'),
        $listenerPath
    )
    $missingRunnerPaths = @($requiredRunnerPaths | Where-Object {
        -not (Test-Path -LiteralPath $_ -PathType Leaf)
    })
    if ($missingRunnerPaths.Count -ne 0) {
        throw "arquivos obrigatorios do runner ausentes: $($missingRunnerPaths -join ', ')"
    }
    $runnerPaths = @($requiredRunnerPaths)
    $rsaParams = Join-Path $dir '.credentials_rsaparams'
    if (Test-Path -LiteralPath $rsaParams -PathType Leaf) {
        $runnerPaths += $rsaParams
    }
    $probeArgs = '-NoProfile -ExecutionPolicy Bypass -File "' + $probeScript +
        '" -ContextPath "' + $ContextPath + '" -ContextDir "' + $contextDir +
        '" -DeleteProbe "' + $deleteProbe + '" -MoveProbe "' + $moveProbe +
        '" -CreateProbe "' + $createProbe + '" -RunnerPaths "' +
        ($runnerPaths -join '|') + '" -ResultPath "' + $probeResult + '"'
    $probeAction = New-ScheduledTaskAction -Execute 'powershell.exe' -Argument $probeArgs
    $probeTrigger = New-ScheduledTaskTrigger -Once -At (Get-Date).AddMinutes(1)
    $probePrincipal = New-ScheduledTaskPrincipal -UserId $networkServiceSid.Value `
        -LogonType ServiceAccount -RunLevel Limited
    $probeSettings = New-ScheduledTaskSettingsSet `
        -ExecutionTimeLimit (New-TimeSpan -Minutes 1)
    try {
        Register-ScheduledTask -TaskName $probeTask -Action $probeAction `
            -Trigger $probeTrigger -Principal $probePrincipal `
            -Settings $probeSettings -Force | Out-Null
        Start-ScheduledTask -TaskName $probeTask
        $deadline = (Get-Date).AddSeconds(30)
        do {
            Start-Sleep -Milliseconds 250
            $probeState = (Get-ScheduledTask -TaskName $probeTask).State
        } while ((-not (Test-Path -LiteralPath $probeResult -PathType Leaf) -or
                $probeState -eq 'Running') -and (Get-Date) -lt $deadline)
        $probeInfo = Get-ScheduledTaskInfo -TaskName $probeTask
        $result = if (Test-Path -LiteralPath $probeResult -PathType Leaf) {
            (Get-Content -LiteralPath $probeResult -Raw).Trim()
        } else { '' }
        if ($probeState -eq 'Running' -or $probeInfo.LastTaskResult -ne 0 -or
            [string]::IsNullOrWhiteSpace($result) -or
            $result -match '=False') {
            throw "probe NETWORK SERVICE falhou: $result"
        }
    } finally {
        Stop-ScheduledTask -TaskName $probeTask -ErrorAction SilentlyContinue
        Unregister-ScheduledTask -TaskName $probeTask -Confirm:$false `
            -ErrorAction SilentlyContinue
        Remove-Item -LiteralPath $probeScript, $probeResult, $deleteProbe, `
            $moveProbe, $createProbe -Force -ErrorAction SilentlyContinue
    }
}

Assert-NotReparsePoint -Path $Root
Assert-NotReparsePoint -Path $dir
Assert-NotReparsePoint -Path $rollback
Assert-NotReparsePoint -Path 'C:\ProgramData\civm'
Assert-NotReparsePoint -Path $contextDir
foreach ($existingPath in @($dir, $rollback)) {
    $existingItem = Get-Item -LiteralPath $existingPath -Force `
        -ErrorAction SilentlyContinue
    if ($null -ne $existingItem) {
        [void](Get-SafeTreeItems -Path $existingPath `
            -AllowedRunnerRoots $allowedRunnerRoots)
    }
}
if (-not (Test-Path -LiteralPath $runCmdPath -PathType Leaf) -or
    -not (Test-Path -LiteralPath $runnerConfigPath -PathType Leaf) -or
    -not (Test-Path -LiteralPath $listenerPath -PathType Leaf)) {
    throw "runner nao provisionado: $dir"
}
$runnerConfig = Get-Content -LiteralPath $runnerConfigPath -Raw |
    ConvertFrom-Json -ErrorAction Stop
if ($runnerConfig.DisableUpdate -ne $true) {
    throw "runner precisa ser reprovisionado com --disableupdate: $dir"
}
if (Test-Path -LiteralPath (Join-Path $dir '.service')) {
    throw "runner ainda possui .service; execute o provisionamento limpo: $dir"
}
$runnerUri = [System.Uri]$runnerConfig.gitHubUrl
$runnerOwner = $runnerUri.AbsolutePath.Trim('/')
$expectedRunnerName = "civm-$($runnerOwner.ToLowerInvariant())-gate-$Index"
if ($runnerUri.Scheme -ne 'https' -or $runnerUri.Host -ne 'github.com' -or
    $runnerOwner -notmatch '^[A-Za-z0-9][A-Za-z0-9-]{0,38}$' -or
    $runnerConfig.agentName -ne $expectedRunnerName) {
    throw 'identidade do runner fora do contrato de org/gate'
}
$publisher = Get-ScheduledTask -TaskName $publisherTask -ErrorAction SilentlyContinue
if ($null -ne $publisher -and $publisher.State.ToString() -ne 'Disabled') {
    throw "publisher precisa estar Disabled antes do rollout: $publisherTask"
}
Quarantine-RemoteRunner -Owner $runnerOwner -Name $runnerConfig.agentName
New-Item -ItemType Directory -Path $workDir, $diagDir -Force | Out-Null
if (-not (Test-Path -LiteralPath $contextDir)) {
    New-Item -ItemType Directory -Path $contextDir | Out-Null
}
Assert-NotReparsePoint -Path $contextDir
Set-ExactProtectedAcl -Path $contextDir -Directory $true `
    -InheritToChildren $false `
    -NetworkServiceRights ([Security.AccessControl.FileSystemRights]::ReadAndExecute)
if (Test-Path -LiteralPath $ContextPath) {
    Assert-NotReparsePoint -Path $ContextPath
    if ((Get-Item -LiteralPath $ContextPath -Force).PSIsContainer) {
        throw "ContextPath nao pode ser diretorio: $ContextPath"
    }
    Remove-Item -LiteralPath $ContextPath -Force
}
New-Item -ItemType File -Path $ContextPath | Out-Null
Set-ExactProtectedAcl -Path $ContextPath -Directory $false `
    -NetworkServiceRights ([Security.AccessControl.FileSystemRights]::Read)

# Never mutate lifecycle state while a gate job is active.
if ((Get-RunnerProcesses -ProcessName 'Runner.Worker.exe').Count -ne 0) {
    throw "gate possui Runner.Worker ativo: $dir"
}
$lifecycleError = $null
try { Disable-And-UnregisterTask } catch { $lifecycleError = $_ }
Stop-RunnerProcesses
if ($null -ne $lifecycleError) { throw $lifecycleError }

# The shared root belongs to the fleet. Never normalize it from one gate:
# inherited ACE changes can propagate through hardlinks in another gate.
$rootRights = [Security.AccessControl.FileSystemRights]::ReadAndExecute
Assert-ExactProtectedAcl -Path $Root -Directory $true -InheritToChildren $false `
    -NetworkServiceRights $rootRights
# Rewrite and verify every target-runner DACL so no stale explicit ACE survives.
$allRunnerItems = Get-SafeTreeItems -Path $dir `
    -AllowedRunnerRoots $allowedRunnerRoots
foreach ($item in $allRunnerItems) {
    $writable = $item.FullName.Equals(
        $workDir, [StringComparison]::OrdinalIgnoreCase) -or
        $item.FullName.StartsWith($workDir + '\', [StringComparison]::OrdinalIgnoreCase) -or
        $item.FullName.Equals($diagDir, [StringComparison]::OrdinalIgnoreCase) -or
        $item.FullName.StartsWith($diagDir + '\', [StringComparison]::OrdinalIgnoreCase)
    $rights = if ($writable) {
        [Security.AccessControl.FileSystemRights]::Modify
    } else { [Security.AccessControl.FileSystemRights]::ReadAndExecute }
    Set-ExactProtectedAcl -Path $item.FullName -Directory $item.PSIsContainer `
        -NetworkServiceRights $rights
}
Invoke-NetworkServiceAclProbe

$action = New-ScheduledTaskAction -Execute $listenerPath -Argument 'run' `
    -WorkingDirectory $dir
$tBoot = New-ScheduledTaskTrigger -AtStartup
$tWatch = New-ScheduledTaskTrigger -Once -At (Get-Date) `
    -RepetitionInterval (New-TimeSpan -Minutes 2) `
    -RepetitionDuration (New-TimeSpan -Days 3650)
$principal = New-ScheduledTaskPrincipal -UserId $networkServiceSid.Value `
    -LogonType ServiceAccount -RunLevel Limited
$settings = New-ScheduledTaskSettingsSet -MultipleInstances IgnoreNew `
    -ExecutionTimeLimit ([TimeSpan]::Zero) -StartWhenAvailable `
    -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries
try {
    Register-ScheduledTask -TaskName $task -Action $action `
        -Trigger $tBoot, $tWatch -Principal $principal -Settings $settings `
        -Force | Out-Null
    $registered = Get-ScheduledTask -TaskName $task -ErrorAction Stop
    $registeredSid = Resolve-AccountSid -Account $registered.Principal.UserId
    if ($registeredSid -ne $networkServiceSid.Value -or
        $registered.Principal.LogonType.ToString() -ne 'ServiceAccount' -or
        $registered.Principal.RunLevel.ToString() -ne 'Limited') {
        throw "task nao confirmou NETWORK SERVICE/ServiceAccount/Limited: $task"
    }
    Start-ScheduledTask -TaskName $task
    $deadline = (Get-Date).AddSeconds($listenerStartTimeoutSeconds)
    do {
        Start-Sleep -Milliseconds 500
        $listeners = Get-RunnerProcesses -ProcessName 'Runner.Listener.exe'
    } while ($listeners.Count -ne 1 -and (Get-Date) -lt $deadline)
    if ($listeners.Count -ne 1) { throw "listener unico nao iniciou: $task" }
    $owner = Invoke-CimMethod -InputObject $listeners[0] -MethodName GetOwner
    $ownerSid = if ($owner.ReturnValue -eq 0) {
        Resolve-AccountSid -Account ($owner.Domain + '\' + $owner.User)
    } else { '' }
    if ($owner.ReturnValue -ne 0 -or $ownerSid -ne $networkServiceSid.Value) {
        throw "listener nao confirmou NETWORK SERVICE: $task"
    }
    if ((Get-ScheduledTask -TaskName $task).State -ne 'Running') {
        throw "task nao permaneceu Running: $task"
    }
    Wait-RemoteRunnerOnline -Owner $runnerOwner -Name $runnerConfig.agentName
} catch {
    $failure = $_
    $cleanupFailures = [System.Collections.Generic.List[string]]::new()
    try {
        Quarantine-RemoteRunner -Owner $runnerOwner -Name $runnerConfig.agentName
    } catch { $cleanupFailures.Add($_.Exception.Message) }
    try { Disable-And-UnregisterTask } catch { $cleanupFailures.Add($_.Exception.Message) }
    try { Stop-RunnerProcesses } catch { $cleanupFailures.Add($_.Exception.Message) }
    if ($cleanupFailures.Count -ne 0) {
        throw "$($failure.Exception.Message); falha na compensacao: $($cleanupFailures -join '; ')"
    }
    throw $failure
}

if ($null -ne (Get-Item -LiteralPath $rollback -Force `
        -ErrorAction SilentlyContinue)) {
    [void](Get-SafeTreeItems -Path $rollback `
        -AllowedRunnerRoots $allowedRunnerRoots)
    Remove-Item -LiteralPath $rollback -Recurse -Force
}
Write-Host "OK: '$task' NETWORK SERVICE/limited; contexto e binarios read-only."
