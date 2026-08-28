// Package civm centralizes shared operational defaults and input validation.
package civm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	osuser "os/user"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	DefaultWorkDir        = "/home/runner/_work"
	DefaultHealthDiskPath = "/"
	DefaultTmpDir         = "/tmp"
	// DefaultCodespaceDir e um sentinel (como DefaultWorkDir): quando o caller nao
	// sobrescreve, o cleanup descobre os codespaces reais via glob /home/*/codespace.
	// Sao clones manuais parados que o CI nunca usa (ele clona em _work) — drift que
	// nenhuma rotina limpava ate o codespace_stale (ponto cego do paridade-pago).
	DefaultCodespaceDir   = "/home/runner/codespace"
	DefaultSystemdDir     = "/etc/systemd/system"
	DefaultUnitsSourceDir = "/opt/civm/deploy/systemd"
	DefaultCivmctlPath    = "/usr/local/bin/civmctl"
	DefaultRunnerVersion  = "2.336.0"

	DefaultGoLinuxAMD64SHA256      = "2b2cfc7148493da5e73981bffbf3353af381d5f93e789c82c79aff64962eb556"
	DefaultRunnerLinuxX64SHA256    = "04cf0be1aff4c3ec3554466c39124ca250e3effd8873bb7e8d68535aa9505d5d"
	DefaultNodeSourceSetup24SHA256 = "6e3d580f5bd7ccf2aa1e8df8d35c60d78e873c3ff8beb282c9bebd914904ad72"
	DefaultYQLinuxAMD64SHA256      = "75d893a0d5940d1019cb7cdc60001d9e876623852c31cfc6267047bc31149fa9"
	DefaultDockerGPGFingerprint    = "9DC858229FC7DD38854AE2D88D81803C0EBFCD88"
	DefaultGitHubCLIGPGFingerprint = "2C6106201985B60E6C7AC87323F3D4EA75716059"

	DefaultCleanupTimeoutMinutes      = 30
	DefaultRunnerTimeoutMinutes       = 10
	DefaultRunnerRemoveTimeoutMinutes = 5
	DefaultRestartTimeoutSeconds      = 30
	DefaultHealthTimeoutSeconds       = 5
	DefaultBillingTimeoutSeconds      = 15
	DefaultPreCleanupPct              = 60
	DefaultHardFailPct                = 90
	DefaultWatchdogThresholdPct       = DefaultPreCleanupPct
	DefaultCapacityMaxDiskPct         = DefaultHardFailPct
	// DefaultMinFreeGB: piso de espaco GUEST livre garantido antes de cada job
	// (job-started). Abaixo disso o hook full-clean (prune+cache+fstrim) restaura
	// >=58 -> cada PR comeca com ~58GB limpos (cabe qualquer build, sem cache).
	DefaultMinFreeGB = 58
	// DefaultEmergencyBypassPct is the disk-usage level at which the
	// disk-watchdog stops deferring SAFE reclaim (cache trim, old /tmp) to an
	// idle tick. In the 2026-06-10 incident the watchdog fired at 83% while a
	// job filled the disk, deferred everything by host-busy (freed=0) and the
	// guest ran to 0% free → sshd wedge. Between this level and HardFailPct the
	// busy-host deferral is the wrong trade.
	DefaultEmergencyBypassPct   = 75
	DefaultReverseMaxAgeHours   = 2
	DefaultRestartVerifySeconds = 3
	DefaultUpgradeVerifySeconds = 5
	// DefaultRunnerAutoRestartPerHour caps watchdog auto-restarts per runner
	// unit per rolling hour (anti restart-loop, RF-6 / ITEM-10 / DT-8).
	DefaultRunnerAutoRestartPerHour = 3
	DefaultRunnerWatchdogMarkerPath = "/var/lib/civm/runner-watchdog-reruns.json"
	// DefaultHooksLogPath is the shared civm hook event log (one JSONL record
	// per job-started/job-completed). The runner watchdog reads its tail to
	// detect a broken-runner sentinel.
	DefaultHooksLogPath = "/var/log/civm/hooks.jsonl"

	// Per-cache size budgets enforced by hook routine cleanup (job-completed).
	// Excedente é removido por mtime ascendente; arquivos com mtime mais novo
	// que DefaultCacheTrimMinProtectHours são preservados.
	//
	// Cada budget é FAMILY-WIDE: cacheCaps() faz glob das variantes nomeadas que
	// os workflows criam (ex. GOCACHE=~/.cache/go-build-acme-services,
	// YARN cache-folder=~/.cache/yarn-acme-web) e divide o budget entre os dirs
	// encontrados. Antes, os caps casavam só os paths default (~/.cache/go-build,
	// ~/.yarn/cache) e os dirs nomeados cresciam SEM LIMITE — go-build-acme-services
	// chegou a 13GB num cap de 5GB, enchendo o VHDX até o host dar PausedCritical.
	DefaultCacheTrimMinProtectHours = 24
	// DefaultCacheInFlightFloorMinutes é o piso de "escrita fresca" do emergency
	// path: sob bypass de disco (≥75%, sem idle guard) o trim PULA qualquer dir de
	// cache com arquivo escrito nos últimos N minutos — um dir com escrita fresca é
	// um install vivo, e trimá-lo mata o job (parcial → ENOENT). 15min cobre um
	// yarn/go install com folga e fica bem abaixo do MinProtect de 24h.
	DefaultCacheInFlightFloorMinutes = 15
	// go-build é WipeWhole (refs cruzadas opacas: sub-trim orfana entrada e o vet
	// quebra). O cap é generoso de propósito — backstop para crescimento
	// descontrolado, não o working-set normal (~2.2GB/dir; 12GB/3dirs = 4GB/dir).
	// O go auto-trima entradas > 5 dias, então normalmente o wipe nunca dispara.
	DefaultCacheGoBuildMaxGB = 12
	DefaultCacheNPMMaxGB     = 3
	// yarn v1 e PackageDepth (atomico), e o cap tambem e generoso (backstop, mesma
	// razao do go-build). O cap de 3GB antigo fazia o disk-watchdog (timer 8min, e o
	// EmergencyBypass do #117 a >=75%) trimar o working-set (~0.84GB/dir x4) NO MEIO
	// de um yarn install: removia o pacote em escrita, o yarn re-fetchava, race,
	// .yarn-metadata.json parcial, ENOENT (quebrava web/tenant-isolation/audit). Como
	// backstop (12GB/4dirs = 3GB/dir) o working-set fica sempre sob o cap, entao o
	// trim e no-op durante o job; o trim atomico so age no crescimento descontrolado.
	DefaultCacheYarnMaxGB         = 12
	DefaultCachePNPMMaxGB         = 5
	DefaultCacheGolangciLintMaxGB = 2
	// Browser binaries are regenerable. Keep a pressure-mode backstop while the
	// idle job-completed boundary removes the directory entirely for paid-CI parity.
	DefaultCachePlaywrightMaxGB = 2

	// Filtro do buildx prune em modo rotineiro: mantém o build cache quente < 24h
	// em vez do agressivo system prune --volumes. (O filtro de image prune por
	// idade foi removido: `until` casa a data do vendor, não do pull, e apagava
	// imagens recém-baixadas debaixo de um deploy concorrente — ver cleanup.go.)
	DefaultDockerBuildxPruneFilter = "until=24h"

	// Label que todo `docker compose` carimba nas imagens que builda. O job-completed
	// reapa as imagens taggeadas do PRÓPRIO run que terminou filtrando por este label
	// igual ao compose project deste runner (`<slot>` ou `<slot>-<run_id>`). É a
	// redução-na-FONTE da issue #137: sem isso, as imagens de service de cada run
	// (~35GB num job de E2E) só saíam como dangling — nunca — e acumulavam na rajada
	// até o panic floor. O escopo por label é seguro porque o slot é box-único por
	// runner (multi-project-isolation): um sibling jamais carrega o mesmo project, e
	// imagem de vendor pull (redis/minio/postgres) não tem label de compose — então o
	// "No such image" race que o PR #135 removeu do path online não volta.
	DefaultDockerComposeProjectLabel = "com.docker.compose.project"

	// Timeout por comando dentro do hook cleanup. Evita que um docker travado
	// segure o runner durante todo o TimeoutStartSec do systemd (30 min).
	DefaultRoutineCleanupCmdTimeoutSecs = 120

	// Reclamação de volume do host (docs/specs/host-volume-reclamation).
	DefaultHostVolumeWarnFreeGB = 30 // alinhado ao runbook ">30GB livres"
	DefaultHostVolumeCritFreeGB = 10 // alinhado ao runbook "<10GB"
	// DefaultHostVolumeHeadroomGB é o mínimo de V: livre ANTES do Optimize-VHD;
	// abaixo disso aborta sem zero-fill (folga p/ crescimento temporário do
	// VHDX na compactação). Calibrado para o host Day-0: V: tem 119GB e o
	// VHDX max tem 110GB; 15GB é uma violação permanente impossível nesse disco.
	DefaultHostVolumeHeadroomGB      = 8
	DefaultHostMetricsPath           = "/var/lib/civm/host-metrics.json" // cópia entregue ao guest
	DefaultHostMetricsMaxAgeMinutes  = 30                                // stale acima disso
	DefaultHostMetricsFileNameOnHost = "civm-host-metrics.json"          // nome do arquivo no host (V:\)
	DefaultMaintenanceStatePath      = "/var/lib/civm/maintenance.json"  // snapshot de drain idempotente
	DefaultMaintenanceLockPath       = "/var/lib/civm/maintenance.lock"  // flock anti-concorrência de enter/exit

	// SPECv3 (host-volume-reclamation/SPECv3.md) + RF-2/DT-1 (civm-self-cleaning-runner, #106).
	// Optimize-VHD é ININTERRUPTÍVEL (Stop-Job não aborta a compactação nativa).
	// O caminho de emergência (V: abaixo do headroom) é admitido em DUAS FASES:
	// Fase 1 (pré-stop) só checa se o budget está habilitado; Fase 2 (autoreclaim.ps1,
	// após Wait-VMState Off) re-mede Get-PSDrive V com o VMRS (~8GB) já liberado e
	// admite via EmergencyAdmits(liveFreeAfterOff, ...). O budget é pré-filtro
	// grosseiro; a folga real pós-Off é o gate AUTORITATIVO — nunca um piso
	// adivinhado, nunca abortando no meio. vmrs_release medido = 8.02GB (06/2026).
	DefaultHostVolumeHardFloorGB     = 1  // piso duro absoluto; nunca operar abaixo
	DefaultHostVolumeScratchBudgetGB = 11 // p100 scratch high-water observado (10, logs do host) + 1; emergência HABILITADA (segura via gate pós-Off RF-2/DT-1)
	DefaultAutoreclaimPressureGB     = 25 // abaixo disso, cadência de DETECÇÃO curta (não de ação)
	DefaultReclaimMinIntervalMin     = 30 // mínimo entre eventos reais de Stop-VM+Optimize

	// Isolamento multi-projeto (docs/specs/multi-project-isolation, ITEM-2/3).
	// CIVM_PORT_BASE é um bloco de DefaultRunnerPortBlockSize portas por runner,
	// base sticky persistida em DefaultPortBlockStatePath (mapa slot->base).
	// A janela [DefaultRunnerPortBlockStart, DefaultRunnerPortWindowEnd) fica
	// acima dos defaults conhecidos dos peers e abaixo da faixa ephemeral do
	// kernel Linux (32768+), evitando colisão com ambos.
	DefaultRunnerPortBlockStart = 20000
	DefaultRunnerPortBlockSize  = 64
	DefaultRunnerPortWindowEnd  = 32000 // < faixa ephemeral do kernel (32768+)
	DefaultPortBlockStatePath   = "/var/lib/civm/port-blocks.json"

	// Reaper de container órfão na fronteira job-started (docs/specs/orphan-port-reaper).
	// Jobs de CI sobem um stack docker-compose com PORTAS FIXAS de host (minio em
	// 127.0.0.1:9020, nginx em :81, etc.). Quando um run anterior não é derrubado
	// — job cancelado, OU o runner que o subiu foi REMOVIDO — o container vira
	// órfão e segura a porta fixa: o próximo job morre em "Bind for 127.0.0.1:9020
	// failed: port is already allocated". O reaper do hook
	// (killWorkRootContainers) só pega órfãos sob o _work root do PRÓPRIO runner —
	// um órfão de OUTRO runner (root diferente) escapa. Aqui o sinal NÃO é o _work
	// root: numa box de 1 runner, na fronteira job-started (ANTES de o job subir o
	// stack), todo container cujo project compose começa com este prefixo é órfão
	// por definição. Prefixo alinhado às imagens de run (`civm-run-{id}-*`).
	DefaultCIOrphanProjectPrefix = "civm-run"
)

