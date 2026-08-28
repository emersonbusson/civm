// Package hook implements GitHub Actions self-hosted runner job hooks.
// Runtime is dispatched by small runner hook scripts into civmctl. The policy
// lives here so it is testable.
package hook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/emersonbusson/civm/internal/cachetrim"
	"github.com/emersonbusson/civm/internal/civm"
	"github.com/emersonbusson/civm/internal/hostdisk"
	"github.com/emersonbusson/civm/internal/idle"
	"github.com/emersonbusson/civm/internal/safedelete"
)

type Event string

const (
	EventJobStarted   Event = "job-started"
	EventJobCompleted Event = "job-completed"
)

type Decision string

const (
	DecisionOK             Decision = "ok"
	DecisionCleanupApplied Decision = "cleanup-applied"
	DecisionRejected       Decision = "rejected"
	DecisionError          Decision = "error"
	// DecisionCleanupDegraded marks a job-completed whose post-job cleanup
	// failed. The failure stays visible (action errors in hooks.jsonl feed the
	// runner-watchdog broken-runner sentinel) but the hook exits 0: post-job
	// hygiene must never flip a finished job's result — the 2026-06-10 incident
	// turned a green Web CI red over a work_root leftover at "Complete runner".
	DecisionCleanupDegraded Decision = "cleanup-degraded"
)

type Action struct {
	Name       string `json:"name"`
	Path       string `json:"path,omitempty"`
	Executed   bool   `json:"executed"`
	BytesFound int64  `json:"bytes_found,omitempty"`
	BytesFreed int64  `json:"bytes_freed,omitempty"`
	Error      string `json:"error,omitempty"`
	Warning    string `json:"warning,omitempty"`
}

type Result struct {
	Event       Event    `json:"event"`
	Decision    Decision `json:"decision"`
	ExitCode    int      `json:"exit_code"`
	Repository  string   `json:"repository,omitempty"`
	RunID       string   `json:"run_id,omitempty"`
	DiskUsedPct int      `json:"disk_used_pct"`
	// WorkRoot is the runner's _work directory; it identifies WHICH runner
	// emitted this record on the shared hooks.jsonl, so the runner watchdog can
	// map a broken-runner sentinel to the right systemd unit (RF-6 / ITEM-10).
	WorkRoot string   `json:"work_root,omitempty"`
	Actions  []Action `json:"actions,omitempty"`
	Error    string   `json:"error,omitempty"`
	// HostLevel/HostVFreeGB carry the Hyper-V host volume state read from the
	// delivered host-metrics snapshot. The guest-% gate above (DiskUsedPct) does
	// not see host pressure: the guest can be comfortable while the host V: volume
	// already crossed a floor (the 2026-06 PausedCritical incident). Surfaced on
	// hooks.jsonl for the watchdog/observability.
	HostLevel   string `json:"host_level,omitempty"`
	HostVFreeGB int64  `json:"host_v_free_gb,omitempty"`
}

type Options struct {
	Event           Event
	Execute         bool
	PreCleanupPct   int
	HardFailPct     int
	MinFreeGB       int
	FilesystemPath  string
	WorkRoot        string
	RunnerTemp      string
	GitHubWorkspace string
	Repository      string
	RunID           string
	// ComposeProject é o COMPOSE_PROJECT_NAME/slot deste runner, injetado no .env
	// pelo `hook install` (CIVM_RUNNER_SLOT). O job-completed o usa para escopar o
	// reap das imagens de run ao compose project deste runner — box-único, então um
	// sibling nunca é tocado. Vazio (env degradado) → o reap vira no-op (fail-safe:
	// sem escopo seguro, não se reapa imagem taggeada).
	ComposeProject string
	LogPath        string
	Now            time.Time
	RunFn          func(ctx context.Context, name string, args ...string) ([]byte, error)
	RemoveAllFn    func(path string) error
	// SafeWorkDeleteFn removes one top-level _work entry, escalating to the
	// privileged wrapper only when a root-owned file (a containerized CI step
	// ran as root and wrote into the mounted _work) blocks the unprivileged
	// delete. Without it, EACCES on a root-owned leftover fails job-completed at
	// "Complete runner" and wedges every later job on this runner. The GuardFn
	// scopes the escalation to a direct child of a safeWorkRoot. Injected so
	// unit tests never call real sudo (DT-v2-1/3/9).
	SafeWorkDeleteFn func(ctx context.Context, path string) safedelete.Result
	// SafeWorkChownFn recursively chowns a reused checkout dir back to the runner
	// user at job-started, so actions/checkout can clean a prior job's root-owned
	// (Docker-as-root) leftover instead of dying with EACCES. Non-destructive.
	// Injected so unit tests never call real sudo.
	SafeWorkChownFn func(ctx context.Context, path string) safedelete.Result
	MkdirAllFn      func(path string, perm os.FileMode) error
	StatfsFn        func(path string) (totalBytes, freeBytes uint64, err error)
	ReadDirFn       func(path string) ([]os.DirEntry, error)
	WalkDirFn       func(root string, fn fs.WalkDirFunc) error
	// HostDiskFn reads the Hyper-V host volume snapshot delivered to the guest and
	// classifies it (ok/warn/crit). The job-started gate uses it so host V:
	// pressure triggers cleanup/rejection even when the guest filesystem % alone
	// would not. Injected so unit tests never touch the metrics file.
	HostDiskFn func() (hostdisk.Report, error)
	// ActivityFn lista os builds de CI ativos no host (ps scan). O cache trim do
	// hook só roda quando NENHUM outro runner tem build ativo — trimar um cache
	// compartilhado que um sibling lê/escreve mid-install remove o arquivo em uso
	// (ENOENT). Injetado para teste; default idle.DefaultActivities.
	ActivityFn func(ctx context.Context) ([]idle.Activity, error)
}

func DefaultOptionsFromEnv(event Event) Options {
	return Options{
		Event:           event,
		PreCleanupPct:   civm.DefaultPreCleanupPct,
		HardFailPct:     civm.DefaultHardFailPct,
		MinFreeGB:       civm.DefaultMinFreeGB,
		FilesystemPath:  "/",
		RunnerTemp:      os.Getenv("RUNNER_TEMP"),
		GitHubWorkspace: os.Getenv("GITHUB_WORKSPACE"),
		Repository:      os.Getenv("GITHUB_REPOSITORY"),
		RunID:           os.Getenv("GITHUB_RUN_ID"),
		ComposeProject:  composeProjectFromEnv(),
		LogPath:         civm.DefaultHooksLogPath,
		Now:             time.Now(),
		RunFn:           defaultRun,
		RemoveAllFn:     os.RemoveAll,
		MkdirAllFn:      os.MkdirAll,
		StatfsFn:        defaultStatfs,
		ReadDirFn:       os.ReadDir,
		WalkDirFn:       filepath.WalkDir,
		HostDiskFn:      defaultHostDisk,
		ActivityFn:      idle.DefaultActivities,
	}
}

// defaultHostDisk reads + classifies the delivered host-metrics snapshot. A read
// error is never fatal to the hook: hostdisk.Check folds an absent/unreadable
// file into a fail-safe crit Report (Stale=true), which WantsCleanup but does
// NOT Block — so a missing metrics file forces cleanup without wedging CI.
func defaultHostDisk() (hostdisk.Report, error) {
	return hostdisk.Check(hostdisk.DefaultOptions())
}

