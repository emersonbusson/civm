# PRD — civmctl (zero-effort VM provisioning + maintenance)

**Status:** approved
**Author:** sessao 2026-05-10
**Discipline links:** Kahneman #1 (WYSIATI), #2 (counterfactual), #3 (numero
nao adjetivo), #5 (lib nova exige entrada).

## 1. Resumo

Construir `civmctl`, Go CLI de **zero-effort** que provisiona e mantém a VM
self-hosted que serve como GitHub Actions runner com label `civm`.
Operações idempotentes, paridade com `ubuntu-latest` (Ubuntu 24.04 LTS),
cleanup e watchdogs automatizados via systemd timers.

## 2. Contexto técnico

| Item | Valor | Source |
|---|---|---|
| OS alvo VM | Ubuntu 24.04.4 LTS | **Confirmado em docs**: actions/runner-images Ubuntu2404-Readme.md |
| Kernel alvo | 6.17.0-1010-azure (best-effort; kernel sai do provedor) | **Confirmado em docs** |
| Go versions | 1.26.3, 1.25.9, 1.24.13, 1.23.12, 1.22.12 | **Confirmado em codebase** (`internal/specs`) |
| Node versions | 24.15.0, 24.14.1, 22.22.2, 20.20.2 | **Confirmado em codebase** (`internal/specs`) |
| Python versions | 3.10.20, 3.11.15, 3.12.13, 3.13.13, 3.14.4 | **Confirmado em docs** |
| Docker | 28.0.4 (Compose v2 = 2.38.2) | **Confirmado em docs** |
| GitHub CLI | 2.89.0 | **Confirmado em docs** |
| git | 2.53.0 | **Confirmado em docs** |
| Hardware GitHub-hosted | 4 vCPU, 16 GB RAM, 14 GB SSD | **Confirmado em docs** |
| Hardware VM dono | superior (128 GB SSD, mais CPU/RAM) | **Confirmado em docs** (`AGENTS.md`, `runbooks/MULTI-PROJECT-RUNNER.md`) |
| Build/lang stack civmctl | Go 1.26 stdlib-only | **Confirmado em codebase** (`cmd/civmctl/`, `internal/**`) |

### WYSIATI — o que NÃO foi visto

- **VM real**: agente sandboxed sem SSH; toda lógica é heurística baseada em
  documentação. Bootstrap end-to-end exige humano executar.
- **Kernel exato**: provedor de VM (cloud, on-prem) define kernel, não
  conseguimos forçar 6.17.0-azure exato — só best-effort `linux-image-generic`.
- **/opt/hostedtoolcache layout**: GitHub usa estrutura específica para
  multi-version. Replicaremos via `setup-go` / `setup-node` actions oficiais
  no momento do job (não pré-instalar todas versões).

## 3. Opção recomendada

**Opção A (escolhida)**: civmctl Go stdlib-only com subcomandos de
provisionamento, manutenção e diagnóstico (`version-pins`, `bootstrap`,
`cleanup`, `health`, `doctor`, `idle-check`, `runner`, watchdogs e CI
helpers). Idempotente. Dry-run por padrão em mutações destrutivas. Timers
systemd para cleanup, disk-watchdog, runner-watchdog, reverse-watchdog e
metrics.

Alternativas consideradas e descartadas:

| Alt | Por que descartado |
|---|---|
| Ansible playbook | overhead de runtime Python; menos auditável que Go |
| Bash script | viola invariante #14 (no .sh em tools/) |
| Docker image custom | divergência de paridade; runner self-hosted oficial não roda em Docker |
| Pacote .deb | over-engineering pra 1 binário |

## 4. Requisitos funcionais (RF)

- **RF-1** `civmctl version-pins`: imprime versões alvo (do `internal/specs`).
- **RF-2** `civmctl bootstrap [--dry-run]`: provisiona Ubuntu 24.04 com tools
  na versão alvo. Idempotente. `--dry-run` é default; `--execute` aplica.
- **RF-3** `civmctl cleanup [--execute]`: limpa Docker, /tmp, _work caches,
  apt cache. Default `--dry-run`. Reporta bytes recuperados. Em `--execute`,
  aborta se detectar job/build ativo ou se não conseguir provar host ocioso.
- **RF-4** `civmctl health`: exibe disk free, memória, runners ativos,
  timers cleanup/disk/runner/reverse/metrics e última execução de cleanup.
  Exit 0 OK, 1 warning, 2 critical. Cleanup/disk-watchdog ausente é crítico;
  runner-watchdog/reverse-watchdog/metrics ausentes são warning.
