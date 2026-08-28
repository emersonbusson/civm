# SPECv4 — Arquitetura unificada do runner box civm (re-fundação)

> Versão de **convergência** após a 3ª rodada do Passo 2.5 (red-team) — o
> "Slice -1" pedido depois do NO-GO do `SPECv3.md`. Baseline preservado:
> `PRD.md`, `SPEC.md`, `SPECv2.md`, `SPECv3.md`. Onde houver conflito, **esta
> versão prevalece**.
>
> **Veredito: GO** — porém **apenas sobre o delta mínimo Day-0** (D1 + per-runner
> cache slot + D3). A migração ampla de 6 eixos do desenho umbrella **permanece
> NO-GO** até a sua fundação ser escrita. O SPECv3 estava certo: o desenho
> derivou do código. Esta rodada re-fundou item-a-item no código real
> (arquivo:linha) e isolou o subconjunto que é shippável Day-0 sem nenhuma
> dependência fantasma.

## Por que esta rodada existe

O SPECv3 fechou NO-GO porque auditou o desenho contra o código e achou a
fundação documental apontando para o vazio (3 deep-dives + 2 DATA-REPORTs
inexistentes, citados ~40x; `runner-isolation.json`/`setup-ci-cache.sh` sem
produtor; isolamento de `$HOME` per-runner sem código). A disciplina manda
auditar até **convergir**. Esta 3ª rodada não tentou salvar o desenho inteiro —
ela perguntou a pergunta certa do Day-0: **existe um corte coerente, já
fundamentado no código, que entrega valor real sem tocar em nenhuma das amarras
fantasmas?** Existe. É o delta mínimo abaixo.

