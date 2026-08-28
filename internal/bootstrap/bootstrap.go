// Package bootstrap provisions an Ubuntu 24.04 VM to be a GitHub Actions
// self-hosted runner with parity to ubuntu-latest. All steps are
// idempotent: each Step.Check returns true when already applied.
package bootstrap

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/specs"
)

// Result is the outcome of one Step.
type Result struct {
	Name        string
	Description string
	AlreadyDone bool
	Executed    bool
	WouldDo     bool // true in dry-run mode if Apply would run
	Err         error
}

// Step is one idempotent provisioning action.
type Step struct {
	Name        string
	Description string
	HardFail    bool
	Check       func(ctx context.Context) (bool, error)
	Apply       func(ctx context.Context) error
}

// Options control the bootstrap run.
type Options struct {
	Execute          bool
	Spec             specs.RunnerImageSpec
	UID              int
	WatchdogTimer    bool   // habilita civmctl-disk-watchdog.timer
	ReverseWatchdog  bool   // habilita civmctl-reverse-watchdog.timer (alarm-of-alarm)
	RunnerWatchdog   bool   // habilita civmctl-runner-watchdog.timer
	MetricsTimer     bool   // habilita civmctl-metrics.timer
	RunReaper        bool   // habilita civmctl-run-reaper.timer (cancela runs de PR fechado)
	MemWatchdog      bool   // habilita civmctl-mem-watchdog.timer (monitora pressao de RAM/swap)
	InstallUnitsFrom string // se nao-vazio, copia .service/.timer de PATH para /etc/systemd/system/ antes de enable
	OSReader         func() (string, error)
	RunFn            func(ctx context.Context, name string, args ...string) ([]byte, error)
	WriteFileFn      func(name string, data []byte, perm os.FileMode) error
	SHA256FileFn     func(path string) (string, error)
}

// DefaultOptions returns sane production defaults.
func DefaultOptions() Options {
	return Options{
		Execute:          false,
		Spec:             specs.Ubuntu2404(),
		UID:              defaultUID(),
		WatchdogTimer:    true, // default: install all timers
		ReverseWatchdog:  true,
		RunnerWatchdog:   true,
		MetricsTimer:     true,
		RunReaper:        true,
		MemWatchdog:      true,
		InstallUnitsFrom: "", // assume admin ja copiou; pode setar pra automatizar
		OSReader:         defaultOSReader,
		RunFn:            defaultRun,
		WriteFileFn:      os.WriteFile,
		SHA256FileFn:     civm.FileSHA256,
	}
}

