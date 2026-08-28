# civm-gate-runner-provision.ps1 — cleanly reprovision a Windows gate runner.
#
# The rollout first quarantines the existing GitHub registration, then performs
# a side-by-side install under an admin-only DACL. The prior install becomes
# exactly one .rollback until least-privilege setup verifies the new listener.
param(
    [Parameter(Mandatory)][System.Security.SecureString]$GitHubToken,
    [int]$Index = 1,
    [string]$Url = 'https://github.com/emersonbusson',
    [string]$RunnerVersion = '2.336.0',
    [string]$RunnerSHA256 = 'd59123a43003e357b0805b5d0f611d0bd2f65ab67d51bd070dd4e7a0f685c162',
    [string]$Root = 'C:\civm-gate',
    [switch]$ResumeStaged
)
$ErrorActionPreference = 'Stop'
if ($Index -lt 1 -or $Index -gt 99) { throw 'Index fora de 1..99' }
$Root = [System.IO.Path]::GetFullPath($Root).TrimEnd('\')
if (-not $Root.Equals('C:\civm-gate', [System.StringComparison]::OrdinalIgnoreCase)) {
    throw 'Root precisa ser exatamente C:\civm-gate'
}
if ($RunnerVersion -notmatch '^[0-9]+\.[0-9]+\.[0-9]+$' -or
    $RunnerSHA256 -notmatch '^[0-9a-fA-F]{64}$') {
    throw 'pin do actions/runner invalido'
}
$uri = [System.Uri]$Url
$owner = $uri.AbsolutePath.Trim('/')
if ($uri.Scheme -ne 'https' -or $uri.Host -ne 'github.com' -or
    $uri.Port -ne 443 -or $uri.UserInfo -ne '' -or $uri.Query -ne '' -or
    $uri.Fragment -ne '' -or
    $owner -notmatch '^[A-Za-z0-9][A-Za-z0-9-]{0,38}$') {
    throw 'Url precisa ter formato https://github.com/owner'
}
$name = "civm-$($owner.ToLowerInvariant())-gate-$Index"
$dir = Join-Path $Root "runner-$Index"
$stage = "$dir.new"
$rollback = "$dir.rollback"
$task = "civm-gate-runner-$Index"
$publisherTask = 'civm-host-orchestrator'
$allowedRunnerRoots = @(foreach ($runnerIndex in 1..99) {
    $runnerRoot = Join-Path $Root "runner-$runnerIndex"
    $runnerRoot
    "$runnerRoot.new"
    "$runnerRoot.rollback"
})
$activeRunnerRoots = @($dir, $stage)
$systemSid = [System.Security.Principal.SecurityIdentifier]'S-1-5-18'
$administratorsSid = [System.Security.Principal.SecurityIdentifier]'S-1-5-32-544'
$networkServiceSid = [System.Security.Principal.SecurityIdentifier]'S-1-5-20'

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

function Set-DaclWithoutPropagation {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][byte[]]$SecurityDescriptor
    )
    Initialize-CivmGateNative
    try {
        [CivmGateNative.SecurityDescriptorWriter]::SetDacl(
            $Path, $SecurityDescriptor)
    } catch {
        throw "falha gravando DACL sem propagacao em ${Path}: " +
            $_.Exception.GetBaseException().Message
    }
}

function Get-SecurityDescriptorWithoutFollowing {
    param([Parameter(Mandatory)][string]$Path)
    Initialize-CivmGateNative
    try {
        return [CivmGateNative.SecurityDescriptorWriter]::GetOwnerAndDacl($Path)
    } catch {
        throw "falha lendo descritor sem seguir reparse point em ${Path}: " +
            $_.Exception.GetBaseException().Message
    }
}

