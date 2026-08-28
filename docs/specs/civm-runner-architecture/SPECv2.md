# SPECv2 — Arquitetura unificada do runner box civm

> **Supersedido por [`SPECv4.md`](./SPECv4.md)** (3ª rodada / re-fundação): GO
> no delta mínimo Day-0. Refs a deep-dives/DATA-REPORTs aqui são provenância
> fantasma — ver o mapa real shipped/gap no SPECv4. Trilha de auditoria.

> Versão melhorada após auditoria do Passo 2.5 (red-team cross-cutting).
> Baseline preservado: `SPEC.md`.
> Motivo: o `SPEC.md` guarda-chuva fecha a ORDEM, mas a auditoria achou 5 riscos
> CROSS-CUTTING que só aparecem na junção dos componentes (não dentro de cada
> deep-dive isolado) — janela de coexistência runner-isolado×compartilhado,
> dupla-proteção `dockerlock` desligada cedo, dependência de PROVENIÊNCIA ÚNICA
> dos dados (#13/#1), warm-up frio × admit, e o gate binário de ITEM-3 mal-amarrado
> ao ITEM-2 real. Onde houver conflito, **esta versão prevalece**.

## Auditoria 2.5 — findings cross-cutting (por severidade)

### X1 (CRÍTICO → resolvido) — janela de coexistência runner-isolado × compartilhado durante ITEM-2

**Seção afetada:** SPEC §Fronteira de atomicidade, ITEM-2/ITEM-3/ITEM-5.

O rollout per-runner (RF-1) NÃO é atômico: durante ITEM-2, alguns runners já têm
`HOME`/`_work`/cache disjuntos e outros ainda compartilham o `$HOME` de `emdev`.
O SPEC dizia "estado parcial aceito" mas não definia o comportamento do cachetrim
e do wipe NESSA janela — um runner isolado escrevendo cache disjunto enquanto um
sibling compartilhado é trimado pelo disk-watchdog pode reintroduzir o race A2
exatamente nos runners ainda-compartilhados, e o wipe-por-job (se ligado cedo)
mata MID-JOB um sibling compartilhado (civm#117).

**Resolução X1 (DT-v2-1):** durante a janela de coexistência:

1. o **wipe-por-job (ITEM-3) permanece DESABILITADO globalmente** até que TODOS os
   8 runners estejam isolados e provados (não por-runner incremental) — gate é
   "todos isolados", não "este isolado". O hook mantém `cleanWorkRoot` atual nos
   runners compartilhados;
2. o **cachetrim backstop/atômico permanece ATIVO** (FUNDAÇÃO) em TODA a janela —
   ele é o que protege os runners ainda-compartilhados; só desce a backstop puro
   após ITEM-2 completo;
3. ITEM-2 grava `/var/lib/civm/runner-isolation.json` (`{ "runner": "...",
   "home": "/home/runnerN", "isolated_at": "<iso>" }`, `os.WriteFile` atômico por
   arquivo) e o gate de ITEM-3 lê o COUNT de entradas == nº de runners ativos.
- **Disciplina** #15 fail-safe + #13 · **Evidência** `runner-isolation.json` com N
  entradas == N runners + POC wipe não-cruzado · **Abort trigger** qualquer runner
  sem entrada → ITEM-3 fica desabilitado.

### X2 (CRÍTICO → resolvido) — `dockerlock` aposentado do eixo cache antes de o efêmero cobrir o eixo de PORTA/rede

**Seção afetada:** ITEM-5, DT-3, matriz RF-5.

ITEM-5 remove o `dockerlock` do CAMINHO de prune após evidência de "2 jobs
docker-heavy isolados sem colisão". Mas o `dockerlock` serializa docker-heavy
box-wide por DOIS motivos acoplados no código: (a) proteção de cache (que o
efêmero substitui) e (b) contenção de daemon Docker concorrente (build paralelo
estoura RAM/IO). O `multi-project-isolation` resolve colisão de
container/rede/PORTA, mas é ORTOGONAL e pode não estar shipado quando ITEM-5
rodar. Aposentar o lock do prune sem confirmar que o eixo de contenção de daemon
está coberto por `internal/admit` (RAM) **mais** isolamento de projeto (porta)
pode reintroduzir colisão de daemon.

**Resolução X2 (DT-v2-2):** ITEM-5 só remove o `dockerlock` do **eixo de
PROTEÇÃO-DE-CACHE do prune**, e a evidência de aposentadoria exige explicitamente
que a contenção de daemon esteja coberta por OUTRO mecanismo no momento do teste:

1. o teste de evidência roda os 2 jobs docker-heavy isolados **sob `internal/admit`
   ativo** (2 slots, RAM-bound) — provando que a serialização de RAM persiste sem
   o lock;
2. se `multi-project-isolation` (portas/`COMPOSE_PROJECT_NAME`) NÃO estiver shipado,
   o `dockerlock` permanece como kill-switch do eixo de PORTA mesmo após sair do
   eixo cache — não é all-or-nothing;
3. o pacote `internal/dockerlock` NUNCA é deletado (DT-3 mantido).
- **Disciplina** #14 retry calibrado + #13 · **Evidência** 2 docker-heavy isolados
  concorrentes SOB admit, sem colisão de cache NEM de daemon · **Abort trigger**
  colisão de daemon observada → lock permanece no eixo de porta.

### X3 (CRÍTICO → resolvido) — decisões de migração penduradas em PROVENIÊNCIA ÚNICA (#13/#1)

**Seção afetada:** todo o SPEC (números de disco/RAM), matriz RF-5/RF-6.

Os números que justificam a ordem e os tetos (18 GB Docker reclamável, 7 G RAM,
~13 GB cache/job, ~17.9 GB volume efêmero) vêm de UMA leitura (`#13`, 2026-06-15)
+ do código, NÃO de série temporal. O SPEC tratava-os como fato estável. Decidir
"Docker é o lever nº1, não o cache" e "apertar cap é proibido" sobre N=1 é
exatamente a ilusão de validade — o working-set real pode variar por PR.

**Resolução X3 (DT-v2-3):** WYSIATI declarado no IMPL + cada decisão de migração
amarrada a uma RE-MEDIÇÃO no Slice 0 antes de habilitar a fatia dependente:

1. Slice 0 RE-MEDE `docker system df`, `du` por cache dir e RAM no guest ANTES de
   ITEM-1; se o Docker reclamável < 10 GB OU o working-set de cache > 30 GB (perto
   do cap 34), a premissa "Docker é o lever, cache não morde" é reavaliada e a
   ordem pode mudar (volta ao PASSO 2);
2. o efeito real do prune é provado por `parseTotalReclaimed` (`cleanup.go:610`),
   não pela leitura #13;
3. OS+`_work`+go-mod=36 GB (número por SUBTRAÇÃO) NÃO gateia nenhuma decisão dura —
   é declarado como o número MENOS medido.
- **Disciplina** #1 WYSIATI + #3 número não adjetivo · **Evidência** Slice 0
  re-medido colado no IMPL · **Abort trigger** Docker reclamável < 10 GB ou cache
  working-set > 30 GB → ordem reavaliada no PASSO 2.

### X4 (CRÍTICO → resolvido) — warm-up frio simultâneo dos 8 × teto de RAM (RF-2 × RF-4)

**Seção afetada:** ITEM-4, fluxo alternativo, RF-4.

Após eviction OU no primeiro deploy do backend (ITEM-4), TODOS os jobs dão cache
miss frio AO MESMO TEMPO e fazem build frio (go ~5.7 GB + yarn 1.5 GB +
playwright 0.6 GB cada). 8 builds frios concorrentes estouram os 7 G — o mesmo
failure mode (`sshd` wedge) que o efêmero deveria aliviar. O SPEC orçava o custo
de disco mas não cruzava com o teto de RAM (RF-4).

**Resolução X4 (DT-v2-4):** o warm-up de ITEM-4 é CONTROLADO e o teto de RAM é o
gate:

1. o warm-up pré-aquece os blobs do working-set conhecido **serialmente** (espelha
   `setup-registry-cache.sh --warm`), não deixa os 8 jobs descobrirem o miss frio
   simultaneamente;
2. mesmo no miss frio residual, o `internal/admit` (2 slots, RAM-bound) é o gate —
   no máximo 2 builds frios concorrentes; o efêmero NÃO sobe o teto (DT-5);
3. o custo de disco do miss frio é orçado e declarado (número), aceito vs corrupção.
- **Disciplina** #15 + #5 worst-case · **Evidência** warm-up serial + admit ativo;
  no máx 2 builds frios concorrentes · **Abort trigger** warm-up frio dos 8 excede
  RAM → admit recusa heavy (NÃO sobe o teto).

### X5 (ALTO → resolvido) — gate binário de ITEM-3 amarrado a "ITEM-2 existe", não a "ITEM-2 funciona"

**Seção afetada:** ITEM-3 §Mapa Kahneman, X1.

O SPEC dizia "ITEM-3 só habilita com ITEM-2 verde" mas "verde" podia ser lido como
"units re-registrados" (existência), repetindo a ilusão de validade — o wipe
por-job é o efeito DESTRUTIVO de maior risco do box (já apagou sibling MID-JOB).

**Resolução X5 (DT-v2-5):** o gate de ITEM-3 é por EFEITO MEDIDO, não por
config-presente:

1. o gate lê `runner-isolation.json` (X1) E exige um teste de efeito gravado:
   `civmctl hook job-completed` num runner de teste apaga só o `_work` dele e um
   `du`/checksum prova que o `HOME` de um sibling com job ativo não mudou 1 byte;
2. enquanto o teste de efeito não estiver verde no guest `gha-ubuntu-2404`, ITEM-3
   é no-op (hook mantém `cleanWorkRoot` atual);
3. red-team confirma: wipe-leve do `$HOME` compartilhado sem isolar por-runner NÃO
   é clean-slate — deixa daemon Docker/overlayfs (a fonte exata da corrupção de
   extract de containerd), `/tmp`, systemd e os 8 runners co-residentes.
- **Disciplina** #13 + #16 · **Evidência** teste de efeito (sibling não tocado)
  verde no guest · **Abort trigger** teste de efeito ausente/vermelho → ITEM-3
  permanece DESABILITADO.

## Open questions

- **OQ-1**: o nº de runners ativos é estável (8) ou varia? O gate de X1
  (`COUNT == N runners`) precisa de uma fonte canônica de N (lista de
  `actions.runner.*` units). Resolver no Slice 0.
- **OQ-2**: o backend de cache local (ITEM-4) usa MinIO ou `registry:2` estendido?
  Decisão delegada ao deep-dive `ephemeral-clean-slate-ci`; não bloqueia a ordem.
- **OQ-3**: `multi-project-isolation` (portas) estará shipado antes de ITEM-5? Se
  não, X2 mantém o lock no eixo de porta — não bloqueia, mas precisa de confirmação
  no momento de ITEM-5.

## Escopo final do IMPL (pós-auditoria)

- **ITEM-1** — Docker prune endurecido (RF-5/RF-6): independente, baixo risco,
  ENTRA primeiro. Gate de merge = par positivo #13 (em-uso sobrevive). Slice 0
  re-mede antes (X3).
- **ITEM-2** — isolamento per-runner (RF-1): grava `runner-isolation.json` (X1);
  POC supervisionado; gate "TODOS isolados", não incremental.
- **ITEM-3** — wipe por-job (RF-3): gate por EFEITO MEDIDO (X5), não config; no-op
  até todos isolados + teste de efeito verde.
- **ITEM-4** — managed cache local (RF-2): warm-up serial controlado (X4); admit é
  o gate de RAM; backend down → cache MISS (fail-open).
- **ITEM-5** — aposentar o lock do eixo CACHE (RF-5): evidência SOB admit ativo
  (X2); lock permanece no eixo de porta se `multi-project-isolation` não shipado;
  pacote nunca deletado.
- **ITEM-6** — serial OOB (RF-7): trilho PARALELO; aceite por 2 números medidos sob
  wedge real em ≥3 incidentes; counterfactual de rollback (PAM >60 s → power-cycle).
- **FUNDAÇÃO intocada**: cachetrim ATIVO em toda a janela de coexistência (X1);
  admit RAM-bound NÃO sobe (RF-4); `cpus:1` (RF-8); autoreclaim host-side.

## Ordem de migração incremental (DURA, pós-auditoria)

```
Slice 0  (re-medir #13: docker df + du + RAM + login serial saudável)   [BASELINE, não-aceite, X3]
   │
   ▼
ITEM-1   Docker prune endurecido + threshold-pct=1                       [independente, gate=par positivo #13]
   │
   ▼
ITEM-2   isolamento per-runner ($HOME/_work/cache disjuntos)             [pré-req DURO; grava runner-isolation.json; gate=TODOS isolados]
   │
   ├──────────────► ITEM-3  wipe por-job                                 [gate=EFEITO MEDIDO sibling-não-tocado, X5; no-op até verde]
   │
   ▼
ITEM-4   managed cache local + warm-up serial                           [warm-up controlado, X4; admit=gate RAM; backend down→MISS]
   │
   ▼
ITEM-5   aposentar dockerlock do eixo CACHE                             [evidência SOB admit, X2; lock fica no eixo porta se MPI não shipado]

ITEM-6   guest-access serial OOB                                        [PARALELO a tudo; aceite=2 números/≥3 incidentes; rollback=power-cycle]
```

Regra dura: efeito PROVADO antes de avançar (#13). Nunca big-bang. O `dockerlock`
é kill-switch entre cada fatia e a evidência.

## Go / No-go

**GO** — com as 5 resoluções cross-cutting (X1–X5) incorporadas ao escopo do IMPL.

Justificativa: cada CRÍTICO foi rebaixado por uma resolução com evidência-por-efeito
e abort trigger numérico; nenhum mecanismo de segurança "existe mas não funciona";
nenhuma violação Day-0 sem exceção documentada (o `dockerlock` kill-switch é
exceção Day-0 registrada com motivo + prazo + rollback; a constante de prune órfã é
fiada Day-0, não compatibilidade). A migração é incremental, ordenada por
dependência dura, com o lock como kill-switch e cada fatia reversível.

Pendências NÃO-bloqueantes para o IMPL fechar no Slice 0: OQ-1 (fonte canônica de
N runners), OQ-3 (status de `multi-project-isolation` no momento de ITEM-5).

Próximo passo: PASSO 3 (IMPL) seguindo a ordem ITEM-1 → ITEM-6, abrindo `IMPL.md` e
citando os requirement IDs (RF-N) + os deep-dives por fatia.