// Run executes every step (or simulates it when Execute=false).
func Run(ctx context.Context, opts Options) []Result {
	if opts.OSReader == nil {
		opts.OSReader = defaultOSReader
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.WriteFileFn == nil {
		opts.WriteFileFn = os.WriteFile
	}
	if opts.SHA256FileFn == nil {
		opts.SHA256FileFn = civm.FileSHA256
	}
	steps := buildSteps(opts)
	out := make([]Result, 0, len(steps))
	for _, s := range steps {
		r := Result{Name: s.Name, Description: s.Description}
		done, err := s.Check(ctx)
		if err != nil {
			r.Err = err
			out = append(out, r)
			if s.HardFail {
				return out
			}
			continue
		}
		r.AlreadyDone = done
		if done {
			out = append(out, r)
			continue
		}
		if !opts.Execute {
			r.WouldDo = true
			out = append(out, r)
			continue
		}
		if err := s.Apply(ctx); err != nil {
			r.Err = err
		} else {
			r.Executed = true
		}
		out = append(out, r)
	}
	return out
}

func buildSteps(opts Options) []Step {
	goVersion, _ := opts.Spec.FindTool("go")
	nodeVersion, _ := opts.Spec.FindTool("node")
	ghVersion, _ := opts.Spec.FindTool("gh")
	yqVersion, _ := opts.Spec.FindTool("yq")

	return []Step{
		{
			Name:        "verify_os",
			Description: "Confirma Ubuntu 24.04 LTS",
			HardFail:    true,
			Check: func(ctx context.Context) (bool, error) {
				out, err := opts.OSReader()
				if err != nil {
					return false, err
				}
				if strings.Contains(out, "VERSION_ID=\"24.04\"") {
					return true, nil
				}
				return false, fmt.Errorf("OS nao e Ubuntu 24.04: %s", firstLine(out))
			},
			Apply: func(ctx context.Context) error {
				return fmt.Errorf("verify_os e read-only; nao ha apply (instale Ubuntu 24.04)")
			},
		},
		{
			Name:        "verify_uid",
			Description: "Confirma execucao como root (UID=0)",
			HardFail:    true,
			Check: func(ctx context.Context) (bool, error) {
				if opts.UID == 0 {
					return true, nil
				}
				return false, fmt.Errorf("bootstrap exige sudo (UID atual=%d)", opts.UID)
			},
			Apply: func(ctx context.Context) error {
				return fmt.Errorf("rode novamente com sudo")
			},
		},
		{
			Name:        "apt_base_packages",
			Description: "Instala build-essential, curl, wget, jq, git, python3, ca-certificates",
			Check:       packagesInstalled(opts, "build-essential", "curl", "wget", "jq", "git", "python3", "python3-pip", "python3-venv", "ca-certificates"),
			Apply: func(ctx context.Context) error {
				if _, err := opts.RunFn(ctx, "apt-get", "update", "-y"); err != nil {
					return err
				}
				_, err := opts.RunFn(ctx, "apt-get", "install", "-y",
					"build-essential", "curl", "wget", "jq", "git", "python3", "python3-pip", "python3-venv", "ca-certificates",
					"gnupg", "lsb-release", "software-properties-common")
				return err
			},
		},
		{
			Name:        "install_go",
			Description: "Instala Go " + goVersion.Preferred() + " em /usr/local/go",
			Check: func(ctx context.Context) (bool, error) {
				out, err := opts.RunFn(ctx, "go", "version")
				if err != nil {
					return false, nil
				}
				want := goVersion.Preferred()
				return strings.Contains(string(out), "go"+want), nil
			},
			Apply: func(ctx context.Context) error {
				return installGoTarball(ctx, opts, goVersion.Preferred())
			},
		},
		{
			Name:        "install_node",
			Description: "Instala Node (any version; setup-node sobrepoe no job)",
			Check: func(ctx context.Context) (bool, error) {
				// Aceita qualquer versao instalada — peer workflows usam
				// actions/setup-node@v5 que sobrepoe no /opt/hostedtoolcache.
				_, err := opts.RunFn(ctx, "node", "--version")
				return err == nil, nil
			},
			Apply: func(ctx context.Context) error {
				return installNodeViaNodeSource(ctx, opts, nodeVersion.Preferred())
			},
		},
		{
			Name:        "install_docker",
			Description: "Instala Docker CE (any version; nunca downgrade)",
			Check: func(ctx context.Context) (bool, error) {
				// Aceita qualquer Docker funcional. Nunca fazer downgrade
				// se ja existe (poderia matar containers em execucao).
				_, err := opts.RunFn(ctx, "docker", "--version")
				return err == nil, nil
			},
			Apply: func(ctx context.Context) error {
				return installDockerCE(ctx, opts)
			},
		},
		{
			Name:        "install_gh",
			Description: "Instala GitHub CLI " + ghVersion.Preferred(),
			Check: func(ctx context.Context) (bool, error) {
				out, err := opts.RunFn(ctx, "gh", "--version")
				if err != nil {
					return false, nil
				}
				return strings.Contains(string(out), ghVersion.Preferred()), nil
			},
			Apply: func(ctx context.Context) error {
				return installGHCLI(ctx, opts)
			},
		},
		{
			Name:        "install_yq",
			Description: "Instala yq " + yqVersion.Preferred() + " em /usr/local/bin/yq",
			Check: func(ctx context.Context) (bool, error) {
				out, err := opts.RunFn(ctx, "yq", "--version")
				if err != nil {
					return false, nil
				}
				return strings.Contains(string(out), yqVersion.Preferred()), nil
			},
			Apply: func(ctx context.Context) error {
				return installYQBinary(ctx, opts, yqVersion.Preferred())
			},
		},
		{
			Name:        "install_systemd_timers",
			Description: "Instala timers: cleanup, disk-watchdog, runner-watchdog, reverse-watchdog e metrics",
			Check: func(ctx context.Context) (bool, error) {
				timers := timerList(opts)
				for _, t := range timers {
					out, err := opts.RunFn(ctx, "systemctl", "is-enabled", t)
					if err != nil || !strings.Contains(string(out), "enabled") {
						return false, nil
					}
				}
				return true, nil
			},
			Apply: func(ctx context.Context) error {
				if opts.InstallUnitsFrom != "" {
					if err := copySystemdUnits(ctx, opts); err != nil {
						return err
					}
					if err := copyDeployScripts(ctx, opts); err != nil {
						return err
					}
				}
				if _, err := opts.RunFn(ctx, "systemctl", "daemon-reload"); err != nil {
					return err
				}
				timers := timerList(opts)
				for _, t := range timers {
					// Idempotente: enable --now em timer ja-enabled e no-op
					if _, err := opts.RunFn(ctx, "systemctl", "enable", "--now", t); err != nil {
						return fmt.Errorf("enable %s: %w", t, err)
					}
				}
				return nil
			},
		},
	}
}

// timerList retorna lista de systemd timers a instalar conforme opts.
func timerList(opts Options) []string {
	// civmctl-buildcache-prune.timer e hygiene base (sempre ligada): um `docker
	// builder prune -af` frequente e BuildKit-safe que roda SEM deferir ao
	// docker-heavy-lock, fechando a lacuna do acumulo de build cache DENTRO de um
	// build pesado (o disk-watchdog defere ao heavy-lock e deixa o disco encher).
	timers := []string{"civmctl-cleanup.timer", "civmctl-buildcache-prune.timer"}
	if opts.WatchdogTimer {
		timers = append(timers, "civmctl-disk-watchdog.timer")
	}
	if opts.RunnerWatchdog {
		timers = append(timers, "civmctl-runner-watchdog.timer")
	}
	if opts.ReverseWatchdog {
		timers = append(timers, "civmctl-reverse-watchdog.timer")
	}
	if opts.MetricsTimer {
		timers = append(timers, "civmctl-metrics.timer")
	}
	if opts.RunReaper {
		timers = append(timers, "civmctl-run-reaper.timer")
	}
	if opts.MemWatchdog {
		timers = append(timers, "civmctl-mem-watchdog.timer")
	}
	return timers
}

func copySystemdUnits(ctx context.Context, opts Options) error {
	source, err := civm.CleanDir(opts.InstallUnitsFrom, "--install-units-from")
	if err != nil {
		return err
	}
	var units []string
	for _, pattern := range []string{"civmctl-*.service", "civmctl-*.timer"} {
		matches, err := filepath.Glob(filepath.Join(source, pattern))
		if err != nil {
			return fmt.Errorf("glob units from %s: %w", source, err)
		}
		units = append(units, matches...)
	}
	if len(units) == 0 {
		return fmt.Errorf("nenhuma unit civmctl-*.service/timer em %s", source)
	}
	for _, unit := range units {
		dst := filepath.Join(civm.DefaultSystemdDir, filepath.Base(unit))
		if _, err := opts.RunFn(ctx, "cp", unit, dst); err != nil {
			return fmt.Errorf("cp %s %s: %w", unit, dst, err)
		}
	}
	return nil
}

// copyDeployScripts instala os scripts de deploy/bin (irmao de deploy/systemd)
// em /usr/local/bin com +x. As units systemd referenciam esses scripts por
// caminho absoluto (ExecStart=/usr/local/bin/X.sh) e tem ConditionPathExists no
// mesmo path; sem instala-los, o systemd PULA a unit silenciosamente — foi o que
// deixou o civmctl-buildcache-prune virar no-op (cache nunca podado). Idempotente
// (`install -m 0755`); tolerante quando o dir bin nao acompanha as units (glob
// vazio -> nada a copiar). Ver deploy/systemd/README.md.
func copyDeployScripts(ctx context.Context, opts Options) error {
	source, err := civm.CleanDir(opts.InstallUnitsFrom, "--install-units-from")
	if err != nil {
		return err
	}
	binSource := filepath.Join(filepath.Dir(source), "bin")
	scripts, err := filepath.Glob(filepath.Join(binSource, "*.sh"))
	if err != nil {
		return fmt.Errorf("glob deploy scripts from %s: %w", binSource, err)
	}
	localBin := filepath.Dir(civm.DefaultCivmctlPath) // /usr/local/bin
	for _, script := range scripts {
		dst := filepath.Join(localBin, filepath.Base(script))
		if _, err := opts.RunFn(ctx, "install", "-m", "0755", script, dst); err != nil {
			return fmt.Errorf("install %s %s: %w", script, dst, err)
		}
	}
	return nil
}

func packagesInstalled(opts Options, names ...string) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		args := append([]string{"-W", "-f", "${Package} ${Status}\n"}, names...)
		out, err := opts.RunFn(ctx, "dpkg-query", args...)
		if err != nil {
			return false, nil
		}
		txt := string(out)
		for _, n := range names {
			if !strings.Contains(txt, n+" install ok installed") {
				return false, nil
			}
		}
		return true, nil
	}
}

