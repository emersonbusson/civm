---
slug: civm-disk-gate-per-batch
title: SPECv5 — Disco limpo por PR via gate POR-EVENTO (transição running>0→0)
milestone: —
issues: []
---

# SPECv5 — 1 compactação por PR, por EVENTO (não por timer)

> **Nota de reconciliação (implementação):** o piso `AdmitFloorGB` foi elevado de **51→55** no código vivo (`deploy/windows/civm-orchestrator-decision.ps1`). Onde este doc cita "51", o valor vivo é **AdmitFloorGB=55** (guest floor 40; clean-slate por job `MinFreeGB`=58).

> Revisão de SPECv4 após feedback do usuário + **medição** na box. SPECv4 mirava
> "≥51 por batch" e a 1ª implementação compactava por **gap** (`running==0`), o que
> **thrashou** sob carga. A v4.1 (cooldown por-TEMPO, 20min) matou o thrash mas era a
> forma errada. **SPECv5 troca o trigger para POR-EVENTO:** compacta 1x quando o PR
> termina (transição `running` >0→0). Auto-contida; baselines preservados (`SPEC.md`,
> `SPECv2/3/4.md`).

## 1. Correção de premissa (medição, Kahneman #3/#13)

`docker system df` no guest (2026-06-21): docker ocupa **~5 GB** (1 imagem 37 MB + 53
volumes 4.9 GB + 0 build cache); guest `/` 108 G, 54 G usado. Um build do stack inteiro
usa ~10–25 GB transitórios → **um PR cabe FOLGADO em 58 GB**. O "não cabe em 58" de uma
análise anterior foi **chute, não medição** — errado. O que enche o disco é **acumulação**
(volumes/imagens/cache de runs antigos) + o VHDX que só encolhe via `Optimize-VHD` + o
**thrash do gate antigo** (compactava a cada gap `running==0`) — **não** o tamanho de um PR,
e **não** concorrência: o acme tem **1 runner só** → jobs pesados já rodam 1 por vez.

## 2. Gate POR-EVENTO (a mudança central)

`Get-RunCount` conta `workflow_runs in_progress` → `running` fica **>0 o PR inteiro** e
**→0 quando o PR termina**. Logo a transição `running` **>0→0 é o boundary natural do PR**.

- **Trigger:** o gate warm compacta SÓ quando
  `prevRunning>0 AND running==0 AND queued>0 AND V<AdmitFloorGB(51) AND attempts<2`.
- **`prevRunning`**: o `running` do tick anterior, persistido no `state` (caller grava
  `$state.prevRunning = $running` pós-switch, todo tick).
- **Durante o PR (`running>0`): nunca compacta** — o tenant-smoke (>1h) roda inteiro sem
  parar (o gate exige `running==0`).
- **Anti-thrash inerte:** `running` preso em 0 (pós-compact, VM religando, jobs não
  começaram) → `prevRunning==0` → **sem transição → sem re-compactar**. Sem timer.
- **Substitui** o `CanBoundaryCompact`/cooldown da v4.1 (removidos de decision.ps1 + caller).
- **Sem cache:** `Invoke-GuestFullClean` (já) faz `docker system prune -af --volumes` +
  `builder prune -af` + limpa `~/.cache`/`_work`/`/tmp`/journal → remove a acumulação +
  `Optimize-VHD` → ~58.
- **Backstops inalterados:** `panic` (<18) e `reclaim_before_admit` (VM-Off + fila + V<51).

## 3. Serialização — JÁ É NATURAL (1 runner), SEM lock

**Correção (2026-06-22):** o acme tem **um único runner self-hosted** (`civm-acme-org`,
labels `[self-hosted, civm]`); os jobs pesados rodam em `[self-hosted, civm]` → **um por vez**
por natureza. Não existe "concorrência pesada" no acme pra serializar → **nenhum lock é
necessário**. `civmctl lock` / `CIVM_E2E_RUNNER_AVAILABLE` **NÃO** fazem parte deste slice.

⚠️ Versão anterior deste SPEC dizia que `CIVM_E2E_RUNNER_AVAILABLE=true` ativava um
`civmctl lock --scope docker-heavy` — **ERRADO**. Essa variável **troca o runner-alvo** dos
e2e para `[self-hosted, civm, civm-e2e]`, e o runner **`civm-e2e` não existe** (confirmado:
o runner acme só tem o label `civm`) → setá-la **travaria** os e2e queued. Foi setada por
engano e **revertida** (deletada). O gate por-evento + o runner único entregam
"~58 GB por PR, 1 PR por vez" — paridade com o CI pago — **sem** lock.

## 4. Precedência (inalterada vs SPECv4)
`Off→admit barrier` → `panic(<18)` → **gate warm por-evento** → `warn(<28)` →
`mark_busy` → `idle stop_and_compact`. O gate por-evento precede o warn (warn é p/ job
rodando; só poda online, não recupera o V: do host).

## 5. Testes (decision.test.ps1) — 62 PASS / 0 FAIL (PS 5.1)
- Decision-table: `running` preso 0 (prevRunning=0) + V<51 → `mark_busy` (não compacta);
  transição >0→0 + V<51 → `boundary_compact`; `running>0` → nunca compacta.
- Unit: `Update-AdmitAttempts` / `Resolve-AdmitTransition` (código REAL, Kahneman #13).
- Stateful `Test-PrLifecycle`: ciclo (0,2,2,0,0,0) → compacta **EXATAMENTE 1x** (só na
  transição 2→0, não nos 0 presos). Convergência ≤2 compacts (anti-deadlock) preservada.

## 6. Validação ao vivo — PARCIAL (honesto)
- ✅ **Anti-thrash provado ao vivo:** `running=0, prevRunning=0, V=41<51, queued>0` por
  10+min → **zero compactações** (só `disk_below_floor_admitted`).
- ⚠️ **Compactar-na-transição-real não observado** — ARTEFATO: re-runs reusam `created_at`
  original; os runs testados tinham **22.6h** → `Get-RunCount` filtra `created_at>12h`
  (staleness) → não contados → `running=0` mesmo rodando. Em produção, runs frescos
  (created_at≈now) são contados → o gate dispara. Fechar com 1 run fresco (organico/dispatch).
- ✅ **Lock ativado não quebrou CI:** Web CI falhou em `yarn format:check` (formatação do
  PR), hooks civm OK (disk 58%).

## 7. Arquivos
- `deploy/windows/civm-orchestrator-decision.ps1` — param `PrevRunning`; gate por transição.
- `deploy/windows/civm-vm-orchestrator.ps1` — `state.prevRunning`; lê/passa/grava por tick;
  cooldown removido.
- `deploy/windows/civm-orchestrator-decision.test.ps1` — casos por-evento + `Test-PrLifecycle`.
- Deploy: `activate-orchestrator.ps1` (Unregister→copia→AST→Register, ExecutionTimeLimit PT2H).
