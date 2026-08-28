---
slug: civm-disk-gate-per-batch
title: Disco limpo (≥51 GB) por batch — PR e re-run reproduzem o "disco fresco por job" do pago
milestone: —
issues: []
---

# PRD — Disco limpo (≥51 GB) por batch: PR e re-run reproduzem o "disco fresco por job" do pago

> Tipo: mudança na **decisão do orchestrator** da VM (componente Windows host:
> `civm-orchestrator-decision.ps1` + decision-table pwsh). Sem schema de banco,
> sem endpoint, sem componente novo no guest.
> Política Day-0: civm não tem produção viva com dados legados; backfill = N/A.
> Solução primária única.
> Origem: decisão do usuário (2026-06-21) consolidando a política de disco como
> invariante **por-batch**. Já registrada como nota em
> `docs/specs/orchestrator-scale-to-zero/SPEC.md` §2 ("Política definitiva") +
> RF-10; este PRD a promove a slice própria (PRD → SPEC → IMPL) por ser mudança
> estrutural num caminho que controla power-state e pode matar job.

---

## 1. Resumo

O CI **pago** dá a cada job uma VM efêmera nova com disco fresco. A box `civm` é
share-everything (1 VHDX dinâmico num `V:` de ~120 GB, 8 runners) e o VHDX
**cresce e não encolhe sozinho** — sob CI sustentado o disco entra em death-spiral
(**`V:` 39 → 16 GB/h**, `validation.md` 2026-06-17 18:38). Para manter **paridade
com o pago de forma transparente** (sem nenhum passo de disco nos workflows
consumidores), a box precisa garantir um piso de disco limpo **no início de todo
batch de checks**.

A política definitiva é: **antes de todo batch — PR inicial OU re-run, com a VM
warm ou Off — o orchestrator limpa todos os caches (`docker system prune -af
--volumes` + `docker builder prune -af` + `~/.cache/*`) e compacta o VHDX
(`Stop-VM` + `Optimize-VHD -Mode Full`) até `V:` ≥ `AdmitFloorGB` (51 GB); só
então o batch inicia. PR na fila respeita o mesmo fluxo.**

Hoje isso só é garantido no cold-start (`reclaim_before_admit`, VM-Off + fila,
`V<51`) e parcialmente no gap (`boundary_compact`, `V<40`). **Não** há garantia de
≥51 antes de um re-run quando a VM está warm e `40 ≤ V < 51` — exatamente o caso
"o PR terminou, peço re-run" descrito pelo usuário. Este PRD fecha esse gap
elevando o gate a invariante de **todo** início de batch.

Valor: cada PR/re-run começa com disco previsível e suficiente (paridade com o
pago), **sem** o workflow acme precisar de qualquer step de disco — a conformidade
é 100% da box.

---

## 2. Contexto técnico (estado atual)

Fonte: `docs/specs/orchestrator-scale-to-zero/SPEC.md` (que cita o código).

- **Decisão pura testável.** `Get-OrchestratorDecision` (`civm-orchestrator-decision.ps1`)
  recebe o estado observado (power-state, fila do GitHub, `V:` livre) e devolve
  **uma** ação string sem tocar a VM; o `switch` em `civm-vm-orchestrator.ps1`
  executa. Isso permite a **decision-table** testar o mesmo módulo deployado sem
  Hyper-V (SPEC §2).
- **Pisos de disco (Host `V:`, GB livres)** — SPEC §4.1:
  `AdmitFloorGB=51` · `BoundaryCompactFloorGB=40` · `WarnFloorGB=28` ·
  `PanicFloorGB=18`.
- **Ações de disco hoje** (SPEC §2 tabela, §4):
  - `reclaim_before_admit` — VM **Off** + fila + `V<51` → limpa+compacta antes de
    admitir o batch. (Só cold-start.)
  - `boundary_compact` — VM Running + `Running==0` + `Queued>0` + `V<40` →
    `Invoke-StopAndCompact` no gap, **sem matar job**.
  - `warn_clean` — `V<28` → poda online (`docker builder prune -af` + `fstrim`),
    sem stop.
  - `panic_compact` — `V<18` + `CanPanic` → compacta offline **mesmo ocupado**
    (único que **mata** job).
  - `stop_and_compact` — idle ≥ `IdleStopMinutes` → limpa-tudo + compacta.
- **Primitivos reusáveis:** `Invoke-StopAndCompact` (limpa todos os caches +
  `Optimize-VHD`), `civm-reclaim-gate.ps1` (gate de 2 fases, evita compactar sem
  slack), `AdmitReclaimAttempts<2` (tentativas limitadas de reclaim).
