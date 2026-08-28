package hostdisk

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// dangerousMaxMin matches the [math]::Max(0, ... ) / [math]::Min(0, ... ) form
// where the clamp literal is a bare Int32 0.
//
// The civm-vhdx-autoreclaim worker once called [math]::Max(0, <int64 bytes>).
// A bare 0 is Int32, which pins .NET overload resolution to Max(int, int) and
// throws "Valor era muito grande ou muito pequeno para Int32" on any byte value
// above Int32.MaxValue (~2 GiB). That aborted every reclaim run, so the dynamic
// VHDX was never compacted and the Hyper-V host volume (V:) silently filled
// until the runner wedged. Always clamp with [int64]0 (or 0L) so the
// Max(long, long) overload is selected. This guard keeps the Int32 form from
// ever returning to any deploy/windows script.
var dangerousMaxMin = regexp.MustCompile(`\[math\]::(Max|Min)\(\s*0\s*,`)
var powerShellScopedVariable = regexp.MustCompile(`\$([A-Za-z_][A-Za-z0-9_]*):`)

func TestWindowsScriptsHaveNoInt32MaxMinLiteral(t *testing.T) {
	dir := filepath.Join("..", "..", "deploy", "windows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read deploy/windows: %v", err)
	}
	scanned := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ps1") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		scanned++
		for i, line := range strings.Split(string(data), "\n") {
			if dangerousMaxMin.MatchString(line) {
				t.Errorf("%s:%d clamps with a bare Int32 0 literal, which overflows "+
					"on byte values >2 GiB; cast to [int64]0 to force Max(long,long): %s",
					entry.Name(), i+1, strings.TrimSpace(line))
			}
		}
	}
	if scanned == 0 {
		t.Fatal("no .ps1 files scanned under deploy/windows")
	}
}

func TestWindowsScriptsHaveNoAmbiguousInterpolatedVariableColon(t *testing.T) {
	allowedScopes := map[string]bool{
		"alias": true, "env": true, "function": true, "global": true,
		"local": true, "private": true, "script": true, "using": true,
		"variable": true,
	}
	dir := filepath.Join("..", "..", "deploy", "windows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read deploy/windows: %v", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".ps1") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, match := range powerShellScopedVariable.FindAllStringSubmatch(line, -1) {
				if !allowedScopes[strings.ToLower(match[1])] {
					t.Errorf("%s:%d has ambiguous $%s: interpolation; use ${%s}: instead",
						entry.Name(), i+1, match[1], match[1])
				}
			}
		}
	}
}

func TestActiveGenerationRolloutUsesDynamicGateLabels(t *testing.T) {
	paths := []string{
		filepath.Join("..", "..", "runbooks", "GENERATION-CLEAN-BOUNDARY-ROLLOUT.md"),
		filepath.Join("..", "..", "docs", "specs", "generation-clean-boundary", "SPECv2.md"),
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		source := string(data)
		for _, forbidden := range []string{
			"CIVM_GENERATION_GATE_LABELS=false",
			"must use only the static labels",
			"usa apenas `self-hosted,civm-gate` estático",
		} {
			if strings.Contains(source, forbidden) {
				t.Errorf("%s revives superseded static gate contract %q", path, forbidden)
			}
		}
		if !strings.Contains(source, "label dinâmica") {
			t.Errorf("%s does not document dynamic generation admission", path)
		}
	}
}

