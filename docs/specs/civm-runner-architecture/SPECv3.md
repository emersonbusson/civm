# SPECv3 — Arquitetura unificada do runner box civm

> **Convergido por [`SPECv4.md`](./SPECv4.md)** (3ª rodada / re-fundação): este
> NO-GO estava certo. O SPECv4 re-fundou no código e isolou o subconjunto
> shippável Day-0 (GO no delta mínimo D1 + per-runner slot + D3). Trilha de
> auditoria — onde conflitar, o SPECv4 prevalece.

> Versão melhorada após a 2ª rodada de auditoria do Passo 2.5 (red-team).
> Baseline preservado: `SPEC.md` e camada anterior `SPECv2.md`.
> Motivo: a 1ª rodada (SPECv2, X1–X5) fechou **GO** auditando a JUNÇÃO LÓGICA dos
> componentes, mas tratou os deep-dives e o mapeamento `dockerlock`/`admit`/MPI
> como verdade estável — exatamente a ilusão de validade (#13) aplicada à própria
> SPEC. A 2ª rodada atacou cada resolução X1–X5 contra o CÓDIGO REAL (arquivo:linha),
> não contra a prosa da SPEC. Achou **3 buracos estruturais que invalidam o GO** e
> **12 no-gos novos** verificados em arquivo. Onde houver conflito, **esta versão
> prevalece**.

## Por que esta rodada existe

A disciplina manda auditar até CONVERGIR. A 1ª rodada não convergiu — ela fechou
GO sobre premissas de codebase que **não se sustentam na verificação**. A regra
do red-team é atacar o código, não a prosa que descreve o código (#13: existência
≠ função). A 2ª rodada rodou `ls`, `grep` e leu o código nos pontos exatos que
X1–X5 declararam "resolvidos" e encontrou que a fundação documental e três
amarras de gate apontam para o vazio.

**Veredito antecipado: NO-GO.** Detalhe abaixo, fechamento em §Go/No-go.

---

## Achados NOVOS da 2ª rodada (Y-N), por lente

> Severidades: CRITICAL bloqueia merge; HIGH deve ser resolvido antes do ITEM
> dependente; MEDIUM é resíduo-aceito documentado. Todos verificados contra o
> código real, não contra a SPEC.

### Lente A — Re-auditoria das resoluções X1–X5 contra o código real

#### YA-1 (CRITICAL) — X1: o cap de cache por-HOME não vê as caches isoladas; o sweep raiz deriva HOME por PATH, não por env

**Seção afetada:** SPECv2 X1/DT-v2-1; SPEC ITEM-2; FUNDAÇÃO cachetrim.

- **Por que a 1ª passou:** X1 declarou "o cachetrim backstop/atômico permanece
  ATIVO em TODA a janela — ele protege os ainda-compartilhados" e "caps INALTERADOS
  (invariante A2)". Tratou o cachetrim como caixa-preta. Não cruzou o mecanismo de
  derivação de HOME do sweep raiz com o HOME efetivo que ITEM-2 dá ao runner isolado.
- **Verificado:** `internal/cleanup/cleanup.go:313` `cacheHomeRoots` deriva o home
  por PATH: `filepath.Dir(filepath.Dir(r))` sobre o glob
  `/home/*/actions-runner-*/_work` (`cleanup.go:301`). Logo o sweep raiz só enxerga
  `/home/emdev`. Mas ITEM-2/RF-1 dá ao runner um `HOME=/home/runnerN` por env — e as
  caches do runner isolado vivem sob `/home/runnerN/.cache`, INVISÍVEIS ao sweep raiz.
  A rota do hook (`hook.go:536` `cachetrim.Caps([]string{os.Getenv("HOME")})`) usa o
  HOME efetivo mas a UM home por vez no budget CHEIO, nunca dividido — nem o hook
  impõe o agregado. Resultado: na coexistência, as caches dos runners isolados
  crescem SEM TETO, estourando o backstop de 34 GB — o death-spiral de disco que a
  arquitetura promete matar.
- **Resolução:** ITEM-2 DEVE fazer `cacheHomeRoots` enumerar por HOME EFETIVO (ler o
  env/`.env`/unit de cada runner ativo), não por path de diretório. O gate de
  Slice 0/ITEM-2 prova por EFEITO: `du` da soma de TODAS as caches < 34 GB com ≥1
  runner isolado. A premissa "A2 inalterado" só vale com home-único.
  - **Disciplina** #13 + #15 fail-safe · **Pergunta** "o curador raiz enxerga a
    cache de `/home/runnerN`?" · **Evidência** `du` agregado incluindo o home isolado
    aparece na saída do root-sweep · **Abort trigger** cache de runner isolado fora
    da soma do root-sweep → ITEM-2 não avança.

#### YA-2 (CRITICAL) — X2: o eixo daemon já tem substituto Day-0 (admit docker sub-slot); a resolução defende um lock cuja função de daemon já foi reescrita

**Seção afetada:** SPECv2 X2/DT-v2-2; SPEC ITEM-5; OQ-3.

- **Por que a 1ª passou:** X2 modelou o `dockerlock` como o ÚNICO serializador do
  eixo daemon e preocupou-se em "não remover cedo demais". Não leu `civm.go:165-167`,
  cujo próprio comentário diz que o admit docker sub-slot (`--exclusive=docker`,
  count=1, `/run/civm/admit-docker.lock`) serializa docker-heavy SEM o `dockerlock`
  legado. O substituto Day-0 do eixo daemon JÁ EXISTE no código.
- **Verificado:** há TRÊS mecanismos de serialização com gates de opt-in DIFERENTES
  e ortogonais: (1) `dockerlock` `/run/civm/docker-heavy.lock`, só quando o peer chama
  `civmctl lock --exec` (`cmd/civmctl/lock.go`); (2) admit heavy slots
  `/run/civm/admit-heavy-{1,2}.lock`, só via `civmctl admit --weight heavy`;
  (3) admit docker sub-slot `/run/civm/admit-docker.lock` via `--exclusive docker`
  (`admit.go:293` `grabDockerSubSlot`). admit e dockerlock são locks SEPARADOS. X2
  confundiu serialização de RAM (heavy slots) com serialização de DAEMON (docker
  sub-slot OU dockerlock) — eixos distintos com gates de opt-in distintos. Manter
  dois serializadores de daemon (dockerlock legado + admit docker sub-slot) é
  DUPLICAÇÃO Day-0 proibida, não kill-switch a preservar.
- **Resolução:** ITEM-5 NÃO é sobre "remover do eixo cache"; é consolidar a
  serialização de daemon no admit docker sub-slot (Day-0) e APOSENTAR o `dockerlock`
  legado por inteiro do guest. A evidência tem de provar que TODO step docker-heavy
  roteia pelo `--exclusive=docker`, não que "2 jobs rodaram sob admit". `OQ-3`
  RESOLVIDA: MPI shipado (`internal/portblock`, `install.go:173-175` já escreve
  `CIVM_RUNNER_SLOT`/`CIVM_PORT_BASE`/`COMPOSE_PROJECT_NAME`).
  - **Disciplina** #13 + Day-0 · **Pergunta** "qual mecanismo o job docker-heavy DEVE
    invocar, e é o único vivo?" · **Evidência** todo step docker-heavy usa
    `--exclusive=docker`; `civmctl lock` legado removido (incluindo a regra R4 do
    ci-guard que o recomenda) · **Abort trigger** dois serializadores de daemon vivos
    → viola Day-0, ITEM-5 não fecha.

#### YA-3 (CRITICAL) — Estrutural cross-X: o deep-dive que carrega o diff fino de RF-1/RF-2/RF-3/RF-5 (`ephemeral-clean-slate-ci/`) NÃO EXISTE

**Seção afetada:** SPECv2 inteiro (delegação de diff); SPEC §Escopo; matriz PRD→SPEC.

- **Por que a 1ª passou:** confiou que "o diff fino vive no deep-dive referenciado"
  sem rodar um `validate-templates` nem um `ls` no caminho citado. SSDV3 exige
  rastreabilidade real (Princípio 4).
- **Verificado:** `docs/specs/ephemeral-clean-slate-ci/` NÃO EXISTE (`ls`
  confirma). É referenciado 14× na `SPEC.md`, 18× no `PRD.md`, 1× na `SPECv2.md`
  como o deep-dive que detalha o diff de cada RF. O deep-dive real é
  `docs/specs/civm-self-cleaning-runner/`, cuja numeração RF-N é COMPLETAMENTE
  diferente (RF-1=medição de scratch_high_water, RF-2=gate de duas fases do
  autoreclaim pós-Off, RF-10=registry pull-through). A "resolução por delegação"
  de X1/X3/X5 aponta para conteúdo inexistente. ITEM-2 (RF-1), pré-requisito DURO
  de ITEM-3 e ITEM-5, não tem código (nenhum HOME é setado por-runner em lugar
  nenhum — confirmado) nem SPEC de componente.
- **Resolução:** ou (a) criar `docs/specs/ephemeral-clean-slate-ci/` com RF-1..RF-5
  e os critérios por-efeito, ou (b) re-mapear as 33 referências para a SPEC de
  componente correta e PROVAR que ela cobre isolamento/cache/wipe. Antes disso, o
  IMPL não tem onde ler o diff de 4 dos 6 ITEMs.
  - **Disciplina** #13 + rastreabilidade (Princípio 4) · **Pergunta** "cada link de
    deep-dive resolve no filesystem?" · **Evidência** `validate-templates` verde sobre
    os 3 paths · **Abort trigger** qualquer link ENOENT → matriz de rastreabilidade
    inválida, GO prematuro.

#### YA-4 (HIGH, residuo-aceito) — X4: o warm-up serial espelha `--warm` que aquece IMAGENS, não as build-caches do failure mode

**Seção afetada:** SPECv2 X4/DT-v2-4; SPEC ITEM-4.

- **Por que a 1ª passou:** aceitou "o warm-up pré-aquece os blobs do working-set
  conhecido SERIALMENTE (espelha `setup-registry-cache.sh --warm`)" como mitigação
  do estouro de RAM. Não verificou O QUE o `--warm` existente aquece nem que o
  aquecedor de build-cache (`setup-ci-cache.sh`) é vaporware.
- **Verificado:** `setup-registry-cache.sh --warm` (`deploy/bin/setup-registry-cache.sh`
  `warm_images`) aquece IMAGENS Docker do registry pull-through — categoria diferente
  das build-caches que X4 teme (go-build ~5.7 GB + yarn 1.5 GB + playwright 0.6 GB
  por job). Aquecer imagens NÃO evita o miss frio simultâneo de go-build/yarn que é o
  estouro de RAM. E `deploy/bin/setup-ci-cache.sh` NÃO EXISTE. A 2ª perna de X4
  (admit MaxHeavy=2 como gate de RAM) SE sustenta — é real (`admit.go`).
- **Resolução:** REBAIXAR a 1ª perna — o warm-up serial só vale se `setup-ci-cache.sh`
  aquecer de fato go-build/yarn/playwright (não imagens), e o aceite por EFEITO deve
  medir miss-rate de build-cache pós-warm, não "imagens puxadas". A 2ª perna (admit)
  carrega a segurança. X4 sobrevive REBAIXADO; a redação otimista da 1ª perna induz
  confiança falsa.
  - **Disciplina** #13 · **Evidência** miss-rate de build-cache pós-warm medido ·
    **Abort trigger** `setup-ci-cache.sh` ausente → ITEM-4 só pode confiar no admit.

#### YA-5 (CRITICAL) — X5: o gate por-efeito de ITEM-3 lê `runner-isolation.json`, que NENHUM código escreve; e `install.go` restarta a frota inteira de uma vez

**Seção afetada:** SPECv2 X5/DT-v2-5, X1/DT-v2-1; SPEC ITEM-2/ITEM-3; OQ-1.

- **Por que a 1ª passou:** X5 acertou o PRINCÍPIO (efeito-medido > config-presente)
  e até red-teamou que "wipe-leve do `$HOME` compartilhado deixa daemon
  Docker/overlayfs". Mas declarou o gate "lê `runner-isolation.json` E exige teste
  de efeito gravado" como se o produtor do arquivo existisse. Não verificou o
  produtor nem o mecanismo de re-registro.
- **Verificado:** `runner-isolation.json` NÃO É ESCRITO POR NENHUM CÓDIGO (grep zero
  matches fora dos docs). Os arquivos de estado reais em `/var/lib/civm` são
  `host-metrics.json`, `maintenance.json`, `maintenance.lock`, `marker.json`,
  `port-blocks.json`, `runner-watchdog-reruns.json` — nenhum é esse. Pior:
  `install.go:183` faz `systemctl restart actions.runner.*` — UM glob que reinicia os
  8 runners SIMULTANEAMENTE. Não há caminho de re-registro per-runner staged; a
  janela "alguns isolados, outros não" que X1/X5 assumem como estado-parcial gracioso
  é contradita pelo único mecanismo de wiring, que é all-at-once. E o data-root do
  Docker é `/var/lib/docker` (compartilhado, fora de qualquer `$HOME` per-runner) —
  isolar `$HOME` NÃO isola o daemon.
- **Resolução:** X5 precisa de (a) especificar o produtor real de
  `runner-isolation.json` com seu próprio critério de efeito; (b) trocar o
  restart-glob por staged per-runner; (c) estender o teste de efeito para provar
  isolamento do data-root containerd, não só do `$HOME`. Senão "verde" é ilusão de
  validade no exato ponto destrutivo de maior risco.
  - **Disciplina** #13 + #16 rollback completo · **Pergunta** "quem escreve o sinal
    que o gate lê, e ele reflete o efeito vivo?" · **Evidência** produtor com critério
    de efeito + restart staged + teste containerd · **Abort trigger** gate lê arquivo
    sem produtor → ITEM-3 permanece DESABILITADO.

### Lente B — Box meio-migrado (estados intermediários ITEM-1..6): propriedade/uid, descoberta/glob, rollback no meio

#### YB-1 (CRITICAL) — ITEM-2 nunca decide se "HOME próprio" = uid novo ou só env var; se uid novo, o safedelete passa a RECUSAR cross-uid e re-wedga o box-wide na janela híbrida

**Seção afetada:** SPECv2 X1 (só eixo estado/cache); SPEC ITEM-2.

- **Por que a 1ª passou:** X1 fechou a coexistência apenas no eixo ESTADO/CACHE.
  Nunca tocou o eixo de PROPRIEDADE (uid). A SPEC trata "isolamento" como disjunção
  de PATHs; o PRD descreve ITEM-2 como "cada `actions.runner.*` roda com HOME próprio
  (ex.: `/home/runnerN`)" sem resolver se `/home/runnerN` é um USUÁRIO Unix novo (uid
  distinto) ou só override de env sob o mesmo uid `emdev`.
