package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/hostdisk"
	"github.com/emersonbusson/civm/internal/idle"
	"github.com/emersonbusson/civm/internal/safedelete"
)

func TestAppendLogIncludesWorkRootIdentifier(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "hooks.jsonl")
	opts := Options{Execute: true, LogPath: logPath, MkdirAllFn: os.MkdirAll}
	res := Result{
		Event:    EventJobCompleted,
		Decision: DecisionError,
		WorkRoot: "/home/emdev/actions-runner-acme-org/_work",
		Actions:  []Action{{Name: "work_root", Error: "wrapper rm failed: boom"}},
	}
	if err := appendLog(opts, res); err != nil {
		t.Fatalf("appendLog: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	// The shared hooks.jsonl must carry the runner identity so the watchdog can
	// map a broken-runner sentinel to the right unit (RF-6 / ITEM-10 #74).
	if !strings.Contains(string(data), `"work_root":"/home/emdev/actions-runner-acme-org/_work"`) {
		t.Fatalf("hook record missing work_root runner identifier:\n%s", data)
	}
}

// TestAppendLogEmitsHostVFreeForDrainMeasurement prova que o hooks.jsonl carrega
// host_v_free_gb + host_level. São o ÚNICO traço persistido do V: livre do host
// por job; sem esses campos, o dreno por job (high-water = vfree@started −
// vfree@completed) não seria reconstruível a partir do log — só estimável
// (Kahneman #3). Antes deste fix os campos existiam no Result mas appendLog os
// DESCARTAVA.
func TestAppendLogEmitsHostVFreeForDrainMeasurement(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "hooks.jsonl")
	opts := Options{Execute: true, LogPath: logPath, MkdirAllFn: os.MkdirAll}
	res := Result{
		Event:       EventJobStarted,
		Decision:    DecisionOK,
		HostLevel:   "ok",
		HostVFreeGB: 54,
	}
	if err := appendLog(opts, res); err != nil {
		t.Fatalf("appendLog: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
		t.Fatalf("parse log record: %v\n%s", err, data)
	}
	if v, ok := rec["host_v_free_gb"]; !ok || int64(v.(float64)) != 54 {
		t.Fatalf("hook record missing measured host_v_free_gb=54: %v\n%s", rec["host_v_free_gb"], data)
	}
	if rec["host_level"] != "ok" {
		t.Fatalf("hook record missing host_level=ok: %v\n%s", rec["host_level"], data)
	}
}

// TestJobCompletedReadsHostVFreeForDrainCloseout prova que o job-completed lê o V:
// livre do host (não só o job-started). É o extremo de FECHAMENTO do dreno: sem
// ele, só o início do consumo de V: estaria medido e o high-water ficaria aberto.
func TestJobCompletedReadsHostVFreeForDrainCloseout(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")
	opts := DefaultOptionsFromEnv(EventJobCompleted)
	opts.Execute = true
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 60, nil }
	opts.RemoveAllFn = func(string) error { return nil }
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner-civm/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	// Snapshot do host com 36GB livres no fim do job; pareado a um job-started de
	// 54GB, o dreno medido seria 18GB. Aqui só afirmamos o extremo de fechamento.
	opts.HostDiskFn = func() (hostdisk.Report, error) {
		return hostdisk.Report{Metrics: hostdisk.Metrics{VFreeGB: 36}, Level: "ok"}, nil
	}
	res := Run(context.Background(), opts)
	if res.HostVFreeGB != 36 {
		t.Fatalf("job-completed não capturou o V: livre de fechamento: got %d, want 36", res.HostVFreeGB)
	}
	if res.HostLevel != "ok" {
		t.Fatalf("job-completed não propagou host_level: got %q", res.HostLevel)
	}
}

// TestJobVDrainGB cobre a definição canônica do dreno medido. Cada caso de recusa
// (medição não-confiável -> ok=false) é pareado com um positivo (Kahneman #13:
// existência != função — um par com extremo <=0 jamais vira dreno fantasma).
func TestJobVDrainGB(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name           string
		started, compl int64
		wantDrain      int64
		wantOK         bool
	}{
		{"dreno normal medido", 54, 36, 18, true},
		{"dreno zero (sem consumo líquido)", 51, 51, 0, true},
		{"V: subiu no meio (warn/compact) -> clamp 0", 30, 45, 0, true},
		{"started não medido (<=0) -> não confiável", 0, 36, 0, false},
		{"completed não medido (<=0) -> não confiável", 54, 0, 0, false},
		{"ambos não medidos -> não confiável", 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			drain, ok := JobVDrainGB(tc.started, tc.compl)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if drain != tc.wantDrain {
				t.Fatalf("drain = %d, want %d", drain, tc.wantDrain)
			}
		})
	}
}

func TestJobCompletedCleansWorkspaceButPreservesHotCaches(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// DefaultOptionsFromEnv lê RUNNER_TEMP/GITHUB_WORKSPACE/etc. do ambiente;
	// quando este teste roda no runner self-hosted do próprio civm, esses
	// vars apontam para um work root REAL fora do mock — limpa para isolar.
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")
	var removed []string
	var commands []string
	opts := DefaultOptionsFromEnv(EventJobCompleted)
	opts.Execute = true
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 60, nil }
	opts.RemoveAllFn = func(path string) error { removed = append(removed, path); return nil }
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	// Path real sob /home/ é exigido por safeWorkRoot; o conteúdo do dir é
	// simulado via ReadDirFn — não tocamos o filesystem.
	workRoot := "/home/civm-test/actions-runner-civm/_work"
	opts.WorkRoot = workRoot
	opts.ReadDirFn = func(path string) ([]os.DirEntry, error) {
		if path != workRoot {
			return nil, fmt.Errorf("unexpected ReadDir path: %s", path)
		}
		return []os.DirEntry{
			fakeDirEntry("_tool"),
			fakeDirEntry("_actions"),
			fakeDirEntry("_temp"),
			fakeDirEntry("repo"),
		}, nil
	}
	res := Run(context.Background(), opts)
	if res.Decision != DecisionCleanupApplied || res.ExitCode != 0 {
		t.Fatalf("res=%+v", res)
	}
	joined := strings.Join(removed, "\n")
	if strings.Contains(joined, "_tool") || strings.Contains(joined, "_actions") {
		t.Fatalf("removed hot cache: %v", removed)
	}
	if !strings.Contains(joined, "_temp") || !strings.Contains(joined, "repo") {
		t.Fatalf("missing workspace removals: %v", removed)
	}
	if len(commands) == 0 {
		t.Fatal("expected maintenance commands")
	}
}

type fakeEntry string

func (f fakeEntry) Name() string             { return string(f) }
func (fakeEntry) IsDir() bool                { return true }
func (fakeEntry) Type() os.FileMode          { return os.ModeDir }
func (fakeEntry) Info() (os.FileInfo, error) { return nil, nil }

func fakeDirEntry(name string) os.DirEntry { return fakeEntry(name) }

// TestJobCompletedPurgesHotCachesWhenIdle reproduz os 6,5 GB de caches quentes
// que sobreviveram ao boundary e reduziram o headroom do E2E seguinte. Sem
// sibling ativo, esses caches regeneraveis devem ser efemeros como no CI pago.
func TestJobCompletedPurgesHotCachesWhenIdle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")

	// Cria dirs concretos para os caches sob $HOME — se a função tentar
	// removê-los, vamos detectar via RemoveAllFn captura.
	cachePathsUnderHome := []string{
		filepath.Join(home, ".cache", "go-build"),
		filepath.Join(home, ".cache", "ms-playwright"),
		filepath.Join(home, ".npm", "_cacache"),
		filepath.Join(home, ".yarn", "cache"),
		filepath.Join(home, ".pnpm-store"),
	}
	for _, cache := range cachePathsUnderHome {
		if err := os.MkdirAll(cache, 0o750); err != nil {
			t.Fatal(err)
		}
	}

	var removed []string
	opts := DefaultOptionsFromEnv(EventJobCompleted)
	opts.Execute = true
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 60, nil }
	opts.RemoveAllFn = func(p string) error { removed = append(removed, p); return nil }
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	// Hermetico: sem isso, DefaultOptionsFromEnv usa o ps scan real e na box da
	// CI (outros runners buildando) o cache trim defere -> o teste do trim falha.
	// ActivityFn idle (nenhuma atividade) força o cenario idle.
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }

	res := Run(context.Background(), opts)
	if res.Decision != DecisionCleanupApplied {
		t.Fatalf("decision=%v, want cleanup-applied", res.Decision)
	}
	for _, cache := range cachePathsUnderHome {
		found := false
		for _, got := range removed {
			if got == cache {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("job-completed idle nao removeu cache quente %s", cache)
		}
	}
}

func TestJobCompletedPreservesHotCachesWithSibling(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")
	cache := filepath.Join(home, ".cache", "go-build")
	if err := os.MkdirAll(cache, 0o750); err != nil {
		t.Fatal(err)
	}

	var removed []string
	opts := DefaultOptionsFromEnv(EventJobCompleted)
	opts.Execute = true
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 60, nil }
	opts.RemoveAllFn = func(p string) error { removed = append(removed, p); return nil }
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		return []idle.Activity{{
			PID:     999,
			Command: "go test /home/civm-test/actions-runner-sibling/_work/acme/app",
		}}, nil
	}

	res := Run(context.Background(), opts)
	for _, got := range removed {
		if got == cache {
			t.Fatalf("job-completed removeu cache com sibling ativo: %s", got)
		}
	}
	foundDeferred := false
	for _, action := range res.Actions {
		if action.Name == "cache_trim_deferred_sibling_build" {
			foundDeferred = true
		}
	}
	if !foundDeferred {
		t.Fatalf("esperado defer observavel com sibling ativo: %+v", res.Actions)
	}
}