func installGoTarball(ctx context.Context, opts Options, version string) error {
	url := fmt.Sprintf("https://go.dev/dl/go%s.linux-amd64.tar.gz", version)
	tmp := "/tmp/go-" + version + ".tar.gz"
	if _, err := opts.RunFn(ctx, "curl", "-fsSL", "-o", tmp, url); err != nil {
		return err
	}
	expected, ok := civm.GoLinuxAMD64SHA256(version)
	if !ok {
		return fmt.Errorf("sha256 nao pinado para Go linux-amd64 %s", version)
	}
	actual, err := sha256File(opts, tmp)
	if err != nil {
		return fmt.Errorf("sha256 %s: %w", tmp, err)
	}
	if err := civm.VerifySHA256(actual, expected, "Go "+version+" linux-amd64"); err != nil {
		return err
	}
	if _, err := opts.RunFn(ctx, "rm", "-rf", "/usr/local/go"); err != nil {
		return err
	}
	if _, err := opts.RunFn(ctx, "tar", "-C", "/usr/local", "-xzf", tmp); err != nil {
		return err
	}
	_, err = opts.RunFn(ctx, "ln", "-sf", "/usr/local/go/bin/go", "/usr/local/bin/go")
	return err
}

func installNodeViaNodeSource(ctx context.Context, opts Options, version string) error {
	major := strings.SplitN(version, ".", 2)[0]
	url := fmt.Sprintf("https://deb.nodesource.com/setup_%s.x", major)
	tmp := "/tmp/nodesource-" + major + ".sh"
	if _, err := opts.RunFn(ctx, "curl", "-fsSL", "-o", tmp, url); err != nil {
		return err
	}
	expected, ok := civm.NodeSourceSetupSHA256(major)
	if !ok {
		return fmt.Errorf("sha256 nao pinado para NodeSource setup_%s.x", major)
	}
	actual, err := sha256File(opts, tmp)
	if err != nil {
		return fmt.Errorf("sha256 %s: %w", tmp, err)
	}
	if err := civm.VerifySHA256(actual, expected, "NodeSource setup_"+major+".x"); err != nil {
		return err
	}
	if _, err := opts.RunFn(ctx, "bash", tmp); err != nil {
		return err
	}
	_, err = opts.RunFn(ctx, "apt-get", "install", "-y", "nodejs")
	return err
}

