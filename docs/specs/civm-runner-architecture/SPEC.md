# SPEC — Arquitetura unificada do runner box civm

> **Supersedido por [`SPECv4.md`](./SPECv4.md)** (3ª rodada / re-fundação): GO
> no delta mínimo Day-0. Refs a deep-dives/DATA-REPORTs aqui são provenância
> fantasma — ver o mapa real shipped/gap no SPECv4. Trilha de auditoria.

> SSDV3 PASSO 2. Traduz `docs/specs/civm-runner-architecture/PRD.md` em escopo
> fechado, rastreabilidade por requisito e ordem de migração. Este SPEC é o
> **guarda-chuva**: ele fecha a ORDEM e os GATES cross-cutting; cada ITEM delega o
> diff fino ao SPEC do componente referenciado (não duplica esqueleto Go/`.ps1`).

## Escopo fechado desta implementação

**Entra agora (no trilho unificado, ordem por dependência dura):**

- **ITEM-1 — Docker prune endurecido** (RF-5/RF-6): `dockerPruneSafe` ganha
  `docker volume prune -f` (3ª perna) e, no caminho BUSY, troca `image prune -f`
  por `image prune -a -f --filter until=168h` (fia a constante órfã
  `DefaultDockerImagePruneFilter`); autoreclaim passa `--threshold-pct=1`.
  Diff fino: `docs/specs/vm-disk-budget/SPEC.md` + `ephemeral-clean-slate-ci/SPECv2.md` RF-4.
- **ITEM-2 — Isolamento per-runner** (RF-1): `HOME`/`_work`/cache disjuntos por
  runner nos units systemd. Pré-requisito DURO. Diff fino:
  `docs/specs/ephemeral-clean-slate-ci/SPECv2.md` RF-1.
- **ITEM-3 — Wipe efêmero por-job** (RF-3): reabilitar wipe total em
  `internal/hook` `job-completed`, GATEADO por ITEM-2 provado. Diff fino:
  `ephemeral-clean-slate-ci/SPECv2.md` RF-3.
- **ITEM-4 — Managed cache content-addressed local** (RF-2): `setup-ci-cache.sh` +
  action de fork nos peers + warm-up controlado. Diff fino:
  `ephemeral-clean-slate-ci/SPECv2.md` RF-2.
- **ITEM-5 — Aposentar o `dockerlock` do eixo cache** (RF-5/migração): remover do
  caminho de prune; kill-switch por janela. Diff fino:
  `ephemeral-clean-slate-ci/SPECv2.md` RF-5.
- **ITEM-6 — Guest-access serial OOB** (RF-7): `internal/serialrecover` +
  `civmctl serial-recover` + `.ps1` + register one-time. Trilho PARALELO. Diff
  fino: `docs/specs/guest-access-resilience/SPEC.md`.

**Fica explicitamente fora agora:**

- Apertar os caps de cache (34→20 GB) — re-introduz o race A2 (`vm-disk-budget` B2).
- Subir o teto de admit — RAM-bound (RF-4 INTOCÁVEL).
- Cache per-runner com `CIVM_RUNNER_SLOT` no path (cura definitiva de escrita
  concorrente) — follow-up #16.
- Isolamento de daemon Docker (`COMPOSE_PROJECT_NAME`/portas) — ORTOGONAL
  (`multi-project-isolation`).
- Gate de admit por disco no guest — follow-up (`vm-disk-budget`).
- F3 (working-set ativo > capacidade de hardware) — limite físico; fail-safe é a
  recusa de job (exit 75).

**Dependências já assumidas como prontas (FUNDAÇÃO / JÁ SHIPADO):**

- cachetrim backstop/atômico (`internal/cachetrim`, `internal/civm/civm.go:69-90`).
- `internal/admit` + `internal/memwatchdog` (`civm.go:149-167`).
- autoreclaim host-side (`deploy/windows/civm-vhdx-autoreclaim.ps1`,
  `civm-vhdx-optimize.ps1`, `civm-host-metrics.ps1`).