// TestJobStartedUnderPressureTrimsCachesByAge valida que, sob disk pressure,
// job-started faz trim por idade dos caches ($HOME) em vez de wipe total. O
// wipe total apagava o go-build cache quente de um job concorrente em
// compilação ("could not import ...: no such file or directory").
func TestJobStartedUnderPressureTrimsCachesByAge(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")

	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = true
	opts.PreCleanupPct = 50
	opts.HardFailPct = 95
	// 80% disco usado, acima de PreCleanupPct → cleanup roda.
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 20, nil }
	opts.RemoveAllFn = func(string) error { return nil }
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	// Hermetico: sem isso, DefaultOptionsFromEnv usa o ps scan real e na box da
	// CI (outros runners buildando) o cache trim defere -> o teste do trim falha.
	// ActivityFn idle (nenhuma atividade) força o cenario idle.
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }

	res := Run(context.Background(), opts)

	var hasTrim, hasWholesalePurge bool
	for _, a := range res.Actions {
		switch a.Name {
		case "cache_trim":
			hasTrim = true
		case "cache":
			hasWholesalePurge = true
		}
	}
	if !hasTrim {
		t.Errorf("job-started under pressure should age-trim caches; actions=%+v", res.Actions)
	}
	if hasWholesalePurge {
		t.Errorf("job-started must not wholesale-purge shared caches (races concurrent builds); actions=%+v", res.Actions)
	}
}

// TestJobStartedPreservesActiveWorkspaceUnderDiskPressure valida que, sob disk
// pressure em job-started, o cleanup NÃO apaga o GITHUB_WORKSPACE que o runner
// acabou de criar para o job que está começando — senão o job falha com
// "working directory ... No such file or directory" — NEM o _temp, onde o
// runner já criou os file commands (save_state_*/set_output) do job ativo:
// apagá-lo mata o actions/checkout com "Missing file at path ... save_state"
// (civm#117 smoke, 2026-06-10). Entradas stale de outros repos seguem limpas
// e o _temp segue limpo no job-completed.
func TestJobStartedPreservesActiveWorkspaceUnderDiskPressure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RUNNER_TEMP", "")
	workRoot := "/home/civm-test/actions-runner-acme/_work"
	t.Setenv("GITHUB_WORKSPACE", workRoot+"/acme/app")
	t.Setenv("GITHUB_REPOSITORY", "acme/app")
	t.Setenv("GITHUB_RUN_ID", "1")

	var removed []string
	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = true
	opts.PreCleanupPct = 50
	opts.HardFailPct = 95
	// 80% usado, acima de PreCleanupPct → disk pressure cleanup.
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 20, nil }
	opts.RemoveAllFn = func(p string) error { removed = append(removed, p); return nil }
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = workRoot
	opts.ReadDirFn = func(path string) ([]os.DirEntry, error) {
		if path != workRoot {
			return nil, fmt.Errorf("unexpected ReadDir path: %s", path)
		}
		return []os.DirEntry{
			fakeDirEntry("_tool"),
			fakeDirEntry("_actions"),
			fakeDirEntry("_temp"),
			fakeDirEntry("acme"),
			fakeDirEntry("stale-repo"),
		}, nil
	}

	Run(context.Background(), opts)

	joined := strings.Join(removed, "\n")
	if strings.Contains(joined, filepath.Join(workRoot, "acme")) {
		t.Fatalf("disk-pressure cleanup apagou o workspace ativo %q — quebra o job iniciando; removed=%v", filepath.Join(workRoot, "acme"), removed)
	}
	if strings.Contains(joined, filepath.Join(workRoot, "_temp")) {
		t.Fatalf("job-started apagou _temp do job ativo (file commands save_state/set_output); removed=%v", removed)
	}
	if !strings.Contains(joined, filepath.Join(workRoot, "stale-repo")) {
		t.Fatalf("esperava limpar checkout stale de outro repo; removed=%v", removed)
	}
	if strings.Contains(joined, filepath.Join(workRoot, "_tool")) || strings.Contains(joined, filepath.Join(workRoot, "_actions")) {
		t.Fatalf("removeu cache quente _tool/_actions; removed=%v", removed)
	}
}

func TestJobStartedDemotesCacheDeleteRaceWhenDiskDropsBelowHardFail(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")

	now := time.Now()
	statCalls := 0
	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = true
	opts.Now = now
	opts.PreCleanupPct = 70
	opts.HardFailPct = 90
	opts.StatfsFn = func(string) (uint64, uint64, error) {
		statCalls++
		if statCalls == 1 {
			return 100, 20, nil // 80% used, triggers pressure cleanup.
		}
		return 100, 35, nil // 65% used after cleanup, below hard fail.
	}
	// A large, old cache file the age-trim must remove, but the remove races a
	// concurrent writer ("directory not empty") — must demote to a warning.
	opts.WalkDirFn = walkCacheFiles([]cacheFile{
		{path: "/home/.cache/go-build/old", size: 8 * (int64(1) << 30), mtime: now.Add(-72 * time.Hour)},
	})
	opts.RemoveAllFn = func(p string) error {
		return fmt.Errorf("remove %s: directory not empty", p)
	}
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	// Hermetico: sem isso, DefaultOptionsFromEnv usa o ps scan real e na box da
	// CI (outros runners buildando) o cache trim defere -> o teste do trim falha.
	// ActivityFn idle (nenhuma atividade) força o cenario idle.
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }

	res := Run(context.Background(), opts)
	if res.Decision != DecisionCleanupApplied || res.ExitCode != 0 {
		t.Fatalf("res=%+v", res)
	}
	foundWarning := false
	for _, action := range res.Actions {
		if action.Warning != "" && strings.Contains(action.Warning, "directory not empty") {
			foundWarning = true
		}
		if action.Error != "" {
			t.Fatalf("cache race should be warning, got action error: %+v", action)
		}
	}
	if !foundWarning {
		t.Fatalf("missing cache race warning: %+v", res.Actions)
	}
}

func TestJobStartedRejectsWhenDiskStillTooHigh(t *testing.T) {
	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = false
	opts.PreCleanupPct = 70
	opts.HardFailPct = 90
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 5, nil }
	opts.LogPath = ""
	res := Run(context.Background(), opts)
	if res.Decision != DecisionRejected || res.ExitCode == 0 {
		t.Fatalf("res=%+v", res)
	}
}

func TestRunErrorsWhenStatfsFails(t *testing.T) {
	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 0, 0, fmt.Errorf("EIO") }
	opts.LogPath = ""
	res := Run(context.Background(), opts)
	if res.Decision != DecisionError || res.ExitCode == 0 || res.Error == "" {
		t.Fatalf("res=%+v", res)
	}
}

func TestRunErrorsOnUnsupportedEvent(t *testing.T) {
	opts := DefaultOptionsFromEnv(Event("bogus"))
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 50, nil }
	opts.LogPath = ""
	res := Run(context.Background(), opts)
	if res.Decision != DecisionError {
		t.Fatalf("expected error on unknown event, got %+v", res)
	}
}

func TestCleanWorkRootRejectsUnsafeRoot(t *testing.T) {
	a := cleanWorkRoot(context.Background(), Options{Execute: true}, "/etc/passwd", false)
	if a.Error == "" {
		t.Fatalf("expected unsafe error, got %+v", a)
	}
}

func TestCleanWorkRootHandlesReadDirError(t *testing.T) {
	opts := Options{
		Execute:   true,
		ReadDirFn: func(string) ([]os.DirEntry, error) { return nil, fmt.Errorf("EACCES") },
	}
	a := cleanWorkRoot(context.Background(), opts, "/home/x/actions-runner/_work", false)
	if a.Error == "" {
		t.Fatalf("expected ReadDir error to propagate, got %+v", a)
	}
}

func TestCleanWorkRootSkipsMissingDir(t *testing.T) {
	opts := Options{
		Execute:   true,
		ReadDirFn: func(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist },
	}
	a := cleanWorkRoot(context.Background(), opts, "/home/x/actions-runner/_work", false)
	if a.Error != "" {
		t.Fatalf("missing dir should be silent, got %+v", a)
	}
}

func TestCleanWorkRootPropagatesRemoveError(t *testing.T) {
	opts := Options{
		Execute: true,
		ReadDirFn: func(string) ([]os.DirEntry, error) {
			return []os.DirEntry{fakeDirEntry("repo")}, nil
		},
		RemoveAllFn: func(string) error { return fmt.Errorf("EBUSY") },
	}
	// A non-permission RemoveAll error (EBUSY) is NOT a root-owned-file case:
	// safedelete never escalates and surfaces it so job-completed stays fatal.
	opts.SafeWorkDeleteFn = newSafeWorkDelete(opts.RemoveAllFn)
	a := cleanWorkRoot(context.Background(), opts, "/home/x/actions-runner/_work", false)
	if a.Error == "" {
		t.Fatalf("expected RemoveAll error to propagate, got %+v", a)
	}
}

func TestCleanWorkRootRootOwnedEscalationKeepsRunnerUnwedged(t *testing.T) {
	// A root-owned leftover (a CI Docker step ran as root) escalates and is
	// reclaimed; cleanWorkRoot must report NO error so job-completed does not
	// wedge the runner at "Complete runner".
	var escalated []string
	opts := Options{
		Execute: true,
		ReadDirFn: func(string) ([]os.DirEntry, error) {
			return []os.DirEntry{fakeDirEntry("repo"), fakeDirEntry("_temp")}, nil
		},
		SafeWorkDeleteFn: func(_ context.Context, path string) safedelete.Result {
			escalated = append(escalated, path)
			return safedelete.Result{Escalated: true}
		},
	}
	a := cleanWorkRoot(context.Background(), opts, "/home/x/actions-runner/_work", false)
	if a.Error != "" {
		t.Fatalf("root-owned escalation must not surface an error, got %q", a.Error)
	}
	if len(escalated) != 2 {
		t.Fatalf("expected both entries reclaimed, got %v", escalated)
	}
}