A diferença de método: o SPECv3 provou o que está QUEBRADO; o SPECv4 isola o que
está PRONTO-PARA-SHIP. Os dois são a mesma disciplina (#13: existência ≠ função)
aplicada nas duas direções.

## Mapa real shipped/gap (8 pilares, verificados em arquivo:linha)

| Pilar | Estado | Evidência / lacuna |
| --- | --- | --- |
| `admit` + MPI heavy-slot + docker sub-slot | **shipped** | `internal/admit` (WeightHeavy/Light, MaxHeavy=2, `/run/civm/admit-heavy-{1,2}.lock`, `--exclusive=docker` → `/run/civm/admit-docker.lock`, memwatchdog CheckFn fail-closed, reapOrphan, Release idempotente). INTOCÁVEL. |
| MPI / `runner-isolation` (portblock) | **shipped** | `internal/portblock` grava `/var/lib/civm/port-blocks.json` atômico; `install.go:167-178` aloca bloco por runner e escreve `CIVM_RUNNER_SLOT`/`CIVM_PORT_BASE`/`COMPOSE_PROJECT_NAME` no `.env`. OQ-3 resolvido. |
| gate self-heal (devctl + ci-router) | **shipped** | `run.go:787,800` (`yarnInstallResilient`/`installWithCacheRecovery`); `ci-router.yml` go-vet + yarn clean+retry; `applyCivmBoxBuildDefaults` `NEXT_BUILD_CPUS=1` (`run.go:98-102`). |
| ephemeral-ci spine (post-job + cron) | **shipped (spine)** | `internal/hook` job-completed (`hook.go:244-332`) mata órfãos, apaga `_work` velho, trima caches, `dockerPruneSafe` (só dangling); `internal/cleanup` cron com host-busy deferral; `diskwatchdog` horário; `safedelete`. Prune agressivo / trim per-job named-dir / hook de job-cancel: **deferidos**. |
| `dockerlock` | **shipped (não aposentado)** | `internal/dockerlock` (553 linhas) + `cmd/civmctl/lock.go` + ci-guard R4 + acme `web.yml:182` em produção. Aposentadoria é **planejada** (SPECv3 DT-v3-8), **não executada**. O desenho deve parar de tratar ITEM-5 como deleção no-op. |
| disk-budget / threshold-pct | **shipped (sem bug)** | `--threshold-pct=0` já clampa para 60 (`diskwatchdog.go:135-137` → `civm.go:37,39`); guest-prune RF-3 shipado (#122, liberou ~17.5GB). **Não há bug.** Ver D2 (rejeitado). |
| cachetrim atômico (3 modos + caps backstop) | **partial → GAP** | Implementado e testado em `fix/cachetrim-hard-ceiling` (#124), **ausente do main** (`dbb18ff` ainda tem trim por arquivo, caps 5GB/3GB). É **D1**. |
| per-runner cache slot | **GAP → fechado nesta rodada** | `CIVM_RUNNER_SLOT` existe no `.env` mas não era usado no path do cache; o cache acme era per-JOB (`yarn-acme-$GITHUB_JOB`). Fechado em acme `5dd2113eb` (ver D-slot). |

## Delta mínimo confirmado (escopo do IMPL — Day-0, GO)

### D1 — Ship `cachetrim-atomic`: merge `fix/cachetrim-hard-ceiling` → main + redeploy

A fundação que o desenho inteiro **assume shipada mas não está**. O main ainda
roda o cachetrim antigo (trim por arquivo, sem hard ceiling, caps 5GB go-build /
3GB yarn) — exatamente o que produz a corrupção ENOENT / "can't import facts"
que originou toda a saga. O branch traz `DefaultCacheGoBuildMaxGB=12` /
`DefaultCacheYarnMaxGB=12`, o struct `Cap{PackageDepth,WipeWhole}`, o
`collectUnits` 3-modos e o `TrimByAge` 2-passes com hard ceiling.

- Arquivos: `internal/civm/civm.go`, `internal/cachetrim/cachetrim.go`,
  `internal/cachetrim/cachetrim_test.go`.
- Sem reescrita: o código já existe e está testado no branch
  (`TestTrimByAgeHardCeiling*`, `TestTrimByAgeDirAtomic*`, `TestTrimByAgeWipeWhole*`).
- Day-0 limpo: o merge substitui a implementação antiga por inteiro (sem
  dual-path, sem shim).
- Risco LOW. Único risco operacional: redeploy do binário `civmctl` na box viva —
  gate por `go test ./... -race` + um tick supervisionado do disk-watchdog.
- Pré-requisito desbloqueado nesta rodada: o job "Build + test civmctl" do #124
  caía por um falso-positivo do `misspell` (PT-BR "independente" → "independence");
  corrigido pela allowlist em `fix/cachetrim-hard-ceiling` (`827829e`).

### D-slot — per-runner cache slot (resíduo aceito do SPECv2, agora fechado)

O SPECv2 (linha 100) marcou per-runner cache slot como follow-up / resíduo
aceito. A evidência viva do acme #1155 promoveu-o a delta confirmado: dois jobs
`web` de PRs distintos, em runners diferentes da box, escreviam a MESMA pasta
`yarn-acme-web` e partiam o pacote multi-arquivo `next` (`yarn lint`:
"Cannot find module 'next/dist/compiled/babel/eslint-parser'").

- Implementado em acme `5dd2113eb`: sufixo `${CIVM_RUNNER_SLOT:+-$CIVM_RUNNER_SLOT}`
  em toda pasta de cache (`web.yml`, `go.yml`, `ci-router.yml`,
  `security-scans.yml`). Um runner roda um job por vez → a pasta vira privada do
  runner → nunca há escrita concorrente. Fora da box (slot vazio) o path fica
  idêntico ao antigo.
- Day-0 limpo: reusa o `CIVM_RUNNER_SLOT` já injetado pelo MPI/portblock; sem
  produtor novo, sem migração de `$HOME`.
- Escopo: é o slot do **cache folder**, não o isolamento amplo de `$HOME`
  per-runner (que segue NO-GO, abaixo).

### D3 — Reground dos docs umbrella (este SPECv4 + banners de supersede)

- Este SPECv4 é a fonte grounded; `PRD.md`/`SPEC.md`/`SPECv2.md`/`SPECv3.md`
  recebem banner de supersede no topo, preservando a trilha de auditoria.
- O desenho para de propor os 5 eixos já shipados (admit-mpi, portblock/MPI,
  gate-selfheal, cpus:1, spine de cleanup seguro) e para de citar a provenância
  fantasma (os 3 deep-dives e 2 DATA-REPORTs inexistentes).

### D2 — REJEITADO (a 3ª rodada matou um fix-fantasma)

A síntese propôs trocar `--threshold-pct=0` por `--force`, alegando que `=0`
dispara o prune "sempre". A auditoria refutou em arquivo: `=0` já clampa para 60
(`diskwatchdog.go:135-137`), o guest-prune RF-3 já shipou e foi validado vivo
(#122, ~17.5GB liberados), e o path horário (systemd) não passa flag → default
60. **Sem ação.** É exatamente para isto que a 3ª rodada existe — barrar um
delta que parece certo e não é (#13).

## Eixos que permanecem NO-GO (migração futura, fundação a escrever)

Estes precisam de spec/decisão net-new e **não** entram no Day-0:

- **RF-1 isolamento de `$HOME` per-runner** — sem código; é a forma forte do
  D-slot (cache folder só cobre o cache, não o `$HOME` inteiro).
- **RF-2 managed cache** (`setup-ci-cache.sh`) — produtor inexistente.
- **RF-3 wipe agressivo / prune `-a` no caminho busy** — hazard de concorrência
  conhecido; o `DefaultDockerImagePruneFilter='until=168h'` é constante órfã
  (zero call-sites), deliberadamente não-cabeada.
- **RF-7 serialização out-of-band (fila "um PR por vez")** — **REJEITADA** (não
  só deferida). A fila existia para evitar a corrupção sob concorrência; isso foi
  resolvido na raiz por isolamento (per-runner cache slot) + caps + `admit`,
  mantendo a concorrência. Serializar desperdiçaria 7 dos 8 runners e enfileiraria
  PRs por horas (a box serve 7 projetos), sem ganho de correção. Cache efêmero por
  PR (a forma forte de RF-2) é PIOR pro disco — cold start re-baixa ~2GB/job, mais
  picos — não melhor; o cap backstop limita o estado estável com menos churn. O
  controle cross-PR que importa é o `admit MaxHeavy=2` (protege a RAM de 7G).
  Evidência viva: o #1155 roda concorrente e seguro. O hook de job-cancel segue
  deferido.
- **Aposentadoria do `dockerlock`** (DT-v3-8) — planejada, não executada;
  exige migrar os workflows que ainda usam `civmctl lock` para
  `civmctl admit --exclusive docker`.

## Correções da gap audit (3ª rodada operacional, 2026-06-15)

Uma auditoria multi-agente com verificação adversarial (15 agents, 47 achados,
**1 confirmado**) revisou os fixes desta sessão contra código + logs reais:

- **D-slot + cap raise CONFIRMADOS corretos**, não band-aids. As 3 mortes de job
  do #1155 NÃO tinham assinatura de OOM (`grep 137|oom|signal killed` = zero) —
  eram corrupção de cache no path bare (anterior ao slot) + um working-dir
  deletado (race classe civm#117). `cachetrim.go:310` curto-circuita antes do
  Pass 2, então o cap 12GB elimina o trim in-flight no path normal.
- **Gap confirmado e corrigido: emergency in-flight floor.** Sob
  `EmergencyBypassIdle` (≥75% disco) o cache trim rodava sem idle guard e o Pass 2
  ignora o MinProtect → podia deletar o working-set de um install vivo. Fix:
  `Options.InFlightFloor` pula dirs com escrita < 15min (só no emergency path).
  civm **#126**, com teste FS-real que fecha o buraco `GlobFn→nil` (#13).
- **`admit` é SHIPPED mas INERTE para acme (#13: existência ≠ função).** O motor
  (`MaxHeavy=2`, cgroup `MemoryMax`) é real e validado, mas é **opt-in** e nenhum
  dos 17 workflows acme o chama (`grep CIVM_JOB_WEIGHT|civmctl admit` = 0). O
  hook do runner (`JOB_STARTED`/`JOB_COMPLETED`, sem `STEP_*`) **não consegue**
  envelopar o comando de um step. Como as mortes do #1155 não eram OOM, gatear a
  RAM é **enhancement futuro**, não o fix atual — e o único caminho viável é
  envelopar o step pesado (`civmctl admit --weight=heavy --exec -- <cmd>` via
  composite action) + um CI guard, não um rewrite do hook.

## Matriz de rastreabilidade (delta GO → código)

| Delta | Repo | Arquivo / commit | Estado |
| --- | --- | --- | --- |
| D1 | civm | `internal/cachetrim/*`, `internal/civm/civm.go` (#124) | branch pronto; merge+deploy pendente |
| D1 unblock | civm | `.golangci.yml` (`827829e`) | shipado no branch |
| D-slot | acme | `web.yml`/`go.yml`/`ci-router.yml`/`security-scans.yml` (`5dd2113eb`) | shipado; validando #1155 |
| D3 | civm | este SPECv4 + banners | esta entrega |
| D2 | — | — | rejeitado (sem ação) |

## Go/No-go (fechamento)

- **GO** no delta mínimo Day-0: **D1 + D-slot + D3**. Cada um é independentemente
  mergeável, Day-0 limpo (sem shim/dual-write/compat/dead-code) e fundamentado em
  arquivo:linha.
- **NO-GO** na migração ampla de 6 eixos: continua exigindo fundação net-new;
  não regredir para tratá-la como pronta.

## Evidência de implementação (loop vivo)

- acme `5dd2113eb` (per-runner cache slot) empurrado → #1155 re-rodando; o
  critério de sucesso é o job `web` passar com a pasta per-runner fresca (`next`
  íntegro, `yarn lint` verde).
- civm `827829e` (misspell allowlist) empurrado → #124 re-rodando; o critério é
  "Build + test civmctl" voltar a verde, desbloqueando o merge do D1.
- Rollback trigger (D1): se um tick supervisionado do disk-watchdog pós-deploy
  apagar qualquer entrada do working-set de um job ativo (ENOENT em cache de job
  vivo), reverter o merge e manter o cachetrim antigo até nova análise.
