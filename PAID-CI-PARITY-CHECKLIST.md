# PAID-CI-PARITY-CHECKLIST — civm self-hosted vs GitHub-hosted (CI pago)

> **Snapshot histórico, não baseline operacional.** Este documento foi
> sucedido por `PAID-CI-PARITY.md`; hardware atual fica em
> `docs/HARDWARE.md`. Valores de RAM desta primeira passada não descrevem o
> padrão atual de `12 GiB` fixos.

> **Propósito.** Esta é a **âncora adversarial de paridade** entre o runner
> `civm` (UMA VM Hyper-V compartilhada por 8 runners) e o que o CI **pago**
> (GitHub-hosted, Ubuntu 24.04) entrega. Para CADA garantia do pago, dizemos
> se a box é **FIEL / PARCIAL / INFIEL / IMPOSSÍVEL** — e a **ação** para
> fechar a lacuna (ou a justificativa de por que é estruturalmente impossível
> nesta box). Sem rubber-stamp: onde a box mente sobre si mesma, dizemos.
>
> **Regra de honestidade (Kahneman #13 — existência ≠ função).** "O hook
> roda", "o cleanup existe", "tem cache slot" NÃO é paridade. Paridade é o
> **efeito** observável: um job começa num estado tão limpo, isolado e
> previsível quanto começaria numa VM efêmera nova do GitHub. Marcamos pelo
> efeito real medido na box (`ssh gha-ubuntu-2404`, read-only) + código
> (`arquivo:linha`), não pela intenção do design.
>
> **Status desta passada:** primeira passada — esqueleto enumerado + veredito
> inicial por dimensão. As linhas-âncora abaixo viram itens de trabalho
> rastreáveis. Confirme cada veredito contra a box viva antes de agir.

## Baseline REAL medido (não herdado de texto — disciplina #3)

| Recurso | GitHub-hosted (pago, público) | civm box (medido 2026-06-16) |
| --- | --- | --- |
| Modelo de execução | **1 VM efêmera nova por job**, destruída ao fim | **1 VM Hyper-V permanente** (`up 2:43h`), 8 runners systemd long-lived |
| CPU | 4 vCPU (público) / 2 (privado) | **12 vCPU** compartilhados entre 8 runners |
| RAM | 16 GB (público) / 8 GB (privado) | **9.9 GiB total** (1.3 GiB free, 7.1 GiB buff/cache, 4 GiB swap c/ 558 MiB usados) |
| Disco | **14 GB SSD fresco por job** | **37,70 GiB compartilhados, 14,03 GiB disponíveis @ 63% usado**, medido em 30/07/2026 |
| Docker daemon | **fresco por job**, próprio, descartado | **1 daemon único** (`29.1.3`, overlayfs, `/var/lib/docker`) — snapshot 30/07: 0 imagens/containers/cache, 174 volumes inativos e 10,44 GB recuperáveis |
| `$HOME` | fresco por job | **/home/emdev ÚNICO** compartilhado pelos 8 runners |
| Rede / portas | namespace de VM próprio | **netns único do host**; portas reservadas por bloco `CIVM_PORT_BASE` (20000–32000) |
| Isolamento entre jobs | hipervisor (VM-por-job) | **nenhum** a nível de kernel; só convenções (slot, port-block, lock) |
| Host pressure | invisível (GitHub absorve) | guest **vê** o V: do host via `/var/lib/civm/host-metrics.json` (`v_free_gb`) |

> Correção factual ao briefing da sessão: a box reporta **12 nproc / 9.9 GiB**
> (não 8 nproc / 7.7 GiB) e **27 GB livres no V: do host** neste snapshot. RAM
> segue apertada; disco segue o eixo crítico.

---

## Esqueleto do checklist — uma linha por dimensão (pago vs civm)

> Legenda do veredito: **FIEL** (efeito equivalente) · **PARCIAL** (mitigado,
> não equivalente) · **INFIEL** (diverge e pode causar falha/risco) ·
> **IMPOSSÍVEL** (estruturalmente inalcançável nesta box — razão obrigatória).

| # | Dimensão | Garantia do PAGO (GitHub-hosted) | Realidade do CIVM (1ª passada) | Veredito | Ação para fechar a lacuna |
| --- | --- | --- | --- | --- | --- |
| 1 | **Ephemerality / clean-slate** | VM nova e destruída por job; zero estado herdado | Runner long-lived; `174` volumes inativos/`10,44 GB` sobreviveram ao boundary de 30/07/2026 | **INFIEL** | Remoção scoped por labels/identidade no boundary (#181) + `compose down -v`; clean-slate total = IMPOSSÍVEL sem VM-por-job |
| 2 | **Isolamento de daemon Docker** | daemon próprio e fresco por job | 1 daemon compartilhado; corrida "No such image" do prune concorrente (causa-raiz corrigida nesta sessão: `7e9cc0d` drop `-a`; backstops acme) | **IMPOSSÍVEL** (daemon-por-runner = NO-GO: RAM+disco+containerd store compartilhado = isolamento falso, SPECv4) | Caminho viável = **serialização docker-heavy** via `civmctl admit --exclusive docker` + label `civm-e2e`; hoje INERTE (nenhum workflow acme chama; usam `flock` repo-local que o ci-guard R4 rejeita) |
| 3 | **Isolamento de disco** | 14 GB SSD próprio por job | filesystem único de `37,70 GiB`; pressão de um job afeta todos; floors `40/58 GB` estão inalcançáveis | **IMPOSSÍVEL** | Cotas por-runner não implementadas; recalibrar somente após limpeza medida (#181/#182) |
| 4 | **Isolamento de FS / `$HOME`** | `$HOME` fresco por job | `/home/emdev` ÚNICO p/ 8 runners; caches compartilhados; **cache slot por-runner** fecha só a pasta de cache (`yarn-acme-$SLOT`, `go-build-acme-$SLOT`), não o `$HOME` inteiro | **PARCIAL** | `$HOME` per-runner (RF-1 forte) segue **NO-GO** (sem código, SPECv4). Manter slot + auditar todo caminho que escreve fora do slot |
| 5 | **Isolamento de rede / portas** | netns próprio da VM | netns único; colisão evitada por `CIVM_PORT_BASE` (bloco de 64, sticky em `port-blocks.json`); janela 20000–32000 abaixo do ephemeral do kernel (`32768`, confirmado) | **PARCIAL** | Funciona se o peer **adota** `${CIVM_PORT_BASE}`; `ci-guard` R2/R3 linta portas estáticas/`COMPOSE_PROJECT_NAME` ausente. Sem netns real, dois jobs ainda compartilham `localhost` |
| 6 | **Secrets** | injetados por job, VM destruída remove rastro | secrets do GitHub idênticos (mesmo protocolo de runner); MAS persistem em `$HOME`/`_work`/disco compartilhado **entre jobs** até o cleanup; daemon e caches compartilhados ampliam a janela | **PARCIAL** | Hook não vaza (`exec civmctl`, SECURITY.md §privilege boundary). Risco = exfiltração cross-job por job malicioso de peer (sem VM teardown). Mitigar: scrub de `_work`/`/tmp` no `job-completed` + nunca rodar peer não-confiável co-residente |
| 7 | **Cache (`actions/cache`)** | cache remoto do GitHub, isolado por chave/escopo | sem `actions/cache` remoto; cache local em `$HOME/.cache/*-acme-$SLOT`; trimado por idade/cap; corrupção ENOENT histórica resolvida por slot + cap | **PARCIAL** | Cache local é mais rápido mas **não isolado por chave** como o pago; pode ficar stale entre branches. Confirmar que `cachetrim-atomic` (D1) está no main + deployado |
| 8 | **Concorrência / paralelismo** | cada job = VM própria; paralelismo ilimitado (plano) | 8 runners → ≤8 jobs simultâneos; `admit MaxHeavy=2` protege RAM (mas INERTE p/ acme); RAM 9.9 GiB é o teto duro | **INFIEL** | Cabear `admit` nos workflows pesados (`civmctl admit --weight=heavy --exec`) + CI guard. Sem isso, 3+ builds pesados podem estourar a RAM/swap |
| 9 | **Versões de tool / OS (runner-image parity)** | Ubuntu 24.04.4; matriz LARGA (Go 1.22-1.25, Node 22/24, Python 3.10-3.14, Ruby, Java, browsers, AWS/Az/GCP CLI, DBs, Android SDK, compiladores) + Docker 28.0.4 | civm pina subconjunto ESTREITO (go/node/python/docker/compose/gh/git/jq/yq); Docker **29.1.3** (à frente do 28.0.4 do pago) | **PARCIAL** | `civmctl parity` cobre só ~9 tools; um workflow que assume Chrome/AWS-CLI/Java falha na box e passa no pago. Ação: expandir o spec OU documentar o subset suportado + `civmctl drift` |
| 10 | **Cleanup pós-job** | implícito (VM destruída) | `job-completed`: mata órfãos, apaga `_work`, trima cache, prune **só dangling** (nunca `-a`/`--volumes` no path concorrente); best-effort, nunca falha o job (#16 incident) | **PARCIAL** | Cleanup é seguro mas **incompleto** vs teardown total: volumes nomeados e imagens taggeadas de runs sobrevivem. Adicionar prune de volumes órfãos por-runId no boundary |
| 11 | **Failure handling / retry** | falha de infra do GitHub → re-spawn transparente | runner-watchdog reinicia runner offline/failed em VM idle; rerun opt-in; `cancel-in-progress` nos workflows; classify de falhas de infra (SigImageEvicted) | **PARCIAL** | Cobertura boa para falha de runner; mas falha **mid-job** por vizinho (OOM/disco/prune) NÃO é re-spawn transparente — o job morre. Medir taxa de morte por contenção |
| 12 | **Observabilidade** | logs/billing do GitHub | `hooks.jsonl`, `host-metrics.json`, `civmctl doctor/health/capacity/metrics`, node_exporter; host-V: visível ao guest | **FIEL** (até superior em alguns eixos) | Manter; garantir que toda morte por contenção emita linha rastreável (work_root + razão) |
| 13 | **Segurança cross-job** | VM-por-job = sem dwell entre jobs; ataque não persiste | **co-residência real** de jobs de peers distintos no mesmo kernel/daemon/`$HOME` (SECURITY.md §threat model admite explicitamente); só boundary de path/argv validado | **IMPOSSÍVEL** (sem VM-por-job, dwell cross-job é inerente) | Não rodar código de peer **não-confiável** co-residente; isolar peers sensíveis em runner/label dedicado; evitar `pull_request_target` + não expor secrets a código não-confiável |
| 14 | **Custo / recurso** | minutos pagos por job (caro em escala) | grátis (self-hosted) — é a razão de existir da box | **FIEL** (vantagem da box) | N/A — o trade-off é deliberado; a box troca isolamento por custo zero |

---

## Leitura adversarial do "CI pago" no acme (armadilha de nome)

O workflow `paid-ci-approval.yml` é **scaffolding de aprovação**, não execução
github-hosted: TODOS os 4 jobs (`validate-civm`, `paid-ci-preflight`,
`paid-validate`, `paid-approval-result`) declaram `runs-on: [self-hosted, civm]`
e rodam `make ci-full` **na própria box**. O único "pago" é o gate do
environment `paid-github-hosted-ci` (exige reviewers + `prevent_self_review`) e
a flag `ENABLE_PAID_GITHUB_HOSTED_CI`. **Implicação:** hoje o "sinal real" é
sempre o run local da box; o caminho github-hosted de verdade só existe quando a
flag flipa E o `runs-on` muda para `ubuntu-latest`. Esta âncora compara a box
contra esse **alvo pago de verdade** (Ubuntu 24.04 efêmero), não contra o
scaffolding atual.

## Veredito de fechamento (1ª passada)

- **Estruturalmente IMPOSSÍVEL na box** (sem VM-por-job): #2 daemon isolado, #3
  disco isolado, #13 segurança cross-job sem dwell. A fidelidade máxima aqui é
  **serialização + dedicação por label**, nunca isolamento de hipervisor.
- **ACHIEVÁVEL com ação** (o "máximo" que o usuário pede): #1 clean-slate de
  volumes/imagens por-runId, #8 cabear `admit` nos pesados, #9 expandir/declarar
  a matriz de tools, #10 prune de órfãos no boundary, #6 scrub de secrets
  pós-job.
- **Já FIEL ou superior:** #12 observabilidade, #14 custo.
- **PARCIAL a endurecer:** #4 (`$HOME` só via slot), #5 (portas só se o peer
  adota), #7 (cache local não isolado por chave), #11 (sem re-spawn mid-job).

> **Próximo passo (2ª passada):** converter cada linha INFIEL/PARCIAL ACHIEVÁVEL
> num item de trabalho com `arquivo:linha`, teste de efeito (não de existência)
> e rollback trigger numérico — no padrão SSDV3/Kahneman já usado nos SPECs do
> repo.