- hooks (`internal/hook`), `dockerPruneSafe` base (`internal/cleanup/cleanup.go`),
  `internal/dockerlock`, `registry:2` pull-through (`deploy/bin/setup-registry-cache.sh`).
- `cpus:1` (`NEXT_BUILD_CPUS=1`, RF-8, já aplicado).
- `sudo.exe` através-do-hypervisor + `Invoke-GuestUnreachableForcedReboot`.

## Matriz de rastreabilidade PRD → SPEC

| PRD  | Implementação no SPEC | Deep-dive (diff fino) |
| ---- | --------------------- | --------------------- |
| RF-1 (isolamento per-runner) | ITEM-2 | `ephemeral-clean-slate-ci/SPECv2.md` RF-1 |
| RF-2 (managed cache local)   | ITEM-4 | `ephemeral-clean-slate-ci/SPECv2.md` RF-2 |
| RF-3 (wipe por-job)          | ITEM-3 | `ephemeral-clean-slate-ci/SPECv2.md` RF-3 |
| RF-4 (admissão por RAM)      | FUNDAÇÃO (intocável; gate em ITEM-2/4) | `runner-memory-admission/SPECv4.md` |
| RF-5 (Docker prune + lock)   | ITEM-1, ITEM-5 | `vm-disk-budget/SPEC.md`, `ephemeral-clean-slate-ci/SPECv2.md` RF-4/RF-5 |
| RF-6 (teto agregado + thr-pct) | ITEM-1 | `vm-disk-budget/SPEC.md` RF-1/RF-3 |
| RF-7 (serial OOB)            | ITEM-6 | `guest-access-resilience/SPEC.md` |
| RF-8 (`cpus:1`)              | FUNDAÇÃO (já shipado) | — |

## Decisões técnicas