// DefaultCIFixedHostPorts é o backstop por-porta do reaper de órfão: hosts ports
// FIXOS que stacks de CI típicos publicam em 127.0.0.1. A detecção PRIMÁRIA é o
// label com.docker.compose.project (DefaultCIOrphanProjectPrefix); esta lista é
// defesa em profundidade. Operadores com portas diferentes sobrescrevem via
// config. A porta 9020 (minio) é um default comum de labs.
var DefaultCIFixedHostPorts = []int{
	81,   // nginx (LOCAL_NGINX_PORT)
	3010, // web (LOCAL_WEB_PORT)
	3011, // metrics dashboard (LOCAL_GRAFANA_PORT)
	5433, // database (LOCAL_PG_PORT, compose base)
	5443, // database override (LOCAL_PG_PORT, compose override)
	6389, // cache / redis (LOCAL_REDIS_PORT)
	8088, // service gateway (LOCAL_SERVICE_GW_PORT)
	8101, // auth service (LOCAL_AUTH_PORT)
	8102, // provisioner service (LOCAL_PROVISIONER_PORT)
	8103, // core service (LOCAL_CORE_PORT)
	8104, // billing service (LOCAL_BILLING_PORT)
	8105, // docs service (LOCAL_DOCS_PORT)
	8106, // messaging service (LOCAL_CHAT_PORT)
	8107, // scheduler service (LOCAL_SCHEDULER_PORT)
	8108, // service worker 1 (LOCAL_WORKER_1_PORT)
	8109, // service worker 2 (LOCAL_WORKER_2_PORT)
	8110, // backend node 1 (LOCAL_NODE_1_PORT)
	8111, // backend node 2 (LOCAL_NODE_2_PORT)
	9020, // object storage API (LOCAL_MINIO_API_PORT)
	9021, // object storage console (LOCAL_MINIO_CONSOLE_PORT)
	9100, // telemetry / prometheus (LOCAL_PROMETHEUS_PORT)
}