- **Cache de build é descartado de propósito** (`docker builder prune --force
  --all` incondicional; `--filter until=` foi removido em `7e9cc0d` por apagar
  imagem vendor recém-puxada → "No such image"). Preservar cache entre runs **já
  falhou** (enche o disco). Invariante.

## 3. Problema / Gap

O predicado de "garantir ≥51 antes de iniciar trabalho" só cobre **VM Off**
(`reclaim_before_admit`). Quando a VM está **warm** e um batch novo vai começar
(`Running==0` + `Queued>0`), o único gate é `boundary_compact`, que só dispara em
`V<40`. Logo, com `40 ≤ V < 51`, um **re-run** (ou um próximo PR) inicia com menos
de 51 GB — violando a política e divergindo da paridade com o pago (que sempre
começa fresco). Em CI sustentado (death-spiral), esse intervalo 40–51 é
frequente entre batches.

## 4. Opção recomendada — `reclaim_before_batch` (generalizar o gate a todo batch)

Adicionar **uma** ação à decisão pura: **`reclaim_before_batch`**, que dispara
quando **um batch novo vai iniciar e o disco está abaixo do piso de admissão**,
**independente do power-state**:

```
Running == 0  AND  Queued > 0  AND  V < AdmitFloorGB(51)   →  reclaim_before_batch
```

Efeito (no `switch`): `Invoke-StopAndCompact` (limpa todos os caches +
`Optimize-VHD`) — reusando o caminho já existente do `reclaim_before_admit` /
`boundary_compact` — e só admite o batch (`start`/`mark_busy`) quando `V` voltar a
≥51. `Running==0` garante que **nenhum job está rodando** (o "PR inteiro terminou"),
então é seguro parar/compactar sem matar trabalho.

Por que esta opção:
- **Uniformiza** PR inicial e re-run e próximo-PR sob o mesmo predicado — o
  orchestrator **não precisa distinguir** "re-run" de "novo PR"; ambos são "batch
  novo prestes a iniciar com `Running==0`".
- **Reusa** `Invoke-StopAndCompact` + gate de 2 fases; muda só a **condição de
  disparo** na decisão pura + casos na decision-table — superfície mínima.
- **Transparente** ao workflow acme (paridade): nada muda no consumidor.
- Mantém `panic_compact` (`<18`, mata job) como fallback de emergência e
  `warn_clean` (`<28`, online, sem stop) como alívio **com job rodando**
  (`Running>0`) — entre batches (`Running==0`) o próprio gate compacta até 51.

Relação com os gates atuais: `reclaim_before_batch` **subsume** o
`boundary_compact` (eleva o piso de 40 para 51 no gap) e **estende** o
`reclaim_before_admit` (que passa a ser o caso VM-Off do mesmo predicado).
**Precedência (definitiva):** `panic` (`<18`) precede o gate; **o gate de admissão
warm precede `warn_clean`** — `warn_clean` (online, não recupera o `V:` do host) só
atua com job rodando (`Running>0`); entre batches (`Running==0`) o gate compacta até
51. A ação implementada é `boundary_compact` retunada (ver SPECv4 DT-6).

## 5. Alternativas descartadas

- **Manter só `boundary_compact` @40 + `reclaim_before_admit` @51-Off:** é o estado
  atual — deixa o buraco 40–51 no re-run warm. Rejeitada (é o gap).
- **Distinguir "re-run" de "PR novo" via API do GitHub:** desnecessário e frágil
  (o índice de runs é stale — RF-7 do orchestrator). O predicado `Running==0 +
  Queued>0 + V<51` cobre ambos sem distinguir.
- **Compactar a cada PR/re-run incondicionalmente (mesmo com `V≥51`):** desperdício
  — `Optimize-VHD` custa ~minutos (cold-start); compactar com disco já saudável só
  adiciona latência. O gate é por **pressão** (`V<51`), não por contagem.
- **Preservar/retornar cache para acelerar (cache durável, `--filter until=`):**
  rejeitada — já falhou (enche o disco; `7e9cc0d`). Fora da realidade da box.

## 6. Requisitos funcionais