| #    | Decisão | Justificativa |
| ---- | ------- | ------------- |
| DT-1 | Este SPEC é guarda-chuva: fecha ORDEM + GATES, delega DIFF aos componentes | Evita duplicar/derivar esqueleto Go/`.ps1` já fechado nos specs de componente; sync rule barata |
| DT-2 | Ordem DURA: ITEM-1 (independente) → ITEM-2 (pré-req) → ITEM-3/ITEM-5 (gated) → ITEM-4 | RF-3 e RF-5 dependem fisicamente de RF-1; wipe sem isolamento repete civm#117 |
| DT-3 | `dockerlock` permanece kill-switch entre ITEM-5 e a evidência (não deletado) | Reversibilidade operacional; o pacote ainda serve o eixo `Stop-VM`/`Optimize-VHD` |
| DT-4 | Endurecer SÓ o caminho BUSY do prune, não o hook job-completed | Mexer no hook reintroduz o bug de wipe MID-JOB (`hook.go:309-317`, civm#117) |
| DT-5 | NÃO apertar os caps de cache; alívio sob pressão vem de Docker (18 GB) | Apertar aproxima o cap do working-set → race `ENOENT` A2 (incidente #1155) |
| DT-6 | `--threshold-pct=1` em vez de tratar `0` como sentinel | `0` reseta a 60 em `diskwatchdog.go:135`; mudar a semântica de 0 afeta outros call-sites |
| DT-7 | ITEM-6 (serial) é trilho PARALELO, não bloqueia o trilho de cache | Canal aditivo OOB; falha de acesso é ortogonal à corrupção de cache |

## Fronteira de atomicidade e política de rollback

- **Fronteira de atomicidade desta implementação**:
  - **Atômico**: cada `os.WriteFile` de estado; cada `docker prune` é uma chamada
    única ao daemon; cada blob content-addressed do managed cache é imutável (save
    é all-or-nothing por hash); cada `register-*.ps1`/`setup-*.sh` reconcilia.
  - **Fora da atomicidade**: o CICLO de migração (ITEM-1→ITEM-6); a transição em que
    runners isolados (RF-1) coexistem com runners ainda compartilhados durante o
    rollout per-runner; a entrega de métricas do host (best-effort SSH).
  - **Estados parciais aceitos nesta fase**: alguns runners isolados e outros não
    (durante ITEM-2), com o `dockerlock` ainda protegendo o eixo cache até ITEM-5;
    cache miss frio enquanto o backend (ITEM-4) aquece.
- **Política de rollback**:
  - **App** (`civmctl` anterior): subcomandos novos (`serial-recover`) viram no-op;
    `dockerPruneSafe` volta a `image prune -f` só-dangling.
  - **Host** (`schtasks /delete`; reverter `Set-VMComPort` em janela com VM Off →
    sempre religar): `civm-serial-console` desregistrado; `--threshold-pct` volta
    a `0`.
  - **Estado** (N/A — Day-0; arquivos/blobs efêmeros).
  - **PROIBIDO**: deixar a VM Off ao fim de qualquer caminho; `--autologin root`;
    subir o teto de admit; remover o `dockerlock` antes da evidência de ITEM-5.
  - **`forward-only` / janela**: `Set-VMComPort` (ITEM-6) e o re-registro de units
    per-runner (ITEM-2) exigem janela; ITEM-5 é forward-only após evidência.

## Mapa Kahneman por etapa crítica

| Etapa / ITEM | Disciplina | Link | Pergunta obrigatória | Evidência mínima | Abort trigger |
| ------------ | ---------- | ---- | -------------------- | ---------------- | ------------- |
| ITEM-1 (volume/image prune) | #13 Ilusão de validade | `disciplines/KAHNEMAN-DISCIPLINES.md` §13 | "O prune remove recurso de job ATIVO?" | integration: imagem tagged >7 d SUMIU **E** imagem EM USO/CREATED<7 d SOBREVIVE (par positivo é gate de merge); volume desanexado colhido **E** refcount>0 preservado | par positivo falha (recurso em-uso some) → ITEM-1 não entra |
| ITEM-2 (isolamento per-runner) | #13 + #15 | §13, §15 | "Wipe de N toca M MID-JOB?" | POC no guest `gha-ubuntu-2404`: wipe de N não altera 1 byte sob `HOME` de M | wipe cruza runner → ITEM-2 não passa; ITEM-3/ITEM-5 ficam DESABILITADOS |
| ITEM-3 (wipe por-job) | #16 + #13 | §16, §13 | "RF-1 está provado por efeito?" | GATE BINÁRIO: só habilita com ITEM-2 verde; senão hook mantém `cleanWorkRoot` atual | RF-1 não provado → wipe-por-job permanece DESABILITADO (repete civm#117) |
| ITEM-4 (managed cache) | #15 Fail-safe | §15 | "Backend down trava o CI?" | timeout duro no restore/save → cache MISS imediato → build frio; `ephemeral_cache_backend_down` (Warn) | backend down causa espera indefinida ou build sobre cache parcial → no-go |
| ITEM-5 (aposentar lock) | #13 + #14 | §13, §14 | "Runners isolados rodam docker-heavy concorrente SEM colisão?" | 2 jobs docker-heavy de runners isolados concorrentes no guest SEM colisão/corrupção | colisão/corrupção observada → lock permanece (kill-switch reativado) |
| ITEM-6 (serial OOB) | #13 + #15 | §13, §15 | "Login serial entra SOB starvation (não com VM saudável)?" | 2 números medidos sob wedge real (`serial_login_latency_ms` + par negativo `ssh true`=timeout no mesmo instante), ≥3 incidentes | PAM serial >60 s em 3/3 → serial deixa de ser PRIMARY, rebaixa para power-cycle host-side |
| RF-4 (admit, intocável) | #15 Fail-safe | §15 | "O efêmero subiu o teto de heavy?" | `DefaultAdmitMaxHeavy=2` inalterado; warm-up frio dos 8 que exceda RAM → admit recusa | teto subido em nome do efêmero → abort (PROIBIDO) |

## Checklist de segurança (pré-implementação)

- [ ] **Isolamento/concorrência**: ITEM-5 só remove o lock do eixo cache APÓS
      evidência (2 jobs docker-heavy isolados concorrentes sem colisão); até lá o
      `dockerlock` é kill-switch.
- [ ] **Exec safety**: `civmctl serial-recover` usa `exec.CommandContext` sem
      shell; `.ps1` sem `Invoke-Expression` de input externo.
- [ ] **Privilégio do host**: `Set-VMComPort`/serial-getty NÃO usam `--autologin
      root`; tasks SYSTEM com direito Hyper-V mínimo.
- [ ] **Input validation**: flags de prune/threshold e JSON de métricas validados
      antes de agir; `--threshold-pct` ∈ [1, 100].
- [ ] **Fail-closed**: backend de cache down → cache MISS (não espera); admit
      CheckFn err → backoff (nunca admite); piso CRIT → recusa de job (exit 75).
- [ ] **Secrets**: senha PAM serial vem de `CIVM_SERIAL_PASS` (env), nunca no repo,
      nunca logada nem persistida em `civm-serial-recover-last.json`.
- [ ] **Logs**: `slog`/JSON no guest; host em `V:\civm-hyperv-maintenance.log`; sem
      PII, sem segredo, sem label de alta cardinalidade.
- [ ] **Int32 clamp**: nenhum `[math]::Max(0, …)` literal nos `.ps1` novos
      (invariante #17, `ps1_safety_test.go`).

## Mudanças de estado / constantes

**Arquivo:** `internal/civm/civm.go` (bloco `const (...)`)

- **Reutilizado, INALTERADO** (RF-6/A2): `DefaultCacheYarnMaxGB=12`,
  `DefaultCacheGoBuildMaxGB=12`, `DefaultCacheNPMMaxGB=3`, `DefaultCachePNPMMaxGB=5`,
  `DefaultCacheGolangciLintMaxGB=2` (soma 34 GB backstop), `PackageDepth`,
  `WipeWhole`.
- **Reutilizado, INTOCÁVEL** (RF-4): `DefaultAdmitMaxHeavy=2`,
  `DefaultAdmitHostReserveMB=2048`, `DefaultAdmitSlotPathPrefix`,
  `DefaultAdmitDockerSlotPath`.
- **Fiado ao call-site Day-0** (RF-5): `DefaultDockerImagePruneFilter="until=168h"`
  (`civm.go:90`) — hoje ZERO call-sites (constante órfã); ITEM-1 a usa no caminho
  BUSY do `dockerPruneSafe`. **NÃO é compatibilidade** — é a constante correta
  ganhando seu uso Day-0.
- **Reutilizado** (RF-6): `DefaultHostVolumeWarnFreeGB=30`,
  `DefaultHostVolumeCritFreeGB=10` (banda 20 GB).
- **Invariante**: `CritFree(10) < WarnFree(30)`; soma dos caps de cache (34) ≤ teto
  agregado provado em `vm-disk-budget`; `MaxHeavy>=1`.
- **Disciplina Kahneman** (constante de prune crítica):
  - **Disciplina**: #13 Ilusão de validade
  - **Link**: `disciplines/KAHNEMAN-DISCIPLINES.md` §13
  - **Pergunta obrigatória**: "fiar a constante ao call-site remove recurso de job
    ativo?"
  - **Evidência mínima**: integration com par positivo (em-uso sobrevive)
  - **Abort trigger**: par positivo falha → não fia a constante

## Arquivos a CRIAR / MODIFICAR (resumo — diff fino nos deep-dives)

Este SPEC guarda-chuva NÃO replica os esqueletos; lista os pontos de toque e
aponta o componente que detalha cada um.

- **MODIFICAR** `internal/cleanup/cleanup.go` (`dockerPruneSafe`, `cleanup.go:525`):
  + 3ª perna `docker volume prune -f`; caminho BUSY usa `image prune -a -f
  --filter until=168h`. Diff: `vm-disk-budget/SPEC.md`, `ephemeral-clean-slate-ci/SPECv2.md` RF-4.
- **MODIFICAR** `deploy/windows/civm-vhdx-autoreclaim.ps1:412`: `--threshold-pct=0`
  → `--threshold-pct=1`. Diff: `vm-disk-budget/SPEC.md` RF-3.
- **MODIFICAR** `internal/hook/hook.go` (`cleanWorkRoot`, `hook.go:335`): wipe total
  por-job GATEADO por RF-1. Diff: `ephemeral-clean-slate-ci/SPECv2.md` RF-3.
- **MODIFICAR** units systemd / bootstrap: `HOME`/`_work`/cache por-runner. Diff:
  `ephemeral-clean-slate-ci/SPECv2.md` RF-1.
- **CRIAR** `deploy/bin/setup-ci-cache.sh` (espelha `setup-registry-cache.sh`):
  backend de cache local + warm-up. Diff: `ephemeral-clean-slate-ci/SPECv2.md` RF-2.
- **CRIAR** `internal/serialrecover/{classify.go,embed.go}` +
  `cmd/civmctl/serialrecover.go` + `deploy/windows/civm-serial-console.ps1` +
  `register-civm-serial-console.ps1`. Diff: `guest-access-resilience/SPEC.md`.
- **MODIFICAR** `internal/dockerlock` chamada no caminho de prune (ITEM-5): remover
  do eixo cache APÓS evidência; pacote NÃO deletado. Diff:
  `ephemeral-clean-slate-ci/SPECv2.md` RF-5.

## Observabilidade

| Evento | Level | Campos |
| ------ | ----- | ------ |
| `dockerPruneSafe` (volume+image -a) | Info | `bytes_reclaimed`, `images_removed`, `volumes_removed`, `busy` |
| `deferred-by-docker-heavy-lock` | Info | `lock_holder` |
| `ephemeral_cache_backend_down` | Warn | `key`, `op` (restore/save), `fallback=cold-build` |
| `serial_recover_*` | Info/Error | `outcome` (5 outcomes), `login_latency_ms`, `stage` |
| `vm_left_off` | Error | `attempts`, `last_error` (PROIBIDO em silêncio) |

Guest = `slog`/JSON (nunca `fmt.Println`); host = `V:\civm-hyperv-maintenance.log`.

## Contratos e documentação viva

| Documento | Atualização | Motivo |
| --------- | ----------- | ------ |
| `internal/civm/civm.go` | Alterar (call-site da constante de prune) | RF-5 |
| `internal/cleanup/cleanup.go` | Alterar | RF-5 (`dockerPruneSafe`) |
| `internal/hook/hook.go` | Alterar | RF-3 (wipe gated) |
| `deploy/windows/*.ps1` + `register-*.ps1` | Criar/Alterar | RF-6/RF-7 |
| `deploy/bin/setup-ci-cache.sh` | Criar | RF-2 |
| `internal/serialrecover/*` + `cmd/civmctl/serialrecover.go` | Criar | RF-7 |
| `runbooks/MULTI-PROJECT-RUNNER.md` §Disk/Cache | Alterar | RF-1/RF-5 |
| `runbooks/RUNBOOK-GUEST-SERIAL-RECOVERY.md` | Criar | RF-7 |
| `disciplines/INVARIANTS.md` | Alterar | novos gates (par positivo prune; gate binário wipe) |
| `README.md` ≡ `AGENTS.md` ≡ `CODEX.md` ≡ `rules/*.md` | Alterar/N/A | sync rule |
| `docs/specs/civm-runner-architecture/IMPL.md` | Criar | registro |

## Ordem de implementação

1. **Slice 0** — baseline no guest (`du`, `docker system df`, RAM, login serial com
   VM saudável). BASELINE, NÃO conta como aceite.
2. **ITEM-1** — Docker prune endurecido + `--threshold-pct=1` (RF-5/RF-6).
   Independente. Par #13 (em-uso sobrevive) é GATE DE MERGE.
3. **ITEM-2** — isolamento per-runner (RF-1). Pré-requisito DURO. POC supervisionado.
4. **ITEM-3** — wipe por-job (RF-3), GATEADO por ITEM-2 verde.
5. **ITEM-4** — managed cache local (RF-2) + warm-up controlado.
6. **ITEM-5** — aposentar o lock no eixo cache (RF-5), kill-switch por janela, após
   evidência de 2 jobs docker-heavy isolados concorrentes.
7. **ITEM-6** (paralelo) — guest-access serial OOB (RF-7), aceite por 2 números em
   ≥3 incidentes.

## Plano de testes

**Guest (Go)**

- **ITEM-1**: unit do arg montado (insuficiente sozinho, #13) + integration contra
  daemon docker real: imagem tagged >7 d SUMIU + imagem EM USO/CREATED<7 d
  SOBREVIVE (gate de merge) + volume desanexado colhido + refcount>0 preservado +
  erro-não-silencioso (`a.Err` setado) + regressão do early-return
  `deferred-by-docker-heavy-lock` (lock ativo → prune não roda).
- **ITEM-3**: unit do gate binário (RF-1 não provado → wipe desabilitado, hook
  preserva workspace ativo + `_temp`).
- **ITEM-4**: unit do timeout duro (backend down → cache MISS, não espera).
- **ITEM-6**: `internal/serialrecover/classify_test.go` table-driven — 5 outcomes
  mutuamente exclusivos RED→GREEN; `embed.go` SHA-256 do `.ps1` casa por construção.

**Host (PowerShell, lint + janela)**

- lint `internal/hostdisk/ps1_safety_test.go` estendido: `--autologin=0` em
  `deploy/**`; `Connect(` com timeout numérico; deadline no estágio cmd
  (`ReadTimeout`/deadline); sem `[math]::Max(0, …)` Int32.
- janela supervisionada: ITEM-2 POC (wipe de N não toca M); ITEM-5 (2 docker-heavy
  isolados concorrentes); ITEM-6 (login serial sob wedge real, par negativo `ssh`).

**Manuais (evidência das etapas críticas)**

- `docker system df` antes/depois do ITEM-1 (bytes reclamados colado no IMPL).
- `du` sob `HOME` de N e M antes/depois do wipe (ITEM-2/ITEM-3).
- `serial_login_latency_ms` + `ssh true`=timeout no mesmo instante, ≥3 incidentes
  (ITEM-6).

## Checklist de validação

**Guest (Go)**

- [ ] `gofmt -w ./...`
- [ ] `golangci-lint run -c .golangci.yml ./...`
- [ ] `go vet ./...`
- [ ] `go test ./... -race -count=1`
- [ ] `go test -count=1 -cover ./internal/...` (≥80% por package)
- [ ] `govulncheck ./...`
- [ ] `go build -ldflags='-s -w' -o /tmp/civmctl ./cmd/civmctl` (compila, <10 MB)

**Host (PowerShell)**

- [ ] lint host (`ps1_safety_test.go`): `--autologin=0`, `Connect(` com timeout,
      deadline no cmd, sem clamp Int32.
- [ ] janela: ITEM-2 wipe não-cruzado; ITEM-5 docker-heavy concorrente sem colisão;
      ITEM-6 login serial sob wedge real; nunca deixa VM Off.

**Docs**

- [ ] Links locais resolvem (`validate-templates`).
- [ ] Sync rule no mesmo commit se contrato/convenção mudou.

**Gates cognitivos**

- [ ] Cada ITEM aponta para `disciplines/KAHNEMAN-DISCIPLINES.md`.
- [ ] Cada ITEM registra pergunta obrigatória, evidência mínima por EFEITO e abort
      trigger.
- [ ] Sem linguagem vaga em pontos críticos sem critério observável.

## Links Kahneman nos passos críticos

- **ITEM-1** → #13 (`disciplines/KAHNEMAN-DISCIPLINES.md` §13): par positivo
  (em-uso sobrevive) é gate de merge, não "dangling some".
- **ITEM-3** → #16/#13: gate binário; wipe sem isolamento provado repete civm#117.
- **ITEM-4** → #15: backend down → fail-open (cache MISS), não morre o CI.
- **ITEM-6** → #13/#15: login serial PROVADO sob starvation, não com VM saudável;
  counterfactual numérico de rollback.
- **RF-4/ITEM-2/ITEM-4** → #15: o efêmero resolve ESTADO, não PRESSÃO; o teto de
  admit é RAM-bound e NÃO sobe.
