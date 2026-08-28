# SPEC — civmctl implementation

**PRD:** `docs/specs/civmctl/PRD.md`
**Status:** approved
**Discipline links:** Kahneman #2 (counterfactual em cada step destrutivo),
#3 (numero antes de adjetivo nos logs).

## Emenda 2026-05-10 — hardening operacional

Este SPEC nasceu no bootstrap do `civmctl`; o hardening posterior adiciona:

- `cmd/civmctl/doctor.go` + `internal/doctor/`: diagnóstico read-only
  consolidado de host, timers, systemd runners e GitHub runners.
- `cmd/civmctl/idlecheck.go` + `internal/idle/`: detector compartilhado de
  host ocioso, com exit `0=idle`, `1=busy`, `2=unknown`.
- `internal/runner` passa a usar o detector antes de
  `restart/remove/upgrade --execute`.
- `internal/health` valida `civmctl-cleanup.timer`,
  `civmctl-disk-watchdog.timer`, `civmctl-runner-watchdog.timer` e
  `civmctl-reverse-watchdog.timer`; emenda posterior adiciona
  `civmctl-metrics.timer` como warning.
- `bootstrap`/`bootstrap-everything` expõem `--runner-watchdog=true` e
  `--reverse-watchdog=true`/`--metrics-timer=true`, habilitando os timers
  quando os unit files estão instalados.
- `civmctl runner watchdog` repara hooks, reinicia runner offline/failed em
  VM idle e, com `--rerun-network-failures --max-run-age=6h`, reroda uma
  vez falhas transientes de rede/checkout em PR aberto recente. O timer
  systemd padrão não passa `--rerun-network-failures`. Guard anti-loop:
  `/var/lib/civm/runner-watchdog-reruns.json`.
- Antes do restart, o watchdog congela a unit ativa, repete o probe de
  `Runner.Worker` e mata todos os processos do cgroup. Isso remove
  `Runner.Listener`/`RunnerService.js` órfãos que `KillMode=process` pode
  preservar após OOM; falha no purge bloqueia o restart.
- `runner watchdog --repos=auto` resolve repo pelo `.runner` do diretório
  real do service quando possível, preservando owners/repos com hífen, e usa
  o parser legado do unit name como fallback best-effort.
- Runners legacy offline são apenas reportados; remoção é manual via
  `gh api -X DELETE`.

## Arquivos a criar

### Modulo Go

| Path | LoC alvo | Função |
|---|---|---|
| `/home/emdev/codespace/civm/go.mod` | 3 | módulo `github.com/emersonbusson/civm`, Go 1.26 |
| `/home/emdev/codespace/civm/cmd/civmctl/main.go` | ≤120 | dispatch + help; só roteia |
| `/home/emdev/codespace/civm/cmd/civmctl/version_pins.go` | ≤40 | comando `version-pins` |
| `/home/emdev/codespace/civm/cmd/civmctl/health.go` | ≤80 | comando `health` |
| `/home/emdev/codespace/civm/cmd/civmctl/cleanup.go` | ≤100 | comando `cleanup` |
| `/home/emdev/codespace/civm/cmd/civmctl/bootstrap.go` | ≤120 | comando `bootstrap` |
| `/home/emdev/codespace/civm/cmd/civmctl/runner.go` | ~350 | comando `runner` (`add/list/remove/restart/upgrade/watchdog`) |
| `/home/emdev/codespace/civm/internal/specs/specs.go` | ≤120 | versões alvo |
| `/home/emdev/codespace/civm/internal/specs/specs_test.go` | ≤60 | testes |
| `/home/emdev/codespace/civm/internal/health/health.go` | ≤180 | lógica health |
| `/home/emdev/codespace/civm/internal/health/health_test.go` | ≤180 | testes |
| `/home/emdev/codespace/civm/internal/cleanup/cleanup.go` | ≤200 | lógica cleanup |
| `/home/emdev/codespace/civm/internal/cleanup/cleanup_test.go` | ≤180 | testes |
| `/home/emdev/codespace/civm/internal/bootstrap/bootstrap.go` | ≤220 | lógica bootstrap |
| `/home/emdev/codespace/civm/internal/bootstrap/bootstrap_test.go` | ≤180 | testes |
| `/home/emdev/codespace/civm/internal/runner/watchdog.go` | ~1000 | runner watchdog, rerun opt-in, inferência `.runner` e render JSON/texto |

### systemd