func TestCleanWorkRootEscalationFailureStaysFatal(t *testing.T) {
	// If the escalation itself is unavailable (sudoers not installed), the
	// terminal error must surface so job-completed stays fatal — never silently
	// swallowed (DT-v2-12).
	opts := Options{
		Execute: true,
		ReadDirFn: func(string) ([]os.DirEntry, error) {
			return []os.DirEntry{fakeDirEntry("repo")}, nil
		},
		SafeWorkDeleteFn: func(context.Context, string) safedelete.Result {
			return safedelete.Result{Escalated: true, Err: fmt.Errorf("sudo: a password is required")}
		},
	}
	a := cleanWorkRoot(context.Background(), opts, "/home/x/actions-runner/_work", false)
	if a.Error == "" {
		t.Fatalf("escalation failure must surface as a fatal error, got %+v", a)
	}
}

func TestEnsureWorkspaceStubFromRepository(t *testing.T) {
	// Apos cleanWorkRoot o runner exige WorkingDirectory=_work/owner/repo
	// no Process.Start do hook. Stub vazio restaura o cwd sem checkout.
	var made []string
	opts := Options{
		Execute:    true,
		Repository: "acme/app",
		MkdirAllFn: func(path string, _ os.FileMode) error {
			made = append(made, path)
			return nil
		},
	}
	a := ensureWorkspaceStub(opts, "/home/x/actions-runner/_work")
	if a.Error != "" {
		t.Fatalf("unexpected error: %q", a.Error)
	}
	if a.Name != "workspace_stub" {
		t.Fatalf("name=%q want workspace_stub", a.Name)
	}
	want := "/home/x/actions-runner/_work/acme/app"
	if a.Path != want {
		t.Fatalf("path=%q want %q", a.Path, want)
	}
	if len(made) != 1 || made[0] != want {
		t.Fatalf("MkdirAll calls=%v want [%s]", made, want)
	}
}

func TestEnsureWorkspaceStubFromGitHubWorkspace(t *testing.T) {
	var made []string
	opts := Options{
		Execute:         true,
		GitHubWorkspace: "/home/x/actions-runner/_work/other/repo",
		MkdirAllFn: func(path string, _ os.FileMode) error {
			made = append(made, path)
			return nil
		},
	}
	a := ensureWorkspaceStub(opts, "/home/x/actions-runner/_work")
	if a.Error != "" {
		t.Fatalf("unexpected error: %q", a.Error)
	}
	want := "/home/x/actions-runner/_work/other/repo"
	if a.Path != want || len(made) != 1 || made[0] != want {
		t.Fatalf("path=%q made=%v want %s", a.Path, made, want)
	}
}

func TestEnsureWorkspaceStubDefaultsGeneric(t *testing.T) {
	// Full clean no guest nao deixa env de repo; fallback generico
	// cobre o org runner (sem GITHUB_REPOSITORY no boot).
	var made []string
	opts := Options{
		Execute: true,
		MkdirAllFn: func(path string, _ os.FileMode) error {
			made = append(made, path)
			return nil
		},
	}
	a := ensureWorkspaceStub(opts, "/home/x/actions-runner/_work")
	want := "/home/x/actions-runner/_work/workspace/job"
	if a.Path != want || len(made) != 1 || made[0] != want {
		t.Fatalf("path=%q made=%v want %s", a.Path, made, want)
	}
}

func TestEnsureWorkspaceStubDryRunSkipsMkdir(t *testing.T) {
	called := false
	opts := Options{
		Execute:    false,
		Repository: "acme/app",
		MkdirAllFn: func(string, os.FileMode) error {
			called = true
			return nil
		},
	}
	a := ensureWorkspaceStub(opts, "/home/x/actions-runner/_work")
	if called {
		t.Fatal("dry-run must not call MkdirAll")
	}
	if a.Executed {
		t.Fatal("Executed must be false in dry-run")
	}
}

func TestEnsureWorkspaceStubRejectsUnsafeRoot(t *testing.T) {
	a := ensureWorkspaceStub(Options{Execute: true, Repository: "a/b"}, "/etc")
	if a.Error == "" {
		t.Fatalf("expected unsafe root error, got %+v", a)
	}
}

func TestEnsureWorkspaceStubPropagatesMkdirError(t *testing.T) {
	opts := Options{
		Execute:    true,
		Repository: "acme/app",
		MkdirAllFn: func(string, os.FileMode) error { return fmt.Errorf("EACCES") },
	}
	a := ensureWorkspaceStub(opts, "/home/x/actions-runner/_work")
	if a.Error == "" {
		t.Fatalf("expected mkdir error, got %+v", a)
	}
}

func TestCleanupIncludesWorkspaceStubAfterClean(t *testing.T) {
	// cleanup() deve recriar o stub DEPOIS do wipe, senao o proximo job
	// falha no Process.Start do hook (cwd ausente).
	opts := Options{
		Execute:    true,
		Repository: "acme/app",
		WorkRoot:   "/home/x/actions-runner/_work",
		ReadDirFn:  func(string) ([]os.DirEntry, error) { return nil, os.ErrNotExist },
		MkdirAllFn: func(string, os.FileMode) error { return nil },
		SafeWorkDeleteFn: func(context.Context, string) safedelete.Result {
			return safedelete.Result{}
		},
		RunFn: func(context.Context, string, ...string) ([]byte, error) { return nil, nil },
		// Disco folgado: so exercita o path de cleanup de rotina (sem emergency).
		StatfsFn:   func(string) (uint64, uint64, error) { return 100 << 30, 80 << 30, nil },
		ActivityFn: func(context.Context) ([]idle.Activity, error) { return nil, nil },
		HostDiskFn: func() (hostdisk.Report, error) { return hostdisk.Report{}, nil },
	}
	actions := cleanup(opts, context.Background(), true)
	var stub *Action
	names := make([]string, 0, len(actions))
	for i := range actions {
		names = append(names, actions[i].Name)
		if actions[i].Name == "workspace_stub" {
			stub = &actions[i]
		}
	}
	if stub == nil {
		t.Fatalf("cleanup missing workspace_stub; got names: %v", names)
	}
	if stub.Path != "/home/x/actions-runner/_work/acme/app" {
		t.Fatalf("stub path=%q", stub.Path)
	}
}

func TestWorkChildGuardScopesEscalation(t *testing.T) {
	cases := []struct {
		name    string
		path    string
		wantErr bool
	}{
		{"direct child", "/home/x/actions-runner/_work/repo", false},
		{"the _work root itself", "/home/x/actions-runner/_work", true},
		{"outside any _work", "/etc/passwd", true},
		{"home dir", "/home/x", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := workChildGuard(tc.path)
			if tc.wantErr && err == nil {
				t.Fatalf("workChildGuard(%q) = nil, want error", tc.path)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("workChildGuard(%q) = %v, want nil", tc.path, err)
			}
		})
	}
}

func TestRemovePathBlocksUnsafe(t *testing.T) {
	for _, p := range []string{"", "  ", "/"} {
		a := removePath(Options{Execute: true}, p, "cache")
		if a.Error == "" {
			t.Fatalf("expected unsafe error for %q, got %+v", p, a)
		}
	}
}

func TestRemovePathPropagatesError(t *testing.T) {
	opts := Options{Execute: true, RemoveAllFn: func(string) error { return fmt.Errorf("ENOSPC") }}
	a := removePath(opts, "/tmp/cache", "cache")
	if a.Error == "" {
		t.Fatalf("expected RemoveAll error, got %+v", a)
	}
}

func TestRenderJSONHook(t *testing.T) {
	var buf bytes.Buffer
	if err := RenderJSON(&buf, Result{Event: EventJobStarted, Decision: DecisionOK}); err != nil {
		t.Fatal(err)
	}
	var parsed Result
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("json: %v", err)
	}
	if parsed.Event != EventJobStarted || parsed.Decision != DecisionOK {
		t.Fatalf("roundtrip: %+v", parsed)
	}
}

func TestRenderTextHookShowsActions(t *testing.T) {
	var buf bytes.Buffer
	RenderText(&buf, Result{
		Event:    EventJobCompleted,
		Decision: DecisionCleanupApplied,
		Error:    "oops",
		Actions: []Action{
			{Name: "ok-action", Path: "/x", Executed: true},
			{Name: "dry-action", Path: "/y", Executed: false},
			{Name: "fail-action", Path: "/z", Error: "boom"},
		},
	})
	out := buf.String()
	for _, want := range []string{"job-completed", "Error: oops", "ok-action", "dry-action", "fail-action", "boom"} {
		if !strings.Contains(out, want) {
			t.Fatalf("text missing %q:\n%s", want, out)
		}
	}
}

func TestAppendLogWritesSlogJSON(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hooks.jsonl")
	opts := Options{
		Execute:    true,
		LogPath:    logPath,
		MkdirAllFn: os.MkdirAll,
	}
	res := Result{
		Event: EventJobStarted, Decision: DecisionOK,
		Repository: "acme/civm", RunID: "12345",
		DiskUsedPct: 42,
	}
	if err := appendLog(opts, res); err != nil {
		t.Fatalf("appendLog: %v", err)
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	// slog.JSONHandler adds time/level/msg + attrs as flat keys.
	var rec map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, data)
	}
	for _, key := range []string{"time", "level", "msg", "event", "decision", "repository", "run_id", "disk_used_pct"} {
		if _, ok := rec[key]; !ok {
			t.Errorf("missing slog field %q in %v", key, rec)
		}
	}
	if rec["event"] != "job-started" || rec["msg"] != "hook event" || rec["level"] != "INFO" {
		t.Errorf("unexpected slog record: %v", rec)
	}
}