function Set-ProtectedAcl {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][bool]$Directory,
        [Parameter(Mandatory)][bool]$NetworkRead,
        [Parameter(Mandatory)][bool]$InheritToChildren
    )
    $acl = if ($Directory) {
        New-Object System.Security.AccessControl.DirectorySecurity
    } else { New-Object System.Security.AccessControl.FileSecurity }
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
    if ($NetworkRead) {
        Add-FileSystemRule -Acl $acl -Sid $networkServiceSid `
            -Rights ([System.Security.AccessControl.FileSystemRights]::ReadAndExecute) `
            -Inheritance $inheritance
    }
    Set-Acl -LiteralPath $Path -AclObject $acl

    Assert-ProtectedAcl -Path $Path -Directory $Directory `
        -NetworkRead $NetworkRead -InheritToChildren $InheritToChildren
}

function Assert-ProtectedAcl {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][bool]$Directory,
        [Parameter(Mandatory)][bool]$NetworkRead,
        [Parameter(Mandatory)][bool]$InheritToChildren
    )
    $inheritance = if ($Directory -and $InheritToChildren) {
        [System.Security.AccessControl.InheritanceFlags]'ContainerInherit,ObjectInherit'
    } else { [System.Security.AccessControl.InheritanceFlags]::None }
    $actual = Get-Acl -LiteralPath $Path
    $rules = @($actual.GetAccessRules(
        $true, $true, [System.Security.Principal.SecurityIdentifier]))
    $expectedCount = if ($NetworkRead) { 3 } else { 2 }
    if (-not $actual.AreAccessRulesProtected -or
        (Resolve-AccountSid -Account $actual.Owner) -ne $systemSid.Value -or
        $rules.Count -ne $expectedCount) {
        throw "DACL de provisionamento divergente: $Path"
    }
    $normalizedFull = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $systemSid, [System.Security.AccessControl.FileSystemRights]::FullControl,
        [System.Security.AccessControl.AccessControlType]::Allow).FileSystemRights
    $normalizedRead = [System.Security.AccessControl.FileSystemAccessRule]::new(
        $networkServiceSid, [System.Security.AccessControl.FileSystemRights]::ReadAndExecute,
        [System.Security.AccessControl.AccessControlType]::Allow).FileSystemRights
    $expected = @{}
    $expected[$systemSid.Value] = [int64]$normalizedFull
    $expected[$administratorsSid.Value] = [int64]$normalizedFull
    if ($NetworkRead) { $expected[$networkServiceSid.Value] = [int64]$normalizedRead }
    $seen = @{}
    foreach ($rule in $rules) {
        $sid = $rule.IdentityReference.Value
        if ($rule.IsInherited -or
            $rule.AccessControlType -ne [System.Security.AccessControl.AccessControlType]::Allow -or
            $rule.InheritanceFlags -ne $inheritance -or
            $rule.PropagationFlags -ne [System.Security.AccessControl.PropagationFlags]::None -or
            -not $expected.ContainsKey($sid) -or $seen.ContainsKey($sid) -or
            [int64]$rule.FileSystemRights -ne $expected[$sid]) {
            throw "ACE de provisionamento inesperada: $Path"
        }
        $seen[$sid] = $true
    }
}

function Grant-AdminTraversal {
    param(
        [Parameter(Mandatory)][string]$Path,
        [switch]$AllowReparsePoint
    )
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    $isReparsePoint = ($item.Attributes -band `
        [System.IO.FileAttributes]::ReparsePoint) -ne 0
    if (-not $item.PSIsContainer -or
        ($isReparsePoint -and -not $AllowReparsePoint) -or
        (-not $isReparsePoint -and
            -not [string]::IsNullOrEmpty([string]$item.LinkType))) {
        throw "diretorio inseguro para travessia administrativa: $Path"
    }
    $binary = Get-SecurityDescriptorWithoutFollowing -Path $Path
    $raw = [System.Security.AccessControl.RawSecurityDescriptor]::new($binary, 0)
    $wasProtected = ($raw.ControlFlags -band `
        [System.Security.AccessControl.ControlFlags]::DiscretionaryAclProtected) -ne 0
    $wasOwnerSid = $raw.Owner.Value
    if ($null -eq $raw.DiscretionaryAcl) {
        throw "diretorio sem DACL para travessia administrativa: $Path"
    }
    $expectedRuleCounts = [System.Collections.Hashtable]::new(
        [System.StringComparer]::Ordinal)
    for ($aceIndex = 0; $aceIndex -lt $raw.DiscretionaryAcl.Count; $aceIndex++) {
        $existingAce = $raw.DiscretionaryAcl[$aceIndex]
        $aceBinary = [byte[]]::new($existingAce.BinaryLength)
        $existingAce.GetBinaryForm($aceBinary, 0)
        $key = [Convert]::ToBase64String($aceBinary)
        $expectedRuleCounts[$key] = 1 + [int]$expectedRuleCounts[$key]
    }
    $insertIndex = 0
    while ($insertIndex -lt $raw.DiscretionaryAcl.Count -and
        ([int]$raw.DiscretionaryAcl[$insertIndex].AceFlags -band
            [int][System.Security.AccessControl.AceFlags]::Inherited) -eq 0) {
        $insertIndex++
    }
    $sidsToAdd = [System.Collections.Generic.List[object]]::new()
    foreach ($sid in @($systemSid, $administratorsSid)) {
        $matchingAllowFound = $false
        $sufficientAllowFound = $false
        for ($aceIndex = 0; $aceIndex -lt $raw.DiscretionaryAcl.Count; $aceIndex++) {
            $candidate = $raw.DiscretionaryAcl[$aceIndex]
            if ($candidate -is [System.Security.AccessControl.QualifiedAce] -and
                $candidate.AceQualifier -eq `
                    [System.Security.AccessControl.AceQualifier]::AccessAllowed -and
                $candidate.SecurityIdentifier.Value -eq $sid.Value) {
                $matchingAllowFound = $true
            }
            if ($candidate -is [System.Security.AccessControl.CommonAce] -and
                $candidate.AceQualifier -eq `
                    [System.Security.AccessControl.AceQualifier]::AccessAllowed -and
                $candidate.SecurityIdentifier.Value -eq $sid.Value -and
                ([int]$candidate.AceFlags -band
                    [int][System.Security.AccessControl.AceFlags]::InheritOnly) -eq 0 -and
                ($candidate.AccessMask -band `
                    [int][System.Security.AccessControl.FileSystemRights]::FullControl) -eq `
                    [int][System.Security.AccessControl.FileSystemRights]::FullControl) {
                $sufficientAllowFound = $true
                break
            }
        }
        if ($sufficientAllowFound) { continue }
        if ($matchingAllowFound) {
            throw "ACE administrativa existente e insuficiente: $Path; " +
                "principal=$($sid.Value)"
        }
        $sidsToAdd.Add($sid)
    }
    foreach ($sid in $sidsToAdd) {
        $ace = [System.Security.AccessControl.CommonAce]::new(
            [System.Security.AccessControl.AceFlags]::None,
            [System.Security.AccessControl.AceQualifier]::AccessAllowed,
            [int][System.Security.AccessControl.FileSystemRights]::FullControl,
            $sid, $false, $null)
        $raw.DiscretionaryAcl.InsertAce($insertIndex, $ace)
        $insertIndex++
        $aceBinary = [byte[]]::new($ace.BinaryLength)
        $ace.GetBinaryForm($aceBinary, 0)
        $ruleKey = [Convert]::ToBase64String($aceBinary)
        $expectedRuleCounts[$ruleKey] = 1 + [int]$expectedRuleCounts[$ruleKey]
    }
    if ($sidsToAdd.Count -ne 0) {
        $updatedBinary = [byte[]]::new($raw.BinaryLength)
        $raw.GetBinaryForm($updatedBinary, 0)
        Set-DaclWithoutPropagation -Path $Path `
            -SecurityDescriptor $updatedBinary
    }
    $actualBinary = Get-SecurityDescriptorWithoutFollowing -Path $Path
    $actualRaw = [System.Security.AccessControl.RawSecurityDescriptor]::new(
        $actualBinary, 0)
    $actualProtected = ($actualRaw.ControlFlags -band `
        [System.Security.AccessControl.ControlFlags]::DiscretionaryAclProtected) -ne 0
    if ($actualProtected -ne $wasProtected -or
        $actualRaw.Owner.Value -ne $wasOwnerSid) {
        throw "reparo alterou owner ou protecao da DACL: $Path"
    }
    $actualRuleCounts = [System.Collections.Hashtable]::new(
        [System.StringComparer]::Ordinal)
    for ($aceIndex = 0; $aceIndex -lt $actualRaw.DiscretionaryAcl.Count; $aceIndex++) {
        $actualAce = $actualRaw.DiscretionaryAcl[$aceIndex]
        $aceBinary = [byte[]]::new($actualAce.BinaryLength)
        $actualAce.GetBinaryForm($aceBinary, 0)
        $key = [Convert]::ToBase64String($aceBinary)
        $actualRuleCounts[$key] = 1 + [int]$actualRuleCounts[$key]
    }
    $allRuleKeys = @($expectedRuleCounts.Keys) + @($actualRuleCounts.Keys) |
        Sort-Object -Unique
    foreach ($key in $allRuleKeys) {
        if ([int]$expectedRuleCounts[$key] -ne [int]$actualRuleCounts[$key]) {
            throw "reparo alterou conjunto de ACEs: $Path; regra=$key; " +
                "esperado=$([int]$expectedRuleCounts[$key]); " +
                "atual=$([int]$actualRuleCounts[$key])"
        }
    }
    foreach ($sid in @($systemSid, $administratorsSid)) {
        $sufficientAceFound = $false
        for ($aceIndex = 0;
            $aceIndex -lt $actualRaw.DiscretionaryAcl.Count;
            $aceIndex++) {
            $candidate = $actualRaw.DiscretionaryAcl[$aceIndex]
            if ($candidate -is [System.Security.AccessControl.CommonAce] -and
                $candidate.AceQualifier -eq `
                    [System.Security.AccessControl.AceQualifier]::AccessAllowed -and
                $candidate.SecurityIdentifier.Value -eq $sid.Value -and
                ([int]$candidate.AceFlags -band
                    [int][System.Security.AccessControl.AceFlags]::InheritOnly) -eq 0 -and
                ($candidate.AccessMask -band `
                    [int][System.Security.AccessControl.FileSystemRights]::FullControl) -eq `
                    [int][System.Security.AccessControl.FileSystemRights]::FullControl) {
                $sufficientAceFound = $true
                break
            }
        }
        if (-not $sufficientAceFound) {
            $actualSummary = @(
                for ($summaryIndex = 0;
                    $summaryIndex -lt $actualRaw.DiscretionaryAcl.Count;
                    $summaryIndex++) {
                    $summaryAce = $actualRaw.DiscretionaryAcl[$summaryIndex]
                    "$($summaryAce.SecurityIdentifier.Value):" +
                        "$($summaryAce.AceQualifier):" +
                        "$($summaryAce.AccessMask):$($summaryAce.AceFlags)"
                }) -join '; '
            throw "reparo nao concedeu travessia administrativa: $Path; " +
                "esperado=$($sid.Value); atual=$actualSummary"
        }
    }
}