- **Verificado:** `safedelete.resolveAndAffirmOwner` (`safedelete.go`) exige que toda
  entrada de `_work` seja owned pelo runner (`OwnerUIDFn`, default `os.Getuid()`) OU
  por root — "a third user's files are still refused" (`safedelete.go:123`). O hook
  roda AS o runner user com `OwnerUIDFn=os.Getuid()`. Se ITEM-2 der uids distintos:
  (a) o root-sweep (`civmctl-cleanup.service`, User=root) varre `_work` de TODOS os
  homes; um leftover de outro uid é recusado como third user e vira erro fatal,
  travando a hygiene; (b) PIOR: se a migração do unit for parcial (uid novo mas
  `.env`/unit ainda com HOME antigo), o hook roda com `os.Getuid()` de um uid e
  `$HOME` de outro → o `validateFixed` protege o HOME errado e a chown reivindica
  para o uid errado, podendo chown-ar checkout de sibling MID-JOB.
- **Resolução:** ITEM-2 DEVE declarar o modelo. RECOMENDADO: env-var-only sob o MESMO
  uid `emdev` (disjunção de PATH sem disjunção de uid), preservando o invariante
  `runner-uid == os.Getuid() == dono dos _work`. Se for uid-novo, safedelete + os 7
  guards precisam de allowlist de uids-de-runner ANTES de ITEM-2, com gate de efeito
  provando que cross-uid leftover não trava o sweep.
  - **Disciplina** #13 + #5 worst-case · **Pergunta** "qual o modelo de uid, e o
    safedelete o tolera na janela híbrida?" · **Evidência** modelo declarado + gate de
    efeito (cross-uid leftover não trava sweep) · **Abort trigger** uid-novo sem
    allowlist → ITEM-2 re-wedga o box.

