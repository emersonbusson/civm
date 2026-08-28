# PAID-CI-PARITY — civm self-hosted vs CI pago (GitHub-hosted)

> **Âncora adversarial de paridade.** Este é o documento de referência canônico
> sobre quão fielmente a box `civm` (UMA VM Hyper-V compartilhada por 8 runners)
> reproduz o ambiente que o CI **pago** entrega (GitHub-hosted, `ubuntu-latest`,
> Ubuntu 24.04 — VM efêmera nova por job). O objetivo declarado pelo usuário é
> **"o mais fiel possível, 100% ou o máximo"**. Este doc diz, para CADA garantia
> do pago, se a box é **FIEL / PARCIAL / INFIEL / IMPOSSÍVEL** — com evidência
> REAL (código `arquivo:linha` + saída da box) e a **AÇÃO** para fechar a lacuna,
> ou a razão de por que é estruturalmente impossível.
>
> **Regra de honestidade (Kahneman #13 — existência ≠ função).** "O hook roda",
> "o cleanup existe", "tem cache slot", "o endpoint responde" NÃO é paridade.
> Paridade é o **efeito** observável: um job começa num estado tão limpo,
> isolado e previsível quanto começaria numa VM efêmera nova do GitHub.
> Marcamos pelo efeito real medido, não pela intenção do design. Sem
> rubber-stamp: onde a box mente verde sobre si mesma, dizemos.
>
> **Legenda do veredito:**
> - **FIEL** ✅ — efeito equivalente ao pago para fins de CI.
> - **PARCIAL** 🟡 — coberto por mitigação, mas com janela residual de divergência.
> - **INFIEL** ❌ — diverge hoje e pode causar falha/risco; corrigível com a AÇÃO.
> - **IMPOSSÍVEL** 🧱 — estruturalmente inalcançável na topologia atual (1 VM,
>   1 daemon, 1 disco, 8 runners share-everything). Só muda com expansão de
>   hardware OU mudança de topologia (VM-por-job).
>
> Auditado em **2026-06-16** contra o repo `civm` (branch
> `fix/busy-branch-image-prune-race`) e a box viva `ssh gha-ubuntu-2404`
> (READ-ONLY). Sucede `CI-PARITY-CHECKLIST.md` e `PAID-CI-PARITY-CHECKLIST.md`
> (rascunhos das passadas anteriores); este é o consolidado verificado.
> Hardware atual: [`docs/HARDWARE.md`](docs/HARDWARE.md). A memória mudou para
> `12 GiB` fixos em 05/08/2026; snapshots menores abaixo são históricos.

---

## 1. Propósito (por que esta âncora existe)

A box `civm` existe para rodar o CI do acme (e dos peers) **de graça**, no lugar
dos minutos pagos do GitHub-hosted. A troca é deliberada: custo zero em troca de
isolamento. O risco dessa troca é **"verde-mudo enganoso"** — o CI passa na box
mas falharia (ou já falhou) no pago, ou pior, falha na box por um motivo de
**infraestrutura compartilhada disfarçado de falha de teste** (foi exatamente a
corrida "No such image" que motivou esta sessão).

Este doc é a **régua**: toda mudança de infra do runner, todo workflow novo
docker-heavy, toda decisão de "isso é fiel o suficiente?" se mede aqui. Ele
distingue, sem auto-engano, o que é **estruturalmente impossível** na topologia
atual do que é **achievável com uma ação concreta** — para que a box convirja de
"verde-mudo" para "PARCIAL honesto com janela residual conhecida", que é o
máximo que 1 VM / 1 daemon / 1 disco / 8 runners permite.

---

## 2. Veredito honesto de paridade (parityScore)

**"100% fiel" é estruturalmente impossível** enquanto a box for *share-everything*
(1 VM, 1 kernel, 1 daemon Docker, 1 disco, 1 `/home/emdev` para 8 runners
systemd long-lived). O pago é *share-nothing por job* (VM efêmera nova,
incinerada ao fim). Essa diferença é de **arquitetura**, não de grau — e ela
deriva diretamente em ~metade das dimensões.

**Placar (16 dimensões):**

- ✅ **FIEL: 3** — actions/cache remoto (idêntico), observabilidade (igual ou
  superior), self-upgrade do control-plane (sem análogo no pago; é a força da box).
- 🟡 **PARCIAL: 6** — `$HOME`/FS, rede de portas, secrets, cache local extra,
  versões de tool/OS, failure handling.
- ❌ **INFIEL (achievável): 3** — clean-slate por job, disco, concorrência/admit
  inerte no peer `e2e-tenant-isolation`.
- 🧱 **IMPOSSÍVEL por construção: 4** — daemon Docker isolado, disco dedicado,
  RAM/CPU dedicados, segurança cross-job sem dwell.

**Tradução:** das 16, **4 são teto duro** da topologia (precisam de VM-por-job ou
hardware novo), **3 são INFIEL corrigíveis hoje** sem hardware, e **6 PARCIAL**
podem endurecer. A maior alavanca isolada de fidelidade acionável hoje é o
**flip `CIVM_E2E_RUNNER_AVAILABLE`** (a admissão docker-heavy via `civmctl
admit --weight heavy --exclusive docker` deve cobrir o `web.yml`) somada a fechar o
**R4 aberto no `e2e-tenant-isolation.yml`** (esse SIM roda docker-heavy no pool
genérico sem admissão central).

> **Nota de método (disciplina #3 — âncora ao número real, não ao adjetivo):**
> os valores absolutos de disco/RAM/cache abaixo são **snapshots voláteis**. Entre
> a 1ª passada e esta verificação a box mudou de `15G livre @ 86%` para `35G livre
> @ 67%`, de `18.62 GB images` para `0 images`, de `9.9 GiB RAM` para `7.8 GiB`.
> Isso **não enfraquece** os vereditos — **reforça** a tese central: o estado da
> box é volátil e compartilhado, ao contrário do disco/daemon fresco-por-job do
> pago. Os vereditos estruturais não dependem do snapshot; os números servem só
> de ilustração da grandeza.

---

## 3. Baseline REAL medido (os dois lados)

### Pago — GitHub-hosted `ubuntu-latest` (Ubuntu 24.04, público)

| Recurso | Garantia |
| --- | --- |
| Modelo de execução | **1 VM efêmera NOVA por job**, provisionada do runner-image, **incinerada ao fim**. Nunca reusada. |
| CPU / RAM | 4 vCPU / 16 GB (público). Dedicados ao job. |
| Disco | ~14 GB livres no `/` SSD (+ `/mnt`), **fresco por job**. |
| Docker daemon | **Próprio e vazio** no início. `docker images`/`volume ls` = 0. |
| `$HOME` / FS | Fresco por job. Zero herança. |
| Rede | Egress datacenter (Azure), IP datacenter, sem NAT doméstico, sem firewall do host. |
| Secrets | Injetados no runner efêmero; somem com a VM. Sem persistência cross-job. |
| Cache | `actions/cache` remoto, isolado por chave/escopo. Nada herda entre jobs. |
| Concorrência | Cada job = sua VM. Paralelismo "ilimitado" (do ponto de vista de contenção local). |
| Tools / OS | Runner-image versionada; matriz LARGA (Go 1.22-1.25, Node 20/22/24, Python 3.10-3.14, Ruby, Java, browsers+drivers, AWS/Az/GCP CLI, DBs, Android SDK). Docker 28.0.4. |
| Cleanup / falha | N/A — VM incinerada. Cleanup é grátis e perfeito; falha de infra re-provisiona limpo. |

### civm — a box REAL (`ssh gha-ubuntu-2404`)

> **Hardware definitivo e exato: [`docs/HARDWARE.md`](docs/HARDWARE.md)** (medido 2026-06-23). Os valores
> abaixo são contexto de paridade; números de disco/RAM/CPU devem bater com o HARDWARE.md.

| Recurso | Realidade |
| --- | --- |
| Modelo de execução | **1 VM Hyper-V permanente**; **8 runners systemd** long-lived, NÃO-efêmeros, reusados job a job (acme-org em piloto **ephemeral** — ver §4 #1/#8). |
| CPU / RAM | **Host:** Ryzen 5 3600 (12 threads), **31.9 GB RAM**. **VM/guest:** **12 GiB fixos** (= VMRS aproximado de 12 GiB no V:) / 12 vCPU compartilhadas. Dynamic Memory `7/7,5/12 GiB` foi removida para o guest não variar entre gerações. |
| Disco | **Host V: = SSD dedicado de 119,24 GiB**. **Guest `/` = 37,70 GiB** no VHDX de máximo lógico `40 GiB`. Medido em 30/07/2026: `73,19 GiB` livres no V: com VM Running e `80,50 GiB` após Off/boundary. Ver `docs/HARDWARE.md`. |
| Docker daemon | **1 daemon único** `29.1.3` (overlayfs, containerd snapshotter, `/var/lib/docker`). Snapshot de 30/07/2026: 0 imagens, 0 containers, 0 build cache e **174 volumes inativos / 10,44 GB recuperáveis**. |
| `$HOME` / FS | **`/home/emdev` ÚNICO** compartilhado pelos 8 runners. |
| Rede | `eth0 <LAN_IP>/24` (**LAN doméstica + NAT**) + `tailscale0 <TAILSCALE_IP>` + `docker0`; `iptables FORWARD policy DROP`. **Não** é egress datacenter. |
| Secrets | Entregues ao runner **persistente** (mesmo protocolo do GitHub); `.env`/`.credentials` por runner em disco compartilhado. |
| Cache | `$HOME/.cache/*-acme-$SLOT` **persiste** entre jobs (de propósito — caro reconstruir). |
| Concorrência | Real e contenciosa: 8 runners disputam RAM/disco/daemon; teto físico duro. `admit MaxHeavy=2` + `dockerlock` (onde adotados). |
| Tools / OS | Subconjunto pinado em `internal/specs/specs.go` (~9 tools); **drift real** vs box e vs pago (ver §4 #9). |
| Cleanup / falha | Hooks `job-started`/`job-completed` + timers; best-effort, **NUNCA incinera**. Falha de infra é **herdável** (a corrida que motivou esta sessão). |

8 runners confirmados: `acme`, `acme-org`, `service-a`, `service-b`,
`civm-self`, `service-c`, `service-d`, `peer` (todos `ephemeral=false`, `.runner` sem o campo).

---

## 4. Checklist mestre de paridade (uma linha por dimensão)

| # | Dimensão | O que o PAGO garante | Estado REAL do civm (evidência) | Paridade | Lacuna | Ação para fechar |
| --- | --- | --- | --- | --- | --- | --- |
| 1 | **Efemeridade / clean-slate por job** | VM nova e destruída; zero estado herdado | Runner long-lived; em 30/07/2026 restaram `174` volumes (`171` Compose named) e `10,44 GB` recuperáveis apesar de `Compacted`. | ❌ | Daemon vê o acúmulo do job anterior; oposto do clean slate | `down -v` por-run + remoção scoped no boundary (`emersonbusson/civm#181`). Clean-slate TOTAL = 🧱 sem VM-por-job |
| 2 | **Isolamento de daemon Docker** | Daemon próprio e vazio por job | 1 daemon único `29.1.3`; corrida "No such image" do prune concorrente já corrigida (`7e9cc0d`); SPEC marca daemon-por-runner como **fora de escopo** | 🧱 | Content store / RAM / disco fisicamente compartilhados; daemon-por-runner = isolamento falso | N/A — 🧱 sem VM-por-job. Máximo viável = **admissão docker-heavy** (`civmctl admit --weight heavy --exclusive docker`, #2 da §6) reduz a janela de corrida a ~zero |
| 3 | **Isolamento de disco** | ~14 GB SSD próprio e fresco por job | 1 filesystem compartilhado de `37,70 GiB`; pressão de um job afeta todos; `disk-watchdog` + `HardFailPct` podem rejeitar job (exit 75). | ❌ (frescor é 🧱) | Sem cotas por-runner; floors legados de `40/58 GB` excedem a capacidade atual | `down -v` + serialização + floors medidos (#181/#182). Frescor real = 🧱 sem clean slate (#1) |
| 4 | **Isolamento de FS / `$HOME`** | `$HOME` fresco por job | `/home/emdev` ÚNICO p/ 8 runners; **cache slot por-runner** (`*-acme-$SLOT`) fecha só a pasta de cache, não o `$HOME` inteiro | 🟡 | `$HOME` per-runner (RF-1 forte) segue NO-GO (sem código, SPECv4) | Manter slot; auditar todo caminho que escreve fora do slot; scrub de `_work`/`/tmp` no boundary |
| 5 | **Isolamento de rede / portas** | netns próprio da VM | netns único do host; colisão evitada por `CIVM_PORT_BASE` (bloco de 64, sticky em `port-blocks.json`); janela `[20000,32000)` **abaixo** do ephemeral do kernel (`32768`, confirmado) | 🟡 | Sem netns real, dois jobs ainda compartilham `localhost`; só funciona se o peer **adota** `${CIVM_PORT_BASE}` | `ci-guard` R1/R2/R3 lintam `container_name`/porta literal/`COMPOSE_PROJECT_NAME` ausente. Sem netns = 🧱 |
| 6 | **Secrets** | Injetados por job; VM destruída remove rastro | Mesmo protocolo de runner do GitHub; MAS persistem em `$HOME`/`_work`/disco compartilhado **entre jobs** até cleanup; daemon e caches compartilhados ampliam a janela | 🟡 | Raio de exposição maior (persistência + vizinhos) que no pago | Scrub de `_work`/`/tmp` no `job-completed`; nunca compartilhar secret via env global; runner dedicado p/ workflow com secret sensível; não rodar peer não-confiável co-residente |
| 7 | **Cache (`actions/cache`)** | Serviço remoto do GitHub, isolado por chave | `actions/cache` remoto é **idêntico** (mesmo serviço); o civm adiciona cache **local** por slot (`*-acme-$SLOT`), trimado por idade/cap | ✅ (o remoto) / 🟡 (o local) | Cache local extra pode mascarar bug de cold-cache (passa na box, falha no pago em cache vazio) | Garantir cache key correto nos workflows; rodar cold-cache periodicamente p/ não mascarar bug |
| 8 | **Concorrência / paralelismo** | Cada job = sua VM; paralelismo sem contenção local | 8 runners → ≤8 jobs; `admit MaxHeavy=2` + `cancel-in-progress` (10 workflows). `e2e-tenant-isolation.yml` ainda precisa declarar a admissão no pool genérico. | ❌ (parcial) | RAM compartilhada de 12 GiB; 3+ pesados sem admit podem estourar RAM/swap | Cabear `civmctl admit --weight heavy --exclusive docker` no `e2e-tenant-isolation.yml` (R4 aberto) + validar a rota equivalente no `web.yml`. Paralelismo ilimitado = 🧱 |
| 9 | **Versões de tool / OS** | Runner-image versionada; matriz LARGA; Docker 28.0.4 | Pin estreito em `specs.go` (~9 tools). Box: git `2.43` **BEHIND** pin `2.53`; docker `29.1.3`/compose `2.40.3` **AHEAD** pins `28.0.4`/`2.38.2`; python `3.12.3` BEHIND `3.12.13` (mascarado como `compatible`) | 🟡 | Pins desatualizados; classificador tolera `ahead`/patch como verde mudo; matriz LARGA do pago não coberta | Atualizar `git` na box; re-sincronizar pins (`specs.go`); tratar `Ahead`/patch como **warning visível**; declarar o subset suportado |
| 10 | **Cleanup pós-job** | Implícito (VM destruída) | `job-completed`: mata órfãos (`killWorkRootContainers`), apaga `_work` próprio, prune **só dangling** (`-f`, nunca `-a`/`--volumes` no path concorrente, `hook.go:349-368`); best-effort, nunca falha o job (`DecisionCleanupDegraded`, exit 0) | 🟡 | Volumes nomeados e imagens taggeadas de runs **sobrevivem** (incompleto vs teardown total) | Prune de volumes órfãos por-runId no boundary; manter o prune disciplinado (já feito) |
| 11 | **Failure handling / retry** | Falha de infra → re-spawn transparente | runner-watchdog reinicia runner offline/failed em VM idle; `cancel-in-progress`; classify de infra-falha (`SigImageEvicted`). Falha **mid-job** por vizinho (OOM/disco/prune) NÃO é re-spawn — o job morre | 🟡 | Sem re-spawn transparente mid-job; morte por contenção é real | Backstops fortes (feitos); medir taxa de morte por contenção; manter pares positivo+recusa (Kahneman #13) |
| 12 | **Observabilidade** | Logs/billing na UI do GitHub | `hooks.jsonl`, `host-metrics.json` (V: do host visível ao guest), `civmctl doctor/health/capacity/metrics`, node_exporter | ✅ (até superior) | Minutos/billing self-hosted não aparecem na UI de billing do GitHub | Manter; garantir que toda morte por contenção emita linha rastreável (work_root + razão) |
| 13 | **Segurança cross-job** | VM-por-job = sem dwell; ataque não persiste | **Co-residência real** de jobs de peers distintos no mesmo kernel/daemon/`$HOME` (SECURITY.md §threat model admite explicitamente); só boundary de path/argv validado; `sudo NOPASSWD` escopado em VM perpétua | 🧱 | Dwell cross-job é inerente sem VM-por-job; blast radius cross-peer | N/A — 🧱. Não rodar código de peer não-confiável co-residente; isolar peers sensíveis em label dedicado; não expor secrets a `pull_request_target` |
| 14 | **Concorrência de admissão — adoção pelo peer** | (parte de #8) | `web.yml` e `e2e-tenant-isolation.yml` devem invocar `civmctl admit --weight heavy --exclusive docker --exec` no mesmo payload Docker-heavy. | ❌ | A admissão existe e está testada no civm, mas os workflows ainda precisam adotá-la e ser exercitados em geração limpa. | Envolver os dois caminhos em admissão explícita; R4 rejeita `flock` local, wrapper em outro step e payload pesado após `&&`. |
| 15 | **Self-upgrade do control-plane** | N/A (GitHub gerencia a image) | `civmctl self-upgrade`: rebuild + swap atômico (`os.Rename`), verificado por subcomando determinístico (`version-pins`, não `--help`); binário-alvo intocado em qualquer falha | ✅ | — (força da box, não lacuna) | N/A — é o mecanismo que propaga correções (como `7e9cc0d`) para a box sem scp/dpkg |
| 16 | **Custo / recurso** | Minutos pagos por job (caro em escala) | Grátis (self-hosted) — é a razão de existir da box | ✅ (vantagem deliberada) | — | N/A — o trade-off é trocar isolamento por custo zero, de propósito |

---

## 5. Estruturalmente impossível na box (e por quê)

Estas dimensões **não fecham** sem VM-por-job ou expansão de hardware. Documentá-las
como divergências aceitas (não bugs ocultos) é o oposto do verde-mudo.

| Dimensão | Por que é 🧱 (com o número real) |
| --- | --- |
| **Daemon Docker isolado (#2)** | 1 único `/var/lib/docker` para 8 runners. Daemon-por-runner (rootless dockerd / `DOCKER_HOST`) está **fora de escopo no SPEC** (`docs/specs/multi-project-isolation/SPEC.md`) porque o **content store do containerd, a RAM e o disco continuam fisicamente compartilhados** — seria isolamento de nome, não de recurso. Daria a ilusão de paridade sem o efeito. |
| **Disco dedicado por job (#3)** | 1 filesystem `/` de `37,70 GiB`. O pago dá ~14 GB **frescos e dedicados** por job; a box tinha `14,03 GiB` livres no snapshot, compartilhados, e pode rejeitar job (exit 75) sob pressão. Frescor exige clean-slate, que exige VM-por-job. |
| **RAM / CPU dedicados (#5/#8)** | **12 GiB fixos para a box inteira** < **16 GB POR JOB** do pago. `MaxHeavy=2` mitiga, mas jobs concorrentes ainda disputam a mesma RAM e 12 vCPU. Um job pode OOM onde o pago não OOMaria. Sem RAM dedicada por job, não há paridade. |
| **Segurança cross-job sem dwell (#13)** | O pago incinera a VM ⇒ ataque de um job não persiste para o próximo. A box tem **co-residência real** de peers no mesmo kernel/daemon/`/home/emdev` (admitido no threat model do `SECURITY.md`). Sem VM-por-job, o dwell cross-job é inerente. Mitiga-se com label dedicado e não rodando peer não-confiável, nunca se elimina. |

**Único fechamento real dessas 4:** runner `--ephemeral` por job (clean-slate +
janela-de-comprometimento curta, ao custo de perder o cache persistente) e/ou
**expansão de hardware** (mais RAM/disco) e/ou **VM-por-job**. Tudo o mais é
mitigação, não paridade.

---

## 6. Achievável — o caminho de máxima fidelidade

O "máximo" que o usuário pede, **sem hardware novo**, em ordem de alavancagem. Cada
item com o estado atual VERIFICADO.

1. **Flipar `CIVM_E2E_RUNNER_AVAILABLE=true`** — *estado: pronto, inerte.*
   O `web.yml` do acme JÁ roteia o E2E pesado ao label `civm-e2e` e JÁ envolve o
   bring-up docker-heavy em `civmctl admit --weight heavy --exclusive docker
   --exec` no mesmo comando. A admissão (cgroup + sub-slot Docker box-wide) existe
   e está testada no civm; o workflow ainda precisa adotá-la e ser validado em
   geração limpa.

2. **Fechar o R4 aberto no `e2e-tenant-isolation.yml`** — *estado: gap real.*
   Esse workflow roda `devctl ci up core`/`full` (docker-heavy real) no pool
   genérico `[self-hosted, civm]` **sem admissão explícita**. O `ci-guard` R4
   (`R4-unlocked-docker-heavy`) **rejeita `flock` repo-local como não-proteção**
   (`ciguard.go:260-268`: bare flock não difere a cleanup; foi o blind spot
   original). Envolver o bring-up em `civmctl admit --weight heavy --exclusive docker`
   fecha a janela de corrida do
   content store para este caminho também (a causa da família "No such image").

3. **`compose -p $COMPOSE_PROJECT_NAME down -v` por-run no peer** — *estado: não feito.*
   Único jeito de recuperar volumes/disco do run **na hora** (não no próximo tick
   de cleanup). Fecha #1 (clean-slate parcial), #3 (disco) e #10 (volumes nomeados
   sobrevivem) de uma vez. Os `174` volumes inativos medidos em 30/07/2026 são
   a prova de que falta.

4. **Subir o registry-mirror `:5000`** — *estado: configurado, morto.*
   `/etc/docker/daemon.json` aponta `registry-mirrors: http://127.0.0.1:5000`, mas
   **nada escuta em `:5000`** (`ss`/`curl` vazios) — pulls saem pela LAN doméstica
   ao Docker Hub, sujeitos a rate-limit. Subir o mirror estabiliza pulls (#7/rede)
   e é alta-recompensa/baixo-esforço (já meio-pronto).

5. **Atualizar `git` + re-sincronizar os pins + endurecer o classificador** —
   *estado: drift medido.* git `2.43.0` (default do Ubuntu 24.04, nunca atualizado)
   está BEHIND o pin `2.53.0`; docker/compose AHEAD; python BEHIND patch. Atualizar
   git, re-pinar `specs.go` (docker→29.x, compose→2.40.x) e tratar `StatusAhead` +
   patch-`compatible` como **warning visível** (não verde mudo) fecha #9.

6. **Prune-safety (a corrida "No such image") — JÁ FEITO nesta sessão.**
   `7e9cc0d` removeu `docker image prune -a --filter until=168h` do branch busy do
   cleanup (o `until` casava a data de build do VENDOR, não do pull, apagando image
   recém-baixada debaixo de um sibling em `compose up`). Job cleanup agora só poda
   dangling (`-f`, nunca `-a`), nunca `system prune --volumes` durante job
   (`cleanup.go:208-214`, `hook.go:349-368`). Backstops no acme: `cleanupRunner`
   sem `-af`, `pipefail`, classify `SigImageEvicted`. **Mantido como invariante.**

7. **`cancel-in-progress`** — *estado: feito (10 workflows).* Reduz a contenção
   cancelando runs supersedidos. Não é paridade (o pago não precisa), mas reduz o
   teto de jobs simultâneos competindo por RAM/disco — mantenedor honesto da #8.

8. **(Teto) runner `--ephemeral` por job** — *estado: NO-GO atual.* É o único
   clean-slate verdadeiro (#1/#3/#6/#13), ao custo de re-registro JIT automatizado
   por job e da **perda do cache persistente** que a box quer. Trade-off explícito:
   só vale se o ganho de fidelidade superar o custo de rebuild de cache.

---

## 7. Como usar esta âncora

- **Toda mudança de infra do runner** (hook, cleanup, admit, dockerlock, portblock,
  ciguard, parity, specs) checa contra este doc: a mudança **fecha** uma linha
  INFIEL/PARCIAL, **mantém** uma 🧱 honesta, ou **introduz** uma regressão? Atualize
  a linha correspondente no §4 com o novo estado + evidência `arquivo:linha`.
- **Todo workflow novo docker-heavy** num peer: ele invoca `civmctl admit --weight
  heavy --exclusive docker --exec` no mesmo payload (não `flock` repo-local — R4
  rejeita)? Roteia ao label certo? Faz `down -v` por-run?
  Se não, é uma nova entrada INFIEL — registre aqui antes de mergear.
- **Todo PR que toca paridade de versão** (`specs.go`): re-rode `civmctl parity`,
  atualize a linha #9 com pin vs box real, e trate `Ahead`/patch como warning, não
  verde mudo (Kahneman #13).
- **Regra de ouro (anti-rubber-stamp):** uma linha só vira ✅ FIEL quando o
  **efeito** é equivalente ao pago — não quando "o mecanismo existe". Existência de
  hook/lock/cache ≠ paridade. Se a verificação não puder provar o efeito na box
  viva, a linha fica 🟡 PARCIAL com a janela residual nomeada.
- **As 4 linhas 🧱** (§5) são divergências **aceitas e documentadas**, não dívida
  oculta: revisá-las só faz sentido se a topologia mudar (VM-por-job, daemon
  isolado, mais RAM/disco). Não "alinhe" um número para fingir paridade que a
  arquitetura não entrega.