func TestAppendLogPromotesErrorLevel(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "hooks.jsonl")
	opts := Options{Execute: true, LogPath: logPath, MkdirAllFn: os.MkdirAll}
	cases := map[Decision]string{
		DecisionError:    "ERROR",
		DecisionRejected: "WARN",
		DecisionOK:       "INFO",
	}
	for dec, wantLevel := range cases {
		_ = os.Remove(logPath)
		if err := appendLog(opts, Result{Event: EventJobStarted, Decision: dec}); err != nil {
			t.Fatalf("appendLog(%s): %v", dec, err)
		}
		data, _ := os.ReadFile(logPath)
		var rec map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(data), &rec); err != nil {
			t.Fatalf("invalid JSON for %s: %v", dec, err)
		}
		if rec["level"] != wantLevel {
			t.Errorf("decision=%s level=%v, want %s", dec, rec["level"], wantLevel)
		}
	}
}

func TestAppendLogNoopWhenDisabled(t *testing.T) {
	if err := appendLog(Options{LogPath: ""}, Result{}); err != nil {
		t.Fatalf("empty path should be noop, got %v", err)
	}
	if err := appendLog(Options{LogPath: "/tmp/x", Execute: false}, Result{}); err != nil {
		t.Fatalf("dry-run should be noop, got %v", err)
	}
}

func TestSafeWorkRoot(t *testing.T) {
	// Legitimate roots MUST still pass — a refusal-only test could lock in an
	// over-tight guard that re-wedges every runner (testing.md: pair refusal
	// with its positive).
	for _, root := range []string{
		"/home/emdev/actions-runner-acme/_work",
		"/home/runner/actions-runner/_work",
		"//home//emdev//actions-runner//_work", // cleans to the canonical shape
	} {
		if !safeWorkRoot(root) {
			t.Fatalf("expected safe: %s", root)
		}
	}
	for _, root := range []string{
		"/",
		"/home/emdev",
		"/tmp/_work",
		"/home/emdev/actions-runner/_tool",
		// Substring decoys the old strings.Contains guard wrongly accepted but
		// the segment-aware glob match rejects (DT-v2-7): "actions-runner" is
		// not the runner-dir segment directly under /home/<user>.
		"/home/x/sub/actions-runner/_work",
		"/home/x/actions-runnerEVIL/deep/_work",
		"/home/x/actions-runner/_work/repo", // a child, not the root itself
	} {
		if safeWorkRoot(root) {
			t.Fatalf("expected unsafe: %s", root)
		}
	}
}

type cacheFile struct {
	path  string
	size  int64
	mtime time.Time
}

func walkCacheFiles(files []cacheFile) func(string, fs.WalkDirFunc) error {
	return func(root string, fn fs.WalkDirFunc) error {
		for _, f := range files {
			d := dirEntryFile{name: filepath.Base(f.path), size: f.size, mtime: f.mtime}
			if err := fn(f.path, d, nil); err != nil {
				return err
			}
		}
		return nil
	}
}

type dirEntryFile struct {
	name  string
	size  int64
	mtime time.Time
}

func (d dirEntryFile) Name() string               { return d.name }
func (dirEntryFile) IsDir() bool                  { return false }
func (dirEntryFile) Type() fs.FileMode            { return 0 }
func (d dirEntryFile) Info() (fs.FileInfo, error) { return fileInfoStub(d), nil }

type fileInfoStub dirEntryFile

func (f fileInfoStub) Name() string       { return f.name }
func (f fileInfoStub) Size() int64        { return f.size }
func (fileInfoStub) Mode() fs.FileMode    { return 0 }
func (f fileInfoStub) ModTime() time.Time { return f.mtime }
func (fileInfoStub) IsDir() bool          { return false }
func (fileInfoStub) Sys() any             { return nil }

func TestTrimCacheByAge(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	const KiB = int64(1024)
	tests := []struct {
		name       string
		files      []cacheFile
		maxBytes   int64
		minProtect time.Duration
		wantFound  int64
		wantFreed  int64
		wantKept   []string // file paths that must NOT be removed
		wantGone   []string // file paths that MUST be removed
	}{
		{
			name:       "empty cache is no-op",
			files:      nil,
			maxBytes:   10 * KiB,
			minProtect: time.Hour,
			wantFound:  0,
			wantFreed:  0,
		},
		{
			name: "under cap is no-op",
			files: []cacheFile{
				{path: "/cache/a", size: 2 * KiB, mtime: now.Add(-7 * 24 * time.Hour)},
				{path: "/cache/b", size: 3 * KiB, mtime: now.Add(-1 * time.Hour)},
			},
			maxBytes:   10 * KiB,
			minProtect: time.Hour,
			wantFound:  5 * KiB,
			wantFreed:  0,
			wantKept:   []string{"/cache/a", "/cache/b"},
		},
		{
			name: "over cap removes oldest first",
			files: []cacheFile{
				{path: "/cache/oldest", size: 4 * KiB, mtime: now.Add(-30 * 24 * time.Hour)},
				{path: "/cache/mid", size: 4 * KiB, mtime: now.Add(-7 * 24 * time.Hour)},
				{path: "/cache/new", size: 4 * KiB, mtime: now.Add(-2 * time.Hour)},
			},
			maxBytes:   8 * KiB,
			minProtect: time.Hour, // new is 2h old → not protected
			wantFound:  12 * KiB,
			wantFreed:  4 * KiB,
			wantGone:   []string{"/cache/oldest"},
			wantKept:   []string{"/cache/mid", "/cache/new"},
		},
		{
			// TETO HARD (incidente 2026-06-15): o cap e absoluto. Pass 1 trima o
			// cold; se ainda acima do cap, Pass 2 trima o hot mais ANTIGO ate caber,
			// preservando o hot mais novo. Antes do fix, hot1+hot2 ficavam e o dir
			// crescia sem limite (o cache de CI sob carga continua chegou a 18GB).
			name: "hard ceiling trims oldest hot when cold cannot meet the cap",
			files: []cacheFile{
				{path: "/cache/hot1", size: 4 * KiB, mtime: now.Add(-10 * time.Minute)},   // mais novo → fica
				{path: "/cache/hot2", size: 4 * KiB, mtime: now.Add(-30 * time.Minute)},   // mais antigo → Pass 2
				{path: "/cache/cold", size: 4 * KiB, mtime: now.Add(-7 * 24 * time.Hour)}, // → Pass 1
			},
			maxBytes:   6 * KiB, // total 12K; cold sozinho deixa 8K > 6K → Pass 2 trima o hot mais antigo
			minProtect: time.Hour,
			wantFound:  12 * KiB,
			wantFreed:  8 * KiB,
			wantGone:   []string{"/cache/cold", "/cache/hot2"},
			wantKept:   []string{"/cache/hot1"},
		},
		{
			name: "stops once target met",
			files: []cacheFile{
				{path: "/cache/a", size: 4 * KiB, mtime: now.Add(-30 * 24 * time.Hour)},
				{path: "/cache/b", size: 4 * KiB, mtime: now.Add(-20 * 24 * time.Hour)},
				{path: "/cache/c", size: 4 * KiB, mtime: now.Add(-10 * 24 * time.Hour)},
			},
			maxBytes:   9 * KiB, // need to free 3 KiB → only removes "a" (4 KiB)
			minProtect: time.Hour,
			wantFound:  12 * KiB,
			wantFreed:  4 * KiB,
			wantGone:   []string{"/cache/a"},
			wantKept:   []string{"/cache/b", "/cache/c"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var removed []string
			opts := Options{
				Execute:     true,
				Now:         now,
				WalkDirFn:   walkCacheFiles(tc.files),
				RemoveAllFn: func(p string) error { removed = append(removed, p); return nil },
			}
			a := trimCacheByAge(opts, "/cache", tc.maxBytes, tc.minProtect)
			if a.Error != "" {
				t.Fatalf("unexpected error: %s", a.Error)
			}
			if a.BytesFound != tc.wantFound {
				t.Errorf("BytesFound=%d, want %d", a.BytesFound, tc.wantFound)
			}
			if a.BytesFreed != tc.wantFreed {
				t.Errorf("BytesFreed=%d, want %d", a.BytesFreed, tc.wantFreed)
			}
			for _, want := range tc.wantGone {
				found := false
				for _, r := range removed {
					if r == want {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected %s to be removed, removed=%v", want, removed)
				}
			}
			for _, want := range tc.wantKept {
				for _, r := range removed {
					if r == want {
						t.Errorf("expected %s to be kept, but it was removed", want)
					}
				}
			}
		})
	}
}

func TestTrimCacheByAgeDryRunDoesNotRemove(t *testing.T) {
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	files := []cacheFile{
		{path: "/cache/old", size: 10 * 1024, mtime: now.Add(-30 * 24 * time.Hour)},
	}
	var removed []string
	opts := Options{
		Execute:     false,
		Now:         now,
		WalkDirFn:   walkCacheFiles(files),
		RemoveAllFn: func(p string) error { removed = append(removed, p); return nil },
	}
	a := trimCacheByAge(opts, "/cache", 1, time.Hour)
	if len(removed) != 0 {
		t.Fatalf("dry-run should not remove anything, got %v", removed)
	}
	// BytesFreed still accounts for what would be freed
	if a.BytesFreed == 0 {
		t.Errorf("dry-run BytesFreed should be > 0 (estimate), got 0")
	}
}

func TestTrimCacheByAgeRejectsUnsafePath(t *testing.T) {
	t.Setenv("HOME", "/home/example")
	for _, p := range []string{"", "  ", "/", "/home/example"} {
		a := trimCacheByAge(Options{Execute: true, Now: time.Now()}, p, 1024, time.Hour)
		if a.Error == "" {
			t.Errorf("expected unsafe path error for %q, got %+v", p, a)
		}
	}
}

func TestTrimCacheByAgeHandlesMissingCache(t *testing.T) {
	opts := Options{
		Execute: true,
		Now:     time.Now(),
		WalkDirFn: func(root string, fn fs.WalkDirFunc) error {
			// Mimic filepath.WalkDir when root is absent: returns the lstat error.
			return &fs.PathError{Op: "lstat", Path: root, Err: fs.ErrNotExist}
		},
	}
	a := trimCacheByAge(opts, "/cache/missing", 1024, time.Hour)
	if a.Error != "" {
		t.Errorf("missing cache should be silent, got %s", a.Error)
	}
	if a.BytesFound != 0 || a.BytesFreed != 0 {
		t.Errorf("missing cache should yield zero stats, got found=%d freed=%d", a.BytesFound, a.BytesFreed)
	}
}

func TestJobCompletedUsesGentleDockerSequence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")
	// Hermetico: o reapRunImages (#137) deriva o escopo de CIVM_RUNNER_SLOT /
	// COMPOSE_PROJECT_NAME (env do runner real na box da CI). Sem limpar os dois, na
	// box o reap roda `image prune -a -f --filter label=<scope>` (scoped, seguro) e a
	// checagem de "no -a" deste teste — que mira a sequencia gentle BASE — quebra.
	// O reap scoped tem cobertura propria (TestJobCompletedReapsOwnRunImages,
	// TestRunImageReapNeverUnscopedPruneAll); aqui isolamos so a base.
	t.Setenv("CIVM_RUNNER_SLOT", "")
	t.Setenv("COMPOSE_PROJECT_NAME", "")
	var commands []string
	opts := DefaultOptionsFromEnv(EventJobCompleted)
	opts.Execute = true
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 60, nil }
	opts.RemoveAllFn = func(string) error { return nil }
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	// Um sibling ativo força a sequência concorrente/gentle. O caminho idle tem
	// cobertura própria e pode usar prune global após o boundary.
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) {
		return []idle.Activity{{
			PID:     999,
			Command: "node /home/civm-test/actions-runner-sibling/_work/acme/app next build",
		}}, nil
	}

	Run(context.Background(), opts)

	joined := strings.Join(commands, "\n")
	for _, want := range []string{
		"docker buildx prune --force --filter until=24h",
		"docker image prune -f",
		"docker container prune -f",
		"docker volume prune -f",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("job-completed missing gentle docker step %q\nGot:\n%s", want, joined)
		}
	}
	// Dangling-only image prune: never `-a` (would delete a concurrent job's
	// pulled/built tagged images) and never aggressive system prune.
	if strings.Contains(joined, "docker image prune -a") {
		t.Errorf("image prune must be dangling-only (no -a), got:\n%s", joined)
	}
	if strings.Contains(joined, "docker system prune") {
		t.Errorf("job-completed should not run aggressive docker system prune, got:\n%s", joined)
	}
}