// newSafeWorkDelete builds the hook-scoped safedelete closure. It routes the
// unprivileged remove through the hook's own RemoveAllFn (so an injected
// RemoveAllFn keeps capturing the happy path) and escalates to the guarded
// wrapper only for the root-owned _work case. The escalation can only ever
// target a direct child of a safeWorkRoot.
func newSafeWorkDelete(removeAllFn func(string) error) func(context.Context, string) safedelete.Result {
	return func(ctx context.Context, path string) safedelete.Result {
		return safedelete.Remove(ctx, safedelete.Options{
			GuardFn:     workChildGuard,
			RemoveAllFn: removeAllFn,
		}, path)
	}
}

// newSafeWorkChown builds the hook-scoped privileged chown closure used to
// reclaim ownership of a reused checkout dir at job-started (same workChildGuard
// scope as the delete closure). Non-destructive — it never removes files.
func newSafeWorkChown() func(context.Context, string) safedelete.Result {
	return func(ctx context.Context, path string) safedelete.Result {
		return safedelete.Chown(ctx, safedelete.Options{GuardFn: workChildGuard}, path)
	}
}

// workChildGuard rejects any path that is not a direct child of a valid
// safeWorkRoot. safeWorkRoot validates the ROOT (under /home, /actions-runner,
// trailing /_work); this adapter derives that root from the candidate and
// confirms the parent/child relation, symmetric to the cleanup guard (DT-v2-9).
func workChildGuard(path string) error {
	clean := filepath.Clean(path)
	parent := filepath.Dir(clean)
	if !safeWorkRoot(parent) {
		return fmt.Errorf("parent %q is not a safe _work root", parent)
	}
	if filepath.Base(clean) == "" {
		return fmt.Errorf("%q is not a direct child of a _work root", clean)
	}
	return nil
}

func Run(ctx context.Context, opts Options) Result {
	applyDefaults(&opts)
	res := Result{Event: opts.Event, Repository: opts.Repository, RunID: opts.RunID, WorkRoot: opts.WorkRoot, Decision: DecisionOK, ExitCode: 0}
	usedPct, err := diskUsedPct(opts)
	if err != nil {
		return finish(opts, errorResult(res, err))
	}
	res.DiskUsedPct = usedPct

	switch opts.Event {
	case EventJobStarted:
		// Host-aware (Bug A do incidente 2026-06): o gate guest-% não vê a pressão
		// do volume V: do host. Lê o snapshot de host-metrics entregue ao guest; um
		// erro de leitura vira um Report crit fail-safe (Stale=true) — força
		// cleanup, mas não bloqueia (telemetria ausente != disco cheio).
		host, _ := opts.HostDiskFn()
		res.HostLevel = host.Level
		res.HostVFreeGB = host.VFreeGB
		// Reclaim the reused checkout dir's ownership BEFORE actions/checkout runs,
		// so a prior job's root-owned (Docker-as-root) leftover does not make
		// checkout fail with EACCES. Unconditional + non-destructive.
		res.Actions = append(res.Actions, reclaimWorkspaceOwnership(ctx, opts)...)
		// Reapa containers de CI órfãos ANTES de o job subir o stack: um órfão de
		// um run anterior (job cancelado) OU de um runner REMOVIDO segura uma porta
		// fixa de host e o "Start local backend stack" do job morre com "port is
		// already allocated". Diferente de killWorkRootContainers (escopado ao
		// _work root deste runner), este reaper NÃO é escopado a um root — é o único
		// que pega o órfão de OUTRO runner. Best-effort, nunca trava o job.
		res.Actions = append(res.Actions, reapOrphanCIContainers(ctx, opts)...)
		// Cleanup dispara por pressão do guest-% OU do host V: (warn/crit). A
		// metade host-aware pega o caso que o guest-% perde: guest enxuto, V: cheio.
		freeGB, freeErr := diskFreeGB(opts)
		// Piso de espaco GUEST: abaixo de MinFreeGB (58) full-clean restaura ~58 antes
		// do build (cada PR limpo, sem cache). Soma aos gatilhos guest-% e host.
		if usedPct >= opts.PreCleanupPct || host.WantsCleanup() || (freeErr == nil && freeGB < opts.MinFreeGB) {
			res.Decision = DecisionCleanupApplied
			// Disk pressure mode: purga caches ($HOME/.cache/go-build, npm, etc.)
			// para liberar espaço suficiente.
			res.Actions = append(res.Actions, cleanup(opts, ctx, true)...)
			if usedAfter, err := diskUsedPct(opts); err == nil {
				res.DiskUsedPct = usedAfter
			}
			if err := firstActionError(res.Actions); err != nil {
				if onlyIgnorableCacheDeleteRaces(res.Actions) {
					demoteIgnorableCacheDeleteRaces(res.Actions)
				} else {
					return finish(opts, errorResult(res, err))
				}
			}
		}
		// Rejeita por hard-fail do guest OU host crit FRESCO (Blocks): V: realmente
		// cheio com telemetria atual. Snapshot stale nunca bloqueia (gap de infra,
		// não prova) — só forçou cleanup acima. O cleanup do guest ajuda o próximo
		// Optimize do host a recuperar; o block evita iniciar run e cair em
		// PausedCritical antes disso.
		switch {
		case res.DiskUsedPct >= opts.HardFailPct:
			res.Decision = DecisionRejected
			res.ExitCode = 75
			res.Error = fmt.Sprintf("disk usage %d%% >= hard fail threshold %d%%", res.DiskUsedPct, opts.HardFailPct)
		case host.Blocks():
			res.Decision = DecisionRejected
			res.ExitCode = 75
			res.Error = fmt.Sprintf("host V: free %dGB at crit floor (level=%s) — refusing job to avoid PausedCritical", host.VFreeGB, host.Level)
		}
	case EventJobCompleted:
		res.Decision = DecisionCleanupApplied
		// Lê o V: livre do host TAMBÉM no fim do job, não só no início. Pareado por
		// run_id com o host_v_free_gb do job-started, o par (start, completed) dá o
		// HIGH-WATER MEDIDO de dreno de V: por job: drain_gb = vfree@started −
		// vfree@completed. Antes, o dreno era ESTIMADO ("~35GB", inferido da taxa
		// ~22GB/h) — número-adjetivo sem medição (Kahneman #3/#5). Com os dois
		// extremos no hooks.jsonl, o p95 real do dreno passa a ser observável e o
		// floor do orchestrator deixa de depender de palpite. A leitura é best-effort
		// (snapshot ausente/stale vira Report fail-safe) e NUNCA falha o job.
		host, _ := opts.HostDiskFn()
		res.HostLevel = host.Level
		res.HostVFreeGB = host.VFreeGB
		// Modo rotineiro: preserva caches hot ($HOME/.cache/go-build, etc.)
		// para evitar invalidar build caches entre jobs concorrentes na VM.
		// Go build cache especialmente é caro de reconstruir (~minutos por
		// stdlib + deps), e wipe a cada job-completed quebrava lint
		// concorrente quando outro PR estava em fila.
		res.Actions = append(res.Actions, cleanup(opts, ctx, false)...)
		if err := firstActionError(res.Actions); err != nil {
			// Post-job hygiene must never fail the job it follows: the runner
			// marks the JOB failed when the completed-hook exits non-zero,
			// turning a green build red over a leftover that job-started's
			// chown and the runner-watchdog sentinel (work_root action error
			// in hooks.jsonl) already recover. Keep the error fully visible —
			// decision=cleanup-degraded, action errors preserved in the log —
			// but exit 0. (Supersedes the DT-v2-12 "stays fatal" stance after
			// the 2026-06-10 incident.)
			res.Decision = DecisionCleanupDegraded
			res.Error = err.Error()
		}
		if usedAfter, err := diskUsedPct(opts); err == nil {
			res.DiskUsedPct = usedAfter
		}
	default:
		return finish(opts, errorResult(res, fmt.Errorf("unsupported hook event %q", opts.Event)))
	}
	return finish(opts, res)
}

