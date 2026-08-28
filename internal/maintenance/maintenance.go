// Package maintenance drains and restores this civm runner host idempotently
// so the Windows-side VHDX compaction task can shut the guest down safely.
//
// Enter stops the local actions.runner.* units and removes the "civm" label
// from each runner registration (repository or organization); Exit restores
// exactly what Enter recorded. State is persisted as JSON in StatePath so Exit
// is deterministic and both operations are no-ops when re-run. A flock on
// LockPath serializes concurrent callers.
package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/idle"
)

// civmLabel is the GitHub runner label toggled during drain/restore.
const civmLabel = "civm"

const orgRunnerNameSuffix = "-org"

const runReaperEnvPath = "/etc/civm/run-reaper.env"

// runnerUnitGlob matches the systemd units of every self-hosted runner.
const runnerUnitGlob = "actions.runner.*"

// timeLayout is the timestamp format persisted in State.DrainedAt.
const timeLayout = time.RFC3339

// RunnerState records what Enter actually changed for a single runner, so Exit
// can restore exactly that and nothing more.
type RunnerState struct {
	Name         string `json:"name"`
	Repo         string `json:"repo,omitempty"` // owner/repo or bare org for org-level runners
	RunnerID     int64  `json:"runner_id,omitempty"`
	Stopped      bool   `json:"stopped"`
	LabelRemoved bool   `json:"label_removed"`
}

// State is the persisted drain snapshot.
type State struct {
	DrainedAt string        `json:"drained_at"`
	Runners   []RunnerState `json:"runners"`
}

// Options configures Enter/Exit. Every side effect is injected so unit tests
// run without real syscalls, exec, ssh, network or /proc access.
type Options struct {
	// Execute applies mutations; when false the call is a dry-run that touches
	// nothing (no systemctl, no gh, no state write).
	Execute bool
	// Force allows Enter to proceed even when idle.Check reports the host busy.
	Force bool
	// RequireStopped makes Enter fail closed until every eligible runner unit is
	// stopped. Partial state is persisted so a later strict retry can finish the
	// drain or Exit can restore the units that were already stopped.
	RequireStopped bool
	// StatePath is the JSON drain snapshot (default DefaultMaintenanceStatePath).
	StatePath string
	// LockPath is the flock file (default DefaultMaintenanceLockPath).
	LockPath string
	// Repos restricts which repos are drained; empty infers from the units.
	Repos []string

	// RunFn runs systemctl (stop/start/list-units).
	RunFn func(ctx context.Context, name string, args ...string) ([]byte, error)
	// GHFn runs the gh CLI for label add/remove.
	GHFn func(ctx context.Context, args ...string) ([]byte, error)
	// IdleCheckFn returns true when the host is provably idle.
	IdleCheckFn func(ctx context.Context) bool

	ReadFileFn  func(path string) ([]byte, error)
	WriteFileFn func(path string, data []byte, perm os.FileMode) error
	RemoveFn    func(path string) error
	MkdirAllFn  func(path string, perm os.FileMode) error
	// LockFn acquires an exclusive lock on path and returns a release func.
	LockFn func(path string) (release func() error, err error)
	NowFn  func() time.Time
}

// DefaultOptions returns production wiring.
func DefaultOptions() Options {
	return Options{
		StatePath:   civm.DefaultMaintenanceStatePath,
		LockPath:    civm.DefaultMaintenanceLockPath,
		RunFn:       defaultRun,
		GHFn:        defaultGH,
		IdleCheckFn: defaultIdleCheck,
		ReadFileFn:  os.ReadFile,
		WriteFileFn: os.WriteFile,
		RemoveFn:    os.Remove,
		MkdirAllFn:  os.MkdirAll,
		LockFn:      defaultLock,
		NowFn:       time.Now,
	}
}