func TestJobCompletedDemotesCommandFailureToWarning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")

	opts := DefaultOptionsFromEnv(EventJobCompleted)
	opts.Execute = true
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 60, nil }
	opts.RemoveAllFn = func(string) error { return nil }
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		// Simula buildx ausente: docker buildx prune falha.
		if name == "docker" && len(args) > 0 && args[0] == "buildx" {
			return nil, fmt.Errorf(`exec: "docker buildx": executable file not found in $PATH`)
		}
		// Simula sudo sem NOPASSWD: fstrim falha.
		if name == "sudo" && len(args) > 0 && args[0] == "fstrim" {
			return nil, fmt.Errorf("sudo: a password is required")
		}
		return nil, nil
	}
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	// Hermetico: sem isso, DefaultOptionsFromEnv usa o ps scan real e na box da
	// CI (outros runners buildando) o cache trim defere -> o teste do trim falha.
	// ActivityFn idle (nenhuma atividade) força o cenario idle.
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }

	res := Run(context.Background(), opts)

	if res.Decision != DecisionCleanupApplied {
		t.Fatalf("decision = %v, want cleanup-applied (warnings shouldn't change decision)", res.Decision)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (warnings shouldn't fail the hook)", res.ExitCode)
	}
	var sawBuildxWarn, sawFstrimWarn bool
	for _, a := range res.Actions {
		if a.Error != "" {
			t.Errorf("routine action %s should have used Warning, got Error=%s", a.Name, a.Error)
		}
		if a.Name == "docker_buildx_prune" && a.Warning != "" {
			sawBuildxWarn = true
		}
		if a.Name == "fstrim" && a.Warning != "" {
			sawFstrimWarn = true
		}
	}
	if !sawBuildxWarn {
		t.Errorf("expected docker_buildx_prune Warning, actions=%+v", res.Actions)
	}
	if !sawFstrimWarn {
		t.Errorf("expected fstrim Warning, actions=%+v", res.Actions)
	}
}

func TestJobStartedCleanupCommandFailureIsNonFatal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")

	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = true
	opts.PreCleanupPct = 50
	opts.HardFailPct = 95
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 20, nil }
	opts.RemoveAllFn = func(string) error { return nil }
	// All job-started cleanup is best-effort. apt-get clean returns exit 100
	// when a sibling job holds the dpkg/apt lock; a fatal cleanup error must
	// not reject the starting job. Only HardFailPct (genuinely full disk)
	// fails closed at job-started.
	opts.RunFn = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "sudo" {
			return nil, fmt.Errorf("apt-get clean failed")
		}
		return nil, nil
	}
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	// Hermetico: sem isso, DefaultOptionsFromEnv usa o ps scan real e na box da
	// CI (outros runners buildando) o cache trim defere -> o teste do trim falha.
	// ActivityFn idle (nenhuma atividade) força o cenario idle.
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }

	res := Run(context.Background(), opts)

	if res.Decision == DecisionError {
		t.Fatalf("job-started cleanup command failure must not be fatal, actions=%+v", res.Actions)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d, want 0 (cleanup warnings must not fail the hook)", res.ExitCode)
	}
	var sawAptWarn bool
	for _, a := range res.Actions {
		if a.Error != "" {
			t.Errorf("job-started cleanup action %s should warn, got Error=%s", a.Name, a.Error)
		}
		if a.Name == "apt_clean" && a.Warning != "" {
			sawAptWarn = true
		}
	}
	if !sawAptWarn {
		t.Errorf("expected apt_clean Warning, actions=%+v", res.Actions)
	}
}

func TestRunWithTimeoutCancelsHungCommand(t *testing.T) {
	originalTimeout := civm.DefaultRoutineCleanupCmdTimeoutSecs
	_ = originalTimeout
	// Mock RunFn that respects ctx and hangs forever otherwise.
	hung := make(chan struct{})
	defer close(hung)
	opts := Options{
		Execute: true,
		RunFn: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-hung:
				return nil, nil
			}
		},
	}
	// Wrap with a parent ctx that has a short deadline so the test doesn't
	// wait the full DefaultRoutineCleanupCmdTimeoutSecs (which is 120s).
	parent, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	a := commandActionWarn(opts, parent, "hung_cmd", "hang")
	elapsed := time.Since(start)
	if elapsed > 2*time.Second {
		t.Fatalf("commandActionWarn took %v, expected to honor parent ctx deadline", elapsed)
	}
	if a.Warning == "" {
		t.Errorf("hung command should produce Warning, got %+v", a)
	}
	if a.Error != "" {
		t.Errorf("hung command in warn mode should not produce Error, got %+v", a)
	}
}

// TestCacheCapsGlobsNamedDirsAndDividesFamilyBudget é a regressão do incidente
// 2026-06 (VHDX -> PausedCritical): os workflows do acme apontam GOCACHE/yarn
// cache-folder para dirs NOMEADOS (~/.cache/go-build-acme-services, ...) que o
// cap antigo (path fixo ~/.cache/go-build) NÃO casava — então cresciam sem
// limite (go-build-acme-services chegou a 13GB num cap de 5GB). cacheCaps agora
// faz glob das variantes e divide o budget da família entre os dirs achados.
func TestCacheCapsGlobsNamedDirsAndDividesFamilyBudget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	mk := func(parts ...string) string {
		p := filepath.Join(append([]string{home}, parts...)...)
		if err := os.MkdirAll(p, 0o750); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// dois dirs go-build nomeados (dividem o budget), um yarn nomeado, lint, npm, pnpm.
	gb1 := mk(".cache", "go-build-acme-services")
	gb2 := mk(".cache", "go-build-acme-devctl")
	yarnNamed := mk(".cache", "yarn-acme-web")
	lint := mk(".cache", "golangci-lint")
	npm := mk(".npm", "_cacache")
	pnpm := mk(".pnpm-store")

	caps := cacheCaps()
	byPath := make(map[string]cacheCap, len(caps))
	for _, c := range caps {
		byPath[c.path] = c
	}
	// Regressão: TODO dir nomeado deve estar coberto (antes eram invisíveis ao trim).
	for _, p := range []string{gb1, gb2, yarnNamed, lint, npm, pnpm} {
		if _, ok := byPath[p]; !ok {
			t.Errorf("cacheCaps() não cobre %s — dir nomeado ficaria sem trim (o bug do VHDX 13GB)", p)
		}
	}
	// Budget da família dividido: 2 dirs go-build => cada um recebe family/2.
	const giB = int64(1) << 30
	wantPerGoBuild := int64(civm.DefaultCacheGoBuildMaxGB) * giB / 2
	if got := byPath[gb1].maxBytes; got != wantPerGoBuild {
		t.Errorf("go-build per-dir cap=%d, want family/2=%d", got, wantPerGoBuild)
	}
	wantProtect := time.Duration(civm.DefaultCacheTrimMinProtectHours) * time.Hour
	for _, c := range caps {
		if c.minProtect != wantProtect {
			t.Errorf("cap %s minProtect=%v, want %v", c.path, c.minProtect, wantProtect)
		}
		if c.maxBytes <= 0 {
			t.Errorf("cap %s has non-positive maxBytes %d", c.path, c.maxBytes)
		}
	}
	// cachePaths must be derived from caps — one source of truth.
	paths := cachePaths()
	if len(paths) != len(caps) {
		t.Fatalf("cachePaths len=%d, caps len=%d — must match", len(paths), len(caps))
	}
	for i, p := range paths {
		if p != caps[i].path {
			t.Errorf("cachePaths[%d]=%s, caps[%d].path=%s — must derive", i, p, i, caps[i].path)
		}
	}
}