// cleanup orchestra a limpeza. purgeCaches=true em disk pressure mode
// remove os caches em $HOME (go-build, npm, yarn, pnpm) por inteiro e roda
// docker system prune agressivo. purgeCaches=false em modo rotineiro
// (job-completed) faz trim por tamanho/idade (preserva quentes <24h) e usa
// cacheTrimIsIdle reporta se é seguro trimar os caches compartilhados: true só
// quando NENHUM outro runner tem atividade. As atividades do próprio runner
// (Runner.Worker + o build deste job) são excluídas pelo prefixo do runner dir
// (ownDirs, já com separador final para não casar acme em acme-org). Fail-safe:
// sem probe, sem como excluir self, ou erro de probe → false (não trima).
func cacheTrimIsIdle(ctx context.Context, opts Options, ownDirs []string) bool {
	if opts.ActivityFn == nil || len(ownDirs) == 0 {
		return false
	}
	acts, err := opts.ActivityFn(ctx)
	if err != nil {
		return false
	}
	for _, a := range acts {
		own := false
		for _, dir := range ownDirs {
			if strings.Contains(a.Command, dir) {
				own = true
				break
			}
		}
		if !own {
			return false
		}
	}
	return true
}

// buildx prune mais brando — evita invalidar cache de jobs concorrentes.
func cleanup(opts Options, ctx context.Context, purgeCaches bool) []Action {
	roots := workRoots(opts)
	caps := cacheCaps()
	// Slack de 10: buildx + image_prune + 2 reap scopes + container/volume prune +
	// apt + journal + fstrim + cache_trim_deferred.
	// Por root: kill containers + cleanWorkRoot + workspace_stub.
	estCap := 3*len(roots) + len(caps) + 10
	actions := make([]Action, 0, estCap)
	for _, root := range roots {
		// Kill orphan containers BEFORE deleting the root: a cancelled job's
		// compose stack keeps running after the job ends, bind-mounted into
		// _work — it makes RemoveAll fail ENOTEMPTY (concurrent writer), fills
		// the disk with logs, and holds deleted-but-open files (the 2026-06-10
		// guest wedge freed ~44GB on reboot). Any running container mounted
		// under THIS runner's root at a hook boundary is an orphan by
		// definition: the runner executes one job at a time and sibling
		// runners mount their own roots.
		actions = append(actions, killWorkRootContainers(ctx, opts, root))
		actions = append(actions, cleanWorkRoot(ctx, opts, root, purgeCaches))
		// Apos wipe do _work/<owner>, o runner tenta job-started com
		// WorkingDirectory=_work/owner/repo. Se o dir nao existe, o Process.Start
		// do bash falha ANTES do hook (CI Router 9s fail, 2026-07-09). Stub vazio
		// restaura o cwd esperado sem reintroduzir conteudo de checkout.
		actions = append(actions, ensureWorkspaceStub(opts, root))
	}
	// O cache trim NUNCA roda enquanto OUTRO runner tem build ativo. O hook roda
	// sob o Runner.Worker do próprio job (ParseActiveProcesses só exclui o PID do
	// hook, não o Worker pai), então derivamos o runner dir dos nossos roots e
	// excluímos toda atividade sob ele — o que sobra é build de outro runner.
	// Trimar o cache compartilhado que um sibling lê/escreve mid-install remove o
	// arquivo em uso → ENOENT (gates/yarn-audit, confirmado na CI real). Sob
	// pressão real o disk-watchdog (idle-gated) + o EmergencyBypass (com floor)
	// cuidam do limite; aqui o fail-safe é NÃO trimar na dúvida.
	ownDirs := make([]string, 0, len(roots))
	for _, r := range roots {
		ownDirs = append(ownDirs, filepath.Dir(r)+string(os.PathSeparator))
	}
	cleanupIsIdle := cacheTrimIsIdle(ctx, opts, ownDirs)
	if cleanupIsIdle {
		for _, c := range caps {
			if !purgeCaches {
				actions = append(actions, purgeCacheAtIdle(opts, c.path))
				continue
			}
			actions = append(actions, trimCacheByAge(opts, c.path, c.maxBytes, c.minProtect))
		}
	} else {
		actions = append(actions, Action{Name: "cache_trim_deferred_sibling_build", Path: "(another runner's build is active)"})
	}
	// Docker prune is always best-effort (commandActionWarn, never fatal) in
	// both modes. Two concurrency hazards on the shared runner:
	//
	//  1. `docker system prune --volumes` unfiltered content GC corrupts a
	//     concurrent `docker pull` on a sibling job ("unable to lease content:
	//     lease does not exist") — so we never run it here.
	//  2. `docker image prune -a` removes ALL unused TAGGED images. The age
	//     `--filter until=` matches on image CREATED date (the vendor build
	//     date), not pull date, so a recently-pulled but old vendor image
	//     (redis, minio, alpine, clamav, postgres base) is deleted out from
	//     under a sibling job that is mid `compose up --build` — the sibling
	//     then fails with "No such image". So job cleanup prunes DANGLING
	//     images only (`-f`, no `-a`): untagged superseded layers are safe to
	//     remove and are the bulk of build churn, while tagged images another
	//     job pulled or built are never touched.
	//
	// Build cache (the largest disk consumer) reaccumulates every run: one smoke
	// building ~15 service images generates ~17G of cache. Under REAL pressure
	// (purgeCaches=true at job-started, disk >= PreCleanupPct) we prune ALL unused
	// build cache (--all), not just the >24h slice: today's cache is exactly what
	// fills the disk, and the until=24h filter left it intact — so the box climbed
	// to HardFail and REJECTED the job (the 2026-06-16 wall: a single re-run drove
	// the guest 76%->95% mid-build). buildx prune touches ONLY build cache (never
	// tagged images, so no "No such image" race like image prune -a) and BuildKit
	// excludes records IN USE by a concurrent build — safe under load: it only
	// sacrifices a sibling's cache-HIT (rebuild from scratch), never correctness.
	// Routine mode (job-completed, purgeCaches=false) keeps the <24h cache so
	// back-to-back jobs still get cache-hits.
	if purgeCaches {
		actions = append(actions, commandActionWarn(opts, ctx, "docker_buildx_prune", "docker", "buildx", "prune", "--force", "--all"))
	} else {
		actions = append(actions, commandActionWarn(opts, ctx, "docker_buildx_prune", "docker", "buildx", "prune", "--force", "--filter", civm.DefaultDockerBuildxPruneFilter))
	}
	actions = append(actions, commandActionWarn(opts, ctx, "docker_image_prune", "docker", "image", "prune", "-f"))
	// Redução-na-FONTE (issue #137): em modo rotineiro (job-completed), além do
	// dangling-only acima, reapa as imagens TAGGEADAS que o próprio run buildou e
	// agora estão sem container. Filtra pelo compose project DESTE runner — o
	// `image prune -f` sozinho nunca tocava essas imagens de service (~35GB/job de
	// E2E) e elas acumulavam na rajada até o panic floor. Em modo pressão
	// (purgeCaches=true, job-started) o run mal começou e suas imagens ainda serão
	// usadas, então o reap por label NÃO roda ali.
	if !purgeCaches {
		actions = append(actions, reapRunContainers(opts, ctx)...)
		actions = append(actions, reapRunImages(opts, ctx)...)
		// O reap por label nao cobre imagens vendor nem imagens que o build
		// tagueou sem o label do compose. Quando nao existe sibling ativo, o
		// daemon inteiro pertence ao boundary concluido: remover toda imagem
		// unused restaura o clean-slate do proximo job. O mesmo gate protege
		// volumes nomeados, que `volume prune -f` preserva por default.
		if cleanupIsIdle {
			actions = append(actions, commandActionWarn(opts, ctx,
				"docker_image_prune_idle", "docker", "image", "prune", "-a", "-f"))
		}
	}
	actions = append(actions, commandActionWarn(opts, ctx, "docker_container_prune", "docker", "container", "prune", "-f"))
	if !purgeCaches && cleanupIsIdle {
		actions = append(actions, commandActionWarn(opts, ctx,
			"docker_volume_prune_idle", "docker", "volume", "prune", "-a", "-f"))
	} else {
		actions = append(actions, commandActionWarn(opts, ctx,
			"docker_volume_prune", "docker", "volume", "prune", "-f"))
	}
	// apt_clean, journal_vacuum and fstrim are opportunistic disk reclaim and
	// must also be best-effort. apt-get clean returns exit 100 when a sibling
	// job holds the dpkg/apt lock, and a fatal cleanup error at job-started
	// would reject the starting job. Never let job-started cleanup fail a job.
	actions = append(actions, commandActionWarn(opts, ctx, "apt_clean", "sudo", "apt-get", "clean"))
	actions = append(actions, commandActionWarn(opts, ctx, "journal_vacuum", "sudo", "journalctl", "--vacuum-time=1d"))
	actions = append(actions, commandActionWarn(opts, ctx, "fstrim", "sudo", "fstrim", "-av"))
	return actions
}