| Path | Função |
|---|---|
| `/home/emdev/codespace/civm/deploy/systemd/civmctl-cleanup.service` | unit que roda `civmctl cleanup --execute` |
| `/home/emdev/codespace/civm/deploy/systemd/civmctl-cleanup.timer` | timer diário 04:00 UTC |
| `/home/emdev/codespace/civm/deploy/systemd/civmctl-disk-watchdog.service` | unit que roda `civmctl disk-watchdog --execute` |
| `/home/emdev/codespace/civm/deploy/systemd/civmctl-disk-watchdog.timer` | timer hourly com cleanup agressivo se disk > DefaultPreCleanupPct (60%) |
| `/home/emdev/codespace/civm/deploy/systemd/civmctl-runner-watchdog.service` | unit que roda `civmctl runner watchdog --execute --repos=auto --json` |
| `/home/emdev/codespace/civm/deploy/systemd/civmctl-runner-watchdog.timer` | timer OnBootSec=2min, OnUnitActiveSec=2min |
| `/home/emdev/codespace/civm/deploy/systemd/civmctl-reverse-watchdog.service` | unit que alerta se disk-watchdog ficou stale |
| `/home/emdev/codespace/civm/deploy/systemd/civmctl-reverse-watchdog.timer` | timer 4h alarm-of-alarm |
| `/home/emdev/codespace/civm/deploy/systemd/civmctl-metrics.service` | unit que grava metrics Prometheus textfile |
| `/home/emdev/codespace/civm/deploy/systemd/civmctl-metrics.timer` | timer minutely para metrics read-only |
| `/home/emdev/codespace/civm/deploy/systemd/README.md` | instalação manual: `cp` + `systemctl enable --now` |

### Documentação

| Path | Mudança |
|---|---|
| `/home/emdev/codespace/civm/README.md` | adicionar seção "Bootstrap em 1 comando" |
| `/home/emdev/codespace/civm/runbooks/MULTI-PROJECT-RUNNER.md` | substituir steps manuais por civmctl; adicionar referência ao PRD |
| `/home/emdev/codespace/civm/.github/workflows/ci.yml` | adicionar job `build-civmctl` (go vet + go test + go build) |
| `/home/emdev/codespace/civm/docs/specs/civmctl/IMPL.md` | criar no fim com hashes + métricas |

## Diff conceitual de cada arquivo

### `internal/specs/specs.go`

Estrutura de dados (typed, sem stringly-typed):

```
type ToolVersion struct {
    Name     string
    Versions []string  // primeira é "preferred"
    Source   string    // URL upstream para audit
}

type RunnerImageSpec struct {
    OSDistro     string
    OSVersion    string
    Kernel       string  // best-effort
    UpstreamURL  string  // actions/runner-images Ubuntu2404-Readme.md
    Tools        []ToolVersion
}

func Ubuntu2404() RunnerImageSpec { ... }  // hardcoded com versoes do PRD §2
```

### `cmd/civmctl/main.go`

Dispatcher minimal:

```
func main() {
    if len(os.Args) < 2 {
        printHelp(); os.Exit(64)
    }
    switch os.Args[1] {
    case "version-pins": runVersionPins(os.Args[2:])
    case "health":       runHealth(os.Args[2:])
    case "cleanup":      runCleanup(os.Args[2:])
    case "bootstrap":    runBootstrap(os.Args[2:])
    case "runner":       runRunner(os.Args[2:])
    case "--help", "-h", "help":
        printHelp(); os.Exit(0)
    default:
        fmt.Fprintf(os.Stderr, "comando desconhecido: %s\n", os.Args[1])
        printHelp(); os.Exit(64)
    }
}
```

### `internal/health/health.go`

Coleta read-only:

```
type Report struct {
    Disk    DiskInfo
    Memory  MemInfo
    Runners []RunnerStatus
    Timers  []TimerStatus  // cleanup, disk, runner, reverse, metrics
    LastCleanup *time.Time
    Exit    int  // 0/1/2
}

func Collect(ctx context.Context, opts Options) (Report, error) {
    // 1. df -BG / (ou path passado)
    // 2. /proc/meminfo MemAvailable
    // 3. systemctl list-units --type=service "actions.runner.*"
    // 4. systemctl is-enabled/is-active civmctl-* timers
    // 5. journalctl -u civmctl-cleanup.service --since "24h ago" --reverse -n1
}

func (r Report) Render(w io.Writer) { ... }  // tabela ASCII
```

Em dev machine (sem actions.runner.*), `Runners` retorna `[]` e exit
permanece 0 (não é erro estar fora da VM).

### `internal/cleanup/cleanup.go`

```
type Action struct {
    Name        string  // "docker_prune", "tmp_old", "work_old", "apt_cache"
    Path        string  // afetado (ou "")
    BytesFound  int64
    BytesFreed  int64  // 0 se dry-run
    Err         error
}

type Options struct {
    Execute       bool
    WorkDir       string  // default legado; autodiscover /home/*/actions-runner-*/_work
    TmpThreshold  time.Duration  // 7d default
    WorkThreshold time.Duration  // 14d default
    DockerPrune   bool  // true default
    AptClean      bool  // true default
    SkipIdleGuard bool  // false default; execute aborta se host não ocioso
}

func Run(ctx context.Context, opts Options) ([]Action, error) {
    // executa cada step. dry-run apenas calcula bytes via du/stat.
    // execute roda os comandos reais.
}

func RenderTable(actions []Action, w io.Writer) { ... }
```

Antes de qualquer comando real, `execute=true` roda o guard de ociosidade:

- `ps -eo pid=,ppid=,comm=,args=` sem shell
- bloqueia `Runner.Worker`, `Runner.PluginHost`, processo com `/_work/`,
  `docker build`, `docker compose`, `docker-compose`, `buildx build` e
  `buildctl`
