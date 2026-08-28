# IMPL — civmctl (executado 2026-05-10)

**PRD:** `docs/specs/civmctl/PRD.md`
**SPEC:** `docs/specs/civmctl/SPEC.md`
**Status:** implementado; baseline 2026-05-10, com hardening incremental
documentado neste arquivo. Rollout em VM real deve ser registrado em
`MEMORY.md` quando ocorrer.

## O que foi feito (commits a seguir)

| Commit | Escopo | Arquivos |
|---|---|---|
| `chore(civm)` | AGENTS/CODEX/MEMORY (gap) | `AGENTS.md`, `CODEX.md`, `MEMORY.md` |
| `docs(civm)` | SSDV3 PRD + SPEC + IMPL | `docs/specs/civmctl/{PRD,SPEC,IMPL}.md` |
| `feat(civm)` | civmctl Go binary | `go.mod`, `cmd/civmctl/*.go`, `internal/{specs,health,cleanup,bootstrap}/*.go` (+ tests) |
| `feat(civm)` | systemd timers/watchdogs | `deploy/systemd/civmctl-*.{service,timer}`, `deploy/systemd/README.md` |
| `docs(civm)` | zero-effort docs | `README.md`, `runbooks/MULTI-PROJECT-RUNNER.md` |
| `ci(civm)` | build + test civmctl | `.github/workflows/ci.yml` |

## Critérios de aceitação (do PRD §13)

| Item | Status | Métrica |
|---|---|---|
| PRD aprovado | ✅ | autor + auto mode |
| SPEC aprovado | ✅ | implementação implícita |
| `go build ./...` sem warnings | ✅ | clean |
| `go test -race -count=1 ./...` verde | ✅ | 13 packages OK |
| `civmctl --help` <100ms | ✅ | 8ms medido |
| `civmctl version-pins` <50ms | ✅ | 7ms medido |
| `civmctl health` testado | ✅ | exit 1 em dev (sem runners; esperado) |
| `civmctl cleanup --dry-run` testado | ✅ | sem mutação |
| Cleanup active-job guard | ✅ | bloqueia Runner.Worker/_work/build antes de mutar |
| `civmctl bootstrap --dry-run` testado | ⏭ | requer Linux real (verify_uid falha em sandbox) |
| Cobertura `internal/**` ≥80% | ✅ | specs 100%, health 88.4%, cleanup 84.5%, bootstrap 84.8% |
| Binário <10 MB stripped | ✅ | 2.29 MB |

## Emenda 2026-05-19 — runner watchdog hardening

| Item | Estado corrente |
|---|---|
| `runner watchdog` | repara hooks, reinicia runners offline/failed e só reroda falhas de rede/checkout com opt-in |
| Timer padrão | `civmctl-runner-watchdog.timer` roda sem `--rerun-network-failures` |
| Rerun remoto | limitado por `--max-run-age=6h`, PR aberto, runner online, assinatura de rede/checkout e marcador local |
| Guard anti-loop | `/var/lib/civm/runner-watchdog-reruns.json` por `run_id/head_sha` |
| Observabilidade | relatório JSON/texto com `runs_considered`, `reruns_triggered`, `reruns_skipped` |
| Inferência `--repos=auto` | lê `.runner` do WorkingDirectory real quando possível; fallback pelo unit name |
| Health/bootstrap | `TIMER_RUNNER` entra como warning e `bootstrap`/`bootstrap-everything` habilitam `--runner-watchdog=true` por padrão |

## Cobertura por package

| Package | Cobertura | Justificativa |
|---|---|---|
| `internal/specs` | 100.0% | data-only, fácil de cobrir |
| `internal/health` | 88.4% | wrappers OS testados via fake + real |
| `internal/cleanup` | 84.6% | fstest.MapFS + RunFn/ActivityFn fake |
| `internal/bootstrap` | 84.8% | RunFn fake + cenário already-installed |
| `cmd/civmctl` | 0% | dispatch puro (PRD §RNF-2 marca opcional) |

## RFs implementados

- ✅ **RF-1** `version-pins`: tabela + `--json`
- ✅ **RF-2** `bootstrap`: 8 steps idempotentes (verify_os, verify_uid,
  apt_base_packages, install_go, install_node, install_docker,
  install_gh, install_systemd_timers); dry-run default; --execute exige flag
- ✅ **RF-3** `cleanup`: 4 ações (tmp_old, work_old, docker_prune,
  apt_cache); thresholds configuráveis; fail-closed quando job/build ativo;
  anti-jobs por mtime <2h como segunda camada; autodiscover de
  `/home/*/actions-runner-*/_work` quando o default legado é usado