// composeProjectFromEnv lê a identidade de compose project deste runner do env do
// hook. O `.env` do runner (gravado por `hook install`) traz CIVM_RUNNER_SLOT e
// COMPOSE_PROJECT_NAME=<slot>; preferimos o slot estável e caímos no
// COMPOSE_PROJECT_NAME. Vazio → o reap de imagens vira no-op (fail-safe).
func composeProjectFromEnv() string {
	if slot := strings.TrimSpace(os.Getenv("CIVM_RUNNER_SLOT")); slot != "" {
		return slot
	}
	return strings.TrimSpace(os.Getenv("COMPOSE_PROJECT_NAME"))
}

// runImageReapScopes deriva os labels de compose project a reapar para este run.
// O consumidor usa tanto o slot bare (COMPOSE_PROJECT_NAME default) quanto
// `<slot>-<run_id>` (a forma per-run recomendada na multi-project-isolation), então
// reapamos ambos. Os dois são deste runner — box-único — nunca de um sibling.
// Sem project (env degradado) → nenhum escopo → sem reap.
func runImageReapScopes(opts Options) []string {
	project := strings.TrimSpace(opts.ComposeProject)
	if project == "" {
		return nil
	}
	scopes := []string{project}
	if run := strings.TrimSpace(opts.RunID); run != "" {
		scopes = append(scopes, project+"-"+run)
	}
	return scopes
}

// reapRunImages reapa as imagens taggeadas que o run que terminou buildou, uma
// chamada de `docker image prune -a -f --filter label=<project-label>=<scope>` por
// escopo. O `-a` aqui é SEGURO porque o filtro de label restringe ao compose
// project deste runner e o docker recusa remover imagem com container vivo — então
// nunca toca a imagem de um sibling (label diferente) nem uma base de vendor pull
// (sem label de compose). É a redução-na-FONTE da issue #137. Best-effort:
// commandActionWarn — falha vira Warning, jamais falha o job-completed que segue.
func reapRunImages(opts Options, ctx context.Context) []Action {
	scopes := runImageReapScopes(opts)
	if len(scopes) == 0 {
		// Env degradado: sem escopo seguro, não reapamos nada (vs. um prune sem
		// filtro que reabriria o "No such image" race cross-runner). Marca a
		// decisão para observabilidade no hooks.jsonl.
		return []Action{{Name: "docker_run_image_reap_skipped", Path: "(no compose project in env)"}}
	}
	actions := make([]Action, 0, len(scopes))
	for _, scope := range scopes {
		actions = append(actions, commandActionWarn(opts, ctx, "docker_run_image_reap",
			"docker", "image", "prune", "-a", "-f",
			"--filter", "label="+civm.DefaultDockerComposeProjectLabel+"="+scope))
	}
	return actions
}

