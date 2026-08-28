# Runbook — civm como runner self-hosted compartilhado entre projetos

> **Quando usar:** múltiplos repositórios do mesmo operador rodam CI em
> paralelo no mesmo runner self-hosted `civm`, sem conflito de portas,
> volumes Docker, work directories ou crosstalk de dados.
>
> **Modelo conceitual (importante):** civm e' **mirror visivel no
> GitHub**, nao gate alternativo de validacao. O gate de verdade do
> projeto e' o gate local do peer (ex.: `make ci`, `npm test`,
> `go test ./...` ou equivalente) que cada dev roda no laptop ANTES de
> push (ver [`LOCAL-CI-DISCIPLINE.md`](./LOCAL-CI-DISCIPLINE.md) §"Modelo conceitual").
> Este runner existe pra postar checkmark verde no PR sem custo de
> Actions minutes — a validacao real ja aconteceu antes do push, em cada
> laptop. Aplica-se identicamente a qualquer peer repo: dev valida local,
> push, civm posta verde.
>
> **Companion runbooks:**
> - [`CI-BILLING-FALLBACK.md`](./CI-BILLING-FALLBACK.md) — Camada 1+2 do
>   mirror visivel no GitHub (peer-specific).
> - [`LOCAL-CI-DISCIPLINE.md`](./LOCAL-CI-DISCIPLINE.md) — gate local do
>   peer como validacao real do projeto (mandatorio antes de push).

## Topologia alvo

```
+------------------------------------------------------+
| VM "civm" (Ubuntu 24.04 LTS, 12 vCPU, 12 GiB RAM) |
|                                                      |
|  systemd services:                                   |
|   - actions.runner.<owner>-<repo-a>.civm-a.service   |
|   - actions.runner.<owner>-<repo-b>.civm-b.service   |
|   - actions.runner.<owner>-<repo-c>.civm-c.service   |
|     (or org-level if 3 repos)                        |
|                                                      |
|  Cada runner tem:                                    |
|   - Label: civm                                  |
|   - Work dir: ~/actions-runner-<short>/_work         |
|   - PID separado, processo isolado                   |
|                                                      |
|  Ferramentas no PATH (compartilhadas):               |
|   - go 1.26 (ou actions/setup-go@v5 instala)         |
|   - gh CLI ≥ 2.40 (compartilhado, auth via secret)   |
|   - git ≥ 2.30, curl, jq, bash                       |
|   - docker (opcional; cada compose com project name  |
|     único — ver §"Riscos compartilhados")            |
+------------------------------------------------------+
```

GitHub auto-distribui jobs entre runners disponíveis. Com N=3 runners, até
3 jobs simultâneos. Sobrando demanda, jobs ficam em queue até runner liberar.

**Dimensionamento:** começar com N = número de repos ativos. Escalar se o
gate requerido do peer ficar consistentemente em queue >2 minutos.

## Pattern: 1 runner por peer-repo na mesma VM

GitHub Actions roteia jobs do repo X **somente** pro runner registrado
em X. Não existe runner cross-repo em conta personal (só org-runners
em GitHub Teams/Enterprise).

**Topologia padrão:**

```
<vm>
├── ~/actions-runner-a/               (civm-a -> owner/repo-a)
├── ~/actions-runner-b/               (civm-b -> owner/repo-b)
├── ~/actions-runner-c/               (civm-c -> owner/repo-c)
└── /etc/systemd/system/
    ├── actions.runner.owner-repo-a.civm-a.service
    ├── actions.runner.owner-repo-b.civm-b.service
    └── actions.runner.owner-repo-c.civm-c.service
```

Cada runner usa o mesmo label `civm`. Workflows com
`runs-on: [self-hosted, civm]` no peer X só serão executados pelo
runner de X. Sem crosstalk entre repos.

### Adicionar runner pra novo peer (1 comando)

```bash
# 1. Token de registracao (efemero ~1h, GH escopo "repo")
TOKEN=$(gh api -X POST /repos/<owner>/<repo>/actions/runners/registration-token --jq .token)

# 2. Dry-run primeiro
civmctl runner add --repo=<owner>/<repo> --token="$TOKEN" --short=<short>

# 3. Aplicar na VM
sudo civmctl runner add --repo=<owner>/<repo> --token="$TOKEN" --short=<short> --execute

# 4. Verificar online
gh api /repos/<owner>/<repo>/actions/runners --jq '.runners[]|"\(.name) \(.status)"'
```

### Runner persistente com workspace limpo por job

Modelo atual: runner persistente por repo. Isolamento vem do workspace
per-job e do cleanup operacional, não de runner efêmero/JIT:

- `GITHUB_WORKSPACE` unico per-job (delete + recreate entre jobs)
- `actions/checkout@v7` faz fresh git clone (sem state preservado)
- `civmctl-cleanup.timer` (04:00 UTC) limpa Docker/tmp/_work diariamente,
  mas aborta se detectar job/build ativo
- `civmctl-runner-watchdog.timer` repara hooks e runner offline/failed sem
  mutar se houver job/build ativo; o timer padrão não faz rerun automático
- Multiplos runners da mesma VM tem `--work _work` separado por runner

Cada job comeca do zero, sem crosstalk.

### Rollback de runner peer (1 comando)

```bash
TOKEN=$(gh api -X POST /repos/<owner>/<repo>/actions/runners/remove-token --jq .token)
civmctl runner remove --short=<short> --token="$TOKEN" --execute
```

Faz `svc.sh stop` + uninstall + `config.sh remove` + `rm -rf dir`.
Se stop/uninstall falhar, aborta antes de desregistrar ou remover
diretorio. Token mascarado nos logs.

### Remover runner legacy offline (manual)

`civmctl doctor` apenas reporta runners legacy/stale; nunca apaga
registro GitHub automaticamente. Depois de confirmar que o runner offline
nao e mais usado:

```bash
gh api /repos/<owner>/<repo>/actions/runners \
  --jq '.runners[] | select(.status=="offline") | "\(.id) \(.name)"'

gh api -X DELETE /repos/<owner>/<repo>/actions/runners/<RUNNER_ID>
```

Equivalente manual (se civmctl indisponivel):

```bash
ssh gha-ubuntu-2404 "cd ~/actions-runner-<short> &&
  sudo ./svc.sh stop && sudo ./svc.sh uninstall &&
  ./config.sh remove --token '$TOKEN' &&
  rm -rf ~/actions-runner-<short>"
```