#### YB-2 (HIGH) — Divergência de glob entre os 3 descobridores deixa homes isolados INVISÍVEIS ao cache-trim raiz na janela

**Seção afetada:** SPECv2 X1/X3; SPEC ITEM-2; FUNDAÇÃO cachetrim.

- **Por que a 1ª passou:** X1/X3 focaram em re-medir números, assumindo descoberta
  consistente. A SPEC nunca auditou que o codebase tem TRÊS globs diferentes, que só
  coincidem hoje porque os 8 runners moram no mesmo padrão
  `/home/emdev/actions-runner-<proj>`.
- **Verificado:** (1) root-sweep + `cacheHomeRoots` usa
  `/home/*/actions-runner-*/_work` com hífen OBRIGATÓRIO (`cleanup.go:301`); (2) hook
  `workRootGlob` `/home/*/actions-runner*/_work` SEM hífen obrigatório
  (`hook.go:629`); (3) install `defaultRunnerGlob` `/home/*/actions-runner*` SEM
  hífen (`install.go:21`). Na janela ITEM-2: se o runner isolado for
  `/home/runner1/actions-runner/_work` (sem sufixo `-<proj>`), hook e install
  enxergam mas o root-sweep NÃO casa → `cacheHomeRoots` não inclui `/home/runner1` →
  o cache regenerável desse runner cresce SEM cap, reintroduzindo a death-spiral só
  nos isolados, invisível ao curador raiz.
- **Resolução:** unificar o glob de descoberta numa única constante compartilhada
  ANTES de ITEM-2 (fonte única para os 3 call-sites). Slice 0 prova por EFEITO que
  cada `/home/runnerN` é descoberto pelos TRÊS (um `du` do cache de `/home/runnerN`
  aparece na saída do root-sweep), não "config presente".
  - **Disciplina** #13 · **Pergunta** "os 3 descobridores casam o home isolado?" ·
    **Evidência** `du` do home isolado na saída do root-sweep · **Abort trigger**
    qualquer descobridor não casa → glob não unificado, ITEM-2 não avança.

#### YB-3 (HIGH) — O eixo slot/lock/porta é chaveado no NOME-DO-DIR, decoplado do HOME que ITEM-2 muda; o re-registro pode renomear o dir e colidir `COMPOSE_PROJECT_NAME` box-wide

**Seção afetada:** SPECv2 X2; SPEC ITEM-2; multi-project-isolation DT-v2-12.

- **Por que a 1ª passou:** X2 tratou os eixos (cache, daemon, porta) como blocos
  atômicos shipados-ou-não. Nunca considerou que ITEM-2 (mudar HOME) e o MPI
  (`CIVM_RUNNER_SLOT`/`COMPOSE_PROJECT_NAME` via `upsertEnv`) são aplicados em
  ARQUIVOS e por MECANISMOS diferentes, chaveados em EIXOS diferentes — um pode
  estar half-applied em relação ao outro dentro da janela.
- **Verificado:** o slot/projeto vem de `runnerSlot(dir)` derivado de
  `filepath.Base` do DIR e injetado via `upsertEnv` com
  `{CIVM_RUNNER_SLOT,CIVM_PORT_BASE,COMPOSE_PROJECT_NAME}` (`install.go:167-177`).
  NÃO depende de HOME — depende do nome do diretório. Os slots do admit e o dockerlock
  são paths FIXOS box-wide fora de qualquer HOME — mover HOME não os toca (bom). MAS
  se o re-registro mudar o DIR de `/home/emdev/actions-runner-acme` para
  `/home/runner1/actions-runner` (sem sufixo), `runnerSlot` passa de `acme` para
  `actions-runner` (`install.go:102`: `TrimPrefix` falha → devolve a base inteira).
  Dois runners re-registrados sem sufixo colidem em `COMPOSE_PROJECT_NAME=actions-runner`
  → colisão de container/projeto Docker box-wide — o exato failure mode que o MPI
  existe para impedir.
- **Resolução:** ITEM-2 DEVE preservar o sufixo `-<slot>` no nome do dir isolado (a
  invariante de naming do MPI é pré-condição de ITEM-2). Gate de efeito prova
  `CIVM_RUNNER_SLOT`/`COMPOSE_PROJECT_NAME` ainda DISJUNTOS após o re-registro
  (`docker ps --format {{.Names}} | sort | uniq -d` vazio cross-runner), pareado com
  o positivo (mesmo runner mantém seu slot).
  - **Disciplina** #13 + #16 · **Pergunta** "o re-registro preserva o sufixo de slot?"
    · **Evidência** slots disjuntos pós-re-registro + positivo · **Abort trigger** dois
    runners com mesmo `COMPOSE_PROJECT_NAME` → colisão box-wide.

#### YB-4 (HIGH) — Rollback no MEIO de ITEM-2 deixa `/home/runnerN` órfã e uma entrada STALE em `runner-isolation.json`; o gate "COUNT == N" lê VERDE falso e habilita o wipe destrutivo

**Seção afetada:** SPECv2 X1/X5; SPEC §Fronteira de atomicidade; ITEM-3.