// reapRunContainers remove containers vivos ou parados do compose project que
// acabou. O filtro por label do slot/run cobre serviços sem bind mount no _work,
// que killWorkRootContainers não enxerga, sem alcançar um sibling.
func reapRunContainers(opts Options, ctx context.Context) []Action {
	scopes := runImageReapScopes(opts)
	if len(scopes) == 0 {
		return []Action{{
			Name: "docker_run_container_reap_skipped",
			Path: "(no compose project in env)",
		}}
	}
	a := Action{Name: "docker_run_container_reap", Executed: opts.Execute}
	if !opts.Execute {
		return []Action{a}
	}
	cmdCtx, cancel := context.WithTimeout(ctx,
		time.Duration(civm.DefaultRoutineCleanupCmdTimeoutSecs)*time.Second)
	defer cancel()
	seen := make(map[string]struct{})
	var ids []string
	for _, scope := range scopes {
		out, err := opts.RunFn(cmdCtx, "docker", "ps", "-aq", "--filter",
			"label="+civm.DefaultDockerComposeProjectLabel+"="+scope)
		if err != nil {
			a.Warning = strings.TrimSpace(a.Warning + " list " + scope + ": " + err.Error())
			continue
		}
		for _, id := range strings.Fields(string(out)) {
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return []Action{a}
	}
	if _, err := opts.RunFn(cmdCtx, "docker", append([]string{"rm", "-f"}, ids...)...); err != nil {
		a.Warning = strings.TrimSpace(a.Warning + " remove: " + err.Error())
		return []Action{a}
	}
	a.Path = fmt.Sprintf("reaped %d compose container(s)", len(ids))
	return []Action{a}
}

func cleanWorkRoot(ctx context.Context, opts Options, root string, preserveActiveWorkspace bool) Action {
	a := Action{Name: "work_root", Path: root, Executed: opts.Execute}
	if !safeWorkRoot(root) {
		a.Error = "unsafe work root"
		return a
	}
	// At job-started the runner has already created the active job's
	// GITHUB_WORKSPACE under this root. Deleting it frees almost nothing but
	// breaks the starting job ("working directory ... No such file or
	// directory"). job-completed still cleans it once the job is done.
	protected := ""
	if preserveActiveWorkspace {
		protected = activeWorkspaceEntry(root, opts.GitHubWorkspace)
	}
	entries, err := opts.ReadDirFn(root)
	if err != nil {
		if os.IsNotExist(err) {
			return a
		}
		a.Error = err.Error()
		return a
	}
	for _, entry := range entries {
		name := entry.Name()
		if name == "_tool" || name == "_actions" {
			continue
		}
		// At job-started the runner has ALREADY created the active job's file
		// commands under _work/_temp/_runner_file_commands (save_state_*,
		// set_output); every later step writes through them. Deleting _temp
		// here killed actions/checkout with "Missing file at path ...
		// save_state" (civm#117 smoke, 2026-06-10, surfaced once host V: warn
		// began forcing cleanup on every job-started). job-completed still
		// cleans it — the job is done.
		if preserveActiveWorkspace && name == "_temp" {
			continue
		}
		if protected != "" && name == protected {
			continue
		}
		path := filepath.Join(root, name)
		if opts.Execute {
			// A CI Docker step that ran as root may have written files into the
			// mounted _work that this user cannot unlink (EACCES on unlinkat).
			// safedelete tries the unprivileged remove first and escalates to
			// the guarded wrapper only for that root-owned case, so the runner
			// never wedges at "Complete runner". A terminal error (escalation
			// itself unavailable) surfaces here and stays fatal at job-completed
			// — it is never silently swallowed (DT-v2-1/3/12).
			if res := opts.SafeWorkDeleteFn(ctx, path); res.Err != nil {
				a.Error = res.Err.Error()
				return a
			}
		}
	}
	return a
}

// ensureWorkspaceStub recreates an empty _work/<owner>/<repo> so the next
// job's hooks can start. The Actions runner sets the hook process WorkingDirectory
// to GITHUB_WORKSPACE; if that path is missing after cleanWorkRoot / guest full
// clean, bash fails with "No such file or directory" before the hook script runs.
func ensureWorkspaceStub(opts Options, root string) Action {
	a := Action{Name: "workspace_stub", Path: root, Executed: opts.Execute}
	if !safeWorkRoot(root) {
		a.Error = "unsafe work root"
		return a
	}
	repo := strings.TrimSpace(opts.Repository)
	if repo == "" && strings.TrimSpace(opts.GitHubWorkspace) != "" {
		// Fallback: last two segments of GITHUB_WORKSPACE (.../_work/owner/repo).
		rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(opts.GitHubWorkspace))
		if err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			repo = filepath.ToSlash(rel)
		}
	}
	parts := strings.Split(filepath.ToSlash(repo), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		// Generic stub when env is empty after full clean (org runners often
		// lack GITHUB_REPOSITORY until the job sets it). Not a product name.
		parts = []string{"workspace", "job"}
	}
	path := filepath.Join(root, parts[0], parts[1])
	a.Path = path
	if opts.Execute {
		if err := opts.MkdirAllFn(path, 0o755); err != nil {
			a.Error = err.Error()
		}
	}
	return a
}

// activeWorkspaceEntry returns the top-level entry under root that contains the
// active GITHUB_WORKSPACE, or "" when workspace is empty or not under root.
// Example: root=.../_work, ws=.../_work/acme/app -> "acme". Used at
// job-started so disk-pressure cleanup never deletes the workspace the runner
// just created for the starting job.
func activeWorkspaceEntry(root, workspace string) string {
	if strings.TrimSpace(workspace) == "" {
		return ""
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(workspace))
	if err != nil {
		return ""
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	parts := strings.SplitN(rel, string(filepath.Separator), 2)
	if parts[0] == "" {
		return ""
	}
	return parts[0]
}

// killWorkRootContainers kills running containers that still bind-mount a path
// under this runner's _work root. They are orphans by definition: the runner
// executes one job at a time, sibling runners mount their own roots, and at a
// hook boundary the job that started them is over (typically a cancelled job's
// docker compose stack, which the runner does not tear down). Orphans make
// cleanWorkRoot fail ENOTEMPTY (live writer racing RemoveAll), fill the disk
// with logs, and pin deleted-but-open files. Best-effort: every failure is a
// warning, never fatal — docker being down must not wedge the hook.
func killWorkRootContainers(ctx context.Context, opts Options, root string) Action {
	a := Action{Name: "docker_kill_workroot", Path: root, Executed: opts.Execute}
	if !opts.Execute {
		return a
	}
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(civm.DefaultRoutineCleanupCmdTimeoutSecs)*time.Second)
	defer cancel()
	out, err := opts.RunFn(cmdCtx, "docker", "ps", "-q")
	if err != nil {
		a.Warning = "docker ps: " + err.Error()
		return a
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return a
	}
	inspectArgs := append([]string{"inspect", "--format",
		`{{.Id}}{{range .Mounts}} {{.Source}}{{end}}`}, ids...)
	out, err = opts.RunFn(cmdCtx, "docker", inspectArgs...)
	if err != nil {
		a.Warning = "docker inspect: " + err.Error()
		return a
	}
	prefix := filepath.Clean(root) + string(filepath.Separator)
	var orphans []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		for _, src := range fields[1:] {
			// Path-segment match (never substring): the mount source must be
			// the root itself or live under it.
			if clean := filepath.Clean(src); clean == filepath.Clean(root) ||
				strings.HasPrefix(clean+string(filepath.Separator), prefix) {
				orphans = append(orphans, fields[0])
				break
			}
		}
	}
	if len(orphans) == 0 {
		return a
	}
	if _, err := opts.RunFn(cmdCtx, "docker", append([]string{"kill"}, orphans...)...); err != nil {
		a.Warning = fmt.Sprintf("docker kill %d orphan(s): %s", len(orphans), err.Error())
		return a
	}
	a.Path = fmt.Sprintf("%s (killed %d orphan container(s))", root, len(orphans))
	return a
}

// orphanInspectFormat é o template do `docker inspect` que emite, por container,
// uma linha "ID|projeto-compose|testcontainers|portas-host". O projeto vem do
// label com.docker.compose.project ("<no value>" quando ausente). O label
// org.testcontainers cobre Postgres órfãos que ficam vivos quando um go test é
// cancelado/OOM antes do Ryuk limpar. As host ports vêm de .HostConfig.PortBindings
// (o MAPA DE BINDINGS PEDIDO) — é ele que segura a alocação da porta e causa
// "port is already allocated", e fica populado mesmo quando .NetworkSettings.Ports
// vem vazio (contexto rootless). Usar '|' como separador evita ambiguidade com o
// espaço entre as portas.
const orphanInspectFormat = `{{.Id}}|{{index .Config.Labels "com.docker.compose.project"}}|` +
	`{{index .Config.Labels "org.testcontainers"}}|` +
	`{{range $p, $b := .HostConfig.PortBindings}}{{range $b}}{{.HostPort}} {{end}}{{end}}`