Workflow do peer volta a rodar 100% em `ubuntu-latest` (com risco de
billing-block continuar derrubando jobs em <10s).

## Setup zero-effort (recomendado): civmctl bootstrap

A partir de 2026-05-10, **provisionamento e cleanup são automatizados** via
`civmctl` (Go binary do proprio repo civm). Specs travadas em
`internal/specs/specs.go`, seguindo `actions/runner-images` Ubuntu2404-Readme.md.

```bash
# Numa VM Ubuntu 24.04 LTS limpa, como root:

# 1. Build civmctl (uma vez; precisa Go ≥ 1.26 instalado manualmente OU
#    rodar bootstrap do acme que ja tem Go).
git clone https://github.com/acme/civm.git /opt/civm
cd /opt/civm && go build -o /usr/local/bin/civmctl ./cmd/civmctl

# 2. Confere versoes alvo (paridade com ubuntu-latest)
civmctl version-pins

# 3. Dry-run primeiro (default; nao modifica nada)
sudo civmctl bootstrap

# 4. Aplicar
sudo civmctl bootstrap --execute

# 5. Instalar systemd timers operacionais
sudo cp /opt/civm/deploy/systemd/civmctl-*.service /etc/systemd/system/
sudo cp /opt/civm/deploy/systemd/civmctl-*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now civmctl-cleanup.timer civmctl-disk-watchdog.timer civmctl-runner-watchdog.timer civmctl-reverse-watchdog.timer civmctl-metrics.timer

# 6. Health check (deve retornar exit 0)
civmctl parity
civmctl health
civmctl doctor --repos=auto --json
civmctl capacity --json
civmctl disk-audit --json
```

### Atualizar civmctl em runner existente

Depois do bootstrap inicial, novas versões do binário entram via
`civmctl self-upgrade` (a partir de v1.4.0). Deploy quando um PR de
interesse merge em `main`:

```bash
# Na VM do runner, como qualquer usuario com sudo:
cd /opt/civm && git pull --ff-only
sudo civmctl self-upgrade --execute
```

O subcomando builda em `/usr/local/bin/.civmctl.new`, valida via
`--help`, e faz `os.Rename` atomico para `/usr/local/bin/civmctl`.
Em qualquer falha (build, verify, rename) o binario existente fica
intacto e o temp e' removido — seguro pra rodar sem manutencao.

Dry-run (default) imprime tamanho do binario atual e confirma `source_dir`:

```bash
sudo civmctl self-upgrade
```

Para auditoria em pipelines:

```bash
sudo civmctl self-upgrade --execute --json
# {"executed":true,"target":"/usr/local/bin/civmctl",
#  "verified":true,"swapped":true,"old_size":7160073,"new_size":7158912}
```

Re-instalacao de hooks **nao** e' necessaria: os scripts
`/opt/civm/hooks/job-{started,completed}.sh` continuam invocando
`/usr/local/bin/civmctl` (substituido in-place).

`health`/`doctor` podem retornar warning `LAST cleanup timer nunca rodou`
até o primeiro disparo real do `civmctl-cleanup.timer` (04:00 UTC).
Aceitável apenas até passar a próxima janela diária esperada; depois
vira ação operacional para checar `systemctl list-timers`, journal da
unit `civmctl-cleanup.service` e o estado do timer na VM.

Steps idempotentes do bootstrap (todos check-then-apply):

| Step | O que faz | Skip se |
|---|---|---|
| `verify_os` | confere `/etc/os-release` ID=ubuntu VERSION_ID=24.04 | OS errado → erro |
| `verify_uid` | confere UID=0 | UID≠0 → erro |
| `apt_base_packages` | instala build-essential, curl, wget, jq, git, python3, ca-certificates | dpkg-query reporta ja instalados |
| `install_go` | baixa tarball go1.26.3 com SHA256 e instala em /usr/local/go | `go version` reporta versao alvo |
| `install_node` | baixa NodeSource setup_24.x com SHA256 + apt install nodejs | `node --version` existe |
| `install_docker` | apt repo Docker CE + plugin compose | `docker --version` reporta 28.0.4 |
| `install_gh` | apt repo cli.github.com + apt install gh | `gh --version` reporta 2.89.0 |
| `install_yq` | baixa `yq_linux_amd64` com SHA256 e instala em `/usr/local/bin/yq` | `yq --version` reporta versao alvo |
| `install_systemd_timers` | enable --now cleanup, disk-watchdog, runner-watchdog, reverse-watchdog e metrics | systemctl is-enabled retorna enabled |

Cleanup automatico (systemd timer diário 04:00 UTC):

| Action | O que limpa | Threshold |
|---|---|---|
| `tmp_old` | `/tmp` antigos | mtime >7 dias E >2h |
| `work_old` | artefatos antigos em `/home/*/actions-runner-*/_work`; preserva `_tool` e `_actions` | mtime >14 dias E >2h |
| `docker_prune` | `docker system prune -af --volumes` | (sem threshold; remove tudo nao usado) |
| `apt_cache` | `apt-get clean && apt-get autoremove -y` | (libera /var/cache/apt) |

`civmctl cleanup --dry-run` (default) lista o que seria liberado sem
deletar. `--execute` aplica somente se provar que o host está ocioso.

Garantias anti-crosstalk:

- aborta se detectar `Runner.Worker`, `Runner.PluginHost`, processo em
  `/_work/`, `docker build`, `docker compose`, `buildx` ou `buildctl`
- fail-closed se o detector não conseguir ler processos
- checa no início e revalida antes de cada mutação (`rm -rf`, Docker prune,
  apt clean/autoremove)
- preserva `_work/_tool` e `_work/_actions`, evitando download frio de
  toolchains/actions em todo job
- `flock /run/civmctl-cleanup.lock` impede cleanup diário e disk-watchdog
  de rodarem ao mesmo tempo
- `flock /run/civmctl-runner-watchdog.lock` impede watchdogs de runner
  simultâneos
- mtime <2h continua como segunda camada para arquivos recentes

Runner watchdog automático:

```bash
systemctl list-timers civmctl-runner-watchdog.timer
journalctl -u civmctl-runner-watchdog.service --since "2 hours ago"
civmctl runner watchdog --repos=auto --json
sudo civmctl runner watchdog --execute --repos=owner/repo --rerun-network-failures --max-run-age=6h
```

