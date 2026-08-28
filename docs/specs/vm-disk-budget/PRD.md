---
slug: vm-disk-budget
title: Orçamento holístico de disco do V: — teto agregado + docker-prune endurecido + folga-alvo determinística
milestone: —
issues: []
---
# PRD — Orçamento holístico de disco do V: (guest 108GB / host 120GB)

> SSDV3 PASSO 1. Slug: `vm-disk-budget`. Repo: `civm`.
> Tece os levers de disco JÁ EXISTENTES sob um orçamento agregado com folga-alvo
> em GB. **Não redesenha** trim, autoreclaim nem isolamento — só os referencia e
> adiciona o teto que falta. O lever nº1 é o **Docker (18GB reclamável)**.

## Contexto (Confirmado nos dados — ver `DATA-REPORT.md`)

O runner self-hosted civm roda numa VM Hyper-V `gha-ubuntu-2404`: guest `/` de
108GB (VHDX dinâmico) hospedado no `V:` do host (120GB). Em 2026-06-15 o guest
estava a 73GB usado / 30GB livre (72%); o `V:` a 29GB livre. Os consumidores
medidos: Docker ~27GB (18GB reclamável — o maior lever), cache ~10GB natural
(34GB de cap backstop), OS+_work+go-mod ~36GB, VMRS ~12GB no V:.

Existem 3 atuadores de disco, cada um com seu spec, mas **ninguém soma os
consumidores contra os 108GB com folga-alvo explícita**. Os atuadores são
100% percent-based no guest (pre-cleanup 60%, emergency-bypass 75%, hard-fail
90% — `internal/civm/civm.go:37-47`); o único lever em GB é o gauge read-only
de `internal/health` (Warn 10GB / Crit 3GB, `health.go:95-96`) — sem atuador,
sem cleanup, sem recusa.

## Problema

**O orçamento de disco não tem dono.** Quatro lacunas medidas:

- **GAP-1 (alto) — Docker tagged-unused preso fora do idle.** Nenhum lever solta
  as 8.3GB de imagens TAGGED-unused fora do `docker system prune -af` idle
  (`cleanup.go:506`). Num box ocupado que raramente fica idle, o caminho busy
  (`dockerPruneSafe`, `cleanup.go:525`) só faz `image prune -f` (dangling) — as
  8.3GB ficam presas indefinidamente. É o maior lever reclamável SEM teto.
- **GAP-2 (bug latente) — constante órfã.** `DefaultDockerImagePruneFilter =
  "until=168h"` (`civm.go:90`) é **código morto**: definida, comentada "7 dias",
  com ZERO call-sites. Era o lever exato do GAP-1 (`image prune -a --filter
  until=168h`), nomeado e abandonado.
- **GAP-3 (premissa enganosa) — `threshold-pct=0` não é "sempre rodar".** O
  autoreclaim invoca `civmctl disk-watchdog --threshold-pct=0 --execute`
  (`civm-vhdx-autoreclaim.ps1:412`), mas `Check` reseta `0→60`
  (`diskwatchdog.go:135-136`) — `0` cai no default, não força o prune.
- **GAP-4 (sem teto agregado) — orçamento não reconciliado.** Não há documento
  que some cache (34GB cap) + docker (sem teto) + OS/_work (~36GB) + VMRS (12GB)
  contra os 108/120GB com folga-alvo determinística. A recusa a exit 75 no piso é
  o ÚNICO backstop do orçamento — exatamente o que deveria ser raro
  (`host-volume-reclaim-liveness` RF-4), não estrutural.

## Objetivo

Fixar uma **folga-alvo em GB** para o V:, provar com número que o pior caso a
respeita, e endurecer o lever nº1 (Docker) para soltar os 18GB reclamáveis mesmo
sob carga, **sem redesenhar** os outros planos. Não é objetivo fazer caber um
working-set ATIVO maior que o disco (F3 = hardware, fora de escopo).

## Opção recomendada

**Orçamento agregado com folga-alvo ≥30GB livres no V:** (reusa
`DefaultHostVolumeWarnFreeGB=30`, civm.go:97, já alinhado ao runbook), e
**docker-prune endurecido no caminho BUSY** fiando a constante órfã GAP-2
(`image prune -a -f --filter until=168h`) — solta imagens tagged-unused >7d MESMO
com host busy, sem o risco de hook.go:309-317 (o filtro por CREATED-date só atinge
vendor images antigas, nunca uma recém-puxada por job ativo).

Alternativas descartadas:

- **Apertar os caps de cache para liberar disco** — descartado: o cache é 24GB de
  teto morto inerte (nunca morde o working-set); apertá-lo demais re-introduz o
  trim no meio do install (o incidente que cachetrim-yarn-atomic A2 curou). O
  lever real é Docker, não cache.
- **Teto de docker via mecanismo novo (cgroup/quota no daemon)** — descartado:
  o autoreclaim RF-3 + `disk-watchdog --execute` já são o seam; criar quota nova
  viola "reuso antes de criação".
- **Subir o WARN para drenar mais cedo** — descartado: drenar mata o job in-flight
  (host-volume-reclaim-liveness SPECv2 B1); a alavanca não é mais drain.

## Requisitos funcionais

- **RF-1 — Teto agregado documentado.** Existe UM bloco que soma todos os
  consumidores do V: contra os 108/120GB e prova a folga-alvo no pior caso.
  Folga-alvo = **≥30GB livres** (WARN), recusa = **<10GB** (CRIT). Banda WARN→CRIT
  = 20GB para o reclaim agir antes da recusa.