func TestJobStartedUnderPressurePrunesAllBuildCache(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")
	var commands []string
	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = true
	opts.PreCleanupPct = 50
	opts.HardFailPct = 95
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 20, nil }
	opts.RemoveAllFn = func(string) error { return nil }
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		commands = append(commands, name+" "+strings.Join(args, " "))
		return nil, nil
	}
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	// Hermetico: sem isso, DefaultOptionsFromEnv usa o ps scan real e na box da
	// CI (outros runners buildando) o cache trim defere -> o teste do trim falha.
	// ActivityFn idle (nenhuma atividade) força o cenario idle.
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }

	Run(context.Background(), opts)

	joined := strings.Join(commands, "\n")
	// Sob pressão real (purgeCaches=true) reclamamos TODO o build cache não-usado:
	// o cache de hoje (<24h) é o que encheu a box a 95% mid-build, e o filtro
	// until=24h o deixava intacto. buildx prune --all é concurrency-safe (o
	// BuildKit exclui o cache em uso por um build ativo) — só sacrifica cache-hit.
	if !strings.Contains(joined, "docker buildx prune --force --all") {
		t.Errorf("job-started under pressure must prune ALL build cache (--all), got:\n%s", joined)
	}
	// O until=24h é o modo ROTINEIRO (job-completed); sob pressão ele deixaria o
	// cache de hoje e a box seguiria cruzando o HardFail.
	if strings.Contains(joined, "until=24h") {
		t.Errorf("job-started under pressure must NOT use the until=24h filter, got:\n%s", joined)
	}
	// Unfiltered `docker system prune --volumes` is forbidden: it GCs content a
	// sibling job is actively pulling and can be OOM-killed.
	if strings.Contains(joined, "docker system prune") {
		t.Errorf("job-started must not run unfiltered docker system prune, got:\n%s", joined)
	}
	// Image prune must be dangling-only: `-a` removes a concurrent job's
	// recently-pulled-but-old-vendor-dated tagged images (redis, minio, alpine,
	// clamav, postgres base) mid `compose up --build`, which then fails with
	// "No such image".
	if !strings.Contains(joined, "docker image prune -f") {
		t.Errorf("job-started should run dangling-only docker image prune, got:\n%s", joined)
	}
	if strings.Contains(joined, "docker image prune -a") {
		t.Errorf("job-started image prune must not use -a (deletes sibling images), got:\n%s", joined)
	}
}

// TestJobStartedDockerPruneErrorIsNonFatal proves the regression that broke CI:
// a docker prune that errors or is killed at job-started ("signal: killed")
// must NOT fail the hook and reject the starting job. Docker maintenance is
// best-effort; HardFailPct is the only gate that rejects a job for disk.
func TestJobStartedDockerPruneErrorIsNonFatal(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")
	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = true
	opts.PreCleanupPct = 50
	opts.HardFailPct = 95
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 20, nil }
	opts.RemoveAllFn = func(string) error { return nil }
	opts.RunFn = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		if name == "docker" {
			return nil, fmt.Errorf("signal: killed")
		}
		return nil, nil
	}
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	// Hermetico: sem isso, DefaultOptionsFromEnv usa o ps scan real e na box da
	// CI (outros runners buildando) o cache trim defere -> o teste do trim falha.
	// ActivityFn idle (nenhuma atividade) força o cenario idle.
	opts.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }

	res := Run(context.Background(), opts)
	if res.Decision == DecisionError || res.ExitCode != 0 {
		t.Fatalf("docker prune error at job-started must not fail the hook, got %+v", res)
	}
}

// TestJobStartedHostAwareGate prova a metade host-aware do gate (alavanca 3 do
// fix de footprint): com o guest CONFORTÁVEL (30% < PreCleanupPct), a pressão do
// volume V: do host ainda dispara cleanup (warn) e rejeita o job (crit fresco) —
// exatamente o cenário do incidente 2026-06 (guest 69%, host V: 88%) que o gate
// guest-% sozinho não pegava. Snapshot stale força cleanup mas nunca bloqueia.
func TestJobStartedMinFreeGBFloor(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")
	const giB = uint64(1) << 30
	base := func(freeGB uint64) Options {
		o := DefaultOptionsFromEnv(EventJobStarted) // MinFreeGB=58 (produção)
		o.Execute = true
		o.PreCleanupPct = 99 // guest-% NÃO dispara; isola o piso MinFreeGB
		o.HardFailPct = 100
		o.StatfsFn = func(string) (uint64, uint64, error) { return 100 * giB, freeGB * giB, nil }
		o.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
		o.RemoveAllFn = func(string) error { return nil }
		o.MkdirAllFn = func(string, os.FileMode) error { return nil }
		o.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
		o.WalkDirFn = func(string, fs.WalkDirFunc) error { return nil }
		o.ActivityFn = func(context.Context) ([]idle.Activity, error) { return nil, nil }
		o.LogPath = ""
		o.WorkRoot = "/home/civm-test/actions-runner/_work"
		o.HostDiskFn = func() (hostdisk.Report, error) {
			return hostdisk.Report{Metrics: hostdisk.Metrics{VFreeGB: 80}, Level: "ok"}, nil
		}
		return o
	}
	// free < MinFreeGB(58): full-clean dispara mesmo com guest-% e host ok (cada PR limpo).
	if res := Run(context.Background(), base(50)); res.Decision != DecisionCleanupApplied {
		t.Fatalf("free=50<58 deve disparar cleanup; decision=%v", res.Decision)
	}
	// free >= MinFreeGB(58): o piso NÃO dispara (disco já tem folga).
	if res := Run(context.Background(), base(60)); res.Decision == DecisionCleanupApplied {
		t.Fatalf("free=60>=58 não deve disparar cleanup por MinFreeGB; decision=%v", res.Decision)
	}
}

func TestJobStartedHostAwareGate(t *testing.T) {
	base := func() Options {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("RUNNER_TEMP", "")
		t.Setenv("GITHUB_WORKSPACE", "")
		t.Setenv("GITHUB_REPOSITORY", "")
		t.Setenv("GITHUB_RUN_ID", "")
		o := DefaultOptionsFromEnv(EventJobStarted)
		o.Execute = true
		o.PreCleanupPct = 60
		o.HardFailPct = 95
		o.MinFreeGB = 0 // isola o efeito do HOST: desliga o piso MinFreeGB (statfs fake em bytes)
		// guest 30% usado → o gate guest-% NÃO dispara; isola o efeito do host.
		o.StatfsFn = func(string) (uint64, uint64, error) { return 100, 70, nil }
		o.RemoveAllFn = func(string) error { return nil }
		o.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
		o.MkdirAllFn = func(string, os.FileMode) error { return nil }
		o.LogPath = ""
		o.WorkRoot = "/home/civm-test/actions-runner/_work"
		o.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
		return o
	}
	report := func(level string, vFree int64, stale bool) func() (hostdisk.Report, error) {
		return func() (hostdisk.Report, error) {
			return hostdisk.Report{Metrics: hostdisk.Metrics{VFreeGB: vFree}, Level: level, Stale: stale}, nil
		}
	}

	t.Run("host ok: guest confortável => sem cleanup, sem reject", func(t *testing.T) {
		o := base()
		o.HostDiskFn = report("ok", 66, false)
		res := Run(context.Background(), o)
		if res.Decision == DecisionCleanupApplied {
			t.Errorf("host ok + guest 30%% não deve rodar cleanup; decision=%v", res.Decision)
		}
		if res.Decision == DecisionRejected {
			t.Errorf("host ok não pode rejeitar")
		}
		if res.HostLevel != "ok" || res.HostVFreeGB != 66 {
			t.Errorf("campos host não propagados: level=%q vfree=%d", res.HostLevel, res.HostVFreeGB)
		}
	})

	t.Run("host warn: cleanup apesar do guest confortável", func(t *testing.T) {
		o := base()
		o.HostDiskFn = report("warn", 25, false)
		res := Run(context.Background(), o)
		if res.Decision != DecisionCleanupApplied {
			t.Errorf("host warn deve disparar cleanup mesmo com guest 30%%; got %v", res.Decision)
		}
		if res.ExitCode == 75 {
			t.Errorf("host warn não pode rejeitar")
		}
	})

	t.Run("host crit fresco: cleanup E reject", func(t *testing.T) {
		o := base()
		o.HostDiskFn = report("crit", 6, false)
		res := Run(context.Background(), o)
		if res.Decision != DecisionRejected {
			t.Errorf("host crit fresco deve rejeitar; got %v", res.Decision)
		}
		if res.ExitCode != 75 {
			t.Errorf("exit code do reject = %d, want 75", res.ExitCode)
		}
	})

	t.Run("host crit STALE: cleanup mas SEM reject (gap de infra != disco cheio)", func(t *testing.T) {
		o := base()
		o.HostDiskFn = report("crit", 0, true)
		res := Run(context.Background(), o)
		if res.Decision == DecisionRejected {
			t.Errorf("crit stale NÃO pode rejeitar (não auto-sabotar CI); err=%s", res.Error)
		}
		if res.Decision != DecisionCleanupApplied {
			t.Errorf("crit stale deve ainda disparar cleanup; got %v", res.Decision)
		}
	})
}