const (

	// Serialização de trabalho docker-heavy box-wide (docs/specs/multi-project-isolation,
	// ITEM-4). Um único lock global protege qualquer operação que aloca recursos
	// do daemon (docker compose up/down/run, docker build/buildx, docker pull).
	DefaultDockerHeavyLockPath = "/run/civm/docker-heavy.lock"
	// HOLD: além disso o heartbeat continua (não mata job vivo), apenas marca
	// over_budget=true no lock_release como sinal de alarme (SPECv2 DT-v2-1).
	DefaultDockerHeavyLockBudgetMinutes = 50
	// WAIT: além disso a aquisição falha alto com ErrWaitBudgetExceeded.
	DefaultDockerHeavyLockWaitMinutes = 75
	// Intervalo de reescrita do heartbeat; staleness = heartbeat parado por
	// > 3× este valor OU PID morto OU pidStartTicks divergente (SPECv2 DT-v2-1/3).
	DefaultDockerHeavyHeartbeatSeconds = 30

	// Admissão de jobs por memória (docs/specs/runner-memory-admission/SPECv3.md,
	// §Constantes). `civmctl admit` envelopa o job num service transiente
	// (sudo systemd-run --pipe -p MemoryMax) com no máximo DefaultAdmitMaxHeavy
	// jobs heavy concorrentes, serializados por N flock-slots fixos. light flui
	// sem slot. Liveness = o flock (liberado pelo kernel na morte); sem heartbeat.
	DefaultAdmitMaxHeavy = 2 // teto = nº de slots de flock (invariante: >=1)
	// RAM reservada ao host/SO; MemoryMax efetivo = (MemTotal - isto)/MaxHeavy
	// quando HeavyMaxMB não foi calibrado (DT-v3-5). Invariante: >0.
	DefaultAdmitHostReserveMB = 2048
	// 0 => MemoryMax generoso (MemTotal-host)/MaxHeavy até o pico RSS ser medido
	// sob carga (DT-v3-5: safe-by-default, não mata job legítimo antes de calibrar).
	DefaultAdmitHeavyMaxMB = 0
	// WaitBudget: depois disso Acquire retorna exit tipado exitAdmitWaitTimeout
	// (DT-v3-3: nunca cria slot N+1, nunca trava; o job-timeout do runner decide).
	DefaultAdmitWaitMinutes = 30
	// Prefixo dos N slots de flock heavy: + "{1..MaxHeavy}.lock". O arquivo grava
	// o nome da unit systemd para co-terminação e reap-on-reuse (DT-v3-2).
	DefaultAdmitSlotPathPrefix = "/run/civm/admit-heavy-"
	// Sub-slot docker count=1 do próprio admit (DT-v3-8): --exclusive=docker
	// serializa docker-heavy sem o dockerlock legado de 75 min.
	DefaultAdmitDockerSlotPath = "/run/civm/admit-docker.lock"

	// Escalada de remoção privilegiada para arquivos root-owned no _work
	// (docs/specs/civm-runner-reliability, DT-v2-1/3/5/8). Steps CI que rodam
	// como root dentro de containers gravam arquivos no _work montado que o
	// usuário do runner não consegue apagar (EACCES no unlinkat) — o que trava
	// o "Complete runner" e quebra todos os jobs seguintes naquele runner.
	// O ÚNICO binário sob NOPASSWD é o wrapper validado; chown/rm absolutos
	// são chamados de dentro dele (já root). Caminho absoluto e único elimina a
	// ambiguidade /usr/bin vs /bin do secure_path do sudo.
	DefaultSafeDeleteWrapperPath = "/usr/local/bin/civm-safedelete"
	// Root-owned fixed-command wrapper used by the Windows host to create a
	// generation clean boundary. It is deliberately separate from safedelete:
	// the only NOPASSWD operation for this protocol is an argumentless fixed
	// prepare/resume/check verb, never generic civmctl or systemctl.
	DefaultGenerationBoundaryWrapperPath = "/usr/local/bin/civm-generation-boundary"

	// Raiz de onde installScopedSudoers lê os artefatos versionados em deploy/
	// (espelha DefaultUnitsSourceDir). Single source of truth: o conteúdo do
	// wrapper e do sudoers vive só em deploy/; o binário Go nunca embute cópia
	// (//go:embed é impossível através do boundary do pacote — SPECv2 DT-v2-5).
	DefaultDeploySourceDir = "/opt/civm/deploy"
	// Relativo a DefaultDeploySourceDir: o wrapper root validado (deploy/bin).
	DefaultSafeDeleteWrapperSource         = "bin/civm-safedelete"
	DefaultGenerationBoundaryWrapperSource = "bin/civm-generation-boundary"
	// Relativo a DefaultDeploySourceDir: o drop-in sudoers escopado (deploy/sudoers.d).
	DefaultScopedSudoersSource = "sudoers.d/civm-cleanup"
	// Destino do drop-in sudoers ativo; 0440 root:root, validado por visudo -c
	// antes de ativar (SPECv2 §"Instalação do sudoers", DT-v2-1/3/5).
	DefaultScopedSudoersDropIn = "/etc/sudoers.d/civm-cleanup"

	// GenerationCleanBoundaryCapability is a stable host↔guest compatibility
	// marker. The root wrapper checks it before every privileged operation, and
	// civm-host rejects any marker other than GenerationCleanBoundaryMarker.
	GenerationCleanBoundaryCapability = "generation-clean-boundary"
	GenerationCleanBoundaryMarker     = "civm-generation-boundary/v1"
)