func installDockerCE(ctx context.Context, opts Options) error {
	arch, err := commandOutputTrimmed(ctx, opts, "dpkg", "--print-architecture")
	if err != nil {
		return err
	}
	codename, err := ubuntuCodename(opts)
	if err != nil {
		return err
	}
	steps := [][]string{
		{"install", "-m", "0755", "-d", "/etc/apt/keyrings"},
		{"curl", "-fsSL", "https://download.docker.com/linux/ubuntu/gpg", "-o", "/tmp/docker.asc"},
	}
	for _, s := range steps {
		if _, err := opts.RunFn(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	if err := verifyKeyFingerprint(ctx, opts, "/tmp/docker.asc", civm.DefaultDockerGPGFingerprint, "Docker apt key"); err != nil {
		return err
	}
	if _, err := opts.RunFn(ctx, "install", "-m", "0644", "/tmp/docker.asc", "/etc/apt/keyrings/docker.asc"); err != nil {
		return err
	}
	source := fmt.Sprintf("deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu %s stable\n", arch, codename)
	if err := opts.WriteFileFn("/etc/apt/sources.list.d/docker.list", []byte(source), 0644); err != nil {
		return err
	}
	for _, s := range [][]string{
		{"apt-get", "update", "-y"},
		{"apt-get", "install", "-y", "docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin"},
	} {
		if _, err := opts.RunFn(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	return nil
}

func sha256File(opts Options, path string) (string, error) {
	fn := opts.SHA256FileFn
	if fn == nil {
		fn = civm.FileSHA256
	}
	return fn(path)
}

func ubuntuCodename(opts Options) (string, error) {
	out, err := opts.OSReader()
	if err != nil {
		return "", err
	}
	if v := osReleaseValue(out, "VERSION_CODENAME"); v != "" {
		return v, nil
	}
	if v := osReleaseValue(out, "UBUNTU_CODENAME"); v != "" {
		return v, nil
	}
	if osReleaseValue(out, "VERSION_ID") == "24.04" {
		return "noble", nil
	}
	return "", fmt.Errorf("nao foi possivel inferir codename Ubuntu em /etc/os-release")
}

func osReleaseValue(contents, key string) string {
	prefix := key + "="
	for _, line := range strings.Split(contents, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		value := strings.TrimPrefix(line, prefix)
		return strings.Trim(value, `"`)
	}
	return ""
}

func installGHCLI(ctx context.Context, opts Options) error {
	source := "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main\n"
	arch, err := commandOutputTrimmed(ctx, opts, "dpkg", "--print-architecture")
	if err != nil {
		return err
	}
	source = strings.ReplaceAll(source, "$(dpkg --print-architecture)", arch)
	const tmpKey = "/tmp/githubcli-archive-keyring.gpg"
	if _, err := opts.RunFn(ctx, "curl", "-fsSL", "https://cli.github.com/packages/githubcli-archive-keyring.gpg", "-o", tmpKey); err != nil {
		return err
	}
	if err := verifyKeyFingerprint(ctx, opts, tmpKey, civm.DefaultGitHubCLIGPGFingerprint, "GitHub CLI apt key"); err != nil {
		return err
	}
	if _, err := opts.RunFn(ctx, "install", "-m", "0644", tmpKey, "/usr/share/keyrings/githubcli-archive-keyring.gpg"); err != nil {
		return err
	}
	if err := opts.WriteFileFn("/etc/apt/sources.list.d/github-cli.list", []byte(source), 0644); err != nil {
		return err
	}
	for _, s := range [][]string{
		{"apt-get", "update", "-y"},
		{"apt-get", "install", "-y", "gh"},
	} {
		if _, err := opts.RunFn(ctx, s[0], s[1:]...); err != nil {
			return err
		}
	}
	return nil
}

func verifyKeyFingerprint(ctx context.Context, opts Options, path, expected, label string) error {
	out, err := opts.RunFn(ctx, "gpg", "--show-keys", "--with-colons", path)
	if err != nil {
		return fmt.Errorf("gpg fingerprint %s: %w", path, err)
	}
	return civm.VerifyGPGFingerprint(string(out), expected, label)
}

func installYQBinary(ctx context.Context, opts Options, version string) error {
	url := fmt.Sprintf("https://github.com/mikefarah/yq/releases/download/v%s/yq_linux_amd64", version)
	tmp := "/tmp/yq-" + version + "-linux-amd64"
	if _, err := opts.RunFn(ctx, "curl", "-fsSL", "-o", tmp, url); err != nil {
		return err
	}
	expected, ok := civm.YQLinuxAMD64SHA256(version)
	if !ok {
		return fmt.Errorf("sha256 nao pinado para yq linux-amd64 %s", version)
	}
	actual, err := sha256File(opts, tmp)
	if err != nil {
		return fmt.Errorf("sha256 %s: %w", tmp, err)
	}
	if err := civm.VerifySHA256(actual, expected, "yq "+version+" linux-amd64"); err != nil {
		return err
	}
	_, err = opts.RunFn(ctx, "install", "-m", "0755", tmp, "/usr/local/bin/yq")
	return err
}

func commandOutputTrimmed(ctx context.Context, opts Options, name string, args ...string) (string, error) {
	out, err := opts.RunFn(ctx, name, args...)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(out))
	if value == "" {
		return "", fmt.Errorf("%s returned empty output", name)
	}
	return value, nil
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// RenderTable writes a human-readable table.
func RenderTable(results []Result, opts Options, w io.Writer) {
	mode := "DRY-RUN"
	if opts.Execute {
		mode = "EXECUTE"
	}
	fmt.Fprintf(w, "Modo: %s\n", mode)
	fmt.Fprintf(w, "Spec alvo: %s %s (%s)\n\n", opts.Spec.OSDistro, opts.Spec.OSVersion, opts.Spec.UpstreamURL)
	fmt.Fprintf(w, "%-22s %-50s %s\n", "STEP", "DESCRICAO", "STATUS")
	fmt.Fprintln(w, strings.Repeat("-", 90))
	doneCount, applyCount, errCount := 0, 0, 0
	for _, r := range results {
		status := "?"
		switch {
		case r.Err != nil:
			status = "erro: " + truncate(r.Err.Error(), 30)
			errCount++
		case r.AlreadyDone:
			status = "ja-instalado"
			doneCount++
		case r.Executed:
			status = "aplicado"
			applyCount++
		case r.WouldDo:
			status = "(seria-aplicado)"
		}
		fmt.Fprintf(w, "%-22s %-50s %s\n", r.Name, truncate(r.Description, 50), status)
	}
	fmt.Fprintln(w, strings.Repeat("-", 90))
	fmt.Fprintf(w, "Resumo: %d ja-instalados, %d aplicados, %d erros\n", doneCount, applyCount, errCount)
	if !opts.Execute && errCount == 0 {
		fmt.Fprintln(w, "Para aplicar: rode novamente com sudo + --execute")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// ---- defaults ----

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}