O serviço systemd roda a cada ~2min depois do boot. Primeiro testa
`/var/lib/civm/maintenance.json`: se o snapshot existe, retorna
`maintenance-active` antes de rede, hooks ou systemd; se não puder consultar,
falha fechado com `maintenance-state-unknown`. Só fora de maintenance testa
conectividade com GitHub. Se a rede estiver fora, sai `1` com evento
`network-down` e não muta nada. Quando a rede volta, exige host idle,
reconcilia hooks e reinicia units `actions.runner.*` offline/failed. O timer
padrão usa `civmctl runner watchdog --execute --repos=auto --json`; não passa
`--rerun-network-failures`. Em `--repos=auto`, o watchdog tenta ler o
`.runner` do diretório real do service para preservar owners/repos com hífen;
se falhar, usa o parser legado do unit name.

As units geradas pelo Actions Runner usam `KillMode=process`. Se um OOM mata
`runsvc.sh`, filhos `Runner.Listener`/`RunnerService.js` podem continuar no
cgroup e fazer o listener novo repetir conflito de sessão. O watchdog congela
a unit ativa, prova `Runner.Worker=0`, tenta um stop gracioso, purga resíduos
com `systemctl kill --kill-who=all --signal=SIGKILL` e só depois reinicia. Um
erro do kill só é benigno quando `cgroup.procs` prova zero PIDs; falha de prova
aborta o restart. Não contorne o guard com kill manual durante job.

Para runner de organização que aparece `busy` no GitHub sem `Runner.Worker`
local, o watchdog primeiro exige 5 minutos contínuos dessa divergência. Um
Worker real limpa o dwell; só depois o watchdog tenta um restart e persiste
cooldown de 1h. Eventos `org-busy-dwell` e `org-busy-cooldown` são esperados;
mais de um `runner-restarted` por hora com motivo
`github-busy-without-local-worker` indica binário antigo ou marker ilegível.

Rerun automático é opt-in. Quando alguém roda manualmente ou instala um
drop-in com `--rerun-network-failures --max-run-age=6h`, o watchdog confirma
runner GitHub `online` e usa `gh run rerun <run_id> --failed`. O rerun fica
limitado a: repo permitido por `--repos`, run criado nas últimas 6h, PR
aberto, conclusão `failure`/`cancelled`/`timed_out`, assinatura de
rede/checkout no log (`RPC failed`, `early EOF`, `invalid index-pack`,
`curl 56`, `curl 92`, `GnuTLS recv error` ou `Connection timed out`) e
nenhum marcador local para o mesmo `run_id/head_sha`. O marcador fica em
`/var/lib/civm/runner-watchdog-reruns.json`. O relatório JSON/texto expõe
`runs_considered`, `reruns_triggered` e `reruns_skipped`.

Primeiro rollout do runner-watchdog:

1. Publicar o binário novo na VM.
2. Rodar `civmctl runner watchdog --repos=auto --json` e revisar eventos.
3. Habilitar `civmctl-runner-watchdog.timer` sem rerun automático.
4. Revisar `journalctl -u civmctl-runner-watchdog.service` por pelo menos
   uma execução.
5. Só então testar manualmente:
   `sudo civmctl runner watchdog --execute --repos=owner/repo --rerun-network-failures --max-run-age=6h`.

Override opt-in para timer com rerun depois da validação:

```ini
# /etc/systemd/system/civmctl-runner-watchdog.service.d/rerun.conf
[Service]
ExecStart=
ExecStart=/usr/bin/flock -n /run/civmctl-runner-watchdog.lock /usr/local/bin/civmctl runner watchdog --execute --repos=auto --rerun-network-failures --max-run-age=6h --json
```

Depois do drop-in:

```bash
sudo systemctl daemon-reload
sudo systemctl restart civmctl-runner-watchdog.timer
```

### Quando usar setup manual em vez de civmctl

- VM que **nao e** Ubuntu 24.04 LTS (Debian, RHEL, Arch, etc).
- Quer customizar versoes que divergem do `internal/specs/Ubuntu2404`.
- Bootstrap falhou e quer debugar passo-a-passo.

Nesses casos, ver "## Setup operacional (manual)" abaixo.

## Setup operacional (manual, alternativa)

### 1. Provisionar a VM

O caminho suportado é o bootstrap idempotente. Baixa artefatos root com
SHA256/fingerprint pinado no código antes de instalar:

```bash
sudo civmctl bootstrap --execute
civmctl parity
```

Setup manual sem checksum pinado não é caminho operacional suportado. Se uma
VM não-Ubuntu precisar de port, portar o step correspondente de
`internal/bootstrap` para a distro alvo mantendo o mesmo contrato: download em
arquivo temporário, verificação SHA256/fingerprint e só então instalação.

### 2. Registrar N runners (1 por repo, escalável)

Cada repo gera um token de registro próprio em GitHub Settings > Actions >
Runners > New self-hosted runner ou via `gh api`. Repetir para cada repo:

```bash
TOKEN=$(gh api -X POST /repos/<owner>/<repo>/actions/runners/registration-token --jq .token)
civmctl runner add --repo=<owner>/<repo> --token="$TOKEN" --short=<short>
sudo civmctl runner add --repo=<owner>/<repo> --token="$TOKEN" --short=<short> --execute
```

`civmctl runner add` baixa o tarball do actions/runner com SHA256 pinado,
configura `config.sh`, instala `svc.sh` e mascara o token nos logs.

**Alternativa org-level:** se os 3 repos estão sob a mesma org, registrar runner
no nível da org (Settings > Actions > Runners da organização). 1 runner serve
os 3 repos sem registrar 3 vezes. Recomendado se for o caso.

### 3. Verificar online

```bash
# Por repo
gh api "repos/<owner>/<repo>/actions/runners" --jq '.runners[] | "\(.name) status=\(.status)"'

# Esperado:
# civm-1 status=online
# civm-2 status=online
# ...
```

## Isolamento por job (built-in GitHub Actions)

GitHub Actions garante automaticamente:

- **`GITHUB_WORKSPACE` único por job.** Cada job recebe um diretório
  efêmero `<work-dir>/<repo>/<repo>` que é deletado/recriado entre jobs.
  Sem crosstalk de filesystem entre repos.