// reapOrphanCIContainers roda na fronteira job-started, ANTES de o job subir o
// stack, e força-derruba qualquer container de CI ÓRFÃO box-wide. Diferente de
// killWorkRootContainers (escopado ao _work root do próprio runner via
// bind-mount), este reaper NÃO é escopado a um root — e por isso é o único que
// pega o órfão de OUTRO runner (ex.: o repo-runner que foi REMOVIDO; seu _work
// root sumiu, então nenhum hook escopado o reapa) ou de um container que ficou
// segurando a porta sem o bind-mount esperado.
//
// INVARIANTE DE SEGURANÇA (por que não mata o stack do JOB ATUAL): o GitHub
// Actions dispara o hook job-started ANTES de qualquer step do job rodar — e o
// stack do job só sobe num step posterior ("Start local backend stack"). Logo,
// na fronteira job-started o stack do job atual AINDA NÃO EXISTE: todo container
// rodando que case o critério de órfão é, por construção, resíduo de um run/runner
// ANTERIOR. (Numa box de 1 runner que executa um job por vez, não há peer
// concorrente cujo stack pudéssemos matar por engano.)
//
// Best-effort: toda falha é Warning, nunca Error — higiene de job-started não
// pode falhar o job (docker fora do ar, inspect quebrado etc. só viram warning).
func reapOrphanCIContainers(ctx context.Context, opts Options) []Action {
	a := Action{Name: "docker_reap_orphan_ci", Executed: opts.Execute}
	if !opts.Execute {
		return []Action{a}
	}
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(civm.DefaultRoutineCleanupCmdTimeoutSecs)*time.Second)
	defer cancel()
	out, err := opts.RunFn(cmdCtx, "docker", "ps", "-q")
	if err != nil {
		a.Warning = "docker ps: " + err.Error()
		return []Action{a}
	}
	ids := strings.Fields(string(out))
	if len(ids) == 0 {
		return []Action{a}
	}
	inspectArgs := append([]string{"inspect", "--format", orphanInspectFormat}, ids...)
	out, err = opts.RunFn(cmdCtx, "docker", inspectArgs...)
	if err != nil {
		a.Warning = "docker inspect: " + err.Error()
		return []Action{a}
	}
	orphans := orphanIDsFromInspect(string(out))
	if len(orphans) == 0 {
		return []Action{a}
	}
	// stop (não kill): dá ao container a chance de soltar a porta limpo antes do
	// rm; --time 5 limita a espera. Um stop que falha ainda cai no rm -f.
	if _, err := opts.RunFn(cmdCtx, "docker", append([]string{"stop", "--time", "5"}, orphans...)...); err != nil {
		a.Warning = fmt.Sprintf("docker stop %d orphan(s): %s", len(orphans), err.Error())
	}
	if _, err := opts.RunFn(cmdCtx, "docker", append([]string{"rm", "-f"}, orphans...)...); err != nil {
		// rm -f que falha é o único caminho que deixa a porta presa; reporta, mas
		// sem falhar o job (o bring-up subsequente vai expor a colisão de novo se
		// realmente não liberou).
		a.Warning = strings.TrimSpace(a.Warning + fmt.Sprintf(" docker rm %d orphan(s): %s", len(orphans), err.Error()))
		return []Action{a}
	}
	a.Path = fmt.Sprintf("reaped %d orphan CI container(s)", len(orphans))
	return []Action{a}
}

// orphanIDsFromInspect faz o parse da saída do orphanInspectFormat e devolve os
// IDs órfãos. Função pura (sem I/O) para ser testável de forma hermética: cada
// linha vira (id, projeto, testcontainers, hostPorts) e isCIOrphan decide. A
// ordem da entrada é preservada, sem dedupe (o docker inspect já emite uma linha
// por ID único).
func orphanIDsFromInspect(inspectOut string) []string {
	var orphans []string
	for _, line := range strings.Split(inspectOut, "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}
		id := strings.TrimSpace(parts[0])
		project := strings.TrimSpace(parts[1])
		testcontainers := strings.TrimSpace(parts[2])
		ports := strings.Fields(parts[3])
		if id == "" {
			continue
		}
		if isCIOrphan(project, testcontainers, ports) {
			orphans = append(orphans, id)
		}
	}
	return orphans
}

// isCIOrphan é o predicado puro do reaper: um container é órfão de CI quando
//
//	(a) seu project compose começa com DefaultCIOrphanProjectPrefix ("acme")
//	    — sinal PRIMÁRIO; o `devctl ci up` nomeia o project "<slot>-<run_id>"
//	    com fallback "acme", e o compose committed usa name: acme, então o stack
//	    inteiro carrega esse prefixo independente das portas; OU
//	(b) é um container gerado por testcontainers (`org.testcontainers=true`),
//	    que é exclusivo de testes de CI neste runner e pode sobrar quando o
//	    processo Go morre por OOM/cancel antes do Ryuk limpar; OU
//	(c) ele publica uma das host ports FIXAS de CI conhecidas
//	    (DefaultCIFixedHostPorts) — defesa em profundidade p/ um container que
//	    segure a porta SEM o label esperado.
//
// O label "<no value>" do template (sem label) é tratado como string vazia, que
// não casa o prefixo. A comparação de project é por prefixo case-insensitive
// (nomes de project compose são lowercased).
func isCIOrphan(project, testcontainers string, hostPorts []string) bool {
	if p := strings.ToLower(strings.TrimSpace(project)); p != "" && p != "<no value>" &&
		strings.HasPrefix(p, civm.DefaultCIOrphanProjectPrefix) {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(testcontainers), "true") {
		return true
	}
	for _, hp := range hostPorts {
		port, err := strconv.Atoi(strings.TrimSpace(hp))
		if err != nil {
			continue
		}
		for _, fixed := range civm.DefaultCIFixedHostPorts {
			if port == fixed {
				return true
			}
		}
	}
	return false
}

// reclaimWorkspaceOwnership runs at job-started, BEFORE actions/checkout, and
// chowns the active job's REUSED checkout dir back to the runner user. A prior
// job's containerized (root) step can leave root-owned files in the same _work
// checkout path on this shared runner; actions/checkout then dies with "EACCES
// rmdir" cleaning the old tree. Previously a chronically-full disk ran the gated
// cleanup every job and masked this; with a healthy disk (the cache-cap fix) the
// gated cleanup is rare and the latent leftover surfaces. chown is
// non-destructive (the dir survives; checkout cleans it), so it runs EVERY
// job-started regardless of disk pressure — unlike the disk-gated,
// workspace-preserving cleanWorkRoot. Best-effort: a chown failure is a warning,
// never a wedge (it leaves us no worse than the checkout EACCES it prevents).
func reclaimWorkspaceOwnership(ctx context.Context, opts Options) []Action {
	if !opts.Execute {
		return nil
	}
	var actions []Action
	for _, root := range workRoots(opts) {
		ws := activeWorkspaceEntry(root, opts.GitHubWorkspace)
		if ws == "" {
			continue
		}
		path := filepath.Join(root, ws)
		a := Action{Name: "workspace_chown", Path: path, Executed: true}
		if res := opts.SafeWorkChownFn(ctx, path); res.Err != nil {
			a.Warning = res.Err.Error()
		}
		actions = append(actions, a)
	}
	return actions
}

func removePath(opts Options, path, name string) Action {
	a := Action{Name: name, Path: path, Executed: opts.Execute}
	if strings.TrimSpace(path) == "" || path == "/" || path == os.Getenv("HOME") {
		a.Error = "unsafe cache path"
		return a
	}
	if opts.Execute {
		if err := opts.RemoveAllFn(path); err != nil {
			a.Error = err.Error()
		}
	}
	return a
}