- **RF-2 — Docker-prune endurecido e seguro-durante-job.** O caminho BUSY do
  `dockerPruneSafe` (cleanup.go:525) DEVE soltar imagens TAGGED-unused com >7d
  (`image prune -a -f --filter until=168h`, fiando a constante GAP-2), além do
  `builder prune until=24h` atual. NUNCA pode remover recurso de container/build
  ativo — o filtro por CREATED-date >7d garante isso por construção.
- **RF-3 — `threshold-pct` semanticamente correto no autoreclaim.** O
  autoreclaim RF-3 (guest-prune) DEVE forçar o prune de fato. Corrigir o sentinel:
  passar `--threshold-pct=1` (mínimo válido, `diskwatchdog.go:138`) em vez de `=0`
  (que vira 60).
- **RF-4 — Reconciliação dos caps preservando o invariante A2.** Os caps backstop
  permanecem o teto de CRESCIMENTO, não a fonte de alívio sob pressão. O orçamento
  prova que o SOMATÓRIO dos caps respeita o teto agregado SEM apertar nenhum cap a
  ponto de o trim morder o working-set (A2 da cachetrim-yarn-atomic: working-set
  sob o cap → trim no-op no job → sem race ENOENT).

## Requisitos não-funcionais

- **RNF-1 — Sem interromper job ativo.** RF-2 não pode tocar imagem em uso por
  container vivo nem recém-puxada por sibling job. O hook job-completed permanece
  intocado (sibling-safe). O caminho idle `system prune -af` permanece atrás de
  `ensureIdle` + lock docker-heavy.
- **RNF-2 — Fail-safe preservado (#16).** A recusa a exit 75 no piso crítico
  continua o backstop; nenhuma mudança a enfraquece. RF-2 age ANTES, tornando a
  recusa rara, não a remove.
- **RNF-3 — Números medidos, não adjetivos (#13).** Todo número do orçamento é do
  código (34 dos caps, 30/10 do WARN/CRIT) ou dos dados (#13: 18 reclaimable, 27
  docker, 36 OS, 12 VMRS). O lever R2 é validado por EFEITO (imagem sumiu), não
  por "o comando foi montado".
- **RNF-4 — Thresholds distintos (#15).** WARN=30 e CRIT=10 são determinísticos e
  DISTINTOS; a banda de 20GB é o espaço para o reclaim agir antes da recusa.

## Critérios de sucesso

1. O teto agregado de pior caso fecha com ≥30GB livres no V: (prova numérica no
   SPEC, RF-1).
2. `dockerPruneSafe` endurecido solta imagens tagged-unused >7d com host busy,
   provado por integration contra daemon docker real com imagem tagged >7d (o
   EFEITO: imagem sumiu) — não por mock afirmando o arg (#13).
3. Par #13: o mesmo teste prova que uma imagem EM USO por container vivo, ou
   CREATED <7d, **NÃO** é removida (o lever legítimo não mata recurso ativo).
4. `--threshold-pct=1` no autoreclaim força o prune (RF-3) onde `=0` o pulava.

## Fora de escopo

- **F3 (working-set ATIVO > capacidade do disco)** — hardware; nenhum lever
  compacta dado em uso. A recusa no piso permanece o fail-safe correto (#15/#16).
  Mitigação é disco maior ou menos concorrência de CI (repo acme). Este PRD prova
  com número ONDE F3 começa, não o resolve.
- **Recusa de admit por disco no guest** — o orçamento NÃO adiciona um novo gate
  de admit; o piso de recusa permanece o exit 75 host-crit já existente. (Uma
  recusa guest-side por disco foi considerada nas auditorias mas fica como
  follow-up, não escopo deste orçamento.)
- **Redesenho de trim/autoreclaim/isolamento** — só são referenciados e tecidos.

## Planos existentes que este orçamento TECE (não duplica)

- `docs/specs/cachetrim-yarn-atomic/SPECv2.md` — caps backstop (34GB) + trim
  atômico/WipeWhole. RF-4 só PROVA que o somatório respeita o teto; não muda
  mecanismo nem cap.
- `docs/specs/host-volume-reclaim-liveness/SPECv2.md` — autoreclaim vivo
  (anti-fantasma RF-1, guest-prune RF-3). RF-3 deste plano corrige 1 arg
  (`threshold-pct=0→1`) do invoke; RF-1 reusa o WARN/CRIT que o reclaim já honra.
- `docs/specs/multi-project-isolation/SPEC.md` — docker-heavy lock + serialização
  box-wide. RNF-1 herda o `LockActiveFn` (cleanup.go:192) que já gateia o prune
  agressivo; não duplica a serialização.

> **Nota honesta:** o `pr-serial-queue` citado no prompt **NÃO existe** como spec
> (`ls docs/specs/` não o lista; o cache efêmero por-PR como lever de disco
> por-PR não está implementado). O lever de footprint por-PR é o
> `multi-project-isolation` (docker-heavy lock + serialização). Este orçamento o
> justifica numericamente: se mesmo com Docker no teto o working-set concorrente
> estourar a capacidade usável (F3), a serialização é o lever, não mais disco.

## Disciplinas Kahneman

- **#13 (existência ≠ função):** todo número é medido; R2 validado por efeito
  (imagem sumiu), não por mock do arg. Um teste hermético do arg `-a --filter`
  NÃO prova reclaim — exige integration contra daemon real.
- **#15 (fail-fast só p/ determinístico):** WARN=30/CRIT=10 distintos; o piso de
  recusa é fail-safe correto, não se relaxa.
- **#16 (a cura não morre com o recurso):** o reclaim agressivo (R2) age ANTES do
  piso; a recusa permanece o backstop que não morre com o disco.
