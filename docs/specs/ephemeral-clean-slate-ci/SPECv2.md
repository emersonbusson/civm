# SPECv2 — CI efêmero clean-slate

> Versão melhorada após auditoria do Passo 2.5.
> Baseline preservado: `SPEC.md`.
> Motivo: 4 CRÍTICOS de robustez operacional do red-team — (C1) backend de cache
> indisponível/lento sem fail-safe explícito; (C2) wipe-leve passando por
> "clean-slate" sem o pré-requisito de isolamento; (C3) `volume prune -f` tocando
> volume de container ativo sem o par positivo; (C4) cache miss frio explodindo o
> tempo de CI sem orçamento. Onde houver conflito, **esta versão prevalece**.

## Resultado da auditoria (Passo 2.5)

| # | Severidade | Finding | Seção do SPEC | Resolução nesta versão |
| --- | --- | --- | --- | --- |
| C1 | CRÍTICO | RF-2/ITEM-6 dizia "backend LOCAL" mas não definia o comportamento OBSERVÁVEL quando o backend está fora OU lento — risco de o CI travar esperando o cache em vez de cair p/ build frio | ITEM-6 | DT-v2-1: timeout DURO no restore/save; fora OU lento-além-do-timeout → cache MISS imediato → build frio. Fail-safe explícito (#16) |
| C2 | CRÍTICO | RF-3/ITEM-5 (wipe-por-job) podia ser lido como aplicável ao `$HOME` compartilhado de hoje — o que repete civm#117 (mata sibling) | ITEM-5 | DT-v2-2: GATE de pré-requisito — o wipe total em job-completed SÓ é habilitado quando RF-1 (HOME disjunto) está PROVADO por efeito no guest. Antes disso, o hook mantém o comportamento atual (preserva caches/workspace) |
| C3 | CRÍTICO | RF-4/ITEM-3 tinha o par #13 como "sub-test" sem afirmar que é BLOQUEANTE — um `volume prune` mal-calibrado remove dado de volume legítimo desanexado | ITEM-3 | DT-v2-3: o par POSITIVO (volume anexado a container vivo sobrevive) é GATE de merge; sem ele, ITEM-2 não entra |
| C4 | CRÍTICO | RF-2 assumia o managed cache como ganho líquido, mas não orçava o pior caso (cache miss FRIO de TODOS os jobs no warm-up do backend) — re-download/recompile explodindo o tempo | ITEM-6 | DT-v2-4: warm-up controlado (espelha `setup-registry-cache.sh --warm`); cache miss frio é DETERMINÍSTICO e ACEITO como trade vs corrupção, com número orçado |

## Escopo fechado do IMPL (o que de fato entra na primeira fatia)

A auditoria expôs que o IMPL não pode ser big-bang. O caminho de migração a partir
do estado atual é **incremental e ordenado por dependência dura**, com o lock
mantido como kill-switch até o fim:

**Fatia 1 (entra primeiro — independente, baixo risco):** ITEM-2 + ITEM-3 (RF-4).
A 3ª perna `volume prune -f` + o par #13 BLOQUEANTE. NÃO depende de RF-1; compõe
com `vm-disk-budget`. É a melhora de espaço imediata e segura.

**Fatia 2 (RF-1, o pré-requisito DURO):** ITEM-4. HOME/cache/`_work` por-runner.
POC supervisionado de 1 PR no guest real, com o TESTE DE EFEITO (wipe de N não toca
M MID-JOB) como GATE. **Nada de RF-3/RF-5 antes deste verde.**

**Fatia 3 (RF-3):** ITEM-5. Reabilitar wipe-por-job em job-completed, GATEADO por
RF-1 provado (DT-v2-2).

**Fatia 4 (RF-2):** ITEM-6. Backend de cache local + action de fork nos peers, com
timeout de fail-safe (DT-v2-1) + warm-up (DT-v2-4).

**Fatia 5 (RF-5):** ITEM-7. Aposentar o lock no eixo cache; kill-switch por janela;
remover só com evidência de efeito (2 jobs isolados sem colisão).

**Fora do IMPL desta entrega:** runner JIT pleno; isolamento de daemon Docker;
subir teto de concorrência; F3.

## Decisões técnicas adicionais (pós-auditoria)

| # | Decisão | Justificativa |
| --- | --- | --- |
| DT-v2-1 | Restore/save de cache com **timeout duro**; estouro OU backend down → cache MISS imediato (build frio), nunca espera indefinida | C1 — o piso é build frio, não CI travado (#16). O CI nunca pode ficar mais lento por esperar um cache que não vem |
| DT-v2-2 | Wipe-por-job em job-completed é **gateado** por RF-1 provado por efeito; sem isso, o hook mantém o comportamento atual (preserva caches/workspace) | C2 — desabilitar o wipe total no `$HOME` compartilhado é o que impede repetir civm#117 |
| DT-v2-3 | O par #13 do volume prune (volume vivo sobrevive) é **GATE de merge**, não teste opcional | C3 — sem o positivo, o teste codifica "unused=descartável" (testing.md §13) |
| DT-v2-4 | Cache miss frio é orçado e ACEITO: warm-up controlado + número (go ~5.7 GB mod+compile, yarn 1.5 GB node_modules); determinístico vs corrupção | C4 — o trade é "um pouco mais lento mas DETERMINÍSTICO" (#15), com o custo medido, não escondido |
| DT-v2-5 | O lock permanece como **kill-switch** entre Fatia 5 e a evidência; removido do eixo cache só após 2 jobs isolados sem colisão no guest | mitiga o risco de aposentar cedo demais (regra dura: efeito antes de remover) |

## Findings resolvidos — detalhe

### C1 — backend de cache indisponível/lento (ITEM-6, DT-v2-1)

**Antes (SPEC):** "backend LOCAL escutando em loopback" — sem dizer o que
acontece quando ele está fora OU responde devagar.

**Agora:** o restore e o save de cache têm timeout DURO (orçado no SPEC; a ordem
de grandeza é de loopback, dezenas de segundos no pior caso). Estouro do timeout
OU backend inalcançável → cache MISS imediato → o build recompila do zero.
**Nunca** o job espera indefinidamente por um cache que não vem (seria o oposto do
objetivo: CI mais lento). Espelha o comportamento fail-open do hook/cleanup já no
código (ex.: `commandActionWarn` torna falha de ferramenta um warning, nunca um
wedge). Evento `ephemeral_cache_backend_down` (Warn).

- **Disciplina #16:** o piso é build frio (determinístico), nunca build sobre
  estado corrompido nem CI travado. **Abort trigger:** backend down causar espera
  indefinida ou build sobre cache parcial.

### C2 — wipe-leve insuficiente / wipe-por-job no HOME compartilhado (ITEM-5, DT-v2-2)

**Antes (SPEC):** RF-3 reabilita o wipe-por-job; o pré-requisito RF-1 estava
listado (DT-6) mas o GATE não era explícito no ITEM-5.

**Agora:** o ITEM-5 carrega um GATE binário — o wipe TOTAL em job-completed só é
habilitado quando RF-1 (HOME disjunto por runner) está PROVADO por efeito no guest
(wipe de N não toca M MID-JOB). Enquanto RF-1 não estiver verde, o hook mantém
EXATAMENTE o comportamento atual (`cleanWorkRoot` preserva o workspace ativo, o
`_temp`, e o `cachetrim` é por-idade). Isto fecha o risco de repetir civm#117 (um
wipe total num `$HOME` compartilhado mata o checkout de um sibling MID-JOB).

Adicionalmente, o red-team confirma o ponto do SPEC: **wipe-leve do `$HOME`
compartilhado (sem isolar por-runner) NÃO é clean-slate suficiente** — deixa o
daemon Docker/overlayfs (a fonte exata da corrupção de extract de containerd),
`/tmp`, systemd e os 8 runners co-residentes compartilhados. Para ser clean-slate
real é preciso o isolamento por-runner (RF-1) — NÃO um wipe mais agressivo sobre o
mesmo box compartilhado.

- **Disciplina #16 + #13:** a cura (wipe) não pode matar um job vivo; o sucesso é o
  efeito (M intacto), não "o wipe rodou". **Abort trigger:** RF-1 não provado →
  wipe-por-job permanece DESABILITADO.

### C3 — prune tocando volume de container ativo (ITEM-3, DT-v2-3)

**Antes (SPEC):** o par #13 era "sub-test".

**Agora:** o par POSITIVO (um volume ANEXADO a um container vivo sobrevive ao
`docker volume prune -f`) é GATE de merge de ITEM-2. A garantia "refcount>0 →
preservado" é do daemon, mas o teste tem de PROVAR o efeito, não presumir. Sem o
positivo, codifica-se a mesma premissa do código ("unused = descartável") — errada
para um volume de backup órfão temporariamente down. O negativo (desanexado é
colhido) e o erro-não-silencioso completam o trio.

- **Disciplina #13:** existência ≠ função; pareie toda recusa com seu positivo
  (testing.md §13). **Abort trigger:** ausência do par positivo → ITEM-2 não
  mergeia.

### C4 — re-pull/recompile frio explodindo o tempo (ITEM-6, DT-v2-4)

**Antes (SPEC):** o managed cache era tratado como ganho líquido.

**Agora:** o pior caso é orçado explicitamente. No warm-up do backend (ou após uma
eviction), TODOS os jobs podem dar cache MISS frio simultaneamente — go ~5.7 GB
(mod + compile) + yarn 1.5 GB (node_modules) + playwright 0.6 GB por job. Mitigação:
warm-up controlado que espelha `setup-registry-cache.sh --warm` (pré-aquece os
blobs do working-set conhecido). O cache miss frio residual é ACEITO como trade —
build determinístico e limpo > working-set parcial corruptível (#15). O custo é o
NÚMERO acima, declarado, não escondido.

- **Disciplina #15 + #3:** cache miss = fail-fast determinístico; o custo é número
  medido, não adjetivo. **Abort trigger:** se o warm-up frio simultâneo dos 8
  runners exceder o teto de RAM (`admit`/`memwatchdog`), o cap recusa heavy — o
  efêmero NÃO sobe o teto (DT-5 do SPEC permanece).

## Caminho de migração a partir do estado atual

O estado atual é a saga de 4 camadas + per-PR lock, todos no `$HOME` compartilhado.
A migração converge a saga em `efêmero + managed-cache` SEM big-bang:

1. **Hoje → Fatia 1:** as 4 camadas e o lock permanecem; adiciona-se só a 3ª perna
   do prune (RF-4). Ganho de espaço imediato, zero risco de cache (refcount-safe).
2. **Fatia 2 (RF-1):** introduz HOME disjunto por-runner. A partir daqui o
   `cachetrim` divide o budget por-HOME (já honra `cacheHomeRoots`); a corrupção de
   cache compartilhado deixa de ser possível por construção — mesmo ANTES de
   reabilitar o wipe.
3. **Fatia 3 (RF-3):** o wipe-por-job substitui a preservação gated-por-disco. O
   `cachetrim` cap vira backstop (não mais o mecanismo primário) — 1ª camada da
   saga DESCE de papel.
4. **Fatia 4 (RF-2):** o managed cache content-addressed substitui o FS mutável
   compartilhado. A 2ª camada (trim atômico) e o medo de trim concorrente DESCEM
   de papel — não há mais cache mutável compartilhado para trimar.
5. **Fatia 5 (RF-5):** o per-PR lock sai do eixo cache (mantido só no eixo
   Optimize-VHD + kill-switch). A 5ª camada (lock como proteção-de-cache)
   APOSENTADA.

Ao fim: a saga (backstop cap, trim atômico, hooks gated, lock) colapsa em
`efêmero (HOME disjunto + wipe-por-job) + managed-cache content-addressed`. O
`cachetrim`, o `autoreclaim`/`host-volume-reclaim-liveness` e o `vm-disk-budget`
NÃO são deletados — passam a gerir SÓ o `registry:2` pull-through e os blobs do
backend de cache, ambos content-addressed que não corrompem (referência, não
reescrita).

## Limites honestos (preservados da auditoria)

1. **NÃO é runner efêmero pleno.** A entrega é "DOCKER + `_work`/cache efêmeros por
   job num daemon/SO persistente com HOME disjunto", não runner JIT. A deriva de SO
   (apt/kernel/tool-versions) persiste; "PR novo" aqui significa FS de PROJETO
   limpo + cache verificado, não VM virgem.
2. **O efêmero NÃO sobe o teto de concorrência.** RAM-bound = 2 (`admit`). Remover
   o cap reintroduz OOM. Ortogonal ao efêmero.
3. **`volume prune -f` é a única perna com risco novo** — image+builder já rodam em
   produção (issue #70). O par #13 (DT-v2-3) é o gate.
4. **Números de proveniência única:** os ~18 GB reclaimable e o teto de 7 GB vêm de
   UMA leitura (#13, 2026-06-15) + do código; o efeito real só se prova rodando o
   prune (`parseTotalReclaimed`) e o POC no guest. Este checkout é a VM WSL2 de
   15 GB/1 TB, NÃO o box-alvo — a feasibilidade final é confirmada NO guest
   `gha-ubuntu-2404` (POC de 1 PR antes de aposentar o lock).
5. **A migração depende de DOIS pré-requisitos aceitos:** (i) re-download/re-pull
   por-job é real (mitigado por registry/managed cache LAN, não-zero); (ii) o HOME
   por-runner multiplica o footprint de cache em disco antes da mitigação — medir o
   `V:` após o split (DT-v2-4 / o `vm-disk-budget` é o backstop).

## Está pronto para implementação?

Sim, com o escopo do IMPL acima e os 5 gates (DT-v2-1..5). A Fatia 1 (RF-4) pode
começar imediatamente (independente, par #13 bloqueante). As Fatias 2-5 seguem a
dependência dura RF-1 → RF-3/RF-5, com o lock como kill-switch até a evidência de
efeito no guest real.

**go** — com os gates DT-v2-1..5 e a confirmação obrigatória no guest
`gha-ubuntu-2404` antes de aposentar o lock (RF-5).
