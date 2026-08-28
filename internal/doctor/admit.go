package doctor

import (
	"context"
	"fmt"
	"strings"

	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/memwatchdog"
)

// cgroupControllersPath lists the cgroup v2 controllers available at the root.
// Its presence proves a cgroup2fs mount; "memory" in it proves the controller
// `admit`'s MemoryMax relies on is delegated (SPECv3 ITEM-7).
const cgroupControllersPath = "/sys/fs/cgroup/cgroup.controllers"

// meminfoPath is the source for the RAM-fit invariant.
const meminfoPath = "/proc/meminfo"

// admitProbeMemMB is the tiny MemoryMax used by the run-as-user probe service.
const admitProbeMemMB = 64

// collectAdmitChecks reports the three SPECv3 ITEM-7 admit preconditions as host
// checks: cgroup v2 + memory controller, the systemd-run-as-runner-user
// capability (DT-v3-1), and the RAM-fit invariant MaxHeavy×effMB <= MemTotal−host.
func collectAdmitChecks(ctx context.Context, opts Options) ([]HostCheck, Severity) {
	user := admitRunnerUser()
	checks := []HostCheck{
		checkAdmitCgroup(opts),
		checkAdmitRunAsUser(ctx, opts, user),
		checkAdmitRAMInvariant(opts),
	}
	worst := SeverityOK
	for _, c := range checks {
		worst = maxSeverity(worst, Severity(c.Severity))
	}
	return checks, worst
}

// checkAdmitCgroup verifies cgroup v2 with the memory controller. Absence is a
// WARNING, not critical: `admit` degrades to a watchdog-gated count limiter
// without the cgroup cap (SPECv3 DT-v3-6).
func checkAdmitCgroup(opts Options) HostCheck {
	data, err := opts.ReadFileFn(cgroupControllersPath)
	if err != nil {
		return HostCheck{
			Name:     "ADMIT_CGROUP",
			Severity: string(SeverityWarning),
			Detail:   fmt.Sprintf("cgroup v2 ausente (%v); admit opera em modo degradado watchdog-gated (DT-v3-6)", err),
		}
	}
	if !hasController(string(data), "memory") {
		return HostCheck{
			Name:     "ADMIT_CGROUP",
			Severity: string(SeverityWarning),
			Detail:   "cgroup v2 presente mas sem controller memory; admit degrada para watchdog-gated (DT-v3-6)",
		}
	}
	return HostCheck{
		Name:     "ADMIT_CGROUP",
		Severity: string(SeverityOK),
		Detail:   "cgroup v2 com controller memory (MemoryMax enforçável)",
	}
}

// checkAdmitRunAsUser proves the DT-v3-1 mechanism: a transient systemd SERVICE
// (`sudo -n systemd-run --pipe --wait -p User=<user> -p MemoryMax=64M -- id
// -un`) runs as the runner user, NOT root. root is CRITICAL (it would write
// root-owned _work and wedge "Complete runner" — the exact v2 footgun). A probe
// command failure is a WARNING (capability gone / no systemd-run).
func checkAdmitRunAsUser(ctx context.Context, opts Options, user string) HostCheck {
	out, err := opts.RunFn(ctx, "sudo", "-n", "systemd-run", "--pipe", "--wait",
		"-p", "User="+user, "-p", fmt.Sprintf("MemoryMax=%dM", admitProbeMemMB),
		"-p", "MemorySwapMax=0", "--", "id", "-un")
	if err != nil {
		return HostCheck{
			Name:     "ADMIT_RUN_AS_USER",
			Severity: string(SeverityWarning),
			Detail:   fmt.Sprintf("probe systemd-run --pipe falhou (%v); admit não pode envelopar jobs ate corrigir NOPASSWD/systemd-run", err),
		}
	}
	got := strings.TrimSpace(string(out))
	if got != user {
		return HostCheck{
			Name:     "ADMIT_RUN_AS_USER",
			Severity: string(SeverityCritical),
			Detail:   fmt.Sprintf("systemd-run --pipe rodou como %q, esperado %q (NUNCA root — gravaria _work root-owned, DT-v3-1)", got, user),
		}
	}
	return HostCheck{
		Name:     "ADMIT_RUN_AS_USER",
		Severity: string(SeverityOK),
		Detail:   fmt.Sprintf("sudo systemd-run --pipe -p MemoryMax roda como %s (não root), cgroup enforça (DT-v3-1)", user),
	}
}

// checkAdmitRAMInvariant verifies MaxHeavy × effMB <= MemTotal − host (SPECv3
// §Constantes invariant). effMB is the calibrated HeavyMaxMB or the generous
// (MemTotal−host)/MaxHeavy. An unreadable meminfo is a WARNING (cannot verify).
func checkAdmitRAMInvariant(opts Options) HostCheck {
	mem, err := memwatchdog.Sample(memwatchdog.Options{
		MeminfoFn: func() (string, error) {
			b, rerr := opts.ReadFileFn(meminfoPath)
			return string(b), rerr
		},
	})
	if err != nil {
		return HostCheck{
			Name:     "ADMIT_RAM_INVARIANT",
			Severity: string(SeverityWarning),
			Detail:   fmt.Sprintf("não foi possível ler MemTotal (%v); invariante de RAM não verificado", err),
		}
	}
	effMB := admitEffectiveMemMB(mem.MemTotalMB)
	budget := mem.MemTotalMB - int64(civm.DefaultAdmitHostReserveMB)
	used := int64(civm.DefaultAdmitMaxHeavy) * effMB
	if used > budget {
		return HostCheck{
			Name:     "ADMIT_RAM_INVARIANT",
			Severity: string(SeverityCritical),
			Detail: fmt.Sprintf("MaxHeavy×effMB=%dMB > MemTotal−host=%dMB (overcommit; baixe MaxHeavy ou HeavyMaxMB)",
				used, budget),
		}
	}
	return HostCheck{
		Name:     "ADMIT_RAM_INVARIANT",
		Severity: string(SeverityOK),
		Detail: fmt.Sprintf("MaxHeavy(%d)×effMB(%dMB)=%dMB <= MemTotal−host=%dMB",
			civm.DefaultAdmitMaxHeavy, effMB, used, budget),
	}
}

// admitEffectiveMemMB mirrors the CLI's effMB: the calibrated HeavyMaxMB when
// set, else the generous (MemTotal−host)/MaxHeavy (DT-v3-5).
func admitEffectiveMemMB(memTotalMB int64) int64 {
	if civm.DefaultAdmitHeavyMaxMB > 0 {
		return civm.DefaultAdmitHeavyMaxMB
	}
	maxHeavy := civm.DefaultAdmitMaxHeavy
	if maxHeavy < 1 {
		maxHeavy = 1
	}
	return (memTotalMB - int64(civm.DefaultAdmitHostReserveMB)) / int64(maxHeavy)
}

// admitRunnerUser returns the runner user the probe should assert: SUDO_USER
// when doctor runs under sudo, else the current process user (DT-v3-1).
func admitRunnerUser() string {
	return civm.ResolveRunnerUser("emdev")
}

// hasController reports whether the cgroup.controllers blob lists name.
func hasController(blob, name string) bool {
	for _, f := range strings.Fields(blob) {
		if f == name {
			return true
		}
	}
	return false
}
