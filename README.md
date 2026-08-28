# civm

[![CI](https://github.com/emersonbusson/civm/actions/workflows/ci.yml/badge.svg)](https://github.com/emersonbusson/civm/actions/workflows/ci.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

**civm** is open-source tooling to **provision and operate self-hosted
GitHub Actions runners** on a Linux VM (and optional Windows Hyper-V host
helpers), with parity pins against `ubuntu-latest`, automated cleanup,
health/doctor, and copy-paste workflow templates.

| You want… | Use… |
| --- | --- |
| Install / maintain the runner VM | `civmctl` (this repo, Go) |
| Windows scale-to-zero Hyper-V brain | sibling project **civm-host** (optional) |
| CI for your product repos | your repos + `templates/*.yml.template` |

**What civm is not**

- Not an application platform or custom orchestrator (GitHub Actions remains the scheduler).
- Not a code linter product — project-specific audits stay in each app repo.
- Not a multi-tenant SaaS fleet: **you** configure which `owner/repo` list a box serves.

## License

[MIT](LICENSE) — see [CONTRIBUTING.md](CONTRIBUTING.md) and [SECURITY.md](SECURITY.md).

## Windows Hyper-V host (optional scale-to-zero)

Production host brain today: PowerShell under `deploy/windows/`; control-plane
tasks run as **SYSTEM**, while `civm-gate` runners use
`NETWORK SERVICE/ServiceAccount/Limited` with protected least-privilege ACLs.
Configure **`Repos`** and **`TokenPaths`** on the host (empty in-repo defaults by design).
Prefer a **host-local lab wrapper** so org/repo fleets never land in git.

- Runbook: [`runbooks/HOST-ORCHESTRATOR-SETUP.md`](runbooks/HOST-ORCHESTRATOR-SETUP.md)
- Behavior: [`docs/specs/orchestrator-scale-to-zero/`](docs/specs/orchestrator-scale-to-zero/)
- Optional C# port (shadow until cutover): sibling project **civm-host**

## Bootstrap (guest Ubuntu 24.04)

On a clean Ubuntu 24.04 LTS VM (as a user with sudo):

```bash
git clone https://github.com/emersonbusson/civm.git /opt/civm   # or your fork
cd /opt/civm
go build -o /usr/local/bin/civmctl ./cmd/civmctl
sudo civmctl bootstrap --execute
sudo cp deploy/systemd/civmctl-*.service deploy/systemd/civmctl-*.timer /etc/systemd/system/
sudo install -d -m 0755 /etc/civm
printf '%s\n' 'CIVM_REAPER_REPOS=<owner/repo[,owner/repo]>' | \
  sudo tee /etc/civm/run-reaper.env >/dev/null
sudo systemctl daemon-reload
sudo systemctl enable --now \
  civmctl-cleanup.timer civmctl-disk-watchdog.timer \
  civmctl-runner-watchdog.timer civmctl-reverse-watchdog.timer \
  civmctl-metrics.timer civmctl-run-reaper.timer
civmctl parity
civmctl health
```

Register a runner (token is ephemeral; never commit it):

```bash
TOKEN=$(gh api -X POST /repos/<owner>/<repo>/actions/runners/registration-token --jq .token)
civmctl runner add --repo=<owner>/<repo> --token="$TOKEN" --short=<short> --execute
```

Prefer **one org-level runner** when many repos share a box (serialization).
See `runbooks/RUNNER-SERIALIZATION.md` and `runbooks/ORG-RUNNER-ADOPTION.md`.

Full multi-runner / disk / host notes: `runbooks/MULTI-PROJECT-RUNNER.md`.

## Comandos civmctl

| Comando | Função |
|---|---|
| `civmctl version-pins` | imprime versoes alvo (paridade com `ubuntu-latest`) |
| `civmctl parity [--json]` | valida ferramentas instaladas na VM contra os pins autoritativos |
| `civmctl bootstrap [--execute]` | provisiona VM (default: dry-run) |
| `civmctl cleanup [--execute] [--managed-volumes]` | limpa Docker, /tmp, artefatos antigos de _work e apt cache; preserva `_work/_tool` e `_work/_actions`; `--managed-volumes` é opt-in do boundary fechado e remove por nome apenas volumes Compose classificados; em `--execute` aborta se detectar job/build ativo |
| `civmctl health` | health check (disk, mem, runners, ultimo cleanup) |
| `civmctl doctor [--repos=auto\|owner/repo,...\|none] [--json]` | visão read-only consolidada: host, hooks, systemd runners e GitHub runners; `auto` infere repos pelos services locais |
| `civmctl idle-check [--json]` | detector read-only de ociosidade: exit `0=idle`, `1=busy`, `2=unknown` |
| `civmctl hook install [--execute] [--runner-glob=...]` | reconcilia scripts `ACTIONS_RUNNER_HOOK_*` e `.env` dos runners |
| `civmctl runner add` | registra runner GitHub Actions self-hosted (mkdir + curl + tar + config.sh + svc.sh install + start) |
| `civmctl runner remove` | desregistra runner; aborta antes de `config.sh remove`/`rm -rf` se stop/uninstall falhar |
| `civmctl drift` | compara pins locais vs upstream actions/runner-images (HTTP fetch) |
| `civmctl billing-status` | detector heuristico de billing-block (zero-PAT, GITHUB_TOKEN suficiente) |
| `civmctl peer-status` | status read-only de adoção/saúde por peer ou fleet: billing, runners online e último run; `--repos=owner/a,owner/b` retorna exit `0=ok`, `1=warn`, `2=critical` |
| `civmctl active-runs [--repos=auto\|owner/a,owner/b\|none] [--include-eta] [--json]` | lista workflow runs in_progress + queued cross-repo com ETA por workflow (avg das últimas N runs success); concorrente via worker pool. Cobre cockpits dashboard sem precisar invocar `gh run list` por repo |
| `civmctl reap-runs --repos=owner/a[,owner/b] [--execute]` | **dono da higiene de fila shared**: force-cancela runs de PR fechado (`pr-not-open`) e SHAs supersedidos em PR aberto (`superseded-sha`). Timer guest 5 min; peers **não** são o dono |
| `civmctl actions-metrics --org=ORG [--period=month\|last-month\|week\|day\|YYYY-MM-DD..YYYY-MM-DD] [--repos=auto\|owner/a,...\|none] [--json]` | agrega minutos billable (API `/organizations/{org}/settings/billing/usage`) + run counts cross-repo num período; espelha a tela "Actions Usage Metrics" do GitHub. Self-hosted minutos NÃO entram (API pública não expõe) |
| `civmctl runner list` | lista runners systemd na VM (parsed; suporta `--json`) |
| `civmctl runner restart` | systemctl restart por --short ou --unit; verifica is-active após delay |
| `civmctl runner upgrade` | upgrade in-place de versão (preserva .runner/.credentials/_work) |
| `civmctl runner watchdog [--execute] [--repos=auto\|owner/repo,...] [--queue-stall-dwell=5m] [--rerun-network-failures]` | repara hooks/runners e detecta sessão org `online,busy=false` sem consumo de fila `self-hosted,civm`; idle-check completo e exatamente 1 restart por incidente; rerun de workflow continua opt-in |
| `civmctl reverse-watchdog` | alerta se disk-watchdog timer parou de disparar (>2h default) |
| `civmctl capacity [--json]` | readiness read-only: disco, services runner, workers ativos e `accepting_jobs` |
| `civmctl metrics dump` | grava métricas Prometheus textfile para node_exporter |
| `civmctl bootstrap-everything` | wrapper: cp systemd units + daemon-reload + bootstrap |
| `civmctl disk-watchdog` | dispara cleanup agressivo se disk >threshold (default 60%); fail-closed se a VM não estiver ociosa |
| `civmctl disk-audit [--json]` | relatório read-only de donos seguros de disco: `_work`, caches runner/home, `codespace`, Docker, `/var/log`, `/var/cache` |
| `civmctl ci local-report` | posta commit status via gh api (cross-peer manual reporter) |
| `civmctl jit-dispatch` | one-shot Linux: entra no slot global via `guard exec`, despacha workflow confiável e entrega o JIT a um driver de isolamento descartável pinado; ativação permanece NO-GO |

### Piloto JIT confiável

O dispatcher externo usa API GitHub `2026-03-10`, exige HTTP 200 com
`workflow_run_id` e não descobre runs por nome ou “mais recente”. Config fica
fora do Git em arquivo 0600; token entra só por stdin; cada label tem nonce de
256 bits e uso único. O flock fixo em `/run/civm` serializa somente
recovery/admissão; o slot pesado é machine-wide e pertence ao Guard. O ledger
persiste lease, run/job/runner, PID+start time, process group, cgroup e identidade
da VM antes de avançar.

O host possui `31,90 GiB`; o teto WSL de `20 GiB` mais os `12 GiB` fixos da VM
somam `32 GiB` antes do piso Windows. Assim, `guard exec` precisa serializar a
admissão e provar por sensor Windows fresco pós-reclaim que há saldo para o
commit da VM, piso Windows aprovado e margem. Teto configurado, leitura Linux
ou bytes solicitados ao reclaim não bastam.

O request JIT envia somente o label nonce, mas o GitHub pode acrescentar labels
padrão read-only (`self-hosted`, SO e arquitetura). O GitHub não oferece bind
atômico JIT→run/job: outro job pode roubar o runner. O dispatcher não mascara
essa corrida; só aceita sucesso quando o job esperado termina no runner ID/nome/
grupo gerado. O roubo deve ser inofensivo porque código candidato roda em VM
descartável sem mount do host, Docker do host ou secret de produto.

O driver externo deve estar pinado por SHA-256, atestar a base imutável antes de
receber o segredo JIT e provar destruição/reset depois do job. O slot só é
liberado após VM destruída, run terminal e runner remoto ausente. A ativação é
**NO-GO** até existir implementação real auditada desse driver, config/credencial
host-local e canário live supervisionado. Não propagar o piloto a peer não
autorizado. Ver
`runbooks/TRUSTED-JIT-DISPATCHER.md`.

### Adicionar runner pra novo peer (1 comando)

```bash
TOKEN=$(gh api -X POST /repos/<owner>/<repo>/actions/runners/registration-token --jq .token)

# Dry-run primeiro:
civmctl runner add --repo=<owner>/<repo> --token=$TOKEN --short=<short>

# Aplicar:
civmctl runner add --repo=<owner>/<repo> --token=$TOKEN --short=<short> --execute
```

Substitui sequencia manual de mkdir + curl + tar + config.sh + svc.sh.
Token mascarado nos logs (mostrado como `***`). Detalhes em
`runbooks/MULTI-PROJECT-RUNNER.md` §"Pattern: 1 runner por peer-repo".
Downloads executados como root têm SHA256 pinado no código antes de
extração, instalação ou execução de script.

PRD/SPEC/IMPL: `docs/specs/civmctl/`.

## Estrutura por audiência

### Maintainers of this repo

| File | Role |
|---|---|
| `README.md` | this file |
| `LICENSE` / `CONTRIBUTING.md` / `SECURITY.md` | open-source baseline |
| `.github/workflows/ci.yml` | GitHub-hosted CI (`ubuntu-latest`) |
| `.gitignore` | keep lab secrets / logs out of git |

### Para quem **administra a VM** (sysadmin do civm)

| Arquivo | Função |
|---|---|
| `runbooks/MULTI-PROJECT-RUNNER.md` | provisionar VM + N runners + tools (parity ubuntu-latest) + timers systemd de cleanup/watchdog (128GB SSD) |
| `runbooks/RUNNER-SERIALIZATION.md` | invariante "1 runner por org": acme serializa no runner ORG (`civm-acme-org`); por quê (concurrent prune, #1184), como verificar e impor (`serialize-runner.ps1`) |
| `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md` | manutenção do VHDX do host (reclamação de volume) — alavancas break-glass quando o orchestrator está pausado |
| `runbooks/LOCAL-CI-DISCIPLINE.md` | filosofia "local CI é gate de verdade, remoto é mirror" |

### Para **peer repos** — **copiar**

| Arquivo | Como adotar |
|---|---|
| `templates/ci-optimistic.yml.template` | `cp ... .github/workflows/ci.yml` no peer; substituir placeholders |
| `templates/ci-router.yml.template` | idem, versão Tier 1 com router |
| `templates/cancel-on-pr-close.yml.template` | **opcional** no peer (latência 0); cancela runs de PR fechado. **Dono da fila = reaper** (`civmctl reap-runs`) |
| `templates/cancel-stale-on-push.yml.template` | **opcional** no peer (latência 0); cancela SHAs supersedidos no push. **Dono da fila = reaper** (`superseded-sha`) |
| `templates/CIVM-USAGE.md` | copiar para `docs/CIVM.md` no peer; ajustar gate local do projeto |
| `templates/COMMUNICATION-STYLE.md` | copiar bloco entre marcadores BEGIN/END pra CLAUDE/AGENTS/CODEX do peer |
| `runbooks/CI-BILLING-FALLBACK.md` | leia para entender as 3 camadas de fallback (referência, não copy) |
| `runbooks/CI-GITHUB-APP-SETUP.md` | rota de upgrade futuro (referência) |

### Para **peer repos** — **referenciar (não copiar)**

| Arquivo | Função |
|---|---|
| `disciplines/KAHNEMAN-DISCIPLINES.md` | 16 disciplinas Sistema 1 vs 2 — referência metodológica |
| `disciplines/SUPERPROMPT.md` | superprompt de auditoria de ruído arquitetural (Kahneman + DDD) — referência |
| `disciplines/SSDV3-PROMPTS.md` | Spec-Driven Dev V3 — prompts copiáveis |
| `disciplines/INVARIANTS.md` | catálogo de invariantes (cada peer escolhe quais adotar) |
| `rules/ssdv3.md`, `rules/testing.md`, `rules/security.md`, `rules/governance.md`, `rules/observability.md` | granular rules `.claude/rules/*` portáveis |
| `runbooks/TRUSTED-JIT-DISPATCHER.md` | contrato, operação futura e gate NO-GO do dispatcher JIT repository-scoped |

## Como o civm runner funciona

1. **Setup uma vez** seguindo `runbooks/MULTI-PROJECT-RUNNER.md`:
   - Provisionar VM Linux (Ubuntu 24.04 LTS, 12 vCPU, 12 GiB fixos,
     SSD host 128G)
   - Instalar toolchains (Go, Node, Docker, gh CLI, etc) — parity ubuntu-latest
   - Registrar N runners GitHub com label `civm`
   - Configurar timers systemd de cleanup, disk-watchdog, runner-watchdog,
     reverse-watchdog e metrics

2. **Scale-to-zero no host (orchestrator):** a VM pesada não fica ligada
   ociosa. A Scheduled Task `civm-host-orchestrator` é o **único dono ativo**
   do power-state; o PowerShell `civm-vm-orchestrator` permanece `Disabled`
   para rollback. Cada geração `<contexto>@<head_sha>` mantém a admissão
   fechada até provar cleanup guest (`--managed-volumes`), `fstrim`,
   `Optimize-VHD`, boot e listener pronto. Assim, um push novo só entra após
   a fronteira limpa do SHA anterior. O detector independente
   `civm-watchdog` verifica owner único, heartbeat C# e latch de processo sem
   iniciar, habilitar ou trocar o owner. Detalhes em
   `runbooks/HOST-ORCHESTRATOR-SETUP.md` e
   `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md`.

3. **Cada peer repo** referencia `runs-on: [self-hosted, civm]` em
   seu próprio `.github/workflows/ci.yml`.

   Regra de seguranca: jobs self-hosted devem rodar apenas PR confiavel
   ou same-repo. Evitar `pull_request_target` e nao expor secrets a codigo
   de fork em runner self-hosted.

4. **Quando billing GitHub OK:** workflow roda em `ubuntu-latest`
   (GitHub-hosted, paga minutos). Quando billing bloqueado: roteia
   para `civm` (sem custo). Detector heurístico documentado em
   `runbooks/CI-BILLING-FALLBACK.md`; templates implementam o roteamento.

5. **PR continua sendo criado** de onde o dev quiser (laptop, gh CLI,
   GitHub UI). civm só executa CI; não cria PRs.

## Adoção em 1 comando (para peer repos)

Não tem 1-comando-mágico — adoção é manual, peer repo decide o que
faz sentido:

```bash
# 1. Copiar a doc operacional curta do peer
mkdir -p <peer>/docs
cp "${CIVM_REPO:-$HOME/codespace/civm}/templates/CIVM-USAGE.md" <peer>/docs/CIVM.md
# Editar o bloco "Gate local do projeto" com o comando real do peer

# 2. Copiar template de workflow (escolher tier)
cp "${CIVM_REPO:-$HOME/codespace/civm}/templates/ci-optimistic.yml.template" \
   <peer>/.github/workflows/ci.yml
# Editar para substituir placeholders pelos gates reais do peer

# 3. Copiar snippet COMMUNICATION-STYLE
# (copiar bloco entre marcadores BEGIN/END em
#  ${CIVM_REPO:-$HOME/codespace/civm}/templates/COMMUNICATION-STYLE.md
#  pra CLAUDE.md, AGENTS.md, CODEX.md do peer)

# 4. Configurar branch protection no GitHub
# Settings > Branches > main > require status check:
#   "Gates (typecheck, test, build, invariants)"

# 5. Verificar adoção/saúde dos peers antes de publicar ou investigar CI
civmctl peer-status --repos=owner/a,owner/b --workflow=ci.yml
```

Audit/discipline-checks ficam no projeto do peer (cada um com sua
própria ferramenta — ex.: acme tem `devctl`).
`peer-status` é observabilidade read-only: consolida sinais para decisão
humana, mas não corrige workspace ou configuração de peer automaticamente.

## Versionamento

`civm` segue SemVer (MAJOR.MINOR.PATCH). Tags + GitHub Releases são
geradas automaticamente por `release-please` a partir de Conventional
Commits em `main`:

- `fix:` → bump patch (`v1.0.0` → `v1.0.1`).
- `feat:` → bump minor (`v1.0.0` → `v1.1.0`).
- `feat!:` ou `BREAKING CHANGE:` no footer → bump major (`v1.0.0` → `v2.0.0`).
- `docs:`, `chore:`, `test:`, `build:`, `style:` não bumpam versão.
- `ci:`, `refactor:`, `perf:` entram no CHANGELOG sem bump (configurável).

Workflow `.github/workflows/release.yml` mantém um PR de release aberto
com título `chore: release civm v<X.Y.Z>`,
`.release-please-manifest.json` bumpado e `CHANGELOG.md` regerado.
Mergear esse PR cria a tag e publica o release. Detalhes operacionais em
`runbooks/RELEASE-AUTOMATION.md` (config, GitHub App de release,
fallbacks, override `release-as`, rollback).
O token primario e um GitHub App dedicado com permissoes minimas
`contents: write`, `pull-requests: write`, `issues: write` e
`metadata: read`, configurado pelos secrets `RELEASE_APP_ID` e
`RELEASE_APP_PRIVATE_KEY`.
Internamente `civm` no título é texto cosmético, não `package-name` do
release-please. Em PR agrupado a branch é `release-please--branches--main`
sem componente; configurar `package-name: civm` faz o release-please esperar
componente na branch e abortar antes de criar a tag.

Peer repos podem travar em versão se quiserem (ex.: `git checkout v1.2.0`
antes de copiar templates).

## Governança de PR

PRs devem linkar issue com `Closes #NNN`, `Fixes #NNN` ou
`Resolves #NNN` quando o trabalho merece rastreio. Para PR puramente
operacional, CI ou documentação sem issue real, usar marcador explícito
na seção `## Issue`: `Sem issue`, `No issue` ou `N/A`. Não criar issue
artificial nem referência falsa só para satisfazer o template.

Toda PR também precisa de pelo menos uma label `type:*`, uma label
`area:*` e responsável coerente com a issue quando houver issue linkada.

## Verificação pós-release

Depois de publicar tag/release, confirmar o estado sem executar mutação:

```bash
gh release view v1.0.0
git status --short --branch
gh run list --workflow=ci.yml --branch=main --limit 5
ssh gha-ubuntu-2404 'civmctl parity'
ssh gha-ubuntu-2404 'civmctl health'
ssh gha-ubuntu-2404 'civmctl doctor --repos=auto --json'
ssh gha-ubuntu-2404 'civmctl active-runs --repos=auto --json'
ssh gha-ubuntu-2404 'civmctl actions-metrics --org=acme --period=month --json'
ssh gha-ubuntu-2404 'civmctl idle-check'
```

O warning `LAST cleanup timer nunca rodou` em `health`/`doctor` é
aceitável até o primeiro disparo real do `civmctl-cleanup.timer`
(janela diária 04:00 UTC). Se persistir após a próxima janela diária
esperada, vira ação operacional: verificar timer, journal da unit
`civmctl-cleanup.service` e execução do cleanup na VM.

`civmctl-runner-watchdog.timer` roda a cada ~2min após o boot. A unit
usa `civmctl runner watchdog --execute --repos=auto --json`: se GitHub
estiver acessível e a VM estiver idle, ela repara hooks e reinicia service
offline/failed. Rerun automático não roda no timer padrão; é opt-in via
execução manual ou drop-in com `--rerun-network-failures --max-run-age=6h`.
O guard anti-loop fica em `/var/lib/civm/runner-watchdog-reruns.json` e o
relatório inclui `metrics.runs_considered`, `metrics.reruns_triggered` e
`metrics.reruns_skipped`.
Enquanto `/var/lib/civm/maintenance.json` existir, o watchdog retorna
`maintenance-active` antes de rede, hooks ou systemd e nunca religa o listener
drenado. Falha ao consultar esse estado também bloqueia mutações.

## Histórico

- **2026-05-10** — bootstrap inicial. Estrutura atual: 9 runbooks + 8
  templates + 5 disciplines + 6 rules + próprio CI.