- **Por que a 1ª passou:** X1/X5 raciocinaram só sobre o caminho FORWARD (isolando).
  Nenhum raciocinou sobre o ROLLBACK no meio — que a própria SPEC lista como "estado
  parcial aceito" e a Kahneman #16 exige.
- **Verificado:** (A) `runner-isolation.json` é escrito append-by-runner (SPECv2 X1),
  mas a SPECv2 NUNCA especifica a REMOÇÃO da entrada quando um runner reverte. Se
  runner3 foi isolado (entrada gravada) e depois revertido, a entrada PERMANECE; o
  gate de ITEM-3 lê COUNT==N e fica VERDE enquanto runner3 está de fato COMPARTILHADO
  → habilita o wipe-por-job num box que ainda tem runner no `$HOME` compartilhado:
  repete civm#117 (apaga sibling MID-JOB). (B) Se o rollback só reverte HOME e não
  apaga `/home/runnerN`, a árvore cache+_work órfã fica invisível ao curador (ver
  YB-2) → vazamento de disco permanente.
- **Resolução:** (1) o gate de ITEM-3 lê o efeito VIVO (HOMEs reais dos units
  `actions.runner.*` ativos via `systemctl show`, não o JSON histórico) e exige TODO
  runner ativo isolado AGORA — a fonte canônica de N (OQ-1) DEVE ser a lista viva de
  units, não o arquivo; (2) o rollback de ITEM-2 DEVE incluir purge idempotente de
  `/home/runnerN`.
  - **Disciplina** #13 + #16 rollback completo · **Pergunta** "um rollback no meio
    deixa o box consistente e o gate honesto?" · **Evidência** gate lê units vivos +
    rollback purga `/home/runnerN` · **Abort trigger** gate lê JSON histórico →
    VERDE falso habilita wipe.

#### YB-5 (MEDIUM, residuo-aceito) — Na janela, o cachetrim DIVIDE o orçamento de família entre dirs de homes mistos; cada home isolado encolhe o cap por-dir de TODOS, podendo cair abaixo do working-set go-build

**Seção afetada:** SPECv2 X1/X3/DT-5; FUNDAÇÃO cachetrim.

- **Por que a 1ª passou:** X1 tratou o cachetrim como constante estável na janela e
  DT-5/X3 proibiram apertar caps (34→20). Não percebeu que o ORÇAMENTO POR DIR é
  DINÂMICO: `cachetrim.Caps` divide `familyMaxGB` pelo NÚMERO de dirs descobertos.
- **Verificado:** `cachetrim.go:118-133` `family()` coleta dirs por glob ACROSS todos
  os homes e divide igualmente (`per := familyMaxGB*giB/len(dirs)`, piso 1 GB); o
  root-sweep passa `cacheHomeRoots` = TODOS os homes. go-build working-set ~2.2 GB/dir,
  família 12 GB. Hoje 4 dirs → 3 GB cada (folga). Com 6+ dirs (mais homes isolados) →
  12/6=2 GB < 2.2 → `TrimByAge` deixa de ser no-op DURANTE o job → o
  EmergencyBypass/disk-watchdog volta a `WipeWhole` go-build EM ESCRITA → reintroduz
  `can't import facts` nos runners cujo cap caiu sob o working-set, sem editar
  constante. O gate X3 mede o AGREGADO no Slice 0, não re-mede o per-dir conforme
  homes entram.
- **Resolução:** a divisão do orçamento DEVE ser por-home (cada home recebe a família
  inteira dividida só entre seus dirs) OU o cap deve ter PISO por-dir ≥ working-set
  medido (go-build 2.2 GB) que nunca encolhe com `len(dirs)`.
  - **Disciplina** #13 + #3 número não adjetivo · **Evidência** per-dir cap ≥
    working-set medido com ≥6 dirs · **Abort trigger** per-dir < working-set →
    re-introduz A2.

### Lente C — Proveniência N=1 dos números (janela única 2026-06-15, atípica)

#### YC-1 (CRITICAL) — A cura do N=1 (X3) é ela mesma N=1: Slice 0 re-mede UMA vez, num estado não-escolhido

**Seção afetada:** SPECv2 X3/DT-v2-3; SPEC Slice 0.

- **Por que a 1ª passou:** X3 declarou-se "resolvido" prescrevendo "Slice 0 RE-MEDE",
  mas o próprio texto diagnostica o defeito como "UMA leitura, NÃO série temporal" e a
  resolução prescreve literalmente UMA re-leitura. Trocou um N=1 por outro N=1. A 1ª
  rodada SABE aplicar N≥3 (exige para o login serial, ITEM-6) mas nunca o transportou
  para os números de disco/RAM que gateiam a ORDEM inteira.
- **Resolução:** Slice 0 produz N≥3 leituras em estados EXPLICITAMENTE distintos e
  nomeados: (a) idle frio (pós-reboot, zero jobs), (b) steady-state típico (1-2 jobs
  light), (c) pico (2 heavy concorrentes durante build do next + yarn). Cada decisão
  dura (lever=Docker, cap não aperta, MaxHeavy=2, threshold) cita a leitura PESSIMISTA
  das 3, não a média nem a primeira. O abort trigger dispara se QUALQUER um dos 3
  estados violar.
  - **Disciplina** #1 WYSIATI + #3 número não adjetivo · **Pergunta** "o número que
    gateia a ordem é robusto em 3 estados?" · **Evidência** 3 leituras nomeadas no
    IMPL · **Abort trigger** qualquer estado viola o threshold → ordem reavaliada.

#### YC-2 (CRITICAL) — Contradição de proveniência DENTRO da janela: "7 GB RAM" (headline) vs "VM dynamic 8-12, em ~9 GB" (parêntese, mesma medição)

**Seção afetada:** SPECv2 X4; SPEC RF-4; PRD §Resumo.

- **Por que a 1ª passou:** X4 cruzou warm-up frio × teto de RAM tratando "7 G" como
  teto fixo conhecido. Mas a própria fonte N=1 (PRD) registra DYNAMIC MEMORY:
  headline "7 GB RAM", parêntese da mesma leitura "VM dynamic 8-12, em ~9 GB". Os dois
  números são da MESMA janela e se contradizem em ~30%. O "7G" nem é o total — é um
  ponto instantâneo de um balloon que varia 8-12, capturado durante um re-run de CI
  pesado.
- **Resolução:** resolver a contradição ANTES de qualquer decisão de RAM. O número que
  gateia é o MemTotal MÍNIMO garantido pelo balloon (o piso da faixa, ~8 G se a config
  Hyper-V garante 8-12), não um snapshot. Medir MemTotal nos 3 estados de YC-1 e usar
  o mínimo. `effectiveMemMB` (`admit.go`/`cmd/civmctl/admit.go`) já lê MemTotal em
  runtime e fail-closed — o cap POR-SLOT se auto-adapta — mas o teto de CONTAGEM
  MaxHeavy=2 é estático e foi escolhido contra o snapshot. Validar que MaxHeavy=2
  sobrevive ao piso do balloon.
  - **Disciplina** #3 número não adjetivo + #5 worst-case · **Pergunta** "qual o piso
    garantido de RAM, e MaxHeavy=2 sobrevive a ele?" · **Evidência** MemTotal mínimo
    medido nos 3 estados · **Abort trigger** 2 caps somados > piso do balloon →
    MaxHeavy reavaliado.