- **Variáveis de ambiente isoladas.** `GITHUB_TOKEN`, `RUNNER_NAME`, etc
  são per-job; um job não vê env vars de outro.
- **Cache de actions/setup-go, actions/setup-node** é compartilhado entre
  runs DO MESMO REPO (chave inclui repo+os+lock-file hash). Cross-repo, as
  chaves divergem, sem colisão.

## Riscos compartilhados (NÃO automatizados pelo GitHub Actions)

### 1. Docker daemon shared

Se algum job spina `docker compose`, o daemon docker é único na VM.
Containers não conflitam por isolamento Linux, **mas networks e named
volumes podem colidir** se múltiplos repos usarem o mesmo `--project-name`.

**Regra:** sempre passar `--project-name <repo>-<run-id>` em qualquer
`docker compose` em CI:

```bash
docker compose --project-name "${GITHUB_REPOSITORY##*/}-${GITHUB_RUN_ID}" up -d
```

Um peer cujo ci.yml **não roda docker compose** (ex.: integration usando
stack serverless externo) tem risco zero. Para peer/acme <!-- invariant-waive:#11 -- runbook lista projetos peer no runner -->, **se
precisarem docker**, exigir essa convenção em cada step que invoca
compose.

### 2. Ports fixos

Jobs nunca devem bind portas fixas (`-p 5432:5432`, `-p 6379:6379`).
Múltiplos jobs paralelos = port collision instantânea.

**Regra:** bind portas via `0` (ephemeral) ou via env var:

```yaml
- run: docker run -p ${{ env.PG_PORT }}:5432 postgres
  env:
    PG_PORT: 0  # docker escolhe porta livre
```

OU usar testcontainers-go que automaticamente usa portas ephemeral.

### 3. Filesystem fora do workspace

Job NUNCA escreve em `/tmp/<known-name>`, `/opt/<known>`, `~/.cache/<known>`
ou outro path absoluto que outro job possa também tocar.

**Regra:** todo arquivo temporário via `mktemp -d` (que respeita `TMPDIR` e
gera path único) ou dentro de `${{ runner.temp }}`.

### 4. Disk pressure (CRITICO em VM 128GB)

VM tipica: 128GB SSD. Com 3+ repos peer rodando CI continuamente, sem
limpeza automatica o disco enche em semanas. Budget:

| Item | Tamanho tipico | Notas |
|---|---|---|
| Sistema base (Ubuntu) | 10-15 GB | inevitavel |
| Runner installations (N) | 1-2 GB | ~500MB cada |
| Workspace por job ativo | 1-5 GB | cada repo + dependencias |
| Cache actions/cache | 5-20 GB | acumula ao longo do tempo |
| Docker images + volumes | 10-50 GB | sem cleanup, cresce indefinidamente |
| Go build cache | 2-10 GB | $HOME/.cache/go-build |
| npm/pnpm cache | 1-5 GB | $HOME/.npm |
| Logs + tmp | 1-3 GB | /var/log, /tmp |
| **Buffer disponivel** | **>30 GB** | precisa pra picos |

Setup minimo: **manter sempre >30GB livres**. Abaixo disso, jobs falham
imprevisivelmente.

Ver §"Disk hygiene" abaixo para automacao.

## Runtime de isolamento multi-projeto (primitivo do civm)

> **Fonte de verdade:** `docs/specs/multi-project-isolation/SPECv2.md`.
> Esta seção formaliza, como **primitivo injetado pelo civm**, as três
> regras manuais de §"Riscos compartilhados" (project-name único, portas
> não-fixas, serialização do daemon). As regras manuais continuam
> válidas para peers que ainda não consomem os primitivos; o caminho
> Day-0 é consumir os primitivos abaixo.

### Variáveis injetadas pelo `hook install`

`civmctl hook install --execute` grava, em cada
`/home/*/actions-runner*/.env`, além dos dois hooks de job
(`ACTIONS_RUNNER_HOOK_*`), três variáveis de isolamento por runner
(civm SPECv2 RF-1 / ITEM-6 override / DT-v2-8):

| Var | Origem | Para que serve |
|---|---|---|
| `CIVM_RUNNER_SLOT` | `runnerSlot(dir)` = basename do diretório sem o prefixo `actions-runner-` (DT-v2-12) | Slot estável por runner; base do project-name |
| `CIVM_PORT_BASE` | `portblock.Allocate(slot)` — bloco de 64 portas sticky por slot dentro de `[20000,32000)` (DT-v2-2) | Base do bloco de portas host-publicadas do peer |
| `COMPOSE_PROJECT_NAME` | `= CIVM_RUNNER_SLOT` (default p/ compose fora do devctl) | Project-name docker default; peers com run-uniqueness sobrepõem em runtime |

`upsertEnv` é determinístico e **rejeita** qualquer chave `extra` com
prefixo `ACTIONS_RUNNER_HOOK_*` (DT-v2-8); as chaves de `extra` são
reanexadas em ordem alfabética após os dois hooks.

> **Nota para o acme (consumidor):** o `COMPOSE_PROJECT_NAME` injetado
> é só o `slot`. O acme **sobrepõe em runtime** com
> `projectName() = sanitize(CIVM_RUNNER_SLOT|acme) + "-" + sanitize(GITHUB_RUN_ID|local)`
> para garantir unicidade **por-run** que o slot puro não dá (acme
> SPECv2 DT-v2-1). O `CIVM_PORT_BASE` injetado vira `CIVM_*_PORT =
> base+0..10` e as URLs do `web/.env` (acme SPECv2 §Mapa de portas).

### Bloco de portas — janela `[20000,32000)`

`portblock.Allocate` aloca um bloco de 64 portas por slot dentro de
`[20000,32000)` (187 blocos), com `flock(LOCK_EX)` sobre o `StatePath`
durante todo o ciclo read→find→write e persistência via temp +
`os.Rename` atômico (civm SPECv2 ITEM-3 override / DT-v2-2). Exaustão da
janela → `ErrPortWindowExhausted`, que **falha o `hook install`**
(operador remove runner morto; sem auto-eviction no v1 — DT-v2-19).

A janela `[20000,32000)` é **disjunta** da faixa ephemeral do kernel
(`/proc/sys/net/ipv4/ip_local_port_range`, lower ≥ 32768) e dos
host-ports dos peers — evidência colada no Slice 0 (DT-v2-11). Por isso
jobs nunca devem bind porta fixa fora do bloco alocado; ver §"Riscos
compartilhados" item 2.