function Get-SafeTreeItems {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string[]]$AllowedRunnerRoots,
        [switch]$EnsureAdminTraversal
    )
    $pending = [System.Collections.Stack]::new()
    $pending.Push((Get-Item -LiteralPath $Path -Force))
    $items = [System.Collections.Generic.List[object]]::new()
    while ($pending.Count -ne 0) {
        $item = $pending.Pop()
        if (($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
            if ($EnsureAdminTraversal) {
                Grant-AdminTraversal -Path $item.FullName -AllowReparsePoint
                $item = Get-Item -LiteralPath $item.FullName -Force -ErrorAction Stop
            }
            Assert-SafeOfficialRunnerJunction -Item $item `
                -ExpectedRunnerVersion $RunnerVersion `
                -AllowedRunnerRoots $AllowedRunnerRoots
            $items.Add($item)
            continue
        }
        if (-not [string]::IsNullOrEmpty([string]$item.LinkType)) {
            throw "link de filesystem proibido no escopo do runner: $($item.FullName)"
        }
        if (-not $item.PSIsContainer -and
            (Get-FileLinkCount -Path $item.FullName) -ne 1) {
            throw "link de filesystem proibido no escopo do runner: $($item.FullName)"
        }
        $items.Add($item)
        if ($item.PSIsContainer) {
            if ($EnsureAdminTraversal) {
                Grant-AdminTraversal -Path $item.FullName
            }
            foreach ($child in (Get-ChildItem -LiteralPath $item.FullName -Force)) {
                $pending.Push($child)
            }
        }
    }
    return $items.ToArray()
}