#### YC-3 (HIGH, residuo-aceito) — `DefaultHostVolumeScratchBudgetGB=11` é um p100 (máximo único observado) +1 GB — a estatística mais frágil sob N=1

**Seção afetada:** SPECv2 X3; `civm.go` constantes; host-volume-reclaim deep-dive.

- **Por que a 1ª passou:** X3 listou os números frágeis mas NÃO listou
  `ScratchBudgetGB`, operacionalmente o mais perigoso: `internal/civm/civm.go`
  documenta-o como "p100 scratch high-water observado (10) + 1". Um p100 é a cauda
  mais instável; sob N=1 o "verdadeiro p100" ao longo de estados quase certamente
  excede 10. Esse budget gateia a admissão de emergência de Optimize-VHD.
- **Resolução:** re-derivar de N≥3 janelas que incluam o pior caso (2 heavy +
  Optimize-VHD concorrente), com margem proporcional ao desvio entre janelas, não +1
  fixo. Enquanto só houver 1 janela, marcar a constante PROVISÓRIA no IMPL e fazer o
  gate pós-Off (que re-mede a folga real, `civm.go`) ser o AUTORITATIVO — o que o
  código já faz. Resíduo: o budget pré-filtro pode abortar cedo e mascarar o gate
  autoritativo.
  - **Disciplina** #3 + #13 · **Evidência** p100 de ≥3 janelas · **Abort trigger**
    budget pré-filtro aborta reclaim legítimo → marcar provisória.

#### YC-4 (CRITICAL) — Os thresholds de abort de X3 (Docker<10GB, cache>30GB) são números N=1 não-derivados — a salvaguarda contra N=1 calibrada com N=1

**Seção afetada:** SPECv2 X3/DT-v2-3 (abort trigger).