- **RF-5** `civmctl runner add --token=X --url=Y --labels=civm`:
  registra novo runner GitHub. Idempotente (skip se já existe).
- **RF-6** `civmctl doctor [--repos=a/b,c/d] [--workflow=ci.yml] [--json]`:
  visão read-only consolidada de host, timers, systemd runners e GitHub
  runners. Classifica `civm-*` online como canônico, `legacy-ci-*` offline
  como legacy/stale e runner online sem label `civm` como ambíguo.
- **RF-7** `civmctl idle-check [--json]`: read-only; exit `0=idle`,
  `1=busy`, `2=unknown`.
- **RF-8** `runner restart/remove/upgrade --execute`: bloqueia fail-closed
  se o host estiver ocupado ou desconhecido antes de mutar systemd, arquivos
  ou registro GitHub.
- **RF-9** `runner watchdog [--execute] [--repos=auto|owner/repo,...]
  [--rerun-network-failures] [--max-run-age=6h]`: testa GitHub, repara
  hooks e reinicia runner offline/failed em VM idle. Rerun remoto é opt-in,
  considera só runs recentes de PR aberto e mantém guard anti-loop local por
  `run_id/head_sha`. `--repos=auto` usa `.runner` quando possível e fallback
  pelo unit name quando não conseguir resolver o diretório real. Divergência
  `busy` remoto sem `Runner.Worker` exige dwell persistente de 5 minutos. O
  restart para a unit graciosamente e só aceita falha do purge quando o cgroup
  estiver comprovadamente sem PIDs.
- **RF-10** Help auto-gerado para todos subcomandos (`civmctl --help`,
  `civmctl <sub> --help`).

## 5. Requisitos não-funcionais (RNF)

- **RNF-1** Stdlib-only. Zero deps externas. Justificativa: auditabilidade,
  binário pequeno, build reproduzível.
- **RNF-2** Cobertura de testes ≥80% em `internal/**`. Cobertura de
  `cmd/civmctl/main.go` (dispatch puro) opcional.
- **RNF-3** Binário <10 MB stripped (`go build -ldflags='-s -w'`).
- **RNF-4** `civmctl` sem args ou com `--help` responde em <100 ms.
- **RNF-5** Mutações destrutivas (cleanup, bootstrap) são **dry-run por
  default**. `--execute` exige flag explícita.
- **RNF-8** Cleanup, disk-watchdog e mutações de runner são fail-closed:
  não deletam, prunam, param runner nem fazem upgrade quando há
  `Runner.Worker`, processo dentro de `_work` ou build Docker ativo.
  `runner watchdog --execute --rerun-network-failures --max-run-age=6h`
  também não dispara rerun remoto se o host estiver ocupado ou desconhecido.
- **RNF-6** Logs em PT-BR para usuário; identifiers/comments em inglês.
- **RNF-7** Exit codes consistentes: 0 OK, 1 warning, 2 critical, 64+ erro
  de uso (sysexits.h).

## 6. Fluxos

### Fluxo bootstrap (admin VM, executado uma vez por VM)

```
admin@vm $ sudo civmctl bootstrap --execute
[civmctl] Verificando OS... Ubuntu 24.04.4 LTS [OK]
[civmctl] Instalando packages base via apt-get... [OK]
[civmctl] Instalando Go 1.26.3... [OK]
[civmctl] Instalando Docker 28.0.4 + Compose v2 2.38.2... [OK]
[civmctl] Instalando GitHub CLI 2.89.0... [OK]
[civmctl] Configurando systemd timers cleanup/disk/runner/reverse... [OK]
[civmctl] Pronto. Proximo passo: 'civmctl runner add --token=...'
```

### Fluxo cleanup (systemd timer diário 04:00 UTC)

```
[systemd] Trigger civmctl-cleanup.service
[civmctl] Host idle guard: nenhum Runner.Worker/processo _work/build ativo
[civmctl] Docker prune: liberados 4.2 GB
[civmctl] /tmp older than 7d: liberados 1.1 GB
[civmctl] _work caches older than 14d: liberados 8.7 GB
[civmctl] apt cache: liberados 800 MB
[civmctl] Total: 14.8 GB. Disk free agora: 89 GB / 128 GB.
```

### Fluxo health (operacional)

```
$ civmctl health
DISK    /    89 GB free / 128 GB                     [OK]
MEM     8.2 GB free / 32 GB                          [OK]
RUNNERS civm-1 (online), civm-2 (online)    [OK]
TIMER_CLEANUP civmctl-cleanup.timer         [OK]
TIMER_DISK    civmctl-disk-watchdog.timer   [OK]
TIMER_RUNNER  civmctl-runner-watchdog.timer [OK]
TIMER_REVERSE civmctl-reverse-watchdog.timer [OK]
TIMER_METRICS civmctl-metrics.timer         [OK]
LAST    cleanup 6h ago, recovered 14.8 GB            [OK]
EXIT    0
```