func applyDefaults(opts *Options) {
	if opts.StatePath == "" {
		opts.StatePath = civm.DefaultMaintenanceStatePath
	}
	if opts.LockPath == "" {
		opts.LockPath = civm.DefaultMaintenanceLockPath
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.GHFn == nil {
		opts.GHFn = defaultGH
	}
	if opts.IdleCheckFn == nil {
		opts.IdleCheckFn = defaultIdleCheck
	}
	if opts.ReadFileFn == nil {
		opts.ReadFileFn = os.ReadFile
	}
	if opts.WriteFileFn == nil {
		opts.WriteFileFn = os.WriteFile
	}
	if opts.RemoveFn == nil {
		opts.RemoveFn = os.Remove
	}
	if opts.MkdirAllFn == nil {
		opts.MkdirAllFn = os.MkdirAll
	}
	if opts.LockFn == nil {
		opts.LockFn = defaultLock
	}
	if opts.NowFn == nil {
		opts.NowFn = time.Now
	}
}

// Enter drains this host: it stops the runner units and removes the civm label
// per runner, then persists State. Individual systemctl/gh failures are logged
// as warnings and tolerated; Enter only errors when BOTH the stop and the label
// removal fail for EVERY runner (nothing was drained). When RequireStopped is
// set, every eligible runner must be stopped; a partial snapshot is preserved
// and retried rather than treated as a successful drain.
func Enter(ctx context.Context, opts Options) (State, error) {
	applyDefaults(&opts)
	if opts.RequireStopped && opts.Force {
		return State{}, errors.New("maintenance enter strict: --force is incompatible with a no-interruption drain")
	}

	if !opts.Execute {
		return dryRunEnter(ctx, opts)
	}

	release, err := opts.LockFn(opts.LockPath)
	if err != nil {
		return State{}, fmt.Errorf("maintenance enter: lock %s: %w", opts.LockPath, err)
	}
	defer func() { _ = release() }()

	// Default re-runs are idempotent. Strict re-runs deliberately retry only
	// units that the persisted snapshot could not stop; otherwise a partial
	// maintenance state would become a permanent false green.
	if existing, ok, rerr := readState(opts); rerr != nil {
		return State{}, rerr
	} else if ok {
		if opts.RequireStopped {
			if !opts.Force && !opts.IdleCheckFn(ctx) {
				return existing, errors.New("maintenance enter strict: host nao esta ocioso")
			}
			strictErr := retryStrictDrain(ctx, opts, &existing)
			existing.DrainedAt = opts.NowFn().UTC().Format(timeLayout)
			if werr := writeState(opts, existing); werr != nil {
				return State{}, werr
			}
			if strictErr != nil {
				return existing, strictErr
			}
			if !allRunnersStopped(existing.Runners) {
				return existing, strictDrainError(existing.Runners)
			}
			return existing, nil
		}
		existing.DrainedAt = opts.NowFn().UTC().Format(timeLayout)
		if werr := writeState(opts, existing); werr != nil {
			return State{}, werr
		}
		return existing, nil
	}

	if !opts.Force && !opts.IdleCheckFn(ctx) {
		return State{}, errors.New("maintenance enter: host nao esta ocioso (use --force para drenar mesmo assim)")
	}

	runners, discoverErr := discoverRunners(ctx, opts)
	if discoverErr != nil {
		if opts.RequireStopped {
			return State{}, discoverErr
		}
		warn("systemctl list-units: %v", discoverErr)
		runners = nil
	}
	state := State{DrainedAt: opts.NowFn().UTC().Format(timeLayout)}
	if opts.RequireStopped {
		for _, runner := range runners {
			state.Runners = append(state.Runners, RunnerState{Name: runner.Name, Repo: runner.Repo})
		}
		strictErr := strictDrain(ctx, opts, &state, runners)
		if err := writeState(opts, state); err != nil {
			return State{}, err
		}
		if strictErr != nil {
			return state, strictErr
		}
		if !allRunnersStopped(state.Runners) {
			return state, strictDrainError(state.Runners)
		}
		return state, nil
	}
	drainedAny := false
	for _, rn := range runners {
		applied := drainRunner(ctx, opts, rn)
		state.Runners = append(state.Runners, applied)
		if applied.Stopped || applied.LabelRemoved {
			drainedAny = true
		}
	}
	if len(runners) > 0 && !drainedAny {
		return State{}, errors.New("maintenance enter: falhou parar units E remover label em todos os runners")
	}
	if err := writeState(opts, state); err != nil {
		return State{}, err
	}
	return state, nil
}

func allRunnersStopped(runners []RunnerState) bool {
	for _, runner := range runners {
		if !runner.Stopped {
			return false
		}
	}
	return true
}

func strictDrainError(runners []RunnerState) error {
	pendingStops := 0
	pendingLabels := 0
	for _, runner := range runners {
		if !runner.Stopped {
			pendingStops++
		}
		if runner.Repo == "" || !runner.LabelRemoved {
			pendingLabels++
		}
	}
	return fmt.Errorf("maintenance enter strict: %d listener(s) not stopped; %d runner label(s) not withdrawn", pendingStops, pendingLabels)
}

func retryStrictDrain(ctx context.Context, opts Options, state *State) error {
	if state == nil {
		return errors.New("maintenance enter strict: missing persisted state")
	}
	refs, err := discoverRunners(ctx, opts)
	if err != nil {
		return err
	}
	return strictDrain(ctx, opts, state, refs)
}

// strictDrain first withdraws every runner label, then proves idleness again,
// and only then stops units. The ordering closes the dispatch race between an
// idle probe and systemctl stop: a new job cannot be assigned after labels are
// gone, while an already assigned job is caught by the second idle probe.
func strictDrain(ctx context.Context, opts Options, state *State, refs []runnerRef) error {
	if state == nil {
		return errors.New("maintenance enter strict: missing persisted state")
	}
	byUnit := make(map[string]int, len(state.Runners))
	for i, runner := range state.Runners {
		byUnit[runnerUnitName(runner)] = i
	}
	for _, ref := range refs {
		if _, known := byUnit[ref.Unit]; known {
			continue
		}
		state.Runners = append(state.Runners, RunnerState{Name: ref.Name, Repo: ref.Repo})
		byUnit[ref.Unit] = len(state.Runners) - 1
	}

	for i := range state.Runners {
		runner := &state.Runners[i]
		if runner.LabelRemoved || runner.Repo == "" {
			continue
		}
		runnerID, err := removeLabel(ctx, opts, runner.Repo, runner.Name)
		if err != nil {
			warn("gh remove label %s on %s: %v", civmLabel, runner.Repo, err)
			continue
		}
		runner.RunnerID = runnerID
		runner.LabelRemoved = true
	}
	for _, runner := range state.Runners {
		if runner.Repo == "" || !runner.LabelRemoved {
			return strictDrainError(state.Runners)
		}
	}
	if !opts.IdleCheckFn(ctx) {
		return errors.New("maintenance enter strict: host became busy after labels were withdrawn")
	}
	for i := range state.Runners {
		runner := &state.Runners[i]
		if runner.Stopped {
			continue
		}
		unit := runnerUnitName(*runner)
		if _, err := opts.RunFn(ctx, "sudo", "systemctl", "stop", unit); err != nil {
			warn("systemctl stop %s: %v", unit, err)
			continue
		}
		runner.Stopped = true
	}
	if !allRunnersStopped(state.Runners) {
		return strictDrainError(state.Runners)
	}
	return nil
}

// Exit restores the host using the recorded State, then deletes it. An absent
// State is a no-op (idempotent). Individual restore failures are warnings; a
// failure to delete the State file is a hard error so a stale snapshot is never
// left behind silently.
func Exit(ctx context.Context, opts Options) (State, error) {
	applyDefaults(&opts)

	if !opts.Execute {
		return dryRunExit(opts)
	}

	release, err := opts.LockFn(opts.LockPath)
	if err != nil {
		return State{}, fmt.Errorf("maintenance exit: lock %s: %w", opts.LockPath, err)
	}
	defer func() { _ = release() }()

	state, ok, rerr := readState(opts)
	if rerr != nil {
		return State{}, rerr
	}
	if !ok {
		// Nothing drained — nothing to restore.
		return State{}, nil
	}

	for _, rn := range state.Runners {
		restoreRunner(ctx, opts, rn)
	}

	if err := opts.RemoveFn(opts.StatePath); err != nil && !os.IsNotExist(err) {
		return State{}, fmt.Errorf("maintenance exit: remover state %s: %w", opts.StatePath, err)
	}
	return state, nil
}

func dryRunEnter(ctx context.Context, opts Options) (State, error) {
	if existing, ok, err := readState(opts); err != nil {
		return State{}, err
	} else if ok {
		return existing, nil
	}
	state := State{DrainedAt: opts.NowFn().UTC().Format(timeLayout)}
	runners, err := discoverRunners(ctx, opts)
	if err != nil {
		warn("systemctl list-units: %v", err)
		runners = nil
	}
	for _, rn := range runners {
		state.Runners = append(state.Runners, RunnerState{
			Name:         rn.Name,
			Repo:         rn.Repo,
			Stopped:      true,
			LabelRemoved: rn.Repo != "",
		})
	}
	return state, nil
}

func dryRunExit(opts Options) (State, error) {
	state, ok, err := readState(opts)
	if err != nil {
		return State{}, err
	}
	if !ok {
		return State{}, nil
	}
	return state, nil
}

// runnerRef is a discovered runner unit with its resolved repo.
type runnerRef struct {
	Unit string
	Name string
	Repo string
}

func discoverRunners(ctx context.Context, opts Options) ([]runnerRef, error) {
	out, err := opts.RunFn(ctx, "systemctl", "list-units", "--type=service",
		"--no-legend", "--no-pager", "--all", runnerUnitGlob)
	if err != nil {
		return nil, fmt.Errorf("maintenance enter: enumerate runner units: %w", err)
	}
	allow := repoAllowSet(opts.Repos)
	seen := map[string]struct{}{}
	var refs []runnerRef
	for _, line := range strings.Split(string(out), "\n") {
		unit := firstUnitField(line)
		if unit == "" {
			continue
		}
		repo, name := parseRunnerUnit(unit)
		if name == "" {
			name = unit
		}
		candidate := runnerRef{Unit: unit, Name: name, Repo: repo}
		if len(allow) > 0 && !runnerAllowed(candidate, allow) {
			continue
		}
		if _, dup := seen[unit]; dup {
			continue
		}
		seen[unit] = struct{}{}
		refs = append(refs, candidate)
	}
	sort.SliceStable(refs, func(i, j int) bool { return refs[i].Unit < refs[j].Unit })
	return refs, nil
}

func drainRunner(ctx context.Context, opts Options, rn runnerRef) RunnerState {
	applied := RunnerState{Name: rn.Name, Repo: rn.Repo}
	if _, err := opts.RunFn(ctx, "sudo", "systemctl", "stop", rn.Unit); err != nil {
		warn("systemctl stop %s: %v", rn.Unit, err)
	} else {
		applied.Stopped = true
	}
	if rn.Repo != "" {
		runnerID, err := removeLabel(ctx, opts, rn.Repo, rn.Name)
		if err != nil {
			warn("gh remove label %s on %s: %v", civmLabel, rn.Repo, err)
		} else {
			applied.RunnerID = runnerID
			applied.LabelRemoved = true
		}
	}
	return applied
}

func restoreRunner(ctx context.Context, opts Options, rn RunnerState) {
	if rn.Stopped {
		unit := runnerUnitName(rn)
		if _, err := opts.RunFn(ctx, "sudo", "systemctl", "start", unit); err != nil {
			warn("systemctl start %s: %v", unit, err)
		}
	}
	if rn.LabelRemoved && rn.Repo != "" {
		if err := addLabel(ctx, opts, rn.Repo, rn.Name, rn.RunnerID); err != nil {
			warn("gh add label %s on %s: %v", civmLabel, rn.Repo, err)
		}
	}
}

func removeLabel(ctx context.Context, opts Options, repo, runnerName string) (int64, error) {
	base, err := runnerAPIBase(repo, runnerName)
	if err != nil {
		return 0, err
	}
	runnerID, err := resolveRunnerID(ctx, opts, base, runnerName)
	if err != nil {
		return 0, err
	}
	endpoint := fmt.Sprintf("%s/actions/runners/%d/labels/%s", base, runnerID, civmLabel)
	if _, err := opts.GHFn(ctx, "api", "--method", "DELETE", endpoint); err != nil {
		return 0, fmt.Errorf("gh api DELETE %s: %w", endpoint, err)
	}
	return runnerID, nil
}

func addLabel(ctx context.Context, opts Options, repo, runnerName string, runnerID int64) error {
	base, err := runnerAPIBase(repo, runnerName)
	if err != nil {
		return err
	}
	if runnerID <= 0 {
		runnerID, err = resolveRunnerID(ctx, opts, base, runnerName)
		if err != nil {
			return err
		}
	}
	endpoint := fmt.Sprintf("%s/actions/runners/%d/labels", base, runnerID)
	if _, err := opts.GHFn(ctx, "api", "--method", "POST", endpoint, "-f", "labels[]="+civmLabel); err != nil {
		return fmt.Errorf("gh api POST %s: %w", endpoint, err)
	}
	return nil
}

type runnerAPIPage struct {
	Runners []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"runners"`
}

func runnerAPIBase(repo, runnerName string) (string, error) {
	if strings.Contains(repo, "/") {
		if err := civm.ValidateRepo(repo); err != nil {
			return "", err
		}
		return "repos/" + repo, nil
	}
	if repo == "" || !strings.HasSuffix(runnerName, orgRunnerNameSuffix) {
		return "", fmt.Errorf("runner %q nao tem alvo GitHub seguro em %q", runnerName, repo)
	}
	if err := civm.ValidateRepo(repo + "/runner-id-probe"); err != nil {
		return "", fmt.Errorf("org invalida %q: %w", repo, err)
	}
	return "orgs/" + repo, nil
}

func resolveRunnerID(ctx context.Context, opts Options, base, runnerName string) (int64, error) {
	if strings.TrimSpace(runnerName) == "" {
		return 0, errors.New("nome do runner vazio")
	}
	endpoint := base + "/actions/runners?per_page=100"
	out, err := opts.GHFn(ctx, "api", "--paginate", "--slurp", endpoint)
	if err != nil {
		return 0, fmt.Errorf("gh api GET %s: %w", endpoint, err)
	}
	var pages []runnerAPIPage
	if err := json.Unmarshal(out, &pages); err != nil {
		return 0, fmt.Errorf("parse runners de %s: %w", base, err)
	}
	var found int64
	for _, page := range pages {
		for _, candidate := range page.Runners {
			if candidate.Name != runnerName {
				continue
			}
			if candidate.ID <= 0 || found != 0 {
				return 0, fmt.Errorf("runner %q ambiguo ou com ID invalido em %s", runnerName, base)
			}
			found = candidate.ID
		}
	}
	if found == 0 {
		return 0, fmt.Errorf("runner %q nao encontrado em %s", runnerName, base)
	}
	return found, nil
}

func readState(opts Options) (State, bool, error) {
	data, err := opts.ReadFileFn(opts.StatePath)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, false, nil
		}
		return State{}, false, fmt.Errorf("ler state %s: %w", opts.StatePath, err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return State{}, false, nil
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		return State{}, false, fmt.Errorf("parse state %s: %w", opts.StatePath, err)
	}
	return state, true, nil
}

func writeState(opts Options, state State) error {
	if err := opts.MkdirAllFn(filepath.Dir(opts.StatePath), 0o750); err != nil {
		return fmt.Errorf("criar dir de state %s: %w", opts.StatePath, err)
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("serializar state: %w", err)
	}
	if err := opts.WriteFileFn(opts.StatePath, data, 0o600); err != nil {
		return fmt.Errorf("gravar state %s: %w", opts.StatePath, err)
	}
	return nil
}

func repoAllowSet(repos []string) map[string]bool {
	if len(repos) == 0 {
		return nil
	}
	set := make(map[string]bool, len(repos))
	for _, r := range repos {
		r = strings.TrimSpace(r)
		if r != "" {
			set[r] = true
		}
	}
	return set
}

func runnerAllowed(rn runnerRef, allow map[string]bool) bool {
	if allow[rn.Repo] {
		return true
	}
	if rn.Repo == "" || !strings.HasSuffix(rn.Name, orgRunnerNameSuffix) {
		return false
	}
	for repo := range allow {
		if strings.HasPrefix(repo, rn.Repo+"/") {
			return true
		}
	}
	return false
}

// firstUnitField returns the unit name from a `systemctl list-units` line,
// tolerating the leading ●/○ markers systemd prints for inactive/failed units.
func firstUnitField(line string) string {
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	candidate := fields[0]
	if (candidate == "●" || candidate == "○" || candidate == "*") && len(fields) > 1 {
		candidate = fields[1]
	}
	if !strings.HasPrefix(candidate, "actions.runner.") || !strings.HasSuffix(candidate, ".service") {
		return ""
	}
	return candidate
}

// parseRunnerUnit converts "actions.runner.owner-repo.runner-1.service" into
// repo="owner/repo" and name="runner-1".
func parseRunnerUnit(unit string) (repo, name string) {
	const prefix = "actions.runner."
	const suffix = ".service"
	if !strings.HasPrefix(unit, prefix) || !strings.HasSuffix(unit, suffix) {
		return "", ""
	}
	rest := strings.TrimSuffix(strings.TrimPrefix(unit, prefix), suffix)
	idx := strings.LastIndex(rest, ".")
	if idx == -1 {
		return rest, ""
	}
	repoSegment := rest[:idx]
	name = rest[idx+1:]
	dashIdx := strings.Index(repoSegment, "-")
	if dashIdx == -1 {
		return repoSegment, name
	}
	repo = repoSegment[:dashIdx] + "/" + repoSegment[dashIdx+1:]
	return repo, name
}

// runnerUnitName reconstructs the systemd unit for a recorded runner.
func runnerUnitName(rn RunnerState) string {
	repoSegment := strings.Replace(rn.Repo, "/", "-", 1)
	if repoSegment == "" {
		return fmt.Sprintf("actions.runner.%s.service", rn.Name)
	}
	return fmt.Sprintf("actions.runner.%s.%s.service", repoSegment, rn.Name)
}

// RenderJSON emits the machine-readable drain snapshot.
func RenderJSON(w io.Writer, state State) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(state)
}

// RenderText writes a human-readable drain summary.
func RenderText(w io.Writer, action string, state State) {
	fmt.Fprintf(w, "maintenance %s: drained_at=%s runners=%d\n", action, valueOrNone(state.DrainedAt), len(state.Runners))
	for _, rn := range state.Runners {
		fmt.Fprintf(w, "  %-30s repo=%-24s stopped=%t label_removed=%t\n",
			rn.Name, valueOrNone(rn.Repo), rn.Stopped, rn.LabelRemoved)
	}
}

func valueOrNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "WARN maintenance: "+format+"\n", args...)
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func defaultGH(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if os.Getenv("GH_TOKEN") == "" && os.Getenv("GITHUB_TOKEN") == "" && os.Getenv("GH_CONFIG_DIR") == "" {
		extra, err := ghEnvFromFile(os.ReadFile)
		if err != nil {
			return nil, err
		}
		cmd.Env = append(os.Environ(), extra...)
	}
	return cmd.CombinedOutput()
}

func ghEnvFromFile(readFile func(string) ([]byte, error)) ([]string, error) {
	data, err := readFile(runReaperEnvPath)
	if err != nil {
		return nil, fmt.Errorf("ler credencial GitHub de %s: %w", runReaperEnvPath, err)
	}
	var authEnv []string
	seen := make(map[string]bool, 2)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		key, value, ok := strings.Cut(line, "=")
		if !ok || (key != "GH_TOKEN" && key != "GH_CONFIG_DIR") {
			continue
		}
		if seen[key] {
			return nil, fmt.Errorf("%s duplicado no arquivo de credencial", key)
		}
		seen[key] = true
		value = strings.TrimSpace(value)
		if len(value) >= 2 {
			if (value[0] == '\'' && value[len(value)-1] == '\'') ||
				(value[0] == '"' && value[len(value)-1] == '"') {
				value = value[1 : len(value)-1]
			}
		}
		if value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("%s ausente ou invalido no arquivo de credencial", key)
		}
		if key == "GH_CONFIG_DIR" {
			clean := filepath.Clean(value)
			if !filepath.IsAbs(clean) || clean == string(filepath.Separator) {
				return nil, errors.New("GH_CONFIG_DIR deve ser caminho absoluto e especifico")
			}
			value = clean
		}
		authEnv = append(authEnv, key+"="+value)
	}
	if len(authEnv) == 0 {
		return nil, errors.New("GH_TOKEN ou GH_CONFIG_DIR ausente no arquivo de credencial")
	}
	return authEnv, nil
}

func defaultIdleCheck(ctx context.Context) bool {
	return idle.Check(ctx, idle.DefaultOptions()).Status == idle.StatusIdle
}

// defaultLock takes an exclusive advisory flock on path and returns a release
// func that unlocks and closes the descriptor.
func defaultLock(path string) (func() error, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("criar dir do lock %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("abrir lock %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("flock %s (ja em uso?): %w", path, err)
	}
	return func() error {
		ferr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		cerr := f.Close()
		if ferr != nil {
			return ferr
		}
		return cerr
	}, nil
}