- **Por que a 1ª passou:** X3 introduziu como salvaguarda do N=1 dois thresholds que
  são eles mesmos N=1: 10 ≈ metade do 18 GB medido uma vez, 30 ≈ cap-34 menos folga
  arbitrária. Nenhum tem série temporal nem justificativa de distribuição. Um abort
  trigger que é ele próprio um chute de janela única não é fail-fast determinístico
  (#15: abort ≠ trigger se o trigger é ruído).
- **Resolução:** derivar os thresholds da distribuição das N≥3 leituras de YC-1: o de
  cache = "cap_34 menos a margem do A2-race medida"; o de Docker = "o piso abaixo do
  qual o prune não libera o suficiente para a folga-alvo de 30 GB livres", ambos
  calculados, não adivinhados. Documentar a derivação no IMPL ao lado das 3 leituras.
  - **Disciplina** #15 fail-fast + #3 · **Evidência** thresholds derivados da
    distribuição · **Abort trigger** threshold sem derivação → ruído, não fail-fast.

#### YC-5 (CRITICAL) — Os `DATA-REPORT.md` citados como "Confirmado nos dados" e os deep-dives da migração NÃO EXISTEM

**Seção afetada:** PRD §Resumo/proveniência; SPECv2 X3; matriz inteira.

- **Por que a 1ª passou:** PRD e 1ª rodada tratam a proveniência como "Confirmado nos
  dados (#13, 2026-06-15)" apontando para `ephemeral-clean-slate-ci/DATA-REPORT.md` e
  `vm-disk-budget/DATA-REPORT.md`. Verificado: `docs/specs/ephemeral-clean-slate-ci/`,
  `docs/specs/vm-disk-budget/` e `docs/specs/guest-access-resilience/` NÃO EXISTEM
  (`ls docs/specs/` lista cachetrim-yarn-atomic, host-volume-reclaim-liveness,
  multi-project-isolation, runner-memory-admission, civm-self-cleaning-runner, mas
  NENHUM dos 3). A proveniência N=1 não é nem rastreável — os "dados" que justificam
  18 GB Docker, ~17.9 GB volume, ~13 GB cache/job são citados de relatórios fantasma.
  Existência ≠ função (#13) aplicado à própria fonte.
- **Resolução:** BLOQUEAR até que (a) os `DATA-REPORT.md` sejam escritos com as N≥3
  leituras de YC-1, e (b) os deep-dives existam. Enquanto não existirem, toda marca
  "Confirmado nos dados/em docs" no PRD é factualmente falsa e deve virar
  "Inferência / a medir". Nota: `runner-memory-admission` e `multi-project-isolation`
  EXISTEM e ancoram RF-4/X2; o buraco é específico nos três acima.
  - **Disciplina** #13 + rastreabilidade · **Evidência** DATA-REPORT.md + deep-dives
    no filesystem · **Abort trigger** "Confirmado" sobre doc ausente → reclassificar
    como inferência.

#### YC-6 (MEDIUM) — A janela de medição foi capturada num estado auto-declarado ATÍPICO (re-run de CI pesado + pós-reboot) e usada como baseline normal

**Seção afetada:** SPECv2 X3; PRD §proveniência.

- **Por que a 1ª passou:** X3 reconheceu N=1 mas não registrou que o estado era
  ATÍPICO em DOIS eixos: (1) meio de re-run pesado → Docker build-cache/volumes
  inflados, SUPERESTIMANDO o "18 GB reclamável" e reforçando "Docker é o lever";
  (2) logo após reboot → caches de FS frios, SUBESTIMANDO o working-set de cache,
  reforçando "cache fica longe do cap 34". As DUAS distorções empurram na MESMA
  direção da conclusão do design — o pior tipo de viés: o estado atípico não adiciona
  ruído, ele CONFIRMA a hipótese.
- **Resolução:** a leitura de cache working-set DEVE ser em steady-state quente
  (após vários PRs repovoarem yarn/go-build); a de Docker reclamável em idle (sem
  re-run inflando). Marcar no IMPL que a janela 2026-06-15 é ENVIESADA-A-FAVOR-DA-TESE
  e NÃO pode ser uma das 3 leituras de YC-1 — conta como anti-exemplo.
  - **Disciplina** #1 WYSIATI · **Evidência** cache quente + Docker idle separados ·
    **Abort trigger** baseline reusa a janela enviesada.

#### YC-7 (MEDIUM, residuo-aceito) — MaxHeavy=2 é teto de CONTAGEM estático contra o snapshot, e o cap POR-JOB (HeavyMaxMB) é admitidamente não-calibrado

**Seção afetada:** SPECv2 X4 (RF-4 "intocável"); runner-memory-admission.

- **Por que a 1ª passou:** classificou RF-4/admit como "INTOCÁVEL e ortogonal" sem
  submetê-lo à lente de proveniência. Mas a defesa de RAM tem duas pernas fracas sob
  N=1: (a) `HeavyMaxMB=0` ("generoso") e `runner-memory-admission/SPECv4.md`
  admite que a CALIBRAÇÃO real ("após medir o pico RSS real de jobs heavy") AINDA NÃO
  FOI FEITA; (b) a única defesa efetiva é MaxHeavy=2, dimensionado contra o snapshot
  7G/~9G atípico (YC-2). Se o pico RSS de 2 heavy concorrentes exceder
  `(MemTotal_piso-2048)/2`, o admit conta 2 mas não protege RAM — o sshd-wedge que o
  gate existe pra prevenir.
- **Resolução:** calibrar `HeavyMaxMB` com o pico RSS real de 2 heavy concorrentes
  medido nos 3 estados de YC-1 (que SPECv4 já reconhece pendente); só então afirmar
  MaxHeavy=2 seguro. Até lá é aposta de janela única. O fail-closed de `effectiveMemMB`
  cobre MemTotal ilegível, mas não cobre 2 caps generosos somados > RAM sob balloon
  deflacionado durante warm-up frio dos 8 — o cruzamento que falta medir.
  - **Disciplina** #5 worst-case + #3 · **Evidência** pico RSS de 2 heavy medido ·
    **Abort trigger** 2 caps somados > piso do balloon → MaxHeavy reavaliado.

### Lente D — Gate por-efeito vs config-presente + rollback completo (#13/#16) na SPEC unificada

#### YD-1 (CRITICAL) — Os 3 deep-dives delegados como "diff fino" NÃO EXISTEM — a SPEC inteira pende de proveniência ausente

> Reconciliação: confirma e amplia YA-3 e YC-5 sob a lente de gate-por-efeito.

**Seção afetada:** SPEC §Escopo (delegação); matriz PRD→SPEC; ITEM-6.

- **Por que a 1ª passou:** X3 atacou a proveniência dos NÚMEROS, mas assumiu que o
  DETALHE de implementação (esqueleto, critérios de aceite finos) vivia nos
  deep-dives. Não verificou que os 3 paths retornam ENOENT.
- **Verificado:** PRD/SPEC/SPECv2 referenciam os 3 dirs ~40× ("Diff fino: ...",
  "Deep-dive: ...") e todos os 3 estão ausentes. A guarda-chuva por design NÃO
  replica o esqueleto ("aponta o componente que detalha cada um") — então o critério
  de aceite fino de ITEM-1/3/4/6 não existe em lugar nenhum. ITEM-6 (serial OOB) é o
  pior: não há NENHUM spec de guest-access no repo, então o "diff fino" do RF-7 é
  vapor.
- **Resolução:** antes de qualquer ITEM avançar: ou (a) criar os 3 deep-dives com o
  diff fino e os critérios por-efeito, ou (b) reconhecer que o detalhe migrou para os
  specs que DE FATO existem (`host-volume-reclaim-liveness` cobre vm-disk-budget;
  `civm-self-cleaning-runner` cobre o efêmero) e re-apontar TODA a matriz para os
  paths reais. Gate: a matriz só passa quando cada link resolve via
  `validate-templates`.
  - **Disciplina** #13 + Princípio 4 · **Evidência** `validate-templates` verde ·
    **Abort trigger** link ENOENT → matriz inválida.

#### YD-2 (CRITICAL) — X2 raciocina sobre um eixo PORTA/daemon JÁ shipado e sobre um acoplamento dockerlock↔admit JÁ substituído — modelo de risco do ITEM-5 contra codebase obsoleto

> Reconciliação: confirma YA-2 pela lente de codebase-obsoleto.

**Seção afetada:** SPECv2 X2/DT-v2-2, OQ-3; SPEC ITEM-5.

- **Por que a 1ª passou:** X2 deixou OQ-3 aberto ("MPI estará shipado antes de
  ITEM-5?"). Mas o código prova MPI shipado (`internal/portblock`, `internal/ciguard`
  existem; `install.go:173-175` já escreve `CIVM_RUNNER_SLOT`/`CIVM_PORT_BASE`/
  `COMPOSE_PROJECT_NAME`). A contenção de daemon também já foi reimplementada FORA do
  dockerlock (admit sub-slot docker count=1, `civm.go:165-167`, `admit.go:293`;
  `deploy/systemd/README.md` declara que `--exclusive docker` substitui o
  `civmctl lock --scope docker-heavy` LEGADO). A SPEC trata como pergunta-de-rollout
  algo já-consumado-no-código.
- **Resolução:** re-derivar X2 contra o código: (1) OQ-3 RESOLVIDA — MPI shipado;
  (2) reconhecer que a contenção de daemon vive em DOIS lugares concorrentes
  (dockerlock legado via `civmctl lock`, que ci-guard R4 AINDA recomenda, E admit
  `--exclusive docker`) — DUPLICAÇÃO Day-0 proibida, não kill-switch; (3) o gate de
  ITEM-5 prova que o sub-slot docker serializa o que o dockerlock serializava e então
  DEPRECA o `civmctl lock` legado de fato (incluindo a regra R4 do ci-guard).
  - **Disciplina** #13 + Day-0 · **Evidência** R4 re-apontada ao admit + `civmctl lock`
    removido · **Abort trigger** dois mecanismos vivos para a mesma coisa.

#### YD-3 (HIGH) — ITEM-1 e ITEM-5 modificam e aposentam O MESMO código (`dockerPruneSafe` + defer `deferred-by-docker-heavy-lock`) em direções opostas; o gate de ITEM-5 não mede o efeito que importa

**Seção afetada:** SPEC ITEM-1, ITEM-5.

- **Por que a 1ª passou:** tratou ITEM-1 (endurecer prune) e ITEM-5 (aposentar lock)
  como fatias ordenadas independentes. Mas no código tocam a MESMA função:
  `cleanup.Run` faz early-return `deferredByDockerHeavyLock` quando
  `dockerlock.IsActive` (`cleanup.go:193`) e SÓ ENTÃO chama `dockerPruneSafe`
  (`cleanup.go:207`, alvo do ITEM-1). O dockerlock que ITEM-5 quer aposentar é
  exatamente o guard que impede o prune endurecido de rodar contra um daemon que um
  job docker-heavy usa. Quando ITEM-5 remove o lock do defer, o `image prune -a -f
  --filter until=168h` + `volume prune -f` do ITEM-1 passa a poder rodar CONCORRENTE
  com um build de outro runner — o failure mode do comentário em `hook.go` ("removes
  a recently-pulled but old vendor image -> No such image"; `until` casa CREATED
  date, não PULL date).
- **Resolução:** o gate por-efeito de ITEM-5 prova: com o dockerlock REMOVIDO do defer,
  rodar o prune endurecido CONCORRENTE com `compose up --build` de outro runner e
  provar por efeito que (a) imagem vendor recém-puxada (CREATED>7d mas PULLED agora)
  SOBREVIVE e (b) volume anexado a container vivo SOBREVIVE. Esse par-positivo falta:
  o gate de ITEM-1 testa em-uso-sobrevive COM o dockerlock ativo; ninguém testa SEM
  ele (o estado pós-ITEM-5). ITEM-1 não pode entrar com `image prune -a` no caminho
  BUSY antes de ITEM-5 provar que o admit docker sub-slot cobre a serialização — senão
  o dia que ITEM-5 remove o lock, o prune vira arma carregada sem trava.
  - **Disciplina** #13 par positivo · **Evidência** em-uso-sobrevive SEM dockerlock ·
    **Abort trigger** prune endurecido + sibling build → imagem/volume em uso some.

#### YD-4 (HIGH, residuo-aceito) — ITEM-1 troca `--threshold-pct=0` por `=1` como bug-fix, mas isso muda a semântica de gating (used%>1 sempre verdadeiro → cleanup dispara SEMPRE); o rollback para `=0` restaura o literal, não a intenção

**Seção afetada:** SPEC ITEM-1; `diskwatchdog.go`; `civm-vhdx-autoreclaim.ps1`.

- **Por que a 1ª passou:** nenhum X examinou a semântica de gating do threshold.
- **Verificado:** `opts.ThresholdPct==0 → DefaultWatchdogThresholdPct ==
  DefaultPreCleanupPct == 60` (`diskwatchdog.go:135-136`, `civm.go:37,39`). O host
  autoreclaim chama `disk-watchdog --threshold-pct=0` (`autoreclaim.ps1:412`), então
  hoje dispara prune quando used%>60. Trocar para `=1` faz cleanup disparar quando
  used%>1 — i.e. SEMPRE. O caminho do host já quer prune incondicional ali, mas
  `disk-watchdog --execute` é subcomando GERAL: mudar o efetivo-via-0 de 60 para
  sempre-via-1 altera o gating de qualquer outro call-site que passe 0 ou conte com o
  piso 60.
- **Resolução:** auditar TODOS os call-sites de `disk-watchdog`/`Check` que dependem
  do clamp 0→60 antes de mexer. Se a intenção é "no caminho de liveness do host,
  prune sempre", a correção certa NÃO é baixar o threshold global para 1 — é um flag
  dedicado (`--force-prune`) que não reusa a semântica de threshold. O rollback
  proposto ("--threshold-pct volta a 0") restaura o LITERAL, mas 0 hoje significa 60;
  rollback de valor, não de intenção (#16). Gate por-efeito: provar que com `=1`
  nenhum outro caminho passa a disparar cleanup em used baixo (ex.: cleanup mid-job
  em runner a 5% de disco).
  - **Disciplina** #16 rollback completo · **Evidência** auditoria de call-sites +
    flag dedicado · **Abort trigger** outro call-site dispara cleanup em used baixo.

#### YD-5 (HIGH) — RNF-1 "corrupção zero por construção" é FALSO para o eixo containerd/overlayfs que a própria X5 nomeia como "a fonte exata da corrupção": o efêmero isola `$HOME`/_work/cache mas NÃO o `/var/lib/docker` single-daemon-shared, e ITEM-1 AUMENTA a escrita concorrente nele

**Seção afetada:** PRD RNF-1; SPECv2 X5 red-team; SPEC ITEM-1/ITEM-2.

- **Por que a 1ª passou:** X5 endureceu o gate de ITEM-3 e observou que "wipe-leve do
  `$HOME` compartilhado deixa daemon Docker/overlayfs (a fonte exata da corrupção de
  extract de containerd)". Mas parou aí: nomeou a raiz e não a resolveu em ITEM nenhum.
- **Verificado:** ITEM-2 isola `$HOME`/_work/cache — NÃO o `/var/lib/docker`
  (overlayfs/content store do containerd), que é UM SÓ daemon compartilhado por
  construção (DT-1 descartou DinD-por-job). Após ITEM-1+2+3+4, o RNF-1 ("corrupção
  zero por construção") é FALSO para esse eixo: overlayfs/containerd permanece
  mutável-compartilhado e ITEM-1 AUMENTA a escrita concorrente (`volume prune -f` novo
  + `image prune -a`). `job-started` já roda `docker volume prune -f` +
  `container prune -f` (`hook.go`), prova de que o daemon já é mexido concorrentemente.
- **Resolução:** REBAIXAR RNF-1 honestamente: o efêmero mata a corrupção de cache de
  FILESYSTEM (yarn/go-build), não a de containerd/overlayfs, que é single-daemon-shared
  e fora do escopo dos 6 ITENS. Ou (a) declarar containerd/overlayfs como F-out-of-scope
  explícito com failure mode aceito e o backstop existente (clean+retry), ou (b)
  reconhecer que o admit `--exclusive docker` JÁ serializa o acesso destrutivo ao
  daemon (fecha o círculo com YD-2) mas SÓ para jobs envelopados em admit — jobs que
  não optam por admit ainda corrompem. Gate por-efeito que falta: provar que prune
  concorrente (ITEM-1) + extract de containerd de um sibling não reproduz "unable to
  lease content: lease does not exist". Hoje nenhum teste o exerce, então o RNF-1 é
  afirmação sem efeito-medido — a própria ilusão #13 que a SPEC diz combater.
  - **Disciplina** #13 · **Evidência** prune concorrente + extract sibling não
    reproduz erro de lease · **Abort trigger** RNF-1 afirmado sem teste de efeito.

---

## Reconciliação com as resoluções X1–X5 da SPECv2

> A 2ª rodada não joga fora a SPECv2 — separa o que ela acertou do que ela amarrou
> ao vazio. Critério: a resolução X foi atacada contra o CÓDIGO. CONFIRMADA = o
> mecanismo é real e a resolução se sustenta; RABATIDA = a resolução pende de
> premissa que a verificação derrubou.

| Resolução SPECv2 | Veredito 2ª rodada | Motivo (verificado) |
| ---------------- | ------------------ | ------------------- |
| **X1** — janela de coexistência, cachetrim ATIVO, `runner-isolation.json` COUNT==N | **RABATIDA** | O sweep raiz deriva HOME por PATH e não vê `/home/runnerN` (YA-1); o produtor de `runner-isolation.json` não existe (YA-5); o gate por COUNT é VERDE-falso sob rollback parcial (YB-4). O cachetrim "ATIVO" não cobre os isolados. |
| **X2** — `dockerlock` fica no eixo porta se MPI não shipado; pacote nunca deletado | **RABATIDA** | MPI JÁ shipado (`portblock`/`install.go:173-175`); o eixo daemon JÁ tem substituto Day-0 (admit `--exclusive docker`). Manter dois serializadores é DUPLICAÇÃO Day-0 proibida, não kill-switch (YA-2, YD-2). |
| **X3** — Slice 0 re-mede; abort se Docker<10GB ou cache>30GB | **RABATIDA** | A cura do N=1 é ela mesma N=1 (YC-1); os thresholds de abort são N=1 não-derivados (YC-4); os DATA-REPORT.md citados não existem (YC-5); a janela é enviesada-a-favor-da-tese (YC-6). |
| **X4** — warm-up serial espelha `--warm`; admit é o gate de RAM | **PARCIAL** | 1ª perna RABATIDA: `--warm` aquece IMAGENS, não build-caches, e `setup-ci-cache.sh` não existe (YA-4). 2ª perna CONFIRMADA: admit MaxHeavy=2 é real e carrega a segurança (mas o número 2 é N=1, YC-7). |
| **X5** — gate de ITEM-3 por efeito-medido (sibling não-tocado) | **CONFIRMADA NO PRINCÍPIO, RABATIDA NA AMARRA** | O princípio (#13 efeito > config) é correto e mantido. Mas a amarra concreta lê um arquivo sem produtor (YA-5), o restart é all-at-once (YA-5), e o teste de efeito ignora o data-root containerd compartilhado (YD-5). |

**Síntese:** a SPECv2 acertou a POSTURA (efeito-medido, fail-closed, kill-switch,
N≥3 — onde aplicou) mas errou as PREMISSAS DE CODEBASE em 4 das 5 resoluções. O GO
da 1ª rodada repousava sobre essas premissas. Derrubadas as premissas, o GO cai.

---

## Open questions (atualizadas)

- **OQ-1 (RESOLVIDA EM DIREÇÃO):** a fonte canônica de N runners NÃO é
  `runner-isolation.json` (sem produtor) — DEVE ser a lista viva de units
  `actions.runner.*` via `systemctl show` (YB-4). Confirmar a contagem estável no
  Slice 0.
- **OQ-2 (mantida):** backend do managed cache (ITEM-4) MinIO ou `registry:2`
  estendido — delegada ao deep-dive `ephemeral-clean-slate-ci`, que NÃO existe (YA-3).
  Bloqueia ITEM-4 até o deep-dive existir.
- **OQ-3 (RESOLVIDA):** MPI shipado (`portblock`/`install.go:173-175`). X2 deve ser
  re-derivado para consolidação Day-0 do daemon no admit (YD-2), não manutenção do
  lock.
- **OQ-4 (NOVA):** ITEM-2 usa uid-novo ou env-var-only sob o mesmo uid? Decisão DURA
  pendente — muda o contrato do safedelete (YB-1). RECOMENDADO: env-var-only.
- **OQ-5 (NOVA):** o eixo containerd/overlayfs (`/var/lib/docker`) é F-out-of-scope
  explícito ou coberto pelo admit `--exclusive docker`? RNF-1 não pode afirmar
  "corrupção zero" sem fechar isto (YD-5).

---

## Escopo final do IMPL (pós-2ª auditoria)

> Mudança em relação à SPECv2: o IMPL NÃO pode começar pelos ITENS — precisa de um
> **Slice -1 (fundação documental + de medição)** que feche os buracos estruturais
> antes de qualquer código de ITEM. A ordem ITEM-1..6 da SPECv2 permanece, mas
> GATEADA por pré-condições novas.

- **Slice -1 — Fundação (BLOQUEANTE, sem o qual nenhum ITEM avança):**
  1. criar os 3 deep-dives ausentes OU re-apontar a matriz inteira para os specs reais
     (`host-volume-reclaim-liveness`, `civm-self-cleaning-runner`), com
     `validate-templates` verde (YA-3, YC-5, YD-1);
  2. escrever os `DATA-REPORT.md` com N≥3 leituras NOMEADAS (idle frio / steady /
     pico), excluindo a janela enviesada 2026-06-15 (YC-1, YC-6);
  3. decidir OQ-4 (modelo de uid) e OQ-5 (escopo containerd) por escrito;
  4. unificar o glob de descoberta numa constante compartilhada (YB-2).
- **Slice 0 — Medição (re-mede com N≥3):** thresholds de abort DERIVADOS da
  distribuição, não fração de leitura única (YC-4); MemTotal mínimo do balloon
  (YC-2); pico RSS de 2 heavy para validar MaxHeavy=2 (YC-7); `ScratchBudgetGB`
  re-derivado ou marcado provisório (YC-3); fonte canônica de N = units vivos (OQ-1).
- **ITEM-1 — Docker prune endurecido:** `image prune -a`/`volume prune -f` NÃO entra
  no caminho BUSY antes de ITEM-5 provar a serialização de daemon (YD-3); o
  `threshold-pct` vira flag dedicado `--force-prune`, não reuso de threshold (YD-4).
- **ITEM-2 — isolamento per-runner:** modelo de uid declarado (env-var-only
  recomendado, YB-1); preserva o sufixo `-<slot>` no nome do dir (YB-3); enumera cache
  por HOME efetivo (YA-1); restart staged per-runner, não glob all-at-once (YA-5);
  rollback purga `/home/runnerN` idempotente (YB-4); produtor de
  `runner-isolation.json` definido OU substituído por units vivos (YA-5, OQ-1).
- **ITEM-3 — wipe por-job:** gate lê o efeito VIVO (units `actions.runner.*` isolados
  AGORA), não JSON histórico (YB-4); teste de efeito estende ao data-root containerd
  (YD-5).
- **ITEM-4 — managed cache:** `setup-ci-cache.sh` DEVE existir e aquecer build-caches
  (não imagens); aceite por miss-rate de build-cache pós-warm (YA-4); cap por-dir com
  piso ≥ working-set ou divisão por-home (YB-5).
- **ITEM-5 — consolidar serialização de daemon:** NÃO é "remover do eixo cache" — é
  rotear todo step docker-heavy pelo admit `--exclusive docker` e APOSENTAR o
  `civmctl lock` legado + a regra R4 do ci-guard (YA-2, YD-2); par-positivo
  em-uso-sobrevive SEM dockerlock (YD-3).
- **ITEM-6 — serial OOB:** o deep-dive `guest-access-resilience` NÃO existe — RF-7 é
  vapor até ser escrito (YD-1).
- **RNF-1 rebaixado:** "corrupção zero por construção" só vale para cache de
  FILESYSTEM; containerd/overlayfs é F-out-of-scope ou admit-serializado (YD-5, OQ-5).

---

## Go / No-go

**NO-GO — requer 3ª rodada após o Slice -1 fechar a fundação.**

Justificativa: a 2ª rodada achou **15 findings novos** (9 CRITICAL, 4 HIGH, 2
MEDIUM/resíduo-aceito após rebaixamento), TODOS verificados em arquivo:linha contra
o código real. Eles não são refinamentos cosméticos — são três classes de buraco
que invalidam o GO da SPECv2:

1. **Fundação documental ausente** (YA-3, YC-5, YD-1): 3 deep-dives e 2 DATA-REPORT
   referenciados ~40× não existem no filesystem. O IMPL não tem onde ler o diff de 4
   dos 6 ITENS, e a proveniência "Confirmado nos dados" é factualmente falsa. SSDV3
   Princípio 4 (rastreabilidade) e #13 (existência ≠ função) reprovam.
2. **Gates amarrados ao vazio** (YA-1, YA-5, YB-2, YB-4): o cap de cache não vê os
   homes isolados; o sinal que o gate de ITEM-3 lê não tem produtor; o restart é
   all-at-once contradizendo a janela parcial graciosa; o COUNT==N é VERDE-falso sob
   rollback. O efeito-medido que a SPECv2 prometeu não tem como ser medido.
3. **Codebase obsoleto no modelo de risco** (YA-2, YD-2, YD-3): MPI e o admit
   `--exclusive docker` JÁ shipados reescreveram o eixo daemon; manter o dockerlock
   legado é DUPLICAÇÃO Day-0 proibida, não kill-switch. ITEM-1 e ITEM-5 mexem na mesma
   função em direções opostas sem o par-positivo que cobre o estado pós-migração.

Nenhuma dessas é resolúvel só com redação — todas exigem decisão nova (criar/re-apontar
deep-dives, definir produtor de sinal, decidir modelo de uid, derivar thresholds de
N≥3, consolidar o daemon no admit). Pela regra de saída do Passo 2.5: **finding que
exige decisão nova → volta ao PASSO 2.** Esta SPECv3 é o registro; a próxima versão
(SPECv4 ou SPECv3 atualizada in-place) só fecha GO quando o Slice -1 entregar a
fundação e a 3ª rodada confirmar por efeito.

**Não houve convergência.** A disciplina manda auditar até convergir; a 2ª rodada
encontrou no-go crítico novo, logo a auditoria continua. Convergência (#13) é um
resultado válido — mas só quando uma rodada NÃO acha no-go crítico novo, o que não é
o caso aqui.

Próximo passo: executar o **Slice -1** (fundação documental + medição N≥3), depois
re-rodar o Passo 2.5 (3ª rodada) sobre o escopo atualizado.