- fail-closed se `ps` falhar
- checa no início e novamente antes de cada mutação

Comandos reais (execute=true, somente host ocioso):

- Docker: `docker system prune -af --volumes`
- /tmp: `find /tmp -mtime +7 -delete` (filtro por mtime)
- _work: `find <workdir> -mindepth 2 -maxdepth 4 -name "_actions" -mtime +14 -exec rm -rf {} +`
- apt: `apt-get clean && apt-get autoremove -y`

### `internal/bootstrap/bootstrap.go`

```
type Step struct {
    Name      string
    Idempotent bool   // sempre true
    Check     func(ctx context.Context) (alreadyDone bool, err error)
    Apply     func(ctx context.Context) error
}

func Steps(spec specs.RunnerImageSpec) []Step { ... }

func Run(ctx context.Context, opts Options) ([]Result, error) {
    for _, s := range Steps(opts.Spec) {
        done, err := s.Check(ctx)
        if err != nil { ... }
        if done { result = "skip" }
        else if opts.Execute { err := s.Apply(ctx); ... }
        else { result = "dry-run-would-apply" }
    }
}
```

Steps:

1. Verify OS (`/etc/os-release` com `ID=ubuntu` e `VERSION_ID=24.04`)
2. Install base packages (apt: build-essential curl wget jq yq git)
3. Install Go (download tarball para /usr/local/go-X.Y.Z; symlinks; preferred 1.26.3)
4. Install Node (NodeSource ou nvm-style; preferred 24.15.0 system)
5. Install Python (apt python3.12 + pyenv shims para outras; opcional)
6. Install Docker CE (apt repo oficial Docker)
7. Install gh CLI (apt repo oficial cli.github.com)
8. Install systemd timers (cleanup, disk-watchdog, runner-watchdog,
   reverse-watchdog, metrics)

### systemd files

`deploy/systemd/civmctl-cleanup.service`:

```
[Unit]
Description=civmctl cleanup (disk hygiene da VM civm)
Documentation=https://github.com/emersonbusson/civm/blob/main/docs/specs/civmctl/PRD.md
After=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/local/bin/civmctl cleanup --execute
User=root
Nice=15
IOSchedulingClass=idle
```

`deploy/systemd/civmctl-cleanup.timer`:

```
[Unit]
Description=Trigger civmctl-cleanup.service diariamente

[Timer]
OnCalendar=*-*-* 04:00:00 UTC
Persistent=true
RandomizedDelaySec=300

[Install]
WantedBy=timers.target
```

## Validações em handlers/CLI

- `--dry-run` é **default** em cleanup e bootstrap.
- `--execute` exige flag explícita; se passado, log warning antes.
- Permissões: bootstrap exige `sudo` (uid==0); cleanup pode rodar como user.
- Cleanup nunca toca arquivos com mtime <2h (anti-jobs-em-curso).

## Test discipline

- 100% dos paths de erro testados via mocks de filesystem (`fstest`)
  e mocks de comando (substituir `exec.Command` por interface).
- Tests não tocam `/`, `/etc`, `/usr` reais.
- testdata em `internal/<pkg>/testdata/`.

## Counterfactual disciplinas Kahneman

Cada step destrutivo (cleanup, bootstrap apply) tem rollback documentado
no log:

```
[cleanup] docker_prune: liberados 4.2 GB
  rollback: imagens precisam ser repulladas (custo: ~5 min em 100 Mbps)
```

```
[bootstrap] install_docker: instalado 28.0.4
  rollback: apt-get remove docker-ce docker-ce-cli containerd.io
```

## Documentos referenciados (Kahneman)

- `disciplines/KAHNEMAN-DISCIPLINES.md` — disciplina #1 (WYSIATI: cada
  comando reporta o que ainda não foi visto se aplicável)
- `disciplines/KAHNEMAN-DISCIPLINES.md` — disciplina #2 (counterfactual:
  rollback inline em cada destrutivo)
- `disciplines/INVARIANTS.md` — invariantes #1 (no secrets), #6 (rollback
  trigger), #14 (no .sh in tools/)

## Open questions resolvidas

- ✅ Onde rodar tests sem ambiente Ubuntu 24.04? **Mock filesystem +
  mock exec.Command. Tests não dependem de OS específico.**
- ✅ Como instalar Go multi-version? **Bootstrap instala 1 versão preferred
  (1.26.3) via tarball; jobs que precisam outra usam `actions/setup-go@v5`
  no momento (cache em `/opt/hostedtoolcache/go`).**
- ✅ Como manter sync com upstream actions/runner-images? **Manual via
  `civmctl version-pins` mostra versões compiladas no binário; humano
  atualiza `internal/specs/specs.go` periodicamente. Auto-sync futuro.**

## Aprovação

PRD §13 critério "SPEC aprovado": humano confirma este documento antes
de IMPL avançar para `cmd/civmctl/main.go`.

Em modo autônomo (atual), aprovação implícita ao continuar com Write nos
arquivos listados; humano interrompe se discordar.