// TestJobStartedReclaimsWorkspaceOwnership proves the EACCES fix: at job-started
// the active reused checkout dir is chowned back to the runner (so a prior job's
// root-owned Docker leftover does not make actions/checkout die with EACCES). It
// runs UNCONDITIONALLY — here the disk is healthy (PreCleanupPct not met) and the
// host is ok, so NO disk-pressure cleanup runs, yet the chown still happens.
func TestJobStartedReclaimsWorkspaceOwnership(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")
	ws := "/home/emdev/actions-runner-acme/_work/acme/app"
	t.Setenv("GITHUB_WORKSPACE", ws)

	var chowned []string
	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = true
	opts.PreCleanupPct = 99 // healthy disk -> no gated cleanup
	opts.HardFailPct = 100
	opts.MinFreeGB = 0                                                           // este teste exercita ownership, não o piso MinFreeGB (statfs fake em bytes)
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 70, nil } // 30% used
	opts.RunFn = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.RemoveAllFn = func(string) error { return nil }
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	opts.LogPath = ""
	opts.HostDiskFn = func() (hostdisk.Report, error) {
		return hostdisk.Report{Metrics: hostdisk.Metrics{VFreeGB: 66}, Level: "ok"}, nil
	}
	opts.SafeWorkChownFn = func(_ context.Context, p string) safedelete.Result {
		chowned = append(chowned, p)
		return safedelete.Result{}
	}

	res := Run(context.Background(), opts)

	want := "/home/emdev/actions-runner-acme/_work/acme"
	found := false
	for _, p := range chowned {
		if p == want {
			found = true
		}
	}
	if !found {
		t.Errorf("active workspace entry %q not chowned; chowned=%v", want, chowned)
	}
	var hasAction bool
	for _, a := range res.Actions {
		if a.Name == "workspace_chown" {
			hasAction = true
		}
	}
	if !hasAction {
		t.Errorf("no workspace_chown action; actions=%+v", res.Actions)
	}
	if res.Decision == DecisionCleanupApplied {
		t.Errorf("no disk-pressure cleanup expected on a healthy disk; decision=%v", res.Decision)
	}
}

// TestIsCIOrphan cobre o predicado puro do reaper de órfão: project começando com
// o prefixo ("civm-run") OU publicando uma porta fixa de CI conhecida classifica
// como órfão. Prefixo por PREFIXO; "<no value>" (sem label) nunca casa.
func TestIsCIOrphan(t *testing.T) {
	tests := []struct {
		name      string
		project   string
		testc     string
		hostPorts []string
		want      bool
	}{
		{"project civm-run exato", "civm-run", "", nil, true},
		{"project civm-run-1184 (slot-runid)", "civm-run-1184", "", nil, true},
		{"project CIVM-RUN uppercase (case-insensitive)", "CIVM-RUN-Org-9", "", nil, true},
		{"testcontainers sem compose/porta", "<no value>", "true", nil, true},
		{"testcontainers uppercase", "<no value>", "TRUE", []string{"40000"}, true},
		{"porta fixa minio 9020 sem label", "<no value>", "", []string{"9020"}, true},
		{"porta fixa nginx 81 sem label", "", "", []string{"81"}, true},
		{"porta fixa entre outras não-fixas", "<no value>", "", []string{"40000", "9100", "40001"}, true},
		{"sem label e sem porta fixa => não órfão", "<no value>", "", []string{"40000"}, false},
		{"label vazio e porta vazia => não órfão", "", "", nil, false},
		{"project de OUTRO produto não casa o prefixo", "other-org-3", "", []string{"40000"}, false},
		{"prefixo só no meio do nome não casa", "my-civm-run-stack", "", []string{"40000"}, false},
		{"porta não-numérica é ignorada (não panica)", "<no value>", "", []string{"abc", "9020"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCIOrphan(tt.project, tt.testc, tt.hostPorts); got != tt.want {
				t.Errorf("isCIOrphan(%q, %q, %v) = %v, want %v", tt.project, tt.testc, tt.hostPorts, got, tt.want)
			}
		})
	}
}

// TestOrphanIDsFromInspect prova o parser puro da saída do `docker inspect`
// (formato "ID|projeto|portas"): só os IDs órfãos saem, linhas malformadas/vazias
// são puladas sem panicar, e a ordem de entrada é preservada.
func TestOrphanIDsFromInspect(t *testing.T) {
	out := strings.Join([]string{
		"aaa111|civm-run-1184|<no value>|9020 9021 ", // órfão por project
		"bbb222|<no value>|<no value>|81 ",           // órfão por porta fixa (nginx)
		"ccc333|<no value>|<no value>|40000 ",        // NÃO órfão (porta não-fixa, sem label)
		"ddd444|other-org-2|<no value>|40001 ",       // NÃO órfão (outro produto)
		"fff666|<no value>|true|",                    // órfão por testcontainers
		"",                                           // linha vazia → pulada
		"malformed-line-without-pipes",               // sem '|' → pulada
		"eee555|civm-run|<no value>|",                // órfão por project, sem portas
		"|civm-run-7|<no value>|9100 ",               // id vazio → pulada apesar do match
	}, "\n")
	got := orphanIDsFromInspect(out)
	want := []string{"aaa111", "bbb222", "fff666", "eee555"}
	if len(got) != len(want) {
		t.Fatalf("orphanIDsFromInspect = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("orphan[%d] = %q, want %q (got=%v)", i, got[i], want[i], got)
		}
	}
}

// TestReapOrphanCIContainersStopsAndRemoves prova a orquestração docker do reaper
// via RunFn injetado (hermético, sem docker real): ele lista (ps -q), inspeciona,
// e dá stop+rm SÓ nos órfãos. O efeito real (porta liberada) é provado pelo
// integration test; aqui validamos a sequência de comandos e os alvos.
func TestReapOrphanCIContainersStopsAndRemoves(t *testing.T) {
	var calls [][]string
	opts := Options{
		Execute: true,
		RunFn: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			switch {
			case name == "docker" && len(args) >= 1 && args[0] == "ps":
				return []byte("aaa111\nbbb222\nccc333\n"), nil
			case name == "docker" && len(args) >= 1 && args[0] == "inspect":
				// aaa=órfão(project), bbb=não-órfão, ccc=órfão(porta fixa minio),
				// ddd=órfão(testcontainers sem compose/porta).
				return []byte("aaa111|civm-run-5|<no value>|40000 \n" +
					"bbb222|<no value>|<no value>|40001 \n" +
					"ccc333|<no value>|<no value>|9020 \n" +
					"ddd444|<no value>|true| \n"), nil
			}
			return nil, nil
		},
	}

	actions := reapOrphanCIContainers(context.Background(), opts)

	if len(actions) != 1 || actions[0].Name != "docker_reap_orphan_ci" {
		t.Fatalf("expected single docker_reap_orphan_ci action, got %+v", actions)
	}
	if actions[0].Warning != "" || actions[0].Error != "" {
		t.Errorf("happy path must have no warning/error, got %+v", actions[0])
	}
	// Procura o stop e o rm; ambos devem mirar EXATAMENTE aaa111 e ccc333 (não bbb222).
	var stopArgs, rmArgs []string
	for _, c := range calls {
		if len(c) >= 2 && c[0] == "docker" && c[1] == "stop" {
			stopArgs = c
		}
		if len(c) >= 3 && c[0] == "docker" && c[1] == "rm" && c[2] == "-f" {
			rmArgs = c
		}
	}
	if stopArgs == nil {
		t.Fatalf("no docker stop call; calls=%v", calls)
	}
	if rmArgs == nil {
		t.Fatalf("no docker rm -f call; calls=%v", calls)
	}
	joinedStop := strings.Join(stopArgs, " ")
	joinedRm := strings.Join(rmArgs, " ")
	for _, want := range []string{"aaa111", "ccc333", "ddd444"} {
		if !strings.Contains(joinedStop, want) {
			t.Errorf("docker stop missing orphan %s; got %q", want, joinedStop)
		}
		if !strings.Contains(joinedRm, want) {
			t.Errorf("docker rm missing orphan %s; got %q", want, joinedRm)
		}
	}
	if strings.Contains(joinedRm, "bbb222") {
		t.Errorf("docker rm must NOT touch non-orphan bbb222; got %q", joinedRm)
	}
}