var (
	goLinuxAMD64SHA256 = map[string]string{
		"1.26.3": DefaultGoLinuxAMD64SHA256,
	}
	runnerLinuxX64SHA256 = map[string]string{
		DefaultRunnerVersion: DefaultRunnerLinuxX64SHA256,
	}
	nodeSourceSetupSHA256 = map[string]string{
		"24": DefaultNodeSourceSetup24SHA256,
	}
	yqLinuxAMD64SHA256 = map[string]string{
		"4.52.5": DefaultYQLinuxAMD64SHA256,
	}
)

var (
	repoRe     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
	shortRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	labelRe    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	semverRe   = regexp.MustCompile(`^[0-9]+[.][0-9]+[.][0-9]+$`)
	unitRe     = regexp.MustCompile(`^[A-Za-z0-9_.@-]+[.]service$`)
	userRe     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]{0,63}$`)
	workflowRe = regexp.MustCompile(`^[A-Za-z0-9.][A-Za-z0-9._/-]{0,127}[.]ya?ml$`)
)

// ValidateRepo enforces a GitHub owner/repo shape without shell metacharacters.
func ValidateRepo(repo string) error {
	if !repoRe.MatchString(repo) {
		return fmt.Errorf("repo deve ter formato owner/repo seguro, got %q", repo)
	}
	return nil
}

// ValidateShort accepts only a directory-safe runner suffix.
func ValidateShort(short string) error {
	if !shortRe.MatchString(short) {
		return fmt.Errorf("short deve conter apenas letras, numeros, _ ou -, got %q", short)
	}
	return nil
}

// ValidateLabels accepts a comma-separated allowlist for GitHub runner labels.
func ValidateLabels(labels string) error {
	if labels == "" {
		return fmt.Errorf("label obrigatorio")
	}
	for _, label := range strings.Split(labels, ",") {
		label = strings.TrimSpace(label)
		if !labelRe.MatchString(label) {
			return fmt.Errorf("label invalido %q", label)
		}
	}
	return nil
}

// ValidateSemver rejects runner versions that are not plain x.y.z.
func ValidateSemver(value, field string) error {
	if !semverRe.MatchString(value) {
		return fmt.Errorf("%s deve usar semver x.y.z, got %q", field, value)
	}
	return nil
}

// ValidateServiceUnit restricts user-provided systemd unit names.
func ValidateServiceUnit(unit string) error {
	if !unitRe.MatchString(unit) || strings.Contains(unit, "..") {
		return fmt.Errorf("unit invalida: %q", unit)
	}
	return nil
}

// ValidateUserName restricts --run-as to local Unix-style user names.
func ValidateUserName(name string) error {
	if !userRe.MatchString(name) {
		return fmt.Errorf("run-as invalido: %q", name)
	}
	return nil
}

// ResolveRunnerUser returns the Unix account transient runner services should
// use. SUDO_USER wins when a command is sudo-wrapped; otherwise use the current
// process user or USER. The fallback preserves older single-user lab hosts.
func ResolveRunnerUser(fallback string) string {
	if u := strings.TrimSpace(os.Getenv("SUDO_USER")); u != "" && ValidateUserName(u) == nil {
		return u
	}
	if u, err := osuser.Current(); err == nil {
		name := strings.TrimSpace(u.Username)
		if idx := strings.LastIndexAny(name, `\`); idx >= 0 {
			name = name[idx+1:]
		}
		if name != "" && ValidateUserName(name) == nil {
			return name
		}
	}
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" && ValidateUserName(u) == nil {
		return u
	}
	return fallback
}

// ValidateWorkflowFile restricts gh workflow selectors to local YAML filenames.
func ValidateWorkflowFile(workflow string) error {
	if !workflowRe.MatchString(workflow) ||
		strings.HasPrefix(workflow, "/") ||
		strings.Contains(workflow, "..") {
		return fmt.Errorf("workflow deve ser arquivo .yml/.yaml seguro, got %q", workflow)
	}
	return nil
}

// CleanDir validates and normalizes a user-provided directory path.
func CleanDir(path, field string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("%s obrigatorio", field)
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%s contem byte NUL", field)
	}
	clean := filepath.Clean(path)
	if clean == "." {
		return "", fmt.Errorf("%s nao pode ser diretorio atual", field)
	}
	return clean, nil
}

func GoLinuxAMD64SHA256(version string) (string, bool) {
	value, ok := goLinuxAMD64SHA256[version]
	return value, ok
}

func RunnerLinuxX64SHA256(version string) (string, bool) {
	value, ok := runnerLinuxX64SHA256[version]
	return value, ok
}

func NodeSourceSetupSHA256(major string) (string, bool) {
	value, ok := nodeSourceSetupSHA256[major]
	return value, ok
}

func YQLinuxAMD64SHA256(version string) (string, bool) {
	value, ok := yqLinuxAMD64SHA256[version]
	return value, ok
}

func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func VerifySHA256(actual, expected, label string) error {
	actual = strings.ToLower(strings.TrimSpace(actual))
	expected = strings.ToLower(strings.TrimSpace(expected))
	if expected == "" {
		return fmt.Errorf("sha256 esperado ausente para %s", label)
	}
	if actual != expected {
		return fmt.Errorf("sha256 mismatch for %s: got %s want %s", label, actual, expected)
	}
	return nil
}

func VerifyGPGFingerprint(output, expected, label string) error {
	actual := normalizeFingerprint(output)
	expected = normalizeFingerprint(expected)
	if expected == "" {
		return fmt.Errorf("fingerprint esperado ausente para %s", label)
	}
	if !strings.Contains(actual, expected) {
		return fmt.Errorf("fingerprint mismatch for %s: got output without %s", label, expected)
	}
	return nil
}

func normalizeFingerprint(value string) string {
	value = strings.ToUpper(value)
	return strings.Map(func(r rune) rune {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, value)
}