### Admissão docker-heavy — `civmctl admit --weight heavy --exclusive docker`

O contrato atual para um passo que cria imagens, containers, redes ou volumes
é uma admissão explícita no **mesmo comando** que executa o payload:

```bash
civmctl admit --weight heavy --exclusive docker --wait-minutes 75 --exec -- make up-local
```

Ela combina três proteções do civm: slot heavy com cgroup `MemoryMax`,
backpressure do `mem-watchdog` e sub-slot Docker global de capacidade 1. O
payload após o separador `--` fica dentro da admissão; não divida wrapper e
bring-up em steps distintos nem deixe o bring-up após `&&`. Assim o cleanup
enxerga o holder Docker ativo e não poda o daemon durante a extração.

`civmctl ci-guard` R4 aceita a forma explícita acima. `--weight auto` não é
contrato suficiente para Docker-heavy porque uma variável do workflow pode
resolvê-lo como `light`; use `--weight heavy --exclusive docker`.

### Compatibilidade legada — `civmctl lock --exec --scope docker-heavy`

Um único lock global em `/run/civm/docker-heavy.lock` serializa todo
trabalho docker-heavy entre runners do mesmo daemon. Ele continua disponível
para consumidores em migração, mas novos workflows devem usar `admit` acima.
O formato legado aceito pelo R4 é:

```bash
civmctl lock --exec --scope docker-heavy --budget 50m --wait 75m -- make up-local
```

Semântica (civm SPECv2 ITEM-4 override / DT-v2-1/17):

- `--scope` ∈ {`docker-heavy`}; outro valor → exit `64` (DT-v2-17). O
  `scope` é só rótulo de observabilidade; o lock é global.
- O **heartbeat é estendido enquanto o processo holder vive** (toda a
  vida do `--exec`). `--budget` (HOLD) é **apenas alarme**
  (`over_budget=true` no `lock_release`), **nunca** mata job vivo
  (DT-v2-1).
- Staleness (reclaimável) = **PID morto** OU heartbeat sem atualização há
  > 3× `DefaultDockerHeavyHeartbeatSeconds`. `IsActive` confere PID
  vivo **e** `pid_start_ticks` (campo 22 de `/proc/<pid>/stat`) para não
  reclamar de PID reusado (DT-v2-3).
- Aquisição = `flock(LOCK_EX|LOCK_NB)` com backoff linear 100 ms ± 10 ms
  até `--wait` (DT-v2-4).
- Cleanup/job-started que encontram lock fresco logam
  `deferred-by-docker-heavy-lock` e **retornam cedo sem erro** (no-op),
  re-executando depois (DT-v2-16) — nunca matam o holder vivo.

**Exit codes** (civm SPECv2 §Exit codes / DT-v2-7):

| Código | Significado |
|---|---|
| `0` | `--exec`: comando interno terminou com sucesso |
| _exit do comando_ | `--exec`: propaga o exit do comando interno em falha |
| `64` | flags inválidas / `--scope` desconhecido |
| `75` | não adquiriu o lock dentro do `--wait` budget (`ErrWaitBudgetExceeded`) |
| `77` | falha interna de flock/heartbeat/IO |

Não existe código para "HOLD expirado": por DT-v2-1 não há force-kill;
HOLD só marca `over_budget`.

#### Definição de "docker-heavy" (civm SPECv2 §Definição docker-heavy / DT-v2-10)

- **É docker-heavy (embrulhar em `civmctl admit --weight heavy --exclusive docker --exec`):**
  `docker compose up/down/run`, `docker build`, `docker buildx`,
  `docker pull` — qualquer operação que aloca recursos do daemon
  (imagem/container/rede/volume) e pode colidir com job concorrente.
- **NÃO é:** `docker ps`, `docker logs`, `docker inspect`,
  `docker version` (read-only).

### ci-guard — `civmctl ci-guard`

Lint do compose/workflow do peer contra os invariantes de isolamento
(civm SPECv2 ITEM-5 override / DT-v2-9/14). Disciplina única: **#5
Availability heuristic**.

```bash
civmctl ci-guard --repo-root . --mode report --json
```

| Regra | Detecta | Severidade |
|---|---|---|
| R1 | `container_name` fixo no compose | ERROR |
| R2 | host-port estática no compose | ERROR |
| R3 | compose sem project-name | ERROR |
| R4 | docker-heavy sem admissão explícita do civm | WARN (só `report`) |

`--mode enforce` retorna exit 1 se houver ≥1 ERROR não-waivado; R4
**nunca** conta como violation em `enforce`. Waiver line-based:
`# civm:ci-guard-allow <rule> <motivo>` suprime `<rule>` na próxima
linha significativa; waiver que não casa nenhum finding → WARN
`orphan-waiver`. R4 exige que o payload pesado esteja após o mesmo `--` do
`civmctl admit --weight heavy --exclusive docker --exec`; um `flock` local,
um `lock acquire` ou um wrapper em outro step não suprime o aviso.

### Gating do runner `civm-e2e` (consumidor)

Peers que precisam do bring-up docker-heavy completo (ex.: acme E2E)
usam o label opcional `civm-e2e`, gateado por GitHub variable para não
enfileirar para sempre quando o label ainda não está vivo (acme SPECv2
DT-v2-8):

```yaml
runs-on: ${{ vars.CIVM_E2E_RUNNER_AVAILABLE == 'true' && fromJSON('["self-hosted","civm","civm-e2e"]') || fromJSON('["self-hosted","civm"]') }}
```

O operador civm seta `CIVM_E2E_RUNNER_AVAILABLE=true` quando registra um
runner com o label `civm-e2e` vivo.

### Rollback trigger (isolamento docker-heavy)

Avaliado sobre os **primeiros 5 runs consecutivos de cada peer nas
primeiras 48h** pós-deploy (civm SPECv2 §Rollback trigger v2). Reverter
a slice (`self-upgrade` versão anterior + `rm
/var/lib/civm/port-blocks.json`) se **qualquer**:

- colisão de container (`docker ps --format '{{.Names}}' | sort | uniq -d` ≥1 cross-runner); OU
- `EADDRINUSE` em `[20000,32000)` no journal; OU
- mesmo `COMPOSE_PROJECT_NAME` em >1 runner ativo; OU
- lock-wait p95 > 600000 ms (10 min) sobre os 5 runs; OU
- `ci-guard --mode=enforce` com falso-positivo bloqueante não-waivável; OU
- disco em `DefaultHardFailPct` (90%) com lock docker-heavy fresco.

Abort imediato (não espera 5 runs): qualquer lock-wait único exceder o
`--wait` budget (75 min).

## Runner parity com ubuntu-latest

Para que peer repos rodem na civm **identicamente** ao GitHub-hosted
ubuntu-latest, instalar:

### Toolchains de linguagem

Preferir `sudo civmctl bootstrap --execute`. Instala Go, Node, Python,
Docker, gh e yq conforme `civmctl version-pins`, com checksum/fingerprint
pinado quando há download fora do apt.

```bash
civmctl version-pins
sudo civmctl bootstrap --execute
civmctl parity
```

### Ferramentas de build

Para depuração manual, instalar apenas pacotes de distro via apt. Não usar
instaladores remotos pipe-to-shell; se precisar de tarball/binário direto,
copiar o padrão de verificação de `internal/bootstrap`.

```bash
sudo apt install -y \
  build-essential \
  pkg-config \
  libssl-dev \
  zlib1g-dev \
  postgresql-client \
  redis-tools \
  jq \
  curl \
  wget \
  unzip \
  imagemagick

# Docker
sudo apt install -y docker.io
sudo usermod -aG docker runner
sudo systemctl enable --now docker

# Docker compose plugin
sudo apt install -y docker-compose-plugin
```

### CLIs essenciais

```bash
sudo civmctl bootstrap --execute
civmctl parity
```

### Verificar parity

Preferir o verificador autoritativo do repo:

```bash
civmctl version-pins
civmctl parity
civmctl parity --json
```

`civmctl parity` retorna `0` quando todas as ferramentas instaladas estao
em paridade aceitavel com os pins, `1` quando alguma esta ausente ou
atrasada. `ahead` cobre ferramenta local mais nova; `compatible` cobre
familia operacional equivalente para ferramentas providas pelo Ubuntu base
(ex.: Python 3.12.x e Git 2.x).

## Disk hygiene (automacao obrigatoria em 128GB)

Sem automacao, disco enche em ~30 dias com 3 repos ativos. Setup:

### Limpeza diaria legada via cron

Preferir systemd + `civmctl cleanup --execute`. O script manual abaixo é
referência legada para VM sem civmctl e **não deve ser usado em VM ativa
com runners online**, porque não tem o guard completo de
`Runner.Worker`/`_work`.

```bash
#!/usr/bin/env bash
# /opt/civm/cleanup.sh — legado, substituido por civmctl-cleanup.timer.
# Mantido aqui apenas para port manual sem civmctl.
#
# Crontab: 0 3 * * * /opt/civm/cleanup.sh >> /var/log/civm-cleanup.log 2>&1

set -euo pipefail
echo "=== cleanup $(date -Iseconds) ==="
df -h / | tail -1

if pgrep -af 'Runner.Worker|/_work/|docker build|docker compose|buildx build|buildctl' >/dev/null; then
  echo "SKIP: build/job ativo; cleanup manual abortado"
  exit 0
fi

# 1. Workspaces de jobs antigos (runner deleta, mas tmp persiste as vezes)
find /home/*/actions-runner-*/_work/_temp -mindepth 1 -maxdepth 2 -mtime +3 -exec rm -rf {} + 2>/dev/null || true

# 2. Docker images orfas + build cache
docker image prune -af --filter "until=168h" || true   # >7 dias
docker container prune -f --filter "until=24h" || true
docker volume prune -f || true
docker builder prune -af --filter "until=72h" || true  # >3 dias

# 3. Go build cache (mantem 7 dias)
go clean -cache -modcache 2>/dev/null || true
# OU mais granular:
# find ~/.cache/go-build -type f -atime +7 -delete

# 4. npm/pnpm cache
npm cache verify 2>/dev/null || true
[ -d "$HOME/.pnpm-store" ] && pnpm store prune 2>/dev/null || true

# 5. APT cache (libera 1-2GB facil)
sudo apt clean
sudo apt autoremove -y

# 6. journal logs (manter 7 dias)
sudo journalctl --vacuum-time=7d

# 7. /tmp (manter 1 dia)
sudo find /tmp -mindepth 1 -maxdepth 1 -mtime +1 -exec rm -rf {} + 2>/dev/null || true

echo "--- after cleanup ---"
df -h / | tail -1
echo
```

Se estiver portando uma VM sem `civmctl`, tornar executavel + agendar:

```bash
sudo mkdir -p /opt/civm
sudo cp cleanup.sh /opt/civm/cleanup.sh
sudo chmod +x /opt/civm/cleanup.sh

# Adicionar ao crontab do root
sudo crontab -l 2>/dev/null | { cat; echo "0 3 * * * /opt/civm/cleanup.sh >> /var/log/civm-cleanup.log 2>&1"; } | sudo crontab -
```

### Watchdog legado de espaco em disco

Cron legado que dispara cleanup agressivo quando o disco passa do
threshold (default 60% via `civm.DefaultPreCleanupPct`):

```bash
#!/usr/bin/env bash
# /opt/civm/disk-watchdog.sh — roda a cada hora
# Crontab: 0 * * * * /opt/civm/disk-watchdog.sh

THRESHOLD=60
USAGE=$(df / --output=pcent | tail -1 | tr -dc '0-9')

if [ "$USAGE" -gt "$THRESHOLD" ]; then
  echo "$(date -Iseconds) WARNING: disk at ${USAGE}% — running aggressive cleanup"
  /usr/bin/flock -n /run/civmctl-cleanup.lock /usr/local/bin/civmctl disk-watchdog --threshold-pct="$THRESHOLD" --execute

  # Se ainda alto, NAO nukar docker automaticamente durante CI.
  # Abrir incidente manual: runner pode estar segurando volumes/cache ativos.
  USAGE_AFTER=$(df / --output=pcent | tail -1 | tr -dc '0-9')
  if [ "$USAGE_AFTER" -gt "$THRESHOLD" ]; then
    echo "Still at ${USAGE_AFTER}% — manual intervention required"
    exit 2
  fi
fi
```