// TestReapOrphanCIContainersBestEffort prova o invariante de segurança da
// fronteira job-started: falha do docker (ps erra, ou rm erra) NUNCA vira Error —
// só Warning. Higiene de job-started não pode falhar o job.
func TestReapOrphanCIContainersBestEffort(t *testing.T) {
	t.Run("docker ps falha => warning, sem error", func(t *testing.T) {
		opts := Options{
			Execute: true,
			RunFn: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				return nil, fmt.Errorf("Cannot connect to the Docker daemon")
			},
		}
		actions := reapOrphanCIContainers(context.Background(), opts)
		if len(actions) != 1 {
			t.Fatalf("want 1 action, got %d", len(actions))
		}
		if actions[0].Error != "" {
			t.Errorf("docker ps failure must be Warning, not Error: %+v", actions[0])
		}
		if actions[0].Warning == "" {
			t.Errorf("docker ps failure should surface a Warning: %+v", actions[0])
		}
	})

	t.Run("docker rm falha => warning, sem error", func(t *testing.T) {
		opts := Options{
			Execute: true,
			RunFn: func(_ context.Context, name string, args ...string) ([]byte, error) {
				switch args[0] {
				case "ps":
					return []byte("aaa111\n"), nil
				case "inspect":
					return []byte("aaa111|civm-run-1|<no value>|\n"), nil
				case "rm":
					return nil, fmt.Errorf("device or resource busy")
				}
				return nil, nil
			},
		}
		actions := reapOrphanCIContainers(context.Background(), opts)
		if actions[0].Error != "" {
			t.Errorf("docker rm failure must be Warning, not Error: %+v", actions[0])
		}
		if actions[0].Warning == "" {
			t.Errorf("docker rm failure should surface a Warning: %+v", actions[0])
		}
	})

	t.Run("dry-run não chama docker", func(t *testing.T) {
		called := false
		opts := Options{
			Execute: false,
			RunFn: func(_ context.Context, _ string, _ ...string) ([]byte, error) {
				called = true
				return nil, nil
			},
		}
		actions := reapOrphanCIContainers(context.Background(), opts)
		if called {
			t.Errorf("dry-run must not invoke docker")
		}
		if len(actions) != 1 || actions[0].Executed {
			t.Errorf("dry-run action should be non-executed, got %+v", actions)
		}
	})

	t.Run("nenhum container rodando => no-op limpo", func(t *testing.T) {
		opts := Options{
			Execute: true,
			RunFn: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				if len(args) >= 1 && args[0] == "ps" {
					return []byte("\n"), nil
				}
				t.Errorf("inspect/stop/rm must not run when no containers are up; args=%v", args)
				return nil, nil
			},
		}
		actions := reapOrphanCIContainers(context.Background(), opts)
		if actions[0].Warning != "" || actions[0].Error != "" {
			t.Errorf("empty ps must be clean no-op, got %+v", actions[0])
		}
	})

	t.Run("docker inspect falha => warning, sem error", func(t *testing.T) {
		opts := Options{
			Execute: true,
			RunFn: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "ps":
					return []byte("aaa111\n"), nil
				case "inspect":
					return nil, fmt.Errorf("no such object")
				}
				t.Errorf("stop/rm must not run after inspect fails; args=%v", args)
				return nil, nil
			},
		}
		actions := reapOrphanCIContainers(context.Background(), opts)
		if actions[0].Error != "" {
			t.Errorf("docker inspect failure must be Warning, not Error: %+v", actions[0])
		}
		if actions[0].Warning == "" {
			t.Errorf("docker inspect failure should surface a Warning: %+v", actions[0])
		}
	})

	t.Run("containers rodando mas NENHUM órfão => no-op sem stop/rm", func(t *testing.T) {
		opts := Options{
			Execute: true,
			RunFn: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "ps":
					return []byte("zzz999\n"), nil
				case "inspect":
					// Outro produto, porta não-fixa => não órfão.
					return []byte("zzz999|harmya-org-1|<no value>|40000 \n"), nil
				}
				t.Errorf("stop/rm must not run when no orphan is found; args=%v", args)
				return nil, nil
			},
		}
		actions := reapOrphanCIContainers(context.Background(), opts)
		if actions[0].Warning != "" || actions[0].Error != "" {
			t.Errorf("no-orphan run must be clean no-op, got %+v", actions[0])
		}
	})

	t.Run("stop falha mas rm sucede => warning de stop, sem error, container removido", func(t *testing.T) {
		var rmCalled bool
		opts := Options{
			Execute: true,
			RunFn: func(_ context.Context, _ string, args ...string) ([]byte, error) {
				switch args[0] {
				case "ps":
					return []byte("aaa111\n"), nil
				case "inspect":
					return []byte("aaa111|civm-run-1|<no value>|\n"), nil
				case "stop":
					return nil, fmt.Errorf("stop timeout")
				case "rm":
					rmCalled = true
					return nil, nil
				}
				return nil, nil
			},
		}
		actions := reapOrphanCIContainers(context.Background(), opts)
		if actions[0].Error != "" {
			t.Errorf("stop failure must not be Error (rm is the real reclaim): %+v", actions[0])
		}
		if actions[0].Warning == "" {
			t.Errorf("stop failure should surface a Warning: %+v", actions[0])
		}
		if !rmCalled {
			t.Errorf("rm must still run after a failed stop (stop failure must not abort the reclaim)")
		}
	})
}

// TestJobStartedRunsOrphanReaper prova a integração no caminho Run/job-started: o
// reaper roda na fronteira (ação docker_reap_orphan_ci presente) mesmo num disco
// saudável (sem disk-pressure cleanup), como reclaimWorkspaceOwnership.
func TestJobStartedRunsOrphanReaper(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("RUNNER_TEMP", "")
	t.Setenv("GITHUB_WORKSPACE", "")
	t.Setenv("GITHUB_REPOSITORY", "")
	t.Setenv("GITHUB_RUN_ID", "")

	var reapedID string
	opts := DefaultOptionsFromEnv(EventJobStarted)
	opts.Execute = true
	opts.PreCleanupPct = 99 // disco saudável -> sem cleanup gated
	opts.HardFailPct = 100
	opts.StatfsFn = func(string) (uint64, uint64, error) { return 100, 70, nil }
	opts.RemoveAllFn = func(string) error { return nil }
	opts.MkdirAllFn = func(string, os.FileMode) error { return nil }
	opts.ReadDirFn = func(string) ([]os.DirEntry, error) { return nil, nil }
	opts.LogPath = ""
	opts.WorkRoot = "/home/civm-test/actions-runner/_work"
	opts.HostDiskFn = func() (hostdisk.Report, error) {
		return hostdisk.Report{Metrics: hostdisk.Metrics{VFreeGB: 66}, Level: "ok"}, nil
	}
	opts.RunFn = func(_ context.Context, name string, args ...string) ([]byte, error) {
		if name == "docker" && len(args) >= 1 {
			switch args[0] {
			case "ps":
				return []byte("dead001\n"), nil
			case "inspect":
				return []byte("dead001|civm-run-1186|<no value>|9020 \n"), nil
			case "rm":
				reapedID = "dead001"
			}
		}
		return nil, nil
	}

	res := Run(context.Background(), opts)

	if res.ExitCode != 0 {
		t.Fatalf("orphan reaper at job-started must not fail the hook; res=%+v", res)
	}
	var sawAction bool
	for _, a := range res.Actions {
		if a.Name == "docker_reap_orphan_ci" {
			sawAction = true
		}
	}
	if !sawAction {
		t.Errorf("no docker_reap_orphan_ci action at job-started; actions=%+v", res.Actions)
	}
	if reapedID != "dead001" {
		t.Errorf("orphan dead001 was not reaped via docker rm; reapedID=%q", reapedID)
	}
}

// TestCIFixedHostPortsCoverIncidentPort é a regressão do incidente 2026-06-19: a
// porta do minio (9020) que matou tenant-isolation no #1184/#1186 DEVE estar no
// set de portas fixas do reaper, senão um órfão de minio sem o label escaparia da
// defesa em profundidade.
func TestCIFixedHostPortsCoverIncidentPort(t *testing.T) {
	const minioAPIPort = 9020
	found := false
	for _, p := range civm.DefaultCIFixedHostPorts {
		if p == minioAPIPort {
			found = true
		}
	}
	if !found {
		t.Errorf("DefaultCIFixedHostPorts must include the 2026-06-19 incident port %d (minio API)", minioAPIPort)
	}
	if !isCIOrphan("<no value>", "", []string{fmt.Sprint(minioAPIPort)}) {
		t.Errorf("a container publishing only %d must be classified as a CI orphan", minioAPIPort)
	}
}

// FuzzSafeWorkRoot enforces the safety invariant of safeWorkRoot for arbitrary
// input. Anything safeWorkRoot accepts must, after filepath.Clean, contain
// "/home/" and "/actions-runner", and end in "/_work" — i.e. no path the
// fuzzer constructs may escape the runner work-root whitelist.
func FuzzSafeWorkRoot(f *testing.F) {
	seeds := []string{
		"/home/emdev/actions-runner-acme/_work",
		"/home/runner/actions-runner-1/_work",
		"/home/emdev/actions-runner/_work/../../etc",
		"/home/../home/runner/actions-runner/_work",
		"/etc/passwd",
		"/tmp/_work",
		"/home/emdev/actions-runner/_tool",
		"",
		"//home//emdev//actions-runner//_work",
		"/home/emdev/actions-runner/_work\x00/etc",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, root string) {
		if !safeWorkRoot(root) {
			return
		}
		clean := filepath.Clean(root)
		if !filepath.IsAbs(clean) {
			t.Fatalf("safeWorkRoot accepted non-absolute %q (clean=%q)", root, clean)
		}
		// Anything accepted must match the canonical work-root glob as a
		// path-SEGMENT match — no substring slips through (DT-v2-7).
		ok, err := filepath.Match(workRootGlob, clean)
		if err != nil || !ok {
			t.Fatalf("safeWorkRoot accepted %q but clean=%q does not match %q", root, clean, workRootGlob)
		}
		for _, part := range strings.Split(clean, "/") {
			if part == ".." {
				t.Fatalf("safeWorkRoot accepted %q with traversal component after Clean: %q", root, clean)
			}
		}
	})
}