// cacheCap describes a per-cache size budget for routine trim.
// cacheCap mirrors cachetrim.Cap with this package's field names so the existing
// hook cleanup flow and tests keep their shape; the policy itself lives in the
// shared internal/cachetrim (one source of truth, also used by internal/cleanup).
type cacheCap struct {
	path       string
	maxBytes   int64
	minProtect time.Duration
}

// cacheCaps delegates to the shared cachetrim policy for the runner user's home.
// The hook runs AS the runner user, so $HOME is the right (single) home. The
// disk-pressure cleanup (internal/cleanup, runs as root) applies the SAME policy
// across every /home/* runner home — both go through internal/cachetrim, so the
// named-dir glob + family budget live in exactly one place.
func cacheCaps() []cacheCap {
	shared := cachetrim.Caps([]string{os.Getenv("HOME")}, cachetrim.Deps{})
	out := make([]cacheCap, len(shared))
	for i, c := range shared {
		out[i] = cacheCap{path: c.Path, maxBytes: c.MaxBytes, minProtect: c.MinProtect}
	}
	return out
}

// trimCacheByAge delegates one cache dir's age/size trim to cachetrim and maps
// the result to a hook Action.
func trimCacheByAge(opts Options, root string, maxBytes int64, minProtect time.Duration) Action {
	r := cachetrim.TrimByAge(cachetrim.Options{
		Execute:     opts.Execute,
		Now:         opts.Now,
		WalkDirFn:   opts.WalkDirFn,
		RemoveAllFn: opts.RemoveAllFn,
	}, cachetrim.Cap{Path: root, MaxBytes: maxBytes, MinProtect: minProtect})
	a := Action{Name: "cache_trim", Path: r.Path, BytesFound: r.BytesFound, BytesFreed: r.BytesFreed, Executed: r.Executed}
	if r.Err != nil {
		a.Error = r.Err.Error()
	}
	return a
}

// purgeCacheAtIdle remove um cache regeneravel inteiro somente no boundary de
// job-completed que ja provou ausencia de sibling. O path deve ser descendente
// estrito de HOME; env degradado ou path amplo falha fechado.
func purgeCacheAtIdle(opts Options, path string) Action {
	a := Action{Name: "cache_purge_idle", Path: path, Executed: opts.Execute}
	home := filepath.Clean(os.Getenv("HOME"))
	clean := filepath.Clean(path)
	rel, err := filepath.Rel(home, clean)
	if err != nil || home == "." || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		a.Error = "unsafe cache path outside HOME"
		return a
	}
	if !opts.Execute {
		return a
	}
	if err := opts.RemoveAllFn(clean); err != nil && !errors.Is(err, fs.ErrNotExist) {
		a.Error = err.Error()
	}
	return a
}

// commandActionWarn é a variante tolerante: falha de comando vira Warning,
// não Error. Usada no modo rotineiro (job-completed) para que ferramentas
// ausentes (docker buildx em hosts antigos, fstrim em FS sem suporte) não
// derrubem o hook. O cleanup é best-effort entre jobs; o que importa é o
// hook retornar exit 0 para o runner continuar normalmente.
func commandActionWarn(opts Options, ctx context.Context, actionName, name string, args ...string) Action {
	return runWithTimeout(opts, ctx, actionName, true /*errorAsWarning*/, name, args...)
}

// runWithTimeout aplica DefaultRoutineCleanupCmdTimeoutSecs por comando.
// Evita que um docker travado segure o hook durante todo o TimeoutStartSec
// do systemd (30 min). Cada comando tem orçamento próprio.
func runWithTimeout(opts Options, ctx context.Context, actionName string, errorAsWarning bool, name string, args ...string) Action {
	a := Action{Name: actionName, Executed: opts.Execute}
	if !opts.Execute {
		return a
	}
	cmdCtx, cancel := context.WithTimeout(ctx, time.Duration(civm.DefaultRoutineCleanupCmdTimeoutSecs)*time.Second)
	defer cancel()
	if _, err := opts.RunFn(cmdCtx, name, args...); err != nil {
		msg := err.Error()
		if errorAsWarning {
			a.Warning = msg
		} else {
			a.Error = msg
		}
	}
	return a
}

func workRoots(opts Options) []string {
	seen := map[string]struct{}{}
	var roots []string
	add := func(root string) {
		root = filepath.Clean(strings.TrimSpace(root))
		if safeWorkRoot(root) {
			if _, ok := seen[root]; !ok {
				seen[root] = struct{}{}
				roots = append(roots, root)
			}
		}
	}
	if opts.WorkRoot != "" {
		add(opts.WorkRoot)
	}
	if opts.RunnerTemp != "" {
		add(filepath.Dir(opts.RunnerTemp))
	}
	if opts.GitHubWorkspace != "" {
		add(filepath.Dir(filepath.Dir(opts.GitHubWorkspace)))
	}
	// NO global fallback. A hook may only ever touch ITS OWN runner's root,
	// derived from the job env above. On 2026-06-10, job-started hooks firing
	// with a degraded env (empty RUNNER_TEMP/GITHUB_WORKSPACE) fell back to
	// discovering ALL /home/*/actions-runner*/_work roots and — with no active
	// workspace to protect — deleted a sibling runner's checkout MID-JOB
	// (civm#117's go test lost its tree at 20:12:44Z). Sweeping every root
	// belongs to civmctl cleanup/disk-watchdog (root, idle-gated), never to a
	// per-job hook. Empty env → no roots → cleanup no-op (fail-safe).
	sort.Strings(roots)
	return roots
}

// workRootGlob is the single canonical shape of a runner _work root:
// /home/<user>/actions-runner*/_work. Discovery and the safeWorkRoot guard both
// match it via filepath.Match, whose '*' never crosses a path separator — a
// path-SEGMENT match, not a substring, so a decoy like
// /home/x/sub/actions-runnerEVIL/deep/_work cannot slip past the privileged
// delete guard (DT-v2-7; testing.md "guard text must match guard behavior").
const workRootGlob = "/home/*/actions-runner*/_work"

func safeWorkRoot(root string) bool {
	clean := filepath.Clean(root)
	if !filepath.IsAbs(clean) {
		return false
	}
	ok, err := filepath.Match(workRootGlob, clean)
	return err == nil && ok
}

// cachePaths deriva a lista de paths de cacheCaps() — fonte única de verdade.
// Usada pelo modo disk-pressure (wipe total) e por testes que validam o set.
func cachePaths() []string {
	caps := cacheCaps()
	if len(caps) == 0 {
		return nil
	}
	paths := make([]string, len(caps))
	for i, c := range caps {
		paths[i] = c.path
	}
	return paths
}

func diskUsedPct(opts Options) (int, error) {
	total, free, err := opts.StatfsFn(opts.FilesystemPath)
	if err != nil {
		return 0, err
	}
	if total == 0 {
		return 0, fmt.Errorf("statfs total = 0")
	}
	return int((total - free) * 100 / total), nil
}