## 7. Modelo de dados

Stateless. Não há banco. Estado lido em runtime do sistema operacional
(filesystem, processos, systemd, GitHub API).

## 8. API / Interfaces

CLI subcommands. Sem HTTP API. GitHub API consumida via `gh` CLI (já
instalado pelo bootstrap) ou `curl` direto, não via SDK.

## 9. Dependências e riscos

| Dep | Versão | Justificativa | Rollback |
|---|---|---|---|
| Go stdlib | 1.26 | linguagem | downgrade Go (improvável) |
| `apt-get` | sistema | provisioning Ubuntu | sem rollback (Ubuntu base) |
| `systemd` | sistema | timers de cleanup/watchdogs | rollback para desabilitar timers e operar manualmente |
| `gh` CLI | 2.89.0 | runner registration via GitHub API | rollback para PAT manual |

**Riscos:**

- R1: cleanup deleta arquivo em uso por job ativo. **Mitigação**: cleanup
  usa guard de ociosidade fail-closed antes de qualquer mutação e revalida
  antes de `rm -rf`, Docker prune e apt clean; mtime <2h é segunda camada.
- R2: bootstrap quebra estado prévio da VM. **Mitigação**: idempotente; cada
  step verifica antes de instalar; `--dry-run` default.
- R3: kernel atualizado pelo provedor diverge de 6.17.0-azure. **Mitigação**:
  documentar best-effort; jobs Linux são portáveis entre kernels recentes.

## 10. Estratégia de implementação

Fases sequenciais, cada uma comitavel:

1. `internal/specs/` — versões alvo + tests
2. `cmd/civmctl/main.go` + `cmd/civmctl/version_pins.go`
3. `internal/health/` + `cmd/civmctl/health.go`
4. `internal/cleanup/` + `cmd/civmctl/cleanup.go`
5. `internal/bootstrap/` + `cmd/civmctl/bootstrap.go`
6. `cmd/civmctl/runner.go` (gh CLI wrapper)
7. `deploy/systemd/civmctl-{cleanup,disk-watchdog,runner-watchdog,reverse-watchdog,metrics}.{service,timer}`
8. Update `runbooks/MULTI-PROJECT-RUNNER.md` e `README.md`
9. Update `.github/workflows/ci.yml` para build + test
10. Update `MEMORY.md` com hashes finais

## 11. Documentos a atualizar

- `README.md` — adicionar seção "Bootstrap em 1 comando"
- `AGENTS.md` — adicionar comandos diários
- `runbooks/MULTI-PROJECT-RUNNER.md` — substituir steps manuais por civmctl
- `MEMORY.md` — entry desta sessão

## 12. Fora de escopo

- Suporte windows-latest, macos-latest (CODEX.md DEFERRED)
- GitHub App authentication para runner registration (PAT funciona)
- Métricas Prometheus (node_exporter cobre)
- Multi-VM orchestration
- TUI/Web UI (CLI puro)

## 13. Critérios de aceitação

- [x] PRD aprovado (este documento)
- [ ] SPEC aprovado
- [ ] `go build ./...` sem warnings
- [ ] `go test -race -count=1 ./...` verde
- [ ] `civmctl --help` responde em <100ms
- [ ] `civmctl version-pins` lista todas versões em <50ms
- [ ] `civmctl health` testado em dev machine (modo degraded é OK)
- [ ] `civmctl cleanup --dry-run` testado em dev machine
- [ ] `civmctl bootstrap --dry-run` testado em dev machine (não --execute)
- [ ] Cobertura `internal/**` ≥80%
- [ ] Binário <10 MB stripped

## 14. Validação

- `go vet ./...` sem warnings
- `go test -race -count=1 -cover ./...` reporta cobertura por package
- `du -h $(go env GOPATH)/bin/civmctl` confere tamanho
- `time civmctl --help` confere latência
- Manual: `civmctl health` em dev machine, esperado: warnings sobre falta
  de runners (esperado fora de VM), disk OK, mem OK

## Rollback trigger

Se em 6 meses (2026-11-10) civmctl não estiver provisionando ≥1 VM nova OU
se cleanup quebrar disco em produção (data loss), reavaliar:

- Voltar para runbook manual + `runbooks/MULTI-PROJECT-RUNNER.md` antigo
- Considerar Ansible playbook se múltiplas VMs entraram

Decisão de reverter exige humano + ADR explicando por que civmctl falhou.