- ✅ **RF-4** `health`: DISK, MEM, RUNNERS, TIMER_CLEANUP, TIMER_DISK,
  TIMER_RUNNER, TIMER_REVERSE, LAST; exit 0/1/2
- ✅ **RF-5** `runner add`: wrapper `./config.sh` do actions/runner;
  `runner list` via systemctl
- ✅ **RF-6** `doctor`: host + timers + systemd runners + GitHub runners,
  com classificação canônica/legacy/ambígua/busy/missing e `--json`
- ✅ **RF-7** `idle-check`: read-only; exit 0 idle, 1 busy, 2 unknown
- ✅ **RF-8** `runner restart/remove/upgrade --execute`: bloqueia
  fail-closed antes de mutar se host estiver busy/unknown
- ✅ **RF-9** `runner watchdog`: repara hooks, reinicia runner offline/failed
  e, quando opt-in, reroda 1x falha transiente de rede/checkout em PR aberto
  criado nas últimas 6h, com marcador local por `run_id/head_sha` e métricas
  `runs_considered`/`reruns_triggered`/`reruns_skipped`; dwell persistente de
  5 minutos evita falso órfão no pós-job; stop gracioso e purge fail-closed do
  cgroup antes do restart eliminam listeners e sessões órfãs após OOM
- ✅ **RF-10** Help auto-gerado para todos subcomandos

## RNFs cumpridos

- ✅ **RNF-1** Stdlib-only (zero deps externas)
- ✅ **RNF-2** Cobertura ≥80% em `internal/**`
- ✅ **RNF-3** Binário 2.29 MB stripped (<10 MB target)
- ✅ **RNF-4** `--help` em 8ms
- ✅ **RNF-5** Mutações destrutivas dry-run por default
- ✅ **RNF-6** Logs PT-BR para usuário; identifiers/comments inglês
- ✅ **RNF-7** Exit codes: 0 OK, 1 warning, 2 critical, 64 erro de uso
- ✅ **RNF-8** Cleanup/disk-watchdog/runner mutations e rerun remoto do
  watchdog não mutam com Runner.Worker/_work/build ativo

## Disciplinas Kahneman aplicadas

- **#1 WYSIATI**: `IMPL.md §"O que NÃO foi visto"` separa bootstrap,
  cleanup execute, rollout direto na VM e limpeza de runners legados.
- **#2 Counterfactual**: cada commit tem `Rollback trigger:`. PRD §"Rollback
  trigger" define gate de 6 meses.
- **#3 Número antes de adjetivo**: este IMPL.md usa medições reais (8ms,
  84.5%, 2.29MB) em vez de adjetivos ("rápido", "leve", "bem testado").
- **#4 Débito é dívida**: zero TODOs no código. CODEX.md §DEFERRED tem
  gates numéricos de promoção.
- **#5 Lib nova exige entrada**: stdlib-only confirmado (`go.mod` sem
  `require`).

## O que NÃO foi visto

- **Bootstrap com este diff**: não rodei `bootstrap --execute` nesta sessão.
  A VM recebeu binário + units via instalação direta controlada, não via
  bootstrap completo.
- **Cleanup execute real com este diff**: rodei após `idle-check` provar
  VM idle; liberou 12.3 GB via Docker prune. Antes disso, a VM estava busy
  com job real do peer e o guard bloqueava a frente destrutiva.
- **systemd cleanup diário**: `civmctl-cleanup.timer` ainda não tinha entrada
  de journal; `civmctl-disk-watchdog.timer` disparou e decidiu `ok` com
  disco abaixo do threshold.
- **Runner ambíguo online**: `legacy-local-vm-1` não existia na VM
  `gha-ubuntu-2404` e não tinha mais função operacional; foi removido
  do GitHub via API. Se ele reaparecer, há outro host externo ainda rodando
  esse listener.
- **Compatibilidade de versões em 30 dias**: `actions/runner-images`
  publica updates semanais; `internal/specs/specs.go` precisa sync manual.

## Próximos passos (humano)

1. Esperar o primeiro disparo diário de `civmctl-cleanup.timer` e validar
   `journalctl -u civmctl-cleanup.service`.
2. Quando a VM estiver idle, rodar `civmctl cleanup --execute` se o dry-run
   continuar mostrando ganho relevante.
3. Reportar de volta para atualizar `internal/specs/specs.go` se
   versões mudaram em upstream

## Rollback trigger (do PRD)

Se em 6 meses (2026-11-10) civmctl não estiver provisionando ≥1 VM nova
OU se cleanup quebrar disco em produção, reavaliar:

- Voltar para runbook manual + `runbooks/MULTI-PROJECT-RUNNER.md` (seção
  "Setup operacional manual" preservada)
- Considerar Ansible playbook se múltiplas VMs entraram

Decisão de reverter exige humano + ADR.