// diskFreeGB retorna o espaco GUEST livre em GB (statfs). O job-started usa p/ o
// piso MinFreeGB: abaixo dele, full-clean restaura >=58 antes do build.
func diskFreeGB(opts Options) (int, error) {
	_, free, err := opts.StatfsFn(opts.FilesystemPath)
	if err != nil {
		return 0, err
	}
	return int(free / (1024 * 1024 * 1024)), nil
}

// JobVDrainGB é a definição CANÔNICA do dreno de V: por job: quanto de V: livre
// (GB no host) o job consumiu, medido como o high-water entre os dois extremos do
// hooks.jsonl — vfree no job-started menos vfree no job-completed. É o número que
// substitui a estimativa "~35GB" (Kahneman #3/#5: número MEDIDO, não adjetivo) e
// que calibra o floor do orchestrator (`floor >= host_crit + p95(drain) + safety`).
//
// vStartedGB/vCompletedGB são os `host_v_free_gb` dos dois registros pareados pelo
// mesmo run_id. Retorna (0, false) quando a medição NÃO é confiável — qualquer
// extremo <= 0 significa "não medi" (snapshot ausente/stale dobrado em fail-safe),
// e um par desses jamais deve virar um dreno fantasma de dezenas de GB. Um delta
// negativo (o V: SUBIU durante o job — um warn_clean/compact rodou no meio) é
// clampado a 0: aquele job não drenou líquido, e um dreno negativo poluiria o p95.
func JobVDrainGB(vStartedGB, vCompletedGB int64) (int64, bool) {
	if vStartedGB <= 0 || vCompletedGB <= 0 {
		return 0, false
	}
	drain := vStartedGB - vCompletedGB
	if drain < 0 {
		return 0, true
	}
	return drain, true
}

func finish(opts Options, res Result) Result {
	if res.ExitCode == 0 && (res.Decision == DecisionError || res.Decision == DecisionRejected) {
		res.ExitCode = 1
	}
	_ = appendLog(opts, res)
	return res
}

func errorResult(res Result, err error) Result {
	res.Decision = DecisionError
	res.ExitCode = 1
	if err != nil {
		res.Error = err.Error()
	}
	return res
}

func firstActionError(actions []Action) error {
	for _, a := range actions {
		if a.Error != "" {
			return fmt.Errorf("%s: %s", a.Name, a.Error)
		}
	}
	return nil
}

func onlyIgnorableCacheDeleteRaces(actions []Action) bool {
	hasIgnorable := false
	for _, a := range actions {
		if a.Error == "" {
			continue
		}
		if !isIgnorableCacheDeleteRace(a) {
			return false
		}
		hasIgnorable = true
	}
	return hasIgnorable
}

func demoteIgnorableCacheDeleteRaces(actions []Action) {
	for i := range actions {
		if isIgnorableCacheDeleteRace(actions[i]) {
			actions[i].Warning = actions[i].Error
			actions[i].Error = ""
		}
	}
}

func isIgnorableCacheDeleteRace(a Action) bool {
	if a.Name != "cache" && a.Name != "cache_trim" {
		return false
	}
	return strings.Contains(strings.ToLower(a.Error), "directory not empty")
}

func appendLog(opts Options, res Result) error {
	if opts.LogPath == "" || !opts.Execute {
		return nil
	}
	if err := opts.MkdirAllFn(filepath.Dir(opts.LogPath), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(opts.LogPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644) //nolint:gosec // G302: hook log intencionalmente world-readable para ops/observabilidade
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	logger := slog.New(slog.NewJSONHandler(f, &slog.HandlerOptions{Level: slog.LevelInfo}))
	level := slog.LevelInfo
	switch res.Decision {
	case DecisionError:
		level = slog.LevelError
	case DecisionRejected, DecisionCleanupDegraded:
		level = slog.LevelWarn
	}
	// host_v_free_gb + host_level entram no hooks.jsonl: são o ÚNICO traço
	// persistido do V: livre do host por job. Pareando os dois registros do mesmo
	// run_id (job-started e job-completed), o dreno de V: por job é MEDIDO
	// (high-water = vfree@started − vfree@completed) em vez de estimado. Sem estes
	// campos no log, o número de dreno que calibra o floor do orchestrator seria
	// reconstruível só por palpite (Kahneman #3: número antes de adjetivo).
	logger.LogAttrs(context.Background(), level, "hook event",
		slog.String("event", string(res.Event)),
		slog.String("decision", string(res.Decision)),
		slog.Int("exit_code", res.ExitCode),
		slog.Int("disk_used_pct", res.DiskUsedPct),
		slog.String("repository", res.Repository),
		slog.String("run_id", res.RunID),
		slog.String("work_root", res.WorkRoot),
		slog.String("host_level", res.HostLevel),
		slog.Int64("host_v_free_gb", res.HostVFreeGB),
		slog.String("error", res.Error),
		slog.Any("actions", res.Actions),
	)
	return nil
}

func RenderJSON(w io.Writer, res Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}

func RenderText(w io.Writer, res Result) {
	fmt.Fprintf(w, "civm hook %s: %s (exit %d, disk=%d%%)\n", res.Event, res.Decision, res.ExitCode, res.DiskUsedPct)
	if res.Error != "" {
		fmt.Fprintf(w, "Error: %s\n", res.Error)
	}
	for _, a := range res.Actions {
		status := "dry-run"
		if a.Executed {
			status = "ok"
		}
		if a.Error != "" {
			status = "error: " + a.Error
		} else if a.Warning != "" {
			status = "warn: " + a.Warning
		}
		fmt.Fprintf(w, "  %-14s %-50s %s\n", a.Name, a.Path, status)
	}
}

func applyDefaults(opts *Options) {
	if opts.PreCleanupPct == 0 {
		opts.PreCleanupPct = civm.DefaultPreCleanupPct
	}
	if opts.HardFailPct == 0 {
		opts.HardFailPct = civm.DefaultHardFailPct
	}
	// MinFreeGB NÃO é defaultado aqui: 0 = desabilitado (o gate `freeGB < MinFreeGB`
	// nunca dispara). Produção habilita via DefaultOptionsFromEnv (=58); testes que
	// montam Options manualmente ficam desabilitados por padrão (sem interferência).
	if opts.FilesystemPath == "" {
		opts.FilesystemPath = "/"
	}
	if opts.RunFn == nil {
		opts.RunFn = defaultRun
	}
	if opts.RemoveAllFn == nil {
		opts.RemoveAllFn = os.RemoveAll
	}
	if opts.SafeWorkDeleteFn == nil {
		opts.SafeWorkDeleteFn = newSafeWorkDelete(opts.RemoveAllFn)
	}
	if opts.SafeWorkChownFn == nil {
		opts.SafeWorkChownFn = newSafeWorkChown()
	}
	if opts.MkdirAllFn == nil {
		opts.MkdirAllFn = os.MkdirAll
	}
	if opts.StatfsFn == nil {
		opts.StatfsFn = defaultStatfs
	}
	if opts.ReadDirFn == nil {
		opts.ReadDirFn = os.ReadDir
	}
	if opts.WalkDirFn == nil {
		opts.WalkDirFn = filepath.WalkDir
	}
	if opts.HostDiskFn == nil {
		opts.HostDiskFn = defaultHostDisk
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
}

func defaultRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func defaultStatfs(path string) (uint64, uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	return uint64(st.Blocks) * uint64(st.Bsize), uint64(st.Bavail) * uint64(st.Bsize), nil
}