- **RF-1.** Todo início de batch (PR inicial OU re-run) só admite com `V ≥ 51`;
  abaixo disso, `reclaim_before_batch` (limpa-tudo + `Optimize-VHD`) roda ANTES de
  iniciar, **warm ou Off**. _(SPECv3 DT-5: no caminho **warm** o gate é **host-only**
  — o snapshot guest é de 10 min, stale demais para decidir um compact de ~8 min
  [Kahneman #15]; o guest é coberto no Off-path pós-`Stop-VM`. A ação implementada
  é `boundary_compact` retunada, não `reclaim_before_batch`.)_
- **RF-2.** `reclaim_before_batch` só dispara com `Running==0` (nenhum job ativo).
  Job rodando + disco crítico continua sendo tratado por `panic_compact` (`<18`,
  pode matar) e `warn_clean` (`<28`, online) — esta ação **nunca** mata job.
- **RF-3 (fail-safe, Kahneman #15).** O reclaim é best-effort com tentativas
  limitadas (reusar `AdmitReclaimAttempts<2`). Se `V` não alcançar 51 após o
  limite, **não deadlocka**: admite o batch com um evento de warning rastreável
  (`disk_below_floor_admitted`) — não travar a CI por disco irrecuperável.
- **RF-4 (precedência definitiva).** `panic_compact` (`<18`, pode matar job) avalia
  **antes** do gate de admissão warm. O **gate precede `warn_clean`**: com
  `Running==0` (entre batches) o gate compacta até 51; `warn_clean` (`<28`, online,
  não recupera o `V:` do host) só atua com **job rodando** (`Running>0`). Ver
  SPECv4 DT-6 + a decision-table.
- **RF-5.** Cache de build segue descartado (`builder prune --all`); proibido
  reintroduzir `--filter until=` (invariante; `7e9cc0d`).
- **RF-6 (observabilidade).** Cada disparo emite linha no
  `V:\civm-orchestrator.log` com a razão e `v_free_gb` antes/depois.

## 7. Abuse / edge cases

- Re-run chega **com job ainda rodando** (`Running>0`): não compacta (RF-2);
  espera o batch corrente terminar.
- Rajada back-to-back sem gap real (novo job entra `in_progress` antes de
  `Running` chegar a 0): `reclaim_before_batch` não dispara; `panic_compact`
  permanece o fallback (mata 1 job se `V<18`) — comportamento atual aceito.
- Disco irrecuperável (volume/imagem presa): RF-3 evita deadlock (admite com
  warning após N tentativas).
- `Optimize-VHD` pendura: reusar `Get-VhdxSizesWithTimeout`/`ExecutionTimeLimit`
  já existentes (SPEC §4 — gate host-aware com timeout).

## 8. Kahneman / risco operacional (candidato a Passo 2.5 red-team)

- **#13 (existência ≠ função):** provar por EFEITO na decision-table —
  `warm + Running==0 + Queued>0 + V=45 → reclaim_before_batch`; `V=55 → start`;
  `Running>0 (qualquer V) → nunca reclaim_before_batch`; `V=45 + Queued==0 →
  idle/stop` (não compacta sem batch). Rodar `pwsh` na box (mesmo módulo deployado).
- **#3 (número, não adjetivo):** piso 51 ancorado no `AdmitFloorGB` medido; não
  inventar novo número.
- **#14 (retry calibrado):** reclaim com tentativas limitadas; sem loop infinito.
- **#15 (fail-safe é default):** disco irrecuperável → admite com warning, nunca
  deadlock; nunca mata job fora do `panic`.
- **#16 (idempotência):** o ciclo multi-tick compacta→admite é re-executável sem
  efeito duplicado (lock `V:\civm-reclaim.lock` + gate de slack).

## 9. Rollback trigger (numérico / observável)

- Um batch (PR ou re-run) iniciar com `v_free_gb < 51` medido no log após o gate.
- OU um batch nunca ser admitido (deadlock) por > 75 min (orçamento de wait).
- OU `panic_compact` (mata job) passar a disparar com mais frequência após a
  mudança (sinal de que o gate entre-batches não está aliviando o disco).
Reverter = restaurar a condição anterior (`boundary_compact` @40 + `reclaim_before_admit`
@51-Off) na decisão pura.

## 10. Fora de escopo

- Serialização transparente de docker-heavy (lock) — relacionado à paridade do
  **workflow acme**, item à parte.
- Pré-build/pull de imagens; cache durável.
- Frescor TOTAL por job (clean-slate real) — só com VM-por-job (🧱,
  `PAID-CI-PARITY.md` §5).

## 11. Validação (o que o SPEC/IMPL deverá provar)

- **Decision-table (pwsh, na box):** novos casos do §8 passam, 0 regressão nos
  casos existentes do orchestrator.
- **Ao vivo:** capturar no `V:\civm-orchestrator.log` um `reclaim_before_batch`
  real (warm + `Running==0` + `Queued>0` + `V<51`) seguido de admissão com
  `V≥51`; e um re-run que só inicia após o ciclo.
- **Registrar em `validation.md`** (categoria `orchestrator-decision` +
  `disk-reclaim`) com dados medidos antes/depois.
- Pipeline: este PRD → **Passo 2** (`SPEC.md`) → **Passo 2.5** (red-team, risco
  operacional) → **Passo 3** (IMPL: `civm-orchestrator-decision.ps1` +
  decision-table + `civm-vm-orchestrator.ps1` switch + deploy via `activate`).