func TestGateRunnerTaskUsesLeastPrivilegeAndReadOnlyContext(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "windows", "civm-gate-task-setup.ps1")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	source := string(data)
	for _, required := range []string{
		"S-1-5-20",
		"-RunLevel Limited",
		"SetAccessRuleProtection($true, $false)",
		"FileSystemRights]::Modify",
		"FileSystemRights]::ReadAndExecute",
		"Join-Path $dir '_work'",
		"Join-Path $dir '_diag'",
		"DisableUpdate",
		"normalizedNetwork",
		"InheritanceFlags -ne $expectedInheritance",
		"PropagationFlags -ne",
		"Resolve-AccountSid",
		"FileSystemRights]::Read",
		"C:\\ProgramData\\civm\\gate\\current-context",
		"Root ou ContextPath fora do escopo canônico",
		"Assert-NotReparsePoint",
		"Assert-NotReparsePoint -Path $rollback",
		"Stop-ScheduledTask",
		"-MethodName GetOwner",
		"$allRunnerItems",
		"create_denied",
		"delete_denied",
		"move_denied",
		"acl_denied",
		"$aclMutationRule = [System.Security.AccessControl.FileSystemAccessRule]::new(",
		"$a.AddAccessRule($aclMutationRule)",
		"Set-Acl -LiteralPath $DeleteProbe -AclObject $a -ErrorAction Stop",
		"$listenerStartTimeoutSeconds = 120",
		"$remoteOnlineTimeoutSeconds = 120",
		"AddSeconds($listenerStartTimeoutSeconds)",
		"AddSeconds($remoteOnlineTimeoutSeconds)",
		"return ,@(Get-CimInstance Win32_Process",
		"runner_write_denied",
		"[string]::IsNullOrWhiteSpace($result)",
		"catch [System.UnauthorizedAccessException]",
		"catch [System.Security.SecurityException]",
		"task permaneceu registrada",
		"processo preservado",
		"Quarantine-RemoteRunner",
		"Wait-RemoteRunnerOnline",
		"Where-Object { $_.type -eq 'custom' }",
		"/labels\" | Out-Null",
		"publisher precisa estar Disabled",
		"$listenerPath -Argument 'run'",
		"Get-SafeTreeItems -Path $dir",
		"System.Security.SecureString]$GitHubToken",
		"Assert-ExactProtectedAcl -Path $Root",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("gate task is missing least-privilege contract %q", required)
		}
	}
	if !strings.Contains(source, "Disable-And-UnregisterTask") ||
		!strings.Contains(source, "Stop-RunnerProcesses") ||
		!strings.Contains(source, "listener nao confirmou NETWORK SERVICE") {
		t.Error("gate task migration must fail closed on stale task or wrong process owner")
	}
	if strings.Contains(source, "-UserId 'SYSTEM'") ||
		strings.Contains(source, "-RunLevel Highest") {
		t.Error("gate task must not run as SYSTEM/Highest")
	}
	if strings.Contains(source, "foreach ($processName in @('Runner.Worker.exe'") {
		t.Error("gate task must never include Runner.Worker in a stop loop")
	}
	if strings.Contains(source, "-Execute $runCmdPath") {
		t.Error("read-only gate task must not execute run.cmd because it copies run-helper.cmd")
	}
	if strings.Contains(source, "gh.exe") {
		t.Error("gate setup must use the in-memory token directly, not ambient gh auth")
	}
	if strings.Contains(source, "Set-ExactProtectedAcl -Path $Root") {
		t.Error("gate setup must validate, not mutate, the shared fleet root")
	}
	quarantine := strings.Index(source, "\nQuarantine-RemoteRunner -Owner")
	removeContext := strings.Index(source, "\n    Remove-Item -LiteralPath $ContextPath -Force")
	disableTask := strings.Index(source, "\ntry { Disable-And-UnregisterTask }")
	stopProcesses := strings.Index(source, "\nStop-RunnerProcesses\n")
	startTask := strings.Index(source, "\n    Start-ScheduledTask -TaskName $task")
	waitOnline := strings.Index(source, "\n    Wait-RemoteRunnerOnline -Owner")
	compensation := strings.LastIndex(source, "\n        Quarantine-RemoteRunner -Owner")
	compensationCleanup := strings.LastIndex(source, "\n    try { Disable-And-UnregisterTask }")
	treePreflight := strings.Index(source, "\nforeach ($existingPath in @($dir, $rollback))")
	runnerFileProbe := strings.Index(source, "\nif (-not (Test-Path -LiteralPath $runCmdPath")
	if quarantine < 0 || removeContext < 0 || disableTask < 0 || stopProcesses < 0 ||
		quarantine > removeContext || quarantine > disableTask || quarantine > stopProcesses {
		t.Error("gate setup must quarantine remote admission before context or local lifecycle mutation")
	}
	if treePreflight < 0 || treePreflight > quarantine {
		t.Error("gate setup must reject unsafe junctions before remote quarantine")
	}
	if runnerFileProbe < 0 || treePreflight > runnerFileProbe {
		t.Error("gate setup must reject unsafe junctions before probing runner files")
	}
	rollbackRevalidation := strings.LastIndex(source, "\n    [void](Get-SafeTreeItems -Path $rollback")
	rollbackRemoval := strings.LastIndex(source, "\n    Remove-Item -LiteralPath $rollback -Recurse -Force")
	if rollbackRevalidation < 0 || rollbackRemoval < 0 || rollbackRevalidation > rollbackRemoval {
		t.Error("gate setup must revalidate rollback immediately before recursive removal")
	}
	if startTask < 0 || waitOnline < 0 || startTask > waitOnline {
		t.Error("gate setup must start the listener before proving remote online state")
	}
	if compensation < 0 || compensationCleanup < 0 || compensation > compensationCleanup {
		t.Error("gate setup catch must re-quarantine before local compensation cleanup")
	}
}