### Monitoramento do cron legado

Logs em `/var/log/civm-cleanup.log`. Verificar semanalmente:

```bash
# Ultimas 5 execucoes
tail -50 /var/log/civm-cleanup.log

# Tendencia de disco
grep "after cleanup" -A1 /var/log/civm-cleanup.log | tail -20
```

Se disco continua subindo apesar da automacao, investigar quem esta
escrevendo fora do workspace (job mal-comportado violando "Filesystem
fora do workspace").

### Rollback trigger (disk hygiene)

Se em 30 dias o disco passar de 90% mais de 3 vezes, escalar:

1. Reduzir N runners (1 por repo em vez de 1+ por repo)
2. Adicionar 2o disco (ou expandir VM se cloud)
3. Migrar caches grandes para volume separado
4. Reavaliar a topologia de runner persistente por repo antes de considerar
   JIT/efemero; exige novo desenho de registro e segurança

### Limpar caches antigos manualmente (interativo)

```bash
find /home/*/actions-runner-*/_work/_temp -mtime +7 -delete
```

(Legado. Em VM civm atual, usar `civmctl-cleanup.timer` em vez de cron.)

## Como peer e acme adotam o padrão router <!-- invariant-waive:#11 -- secao operacional descreve adocao por repos peer -->

O `.github/workflows/ci.yml` do acme é o template de referência.
Estrutura mínima:

1. Job `ci-router` em `runs-on: [self-hosted, civm]` que classifica
   changes + decide `use_local` via heurística.
2. Demais jobs com `runs-on:` conditional via `fromJSON`.
3. Job aggregador final `Gates (typecheck, test, build, invariants)` em
   civm como check canônico para branch protection.
4. `permissions: { actions: read, contents: read }` no topo.
5. `concurrency:` block escopado por `github.workflow + github.ref`.

Para o detector heurístico, peer/acme podem escolher entre 3 tiers <!-- invariant-waive:#11 -- repos peer -->
(em ordem de preferência operacional):

- **Tier 1 — detector via civmctl (rota mais deterministica):** chamar
  `civmctl billing-status --repo=<owner>/<repo> --workflow=ci.yml` no
  step do workflow. Mesma logica do template `ci-router`.
- **Tier 2 — detector vendor-eado:** copiar/binariar `civmctl` no peer
  quando a VM ainda nao tiver `/usr/local/bin/civmctl`. Evita acoplar
  peers a ferramentas de outro projeto.
- **Tier 3 — optimistic-retry pattern (zero-auth, self-healing):** adotar
  `docs/templates/ci-optimistic.yml.template`, que **não usa detector**.
  Sempre tenta `ubuntu-latest` primeiro com `continue-on-error: true`;
  se falhar (incluindo billing block que mata o job em <10s sem step
  rodar), dispara versão local em `civm`. Aggregator passa se ANY um dos
  dois roteamentos completou success. Pros: zero detection logic, zero
  auth, self-healing. Cons: ~5-30s de billing consumido por run quando
  ubuntu-latest morre rapido (custo baixo na pratica).

Tiers 1 e 2 funcionam com `GITHUB_TOKEN` padrão do workflow — sem PAT
extra. Tier 3 é o único que funciona mesmo se o token estiver indisponível
(quase nunca acontece em workflow context, mas é fallback final).

## Checklist de adoção (por repo)

Para cada repo (peer, acme) que vai usar civm: <!-- invariant-waive:#11 -- checklist enumera repos peer -->

- [ ] Runner registrado e online (verificar via `gh api repos/<owner>/<repo>/actions/runners`)
- [ ] Workflow `ci.yml` adota router pattern (template do acme)
- [ ] `civmctl billing-status` chamavel no workflow
- [ ] Branch protection rule de `main`:
  - [ ] `Require status checks to pass before merging` habilitado
  - [ ] `Gates (typecheck, test, build, invariants)` adicionado como
        required
  - [ ] Jobs individuais (lint, test, etc) **removidos** da lista de
        required (consolidados pelo Gates)
- [ ] `permissions: { actions: read, contents: read }` no top do workflow
- [ ] `concurrency:` block escopado por workflow+ref
- [ ] Se usa docker compose: `--project-name <repo>-${GITHUB_RUN_ID}` em
      todos os steps
- [ ] Se usa portas: nunca bind fixo, usar ephemeral
- [ ] Disk free na VM ≥50GB confirmado

## Verificação end-to-end

```bash
# 1. Ver runners online (em qualquer repo dos 3)
gh api "repos/<owner>/<repo>/actions/runners" \
  --jq '.runners[] | select(.labels[] | .name == "civm") | "\(.name) \(.status)"'

# 2. Forçar concorrência: abrir 3 PRs draft simultâneos (1 por repo)
#    com mudança trivial. Verificar que todos rodam em paralelo no
#    civm sem queue.

# 3. Ver histórico de duracao do `Gates` em cada repo:
gh run list --workflow=ci.yml --limit 5 --json databaseId,status,conclusion,startedAt,updatedAt \
  --jq '.[] | "\(.databaseId) \(.conclusion) \(((.updatedAt | fromdateiso8601) - (.startedAt | fromdateiso8601))/60)min"'

# 4. Se 3 jobs em paralelo, esperar p95 do gate ~ tempo do gate solo (sem
#    contention significativa). Se p95 dobrar, escalar N runners.
```

## Capacity planning

Heurística inicial:

| Repos ativos | Workflows típicos | Runners recomendados |
|---|---|---|
| 1 | 1 PR/dia | 1-2 |
| 2 (peer + acme) | 3-5 PR/dia, alguns simultâneos | 3-5 | <!-- invariant-waive:#11 -- linha de capacity planning lista repos peer -->
| 5+ | dezenas de PR | 5-10 + monitoramento de queue |

Métrica de stress: `gh run list --status queued --jq 'length'` retornar
>0 consistentemente = adicionar runner.

## Rollback trigger

Se em 30 dias (2026-06-09) qualquer dos seguintes acontecer:

1. **3+ ocorrências de port collision em CI** (job falhando porque outro
   job da mesma VM bindou porta) → revisar discipline de portas + adicionar
   linter
2. **Qualquer crosstalk de dados confirmado** (job de repo A vendo state
   de repo B) → investigar + abrir incidente; possivelmente migrar para
   N VMs separadas (1 por repo) ao invés de N runners 1 VM
3. **Queue p95 >5 minutos sustentado por 3 dias** → adicionar mais 2
   runners ou escalar VM
4. **Disk free <10 GB** → cleanup script + acelerar TTL de cache

Cada caso reabre este runbook + atualiza secão Capacity planning.

## Histórico

- **2026-05-10** — Primeira versão. Criada após pedido de unificar
  CI de peer + acme no mesmo runner self-hosted. <!-- invariant-waive:#11 -- entrada de historico explicita escopo de adocao -->
  Companion da Camada 1 entregue em ci.yml refactor (commit `7e5835e`).

## Hooks de job e contrato de integração

Numa VM multi-projeto compartilhada por repos de organização e pessoais, o
modelo padrão continua sendo runners persistentes por repo. O host se
comporta como worker gerenciado: limpar fronteiras de job sem destruir
caches quentes de runner.

Instale ou reconcilie a política de hooks com:

```bash
sudo civmctl hook install --execute
```

Estado alvo: o comando cria dois scripts executaveis em `/opt/civm/hooks`
que invocam o binário canônico e atualiza cada
`/home/*/actions-runner*/.env`. O GitHub Actions runner exige path do hook
terminando em `.sh`, `.ps1` ou `.js`; por isso os scripts têm sufixo `.sh`:

```bash
ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/civm/hooks/job-started.sh
ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/opt/civm/hooks/job-completed.sh
```

Cada script executa `civmctl hook job-started|completed --execute`. A
política fica em Go dentro de `internal/hook`; o shell script gerenciado é
só o adaptador exigido pelo runner para paths `.sh`. Symlinks `.sh` legados
de instalações anteriores são substituídos por esses scripts gerenciados.

Para VMs cujo layout não usa `/home/*/actions-runner*`, não edite código nem
crie script local. Passe o layout como flag:

```bash
sudo civmctl hook install --execute \
  --runner-glob='/srv/ci/actions-runner*' \
  --hooks-dir=/opt/civm/hooks \
  --civmctl-path=/usr/local/bin/civmctl
```

`--hooks-dir` e `--civmctl-path` precisam ser paths absolutos. O installer só
atualiza diretórios absolutos cujo basename começa com `actions-runner`, e
recusa roots de sistema/temporários como `/etc`, `/usr`, `/proc`, `/sys`,
`/run`, `/tmp` e `/var/tmp`.

Contrato dos hooks:

- `job-started` checa pressão de disco antes do job. Se o uso estiver acima
  do limite de pré-cleanup (`civm.DefaultPreCleanupPct`, 60% no momento),
  limpa paths seguros de workspace/cache primeiro. Se o disco continuar
  acima do limite hard-fail (90%), rejeita o job antes de a VM entrar em
  estado ruim. Races de cache como `directory not empty` viram warning
  quando o disco já ficou abaixo do hard-fail.
- `job-completed` remove workspace e temporários por job, poda estado Docker
  recuperável (`buildx prune --filter until=24h`, `image prune --filter
  until=168h`, container/volume prune), faz trim por tampa em cada cache
  ($HOME/.cache/go-build máx 5 GB, npm 3 GB, yarn 3 GB, pnpm-store 5 GB),
  limpa apt/journal e roda `fstrim`. Falhas de ferramentas auxiliares
  (buildx ausente, sudo sem NOPASSWD) viram Warning e não derrubam o hook.
- Ambos preservam `_work/_tool` e `_work/_actions`; estes são caches quentes
  de toolchains/actions e não devem ser removidos na higiene normal.
- Eventos de hook acrescentam JSON lines estruturadas em
  `/var/log/civm/hooks.jsonl`.

### Pós-release do binário na VM

Depois de publicar e instalar um novo `/usr/local/bin/civmctl`, valide que a
VM está ociosa antes de reiniciar runners:

```bash
ssh gha-ubuntu-2404 'civmctl idle-check'
```

Se retornar `idle` com exit `0`, reconcilie hooks e reinicie os services:

```bash
ssh gha-ubuntu-2404 'sudo civmctl hook install --execute --json'
```

Valide o contrato com o `doctor`; o JSON deve trazer `hook_checks` com
`HOOK_JOB_STARTED`, `HOOK_JOB_COMPLETED`, `HOOK_RUNNER_ENVS` e
`RUNNER_SERVICES` em severidade `ok`. O modo padrão `--repos=auto` infere
repos pelos services `actions.runner.*`; use `--repos=owner/a,owner/b` se o
nome do service for ambíguo, e `--repos=none` para auditoria offline sem
GitHub:

```bash
ssh gha-ubuntu-2404 'civmctl doctor --repos=auto --json'
```

Para inspeção manual dos `.env`, use:

```bash
ssh gha-ubuntu-2404 'for f in /home/*/actions-runner*/.env; do echo "$f"; grep ^ACTIONS_RUNNER_HOOK_ "$f"; done'
```

Todos os valores devem apontar para paths `.sh` gerenciados pelo civmctl:

```bash
ACTIONS_RUNNER_HOOK_JOB_STARTED=/opt/civm/hooks/job-started.sh
ACTIONS_RUNNER_HOOK_JOB_COMPLETED=/opt/civm/hooks/job-completed.sh
```

Por fim, confirme adoção/saúde dos peers críticos:

```bash
ssh gha-ubuntu-2404 'civmctl peer-status --repos=owner/a,owner/b --workflow=ci.yml'
```

Busson e outras integrações não devem parsear journal nem inferir estado da
VM pelo layout de arquivos. Use o contrato CLI estável:

```bash
civmctl capacity --json
civmctl health --json
civmctl doctor --repos=auto --json
civmctl disk-audit --json
```

`capacity --json` is the lightweight readiness endpoint. It reports disk
pressure, active runner services, active `Runner.Worker` count, and an
`accepting_jobs` boolean for dashboards, orchestration, or guarded commands
in Busson.

`disk-audit --json` is the read-only ownership endpoint. It reports the safe
roots that explain most disk growth on the VM: runner `_work`,
`_work/_tool`, `_work/_actions`, `$HOME/.cache`, `$HOME/go/pkg`,
`$HOME/codespace`, Docker reclaimable, `/var/log` and `/var/cache`.
Directories under `$HOME/codespace` are reported for human decision only;
civm does not auto-delete peer/workspace clones.