function Assert-SafeTreeRoot {
    param([Parameter(Mandatory)][string]$Path)
    try {
        $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    } catch [System.Management.Automation.ItemNotFoundException] {
        return
    }
    if (-not $item.PSIsContainer -or
        ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0 -or
        -not [string]::IsNullOrEmpty([string]$item.LinkType)) {
        throw "raiz de arvore insegura no escopo do runner: $Path"
    }
}

function Ensure-ProtectedSharedRoot {
    if (Test-Path -LiteralPath $Root) {
        Assert-SafeTreeRoot -Path $Root
        Assert-ProtectedAcl -Path $Root -Directory $true -NetworkRead $true `
            -InheritToChildren $false
        return
    }
    New-Item -ItemType Directory -Path $Root | Out-Null
    Set-ProtectedAcl -Path $Root -Directory $true -NetworkRead $true `
        -InheritToChildren $false
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
        throw "reparse point proibido no escopo do runner: $($Item.FullName)"
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

function Protect-AdminTree {
    param(
        [Parameter(Mandatory)][string]$Path,
        [Parameter(Mandatory)][string[]]$AllowedRunnerRoots
    )
    foreach ($item in (Get-SafeTreeItems -Path $Path `
            -AllowedRunnerRoots $AllowedRunnerRoots)) {
        Set-ProtectedAcl -Path $item.FullName -Directory $item.PSIsContainer `
            -NetworkRead $false -InheritToChildren $item.PSIsContainer
    }
}

function Move-StagedRunner {
    $oldMoved = $false
    try {
        if (Test-Path -LiteralPath $dir) {
            Protect-AdminTree -Path $dir -AllowedRunnerRoots $allowedRunnerRoots
            Move-Item -LiteralPath $dir -Destination $rollback
            $oldMoved = $true
        }
        Move-Item -LiteralPath $stage -Destination $dir
    } catch {
        $swapFailure = $_
        if ($oldMoved -and -not (Test-Path -LiteralPath $dir) -and
            (Test-Path -LiteralPath $rollback)) {
            try {
                Move-Item -LiteralPath $rollback -Destination $dir
            } catch {
                throw "$($swapFailure.Exception.Message); falha restaurando rollback: $($_.Exception.Message)"
            }
        }
        throw $swapFailure
    }
}

function Get-RunnerProcesses {
    param(
        [Parameter(Mandatory)][string]$ProcessName,
        [string[]]$Roots = $activeRunnerRoots
    )
    return @(Get-CimInstance Win32_Process -Filter "Name = '$ProcessName'" |
        Where-Object {
            $process = $_
            $process.ExecutablePath -and @($Roots | Where-Object {
                $process.ExecutablePath.StartsWith(
                    $_ + '\',
                    [System.StringComparison]::OrdinalIgnoreCase)
            }).Count -ne 0
        })
}

function Get-RunnerPathTasks {
    $matches = [System.Collections.Generic.List[object]]::new()
    foreach ($candidate in @(Get-ScheduledTask -ErrorAction Stop)) {
        $pointsToRunner = $false
        foreach ($action in @($candidate.Actions)) {
            $actionFields = @(
                [string]$action.Execute,
                [string]$action.Arguments,
                [string]$action.WorkingDirectory)
            if (@($activeRunnerRoots | Where-Object {
                    $runnerRoot = $_
                    @($actionFields | Where-Object {
                        $field = $_
                        $field.Trim('"').Equals(
                            $runnerRoot,
                            [System.StringComparison]::OrdinalIgnoreCase) -or
                        $field.IndexOf(
                            $runnerRoot + '\',
                            [System.StringComparison]::OrdinalIgnoreCase) -ge 0
                    }).Count -ne 0
                }).Count -ne 0) {
                $pointsToRunner = $true
                break
            }
        }
        if ($pointsToRunner) { $matches.Add($candidate) }
    }
    return $matches.ToArray()
}

function Invoke-GitHubApi {
    param(
        [Parameter(Mandatory)][ValidateSet('GET', 'POST', 'DELETE')][string]$Method,
        [Parameter(Mandatory)][string]$Path
    )
    $plainToken = [System.Net.NetworkCredential]::new('', $GitHubToken).Password
    try {
        return Invoke-RestMethod -Method $Method -Uri "https://api.github.com/$Path" `
            -Headers @{
                Accept = 'application/vnd.github+json'
                Authorization = "Bearer $plainToken"
                'X-GitHub-Api-Version' = '2022-11-28'
                'User-Agent' = 'civm-gate-provision'
            } -ErrorAction Stop
    } finally {
        $plainToken = $null
    }
}

function Stop-IdleListener {
    if ((Get-RunnerProcesses -ProcessName 'Runner.Worker.exe').Count -ne 0) {
        throw "gate possui Runner.Worker ativo; processo preservado: $dir"
    }
    foreach ($listener in (Get-RunnerProcesses -ProcessName 'Runner.Listener.exe')) {
        if ((Get-RunnerProcesses -ProcessName 'Runner.Worker.exe').Count -ne 0) {
            throw "gate iniciou Runner.Worker durante quiescencia; processo preservado: $dir"
        }
        Stop-Process -Id $listener.ProcessId -Force -ErrorAction Stop
    }
    $deadline = (Get-Date).AddSeconds(30)
    do {
        Start-Sleep -Milliseconds 250
        $workers = Get-RunnerProcesses -ProcessName 'Runner.Worker.exe'
        if ($workers.Count -ne 0) {
            throw "gate iniciou Runner.Worker durante quiescencia; processo preservado: $dir"
        }
        $listeners = Get-RunnerProcesses -ProcessName 'Runner.Listener.exe'
    } while ($listeners.Count -ne 0 -and (Get-Date) -lt $deadline)
    if ($listeners.Count -ne 0) { throw "listener antigo nao encerrou: $dir" }
}

function Get-RemoteRunner {
    $allRunners = [System.Collections.Generic.List[object]]::new()
    $page = 1
    do {
        $response = Invoke-GitHubApi -Method GET `
            -Path "orgs/$owner/actions/runners?per_page=100&page=$page"
        $batch = @($response.runners)
        foreach ($runner in $batch) { $allRunners.Add($runner) }
        $page++
    } while ($batch.Count -eq 100)
    $matches = @($allRunners | Where-Object { $_.name -eq $name })
    if ($matches.Count -gt 1) { throw "runner remoto duplicado: $name" }
    if ($matches.Count -eq 0) { return $null }
    return $matches[0]
}

function Quarantine-RemoteRunner {
    $remote = Get-RemoteRunner
    if ($null -eq $remote) { return }
    if (@($remote.labels | Where-Object { $_.type -eq 'custom' }).Count -ne 0) {
        Invoke-GitHubApi -Method DELETE `
            -Path "orgs/$owner/actions/runners/$($remote.id)/labels" |
            Out-Null
    }
    $stable = 0
    $deadline = (Get-Date).AddSeconds(15)
    do {
        Start-Sleep -Milliseconds 500
        $remote = Get-RemoteRunner
        $locallyIdle = (Get-RunnerProcesses -ProcessName 'Runner.Worker.exe').Count -eq 0
        $quarantined = $null -eq $remote -or (
            -not $remote.busy -and
            @($remote.labels | Where-Object { $_.type -eq 'custom' }).Count -eq 0)
        if ($locallyIdle -and $quarantined) { $stable++ } else { $stable = 0 }
    } while ($stable -lt 6 -and (Get-Date) -lt $deadline)
    if ($stable -lt 6) { throw "runner nao drenou apos quarentena: $name" }
}

function Get-ServiceExecutablePath {
    param([Parameter(Mandatory)]$Service)
    if ([string]::IsNullOrWhiteSpace($Service.PathName)) { return $null }
    $match = [regex]::Match($Service.PathName, '^(?:"([^"]+)"|(\S+))')
    if (-not $match.Success) { return $null }
    $imagePath = if ($match.Groups[1].Success) {
        $match.Groups[1].Value
    } else { $match.Groups[2].Value }
    try { return [System.IO.Path]::GetFullPath($imagePath) } catch { return $null }
}

function Get-LegacyService {
    $expected = [System.IO.Path]::GetFullPath((Join-Path $dir `
        'bin\RunnerService.exe'))
    $expectedTarget = [System.IO.Path]::GetFullPath((Join-Path $dir `
        "bin.$RunnerVersion\RunnerService.exe"))
    $services = @(Get-CimInstance Win32_Service -ErrorAction Stop)
    $stageMatches = @($services | Where-Object {
        $imagePath = Get-ServiceExecutablePath -Service $_
        $null -ne $imagePath -and $imagePath.StartsWith(
            $stage + '\', [System.StringComparison]::OrdinalIgnoreCase)
    })
    if ($stageMatches.Count -ne 0) {
        throw "service inesperado aponta para staging: $stage"
    }
    $matches = @($services | Where-Object {
        $imagePath = Get-ServiceExecutablePath -Service $_
        $null -ne $imagePath -and $imagePath.StartsWith(
            $dir + '\', [System.StringComparison]::OrdinalIgnoreCase)
    })
    if ($matches.Count -gt 1) {
        throw "mais de um service aponta para o gate: $dir"
    }
    if ($matches.Count -eq 0) { return $null }
    $imagePath = Get-ServiceExecutablePath -Service $matches[0]
    if (-not $imagePath.Equals(
            $expected, [System.StringComparison]::OrdinalIgnoreCase) -and
        -not $imagePath.Equals(
            $expectedTarget, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "service inesperado dentro do gate: $imagePath"
    }
    return Get-Service -Name $matches[0].Name -ErrorAction Stop
}

foreach ($existingPath in @($Root, $dir, $stage, $rollback)) {
    Assert-SafeTreeRoot -Path $existingPath
}
if (Test-Path -LiteralPath $Root) {
    Assert-ProtectedAcl -Path $Root -Directory $true -NetworkRead $true `
        -InheritToChildren $false
}
if ($ResumeStaged -and -not (Test-Path -LiteralPath $stage -PathType Container)) {
    throw "ResumeStaged exige staging existente: $stage"
}
if (-not $ResumeStaged -and (Test-Path -LiteralPath $stage)) {
    throw "staging anterior exige revisao manual: $stage"
}
if (Test-Path -LiteralPath $rollback) {
    throw "rollback anterior exige revisao manual: $rollback"
}
$publisher = Get-ScheduledTask -TaskName $publisherTask -ErrorAction SilentlyContinue
if ($null -ne $publisher -and $publisher.State.ToString() -ne 'Disabled') {
    throw "publisher precisa estar Disabled antes do rollout: $publisherTask"
}
$runnerPathTasks = @(Get-RunnerPathTasks)
$unexpectedTasks = @($runnerPathTasks | Where-Object { $_.TaskName -ne $task })
if ($unexpectedTasks.Count -ne 0 -or $runnerPathTasks.Count -gt 1) {
    throw "task orfa aponta para dir/staging do gate"
}
if ($ResumeStaged) {
    foreach ($processName in @(
            'Runner.Listener.exe', 'Runner.Worker.exe', 'RunnerService.exe')) {
        if ((Get-RunnerProcesses -ProcessName $processName -Roots @($stage)).Count -ne 0) {
            throw "processo ativo no staging durante resume: $processName"
        }
    }
}
$service = Get-LegacyService

# Removing the base label closes remote admission before any local stop. The
# disabled publisher cannot restore a generation label during the dwell.
Quarantine-RemoteRunner
$oldTask = Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue
$lifecycleErrors = [System.Collections.Generic.List[string]]::new()
if ($null -ne $oldTask) {
    try {
        Disable-ScheduledTask -TaskName $task -ErrorAction Stop | Out-Null
    } catch { $lifecycleErrors.Add($_.Exception.Message) }
}
if ($null -ne $service) {
    try {
        Set-Service -Name $service.Name -StartupType Disabled
    } catch { $lifecycleErrors.Add($_.Exception.Message) }
}
if ($null -ne $service) {
    try {
        Stop-Service -Name $service.Name -Force -ErrorAction Stop
        $service.WaitForStatus('Stopped', [TimeSpan]::FromSeconds(30))
    } catch { $lifecycleErrors.Add($_.Exception.Message) }
}
Stop-IdleListener
if ($null -ne $service) {
    try {
        & sc.exe delete $service.Name | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "falha removendo service $($service.Name)" }
    } catch { $lifecycleErrors.Add($_.Exception.Message) }
}
if ($null -ne $oldTask) {
    try {
        Unregister-ScheduledTask -TaskName $task -Confirm:$false -ErrorAction Stop
        if ($null -ne (Get-ScheduledTask -TaskName $task -ErrorAction SilentlyContinue)) {
            throw "task permaneceu registrada: $task"
        }
    } catch { $lifecycleErrors.Add($_.Exception.Message) }
}
Stop-IdleListener
if ((Get-RunnerPathTasks).Count -ne 0) {
    $lifecycleErrors.Add('task permaneceu apontando para dir/staging do gate')
}
if ($lifecycleErrors.Count -ne 0) {
    throw "cleanup local incompleto apos quarentena: $($lifecycleErrors -join '; ')"
}

if (Test-Path -LiteralPath $dir) {
    [void](Get-SafeTreeItems -Path $dir `
        -AllowedRunnerRoots $allowedRunnerRoots -EnsureAdminTraversal)
}

Ensure-ProtectedSharedRoot
if (-not $ResumeStaged) {
    if (Test-Path -LiteralPath $stage) {
        throw "staging apareceu durante a quarentena: $stage"
    }
    New-Item -ItemType Directory -Path $stage | Out-Null
    Set-ProtectedAcl -Path $stage -Directory $true -NetworkRead $false `
        -InheritToChildren $true
    $zip = Join-Path $stage 'runner.zip'
    $src = "https://github.com/actions/runner/releases/download/v$RunnerVersion/actions-runner-win-x64-$RunnerVersion.zip"
    Write-Host "baixando actions/runner v$RunnerVersion ..."
    Invoke-WebRequest -Uri $src -OutFile $zip
    $actualSHA256 = (Get-FileHash -LiteralPath $zip -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualSHA256 -ne $RunnerSHA256.ToLowerInvariant()) {
        throw "SHA256 invalido para actions/runner v$RunnerVersion"
    }
    Expand-Archive -LiteralPath $zip -DestinationPath $stage -Force
    Remove-Item -LiteralPath $zip -Force
    foreach ($item in (Get-SafeTreeItems -Path $stage `
            -AllowedRunnerRoots $allowedRunnerRoots)) {
        if (-not $item.PSIsContainer) { Unblock-File -LiteralPath $item.FullName }
    }
    Protect-AdminTree -Path $stage -AllowedRunnerRoots $allowedRunnerRoots

    Push-Location $stage
    try {
        $registration = Invoke-GitHubApi -Method POST `
            -Path "orgs/$owner/actions/runners/registration-token"
        $RegToken = $registration.token
        if ([string]::IsNullOrWhiteSpace($RegToken)) {
            throw 'API nao retornou registration token'
        }
        & .\config.cmd --unattended --url $Url --token $RegToken `
            --labels 'civm-gate' --name $name --work '_work' `
            --disableupdate --replace
        if ($LASTEXITCODE -ne 0) {
            throw "config.cmd falhou com exit $LASTEXITCODE"
        }
    } finally {
        $RegToken = $null
        Pop-Location
    }
    Quarantine-RemoteRunner
}
Protect-AdminTree -Path $stage -AllowedRunnerRoots $allowedRunnerRoots

$newConfigPath = Join-Path $stage '.runner'
$listenerPath = Join-Path $stage 'bin\Runner.Listener.exe'
foreach ($requiredFile in @(
        $newConfigPath,
        (Join-Path $stage '.credentials'),
        (Join-Path $stage '.credentials_rsaparams'),
        (Join-Path $stage 'run.cmd'),
        $listenerPath)) {
    if (-not (Test-Path -LiteralPath $requiredFile -PathType Leaf) -or
        (Get-Item -LiteralPath $requiredFile -Force).Length -eq 0) {
        throw "staging sem arquivo obrigatorio: $requiredFile"
    }
}
$newConfig = Get-Content -LiteralPath $newConfigPath -Raw |
    ConvertFrom-Json -ErrorAction Stop
$listenerVersion = (Get-Item -LiteralPath $listenerPath -Force).VersionInfo.FileVersion
if ($newConfig.agentName -ne $name -or $newConfig.DisableUpdate -ne $true -or
    $newConfig.gitHubUrl.TrimEnd('/') -ne $Url.TrimEnd('/') -or
    $newConfig.workFolder -ne '_work' -or
    $listenerVersion -ne "$RunnerVersion.0") {
    throw 'pos-condicao .runner divergente'
}
$remote = Get-RemoteRunner
if ($null -eq $remote -or $remote.busy -or $remote.os -ne 'Windows' -or
    $remote.status -ne 'offline' -or $newConfig.agentId -ne $remote.id -or
    @($remote.labels | Where-Object { $_.type -eq 'custom' }).Count -ne 0) {
    throw "pos-condicao remota divergente para $name"
}

Move-StagedRunner
$mode = if ($ResumeStaged) { 'staging validado e retomado' } else { 'provisionado limpo' }
Write-Host "OK: '$name' $mode; execute civm-gate-task-setup.ps1 -Index $Index."