func TestGateRunnerProvisionDisablesAutoUpdate(t *testing.T) {
	path := filepath.Join("..", "..", "deploy", "windows", "civm-gate-runner-provision.ps1")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	source := string(data)
	if !strings.Contains(source, "--disableupdate") {
		t.Error("gate runner must disable self-update before executable ACL becomes read-only")
	}
	if strings.Contains(source, "--runasservice") {
		t.Error("gate runner provisioning must not install the known-broken Windows service")
	}
	for _, required := range []string{
		"2.336.0",
		"d59123a43003e357b0805b5d0f611d0bd2f65ab67d51bd070dd4e7a0f685c162",
		"Get-FileHash",
		"$LASTEXITCODE",
		".rollback",
		"Disable-ScheduledTask",
		"Runner.Worker.exe",
		"$uri.Port -ne 443",
		"task permaneceu registrada",
		"durante quiescencia",
		"Root precisa ser exatamente C:\\civm-gate",
		"reparse point proibido no escopo do runner",
		"Set-ProtectedAcl -Path $stage",
		"Protect-AdminTree -Path $stage",
		"Quarantine-RemoteRunner",
		"publisher precisa estar Disabled",
		"mais de um service aponta para o gate",
		"pos-condicao remota divergente",
		"System.Security.SecureString]$GitHubToken",
		"Invoke-GitHubApi -Method DELETE",
		"registration-token",
		"[switch]$ResumeStaged",
		"runners/$($remote.id)/labels\"",
		"ResumeStaged exige staging existente",
		"-EnsureAdminTraversal",
		"RawSecurityDescriptor",
		"DiscretionaryAcl.InsertAce",
		"SeRestorePrivilege",
		"SeBackupPrivilege",
		"CreateFile(path, WriteDac",
		"CreateFile(path, ReadControl",
		"OpenReparsePoint | BackupSemantics",
		"GetOwnerAndDacl",
		"Get-SecurityDescriptorWithoutFollowing",
		"SetKernelObjectSecurity",
		"RestoreTokenPrivileges",
		"-AllowReparsePoint",
		"rollback sem raiz logica autorizada",
		"$logicalRoot = $rollbackBase",
		"Set-DaclWithoutPropagation",
		"StringComparer]::Ordinal",
		"expectedRuleCounts",
		"ACE administrativa existente e insuficiente",
		"reparo alterou conjunto de ACEs",
		"Get-RunnerPathTasks",
		"task orfa aponta para dir/staging do gate",
		"processo ativo no staging durante resume",
		"service inesperado aponta para staging",
		"staging sem arquivo obrigatorio",
		"$newConfig.agentId -ne $remote.id",
		"$remote.os -ne 'Windows'",
		"Where-Object { $_.type -eq 'custom' }",
		"falha restaurando rollback",
		"Ensure-ProtectedSharedRoot",
		"Assert-ProtectedAcl -Path $Root",
	} {
		if !strings.Contains(source, required) {
			t.Errorf("gate runner clean reprovision is missing %q", required)
		}
	}
	protect := strings.Index(source, "Set-ProtectedAcl -Path $stage")
	download := strings.Index(source, "Invoke-WebRequest")
	rootPreflight := strings.Index(source, "foreach ($existingPath in @($Root, $dir, $stage, $rollback))")
	rootACLPreflight := strings.Index(source, "if (Test-Path -LiteralPath $Root) {\n    Assert-ProtectedAcl -Path $Root")
	quarantine := strings.Index(source, "Quarantine-RemoteRunner\n")
	disable := strings.Index(source, "Disable-ScheduledTask -TaskName $task")
	lastListenerStop := strings.LastIndex(source, "\nStop-IdleListener\n")
	adminHandoff := strings.Index(source, "-AllowedRunnerRoots $allowedRunnerRoots -EnsureAdminTraversal")
	mainSwap := strings.LastIndex(source, "\nMove-StagedRunner\n")
	moveFunction := strings.Index(source, "function Move-StagedRunner")
	oldTreeRevalidation := -1
	oldTreeMove := -1
	if moveFunction >= 0 {
		oldTreeRevalidation = strings.Index(source[moveFunction:], "Protect-AdminTree -Path $dir")
		oldTreeMove = strings.Index(source[moveFunction:], "Move-Item -LiteralPath $dir -Destination $rollback")
	}
	if protect < 0 || download < 0 || protect > download {
		t.Error("gate staging DACL must be protected before download")
	}
	if quarantine < 0 || disable < 0 || quarantine > disable {
		t.Error("remote gate admission must be quarantined before local lifecycle mutation")
	}
	if rootPreflight < 0 || rootACLPreflight < 0 || adminHandoff < 0 || mainSwap < 0 ||
		moveFunction < 0 || oldTreeRevalidation < 0 || oldTreeMove < 0 ||
		lastListenerStop < 0 ||
		rootPreflight > rootACLPreflight || rootACLPreflight > quarantine ||
		quarantine > adminHandoff ||
		lastListenerStop > adminHandoff || adminHandoff > mainSwap ||
		oldTreeRevalidation > oldTreeMove {
		t.Error("old tree must regain safe admin traversal after drain and before revalidation")
	}
	if strings.Contains(source, "Get-Content -LiteralPath $servicePath") {
		t.Error("legacy service discovery must not read an inaccessible runner tree")
	}
	if strings.Contains(source, "labels/$") {
		t.Error("custom labels must use the bulk endpoint without path data")
	}
	if strings.Contains(source, "gh.exe") {
		t.Error("gate provisioning must use the in-memory token directly, not ambient gh auth")
	}
}

func TestGateRunnerScriptsAllowOnlyPinnedOfficialJunctions(t *testing.T) {
	paths := []string{
		"civm-gate-runner-provision.ps1",
		"civm-gate-task-setup.ps1",
	}
	for _, name := range paths {
		path := filepath.Join("..", "..", "deploy", "windows", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		source := string(data)
		for _, required := range []string{
			"Assert-SafeOfficialRunnerJunction",
			"$Item.LinkType -ne 'Junction'",
			"$Item.Name -notin @('bin', 'externals')",
			"$targets.Count -ne 1",
			"[string]$item.LinkType",
			"link de filesystem proibido",
			"[string[]]$AllowedRunnerRoots",
			"$Item.Name).$ExpectedRunnerVersion",
			"junction oficial fora da raiz top-level do runner",
			"junction oficial fora do alvo pinado",
			"$physicalTarget = [System.IO.Path]::GetFullPath",
			"Get-Item -LiteralPath $physicalTarget",
			"alvo fisico da junction nao e sibling real",
			"$items.Add($item)\n            continue",
			"Get-FileLinkCount",
			"GetFileInformationByHandle",
			"NumberOfLinks",
			"SeBackupPrivilege",
			"RestoreTokenPrivileges",
			"MetadataOnly = 0",
		} {
			if !strings.Contains(source, required) {
				t.Errorf("%s is missing pinned junction guard %q", name, required)
			}
		}
		linkCount := strings.Index(source, "Get-FileLinkCount -Path $item.FullName")
		itemAdd := strings.LastIndex(source, "$items.Add($item)")
		if linkCount < 0 || itemAdd < 0 || linkCount > itemAdd {
			t.Errorf("%s must prove native link count before accepting a file", name)
		}
	}
}

func TestGateRunnerScriptsShareNativeLinkInspector(t *testing.T) {
	block := regexp.MustCompile(`(?s)Add-Type -TypeDefinition @'\n(.*?)\n'@`)
	var expected string
	for _, name := range []string{
		"civm-gate-runner-provision.ps1",
		"civm-gate-task-setup.ps1",
	} {
		path := filepath.Join("..", "..", "deploy", "windows", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		matches := block.FindAllStringSubmatch(string(data), -1)
		if len(matches) != 1 || len(matches[0]) != 2 {
			t.Fatalf("%s does not contain exactly one native helper block", name)
		}
		match := matches[0]
		if expected == "" {
			expected = match[1]
			continue
		}
		if match[1] != expected {
			t.Errorf("%s native hardlink helper drifted from provision", name)
		}
	}
}
