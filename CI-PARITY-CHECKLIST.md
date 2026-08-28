# CI-PARITY-CHECKLIST — civm self-hosted vs GitHub-hosted (CI pago)

> **Snapshot histórico, não baseline operacional.** Este documento foi
> sucedido por `PAID-CI-PARITY.md`; hardware atual fica em
> `docs/HARDWARE.md`. Valores de RAM desta auditoria de 16/06/2026 não
> descrevem o padrão atual de `12 GiB` fixos.

> **Âncora adversarial.** Para cada garantia do runner GitHub-hosted (`ubuntu-latest`,
> o "CI pago"), este doc afirma se a box civm self-hosted é **FIEL**, **PARCIAL**,
> **INFIEL** ou **IMPOSSÍVEL** (estrutural), com a evidência REAL (código / saída
> da box) e a **AÇÃO** para fechar a lacuna. Sem rubber-stamp: onde o modelo da box
> não pode ser igual ao pago, dizemos por quê.
>
> Auditado em 2026-06-16 contra o repo `civm` (branch `fix/busy-branch-image-prune-race`)
> e a box viva `ssh gha-ubuntu-2404` (READ-ONLY).
>
> **Legenda do veredito:**
> - **FIEL** — comportamento equivalente ao pago para fins de CI.
> - **PARCIAL** — coberto por mitigação, mas com janela residual de divergência.
> - **INFIEL** — diverge hoje; corrigível com a AÇÃO listada.
> - **IMPOSSÍVEL** — estruturalmente inalcançável na topologia atual (1 VM, 1 daemon,
>   1 disco, 8 runners). Só muda com expansão de hardware OU mudança de topologia.

---

## 0. O modelo REAL dos dois lados (a base de comparação)

### GitHub-hosted (`ubuntu-latest`) — o que o pago garante

| Dimensão | Garantia do pago |
| --- | --- |
| Instância | **VM efêmera nova por job.** Provisionada do image, destruída ao fim. Nunca reutilizada. |
| Isolamento | VM dedicada: kernel, daemon Docker, rede, FS próprios. Zero vizinhos. |
| Disco | ~14 GB livres no `/` SSD + ~60 GB em `/mnt`, **fresco por job**. |
| Daemon Docker | Próprio, vazio no início. `docker images`/`volume ls` = 0. |
| RAM / CPU | 16 GB RAM / 4 vCPU (`ubuntu-latest` padrão), dedicados ao job. |
| Rede | Egress Azure limpo, IP datacenter, sem NAT doméstico, sem firewall do host. |
| Secrets | Injetados pelo runner efêmero, somem com a VM. Sem persistência cross-job. |
| Cache | Só o que `actions/cache` restaura por chave. Nada herda entre jobs. |
| Concorrência | Cada job = sua própria VM. Concorrência ilimitada, sem contenção de recurso. |
| Versões de tool | Image versionada (`runner-images`), idêntica entre jobs do mesmo dia. |
| Cleanup | N/A — a VM é incinerada. "Cleanup" é grátis e perfeito. |
| Falha de infra | Re-provisiona. Um job nunca herda o estado quebrado de outro. |

### civm self-hosted — o que a box REALMENTE é

`ssh gha-ubuntu-2404` (2026-06-16, uptime 2h44, load 7.87 em 12 vCPU):

| Dimensão | Realidade da box |
| --- | --- |
| Instância | **UMA VM Hyper-V persistente.** `uptime` = horas/dias. Reusada por TODOS os jobs. |
| Isolamento | **8 runners** compartilham 1 kernel, 1 daemon Docker, 1 disco, 1 `/home/emdev`. `systemctl` lista `actions.runner.*` × 8 (acme, acme-org, service-a, service-b, civm-self, service-c, service-d, peer). |
| Disco | `/dev/sda2 108G, 88G usado, 15G livre (86%)`. **Não** 128 GB (README desatualizado). Volátil e compartilhado. |
| Daemon Docker | `Server 29.1.3`, **25 images / 18.62 GB**, **106 volumes / 4.34 GB**, **build cache 13.49 GB**. Acumulado entre jobs. |
| RAM / CPU | `9.85 GiB` total / 12 vCPU (foi 7.7 GiB no contexto antigo — **resized**). Dividido por 8 runners. |
| Rede | `eth0 <LAN_IP>/24` (LAN doméstica + NAT), `tailscale0`, `FORWARD policy DROP`. **Não** é egress datacenter. |
| Secrets | Registrados no GitHub repo/org, entregues ao runner **persistente**; o `.env` por runner fica em disco. |
| Cache | `$HOME/.cache/go-build`, npm, yarn etc. **persistem** entre jobs (de propósito — caro reconstruir). |
| Concorrência | Real e contenciosa: load 7.87, jobs disputam RAM/disco/daemon. Serializada por flock-slots. |
| Versões de tool | Pinadas em `internal/specs/specs.go`; **drift real** vs box (ver §9). |
| Cleanup | Hooks `job-started`/`job-completed` + timers (`cleanup`, `disk-watchdog`). Best-effort, NUNCA incinera. |
| Falha de infra | **Herdável**: um job pode envenenar o próximo (a corrida "No such image" que motivou esta sessão). |

**Conclusão da §0:** a diferença não é de grau, é de **arquitetura**. O pago é
*share-nothing por job*; a box é *share-everything entre 8 runners*. Tudo abaixo
deriva disso. A meta "100% fiel" é atingível só nas dimensões onde share-everything
não tem efeito observável pelo job; nas demais, o máximo é PARCIAL com mitigação.

---

## 1. Efemeridade da instância (clean slate por job)

| | |
| --- | --- |
| **Pago** | VM nova por job. Zero herança. |
| **Box** | VM persistente; runner **não-efêmero** (`/home/emdev/actions-runner-acme/.runner` não tem flag ephemeral; reusado job a job). |
| **Veredito** | **INFIEL → PARCIAL** (mitigado por hooks) |

**Evidência:** `.runner` = `{"agentId":22,"agentName":"civm-app",...}` sem `"ephemeral":true`.
O runner roda como serviço systemd `actions.runner.acme-app.civm-app.service` (estado `running` contínuo).
O hook `job-started` (`internal/hook/hook.go:206-250`) faz `reclaimWorkspaceOwnership` (chown do checkout
reusado) + `cleanWorkRoot` sob pressão de disco; `job-completed` (`hook.go:251-273`) limpa `_work`.

**Por que não é FIEL:** o `_work` é limpo, mas **volumes Docker, images e build cache NÃO são por-job**.
`docker volume ls` = 106 volumes órfãos acumulados (`acme-27075821642_postgres_data` … run-IDs antigos).
Um job vê o daemon "sujo" do anterior — o oposto do clean slate.

**AÇÃO p/ fechar:**
1. Runner efêmero real (`config.sh --ephemeral` + re-registro via JIT token por job) **é o único clean-slate verdadeiro**, mas exige re-registro automatizado e quebra o cache persistente que a box quer (trade-off explícito).
2. Alternativa viável sem efemeridade: ao fim de cada job, `docker compose -p $COMPOSE_PROJECT_NAME down -v` no próprio workflow do peer (remove os volumes daquele run). O `cleanup` da box já faz `docker volume prune -f` (órfãos), mas só fora de job-ativo — não substitui o `down -v` por-run.

---

## 2. Isolamento entre jobs (vizinhos)

| | |
| --- | --- |
| **Pago** | VM dedicada. Job não vê nem afeta outro. |
| **Box** | 8 runners, 1 kernel, 1 daemon, 1 disco, 1 home. |
| **Veredito** | **IMPOSSÍVEL** (isolamento real de daemon) → **PARCIAL** (namespacing lógico) |

**Evidência do compartilhamento:** `docker info` → `Name: gha-ubuntu-2404`, `Containers/Images` globais;
um único `/var/lib/docker`. O SPEC `docs/specs/multi-project-isolation/SPEC.md` põe **explicitamente fora de escopo**:
> "dockerd rootless / `DOCKER_HOST` por runner (isolamento real de daemon) — **deferido atrás de gate de
> expansão de disco/RAM**."

**O que a box faz em vez de isolamento real (namespacing lógico):**
- **Identidade por runner** injetada no `.env` (`internal/hook/install.go:173-175`): `CIVM_RUNNER_SLOT`,
  `CIVM_PORT_BASE`, `COMPOSE_PROJECT_NAME`. Confirmado na box: `cat .env` → `CIVM_PORT_BASE=20064`,
  `CIVM_RUNNER_SLOT=acme`, `COMPOSE_PROJECT_NAME=acme`.
- **Blocos de porta disjuntos** por slot (`internal/portblock/portblock.go`): janela `[20000,32000)`,
  64 portas/runner. Box: `/var/lib/civm/port-blocks.json` = 8 slots, bases 20000…20448, disjuntos.
  Acima dos defaults dos peers e **abaixo** da faixa ephemeral do kernel (`/proc/sys/net/ipv4/ip_local_port_range`
  = `32768 60999`) — sem colisão com testcontainers.
- **`ci-guard`** (`internal/ciguard/ciguard.go`) recusa compose com `container_name` fixo (R1), porta-host
  literal (R2), compose sem project-name (R3), e alerta docker-heavy sem admissão explícita (R4).

**Por que continua PARCIAL/IMPOSSÍVEL:** namespacing lógico evita **colisão de nome/porta**, mas NÃO isola:
- o **content store do containerd** (a corrida "No such image": prune de um runner apagava image de outro);
- a **pressão de RAM/disco** (um job OOM/enche-disco afeta vizinhos);
- o **daemon em si** (um `docker system prune` mal-filtrado de um runner atinge todos — já mitigado, ver §6).

**AÇÃO p/ fechar (a melhor possível sem hardware novo):**
- **Admissão docker-heavy** (`internal/admit` + `civmctl admit --weight heavy --exclusive docker --exec`): faz no
  máximo 1 bring-up Docker por vez e impõe cgroup ao payload → reduz a janela de corrida no content store a ~zero.
  **Requer declaração no mesmo comando do peer**; `flock` local ou wrapper em outro step não provam proteção.
- **Runner dedicado `civm-e2e`** (SPEC RF-5 / DT-8): rotear só E2E docker-heavy a 1 runner via
  `runs-on: [self-hosted, civm, civm-e2e]` + flip `CIVM_E2E_RUNNER_AVAILABLE`. **Status: NÃO adotado** —
  `CIVM_E2E_RUNNER_AVAILABLE` só aparece em `docs/specs/.../SPEC.md`, `PRD.md` e `runbooks/MULTI-PROJECT-RUNNER.md`,
  **zero referências em código**. O acme `e2e-tenant-isolation.yml` roda em `[self-hosted, civm]` (pool genérico).

---

## 3. Disco (espaço e frescor)

| | |
| --- | --- |
| **Pago** | ~14 GB `/` + ~60 GB `/mnt`, fresco por job. |
| **Box** | 108 GB total, **15 GB livre (86% usado)**, compartilhado e acumulativo. |
| **Veredito** | **INFIEL → PARCIAL** (gates de disco) |

**Evidência:** `df -h` → `/dev/sda2 108G 88G 15G 86%`. `docker system df` → Images 18.62 GB
(13.58 GB reclaimable), Build Cache 13.49 GB, Volumes 4.34 GB. A `heavy-service:latest` sozinha = **1.87 GB**.

**Mitigações reais na box:**
- `hook.go` gate por `%` (`DefaultPreCleanupPct` dispara cleanup; `DefaultHardFailPct` **rejeita o job** com
  exit 75 — `hook.go:242-245`). É host-aware: lê o snapshot do volume V: do host Hyper-V (`HostDiskFn`,
  `hook.go:211-249`) e rejeita por `host.Blocks()` antes de cair em `PausedCritical`.
- `disk-watchdog` + `EmergencyBypassIdle` (`cleanup.go:86-94,221-233`): quando a box enche durante job ativo,
  faz reclaim seguro (tmp velho + cache trim com floor in-flight) sem esperar idle — correção do incidente
  2026-06-10 (deferiu tudo, disco foi a 0%, sshd travou).

**Por que continua INFIEL:** 15 GB livres compartilhados por 8 runners ≠ ~14 GB **dedicados e frescos** por job.
Dois bring-ups docker-heavy concorrentes (acme E2E ~vários GB de images + volumes) podem cruzar o `HardFailPct`
e **rejeitar um job** (exit 75) — algo que o pago nunca faz. O frescor é impossível sem clean slate (§1).

**AÇÃO p/ fechar:** (a) `compose down -v` por-run no peer (libera volumes do run na hora, não no próximo tick de
cleanup); (b) expandir o disco da VM (única forma de ganhar folga real); (c) manter a lock-serialização (§2) para
nunca ter 2 bring-ups docker-heavy somando disco ao mesmo tempo.

---

## 4. Daemon Docker (estado limpo)

| | |
| --- | --- |
| **Pago** | Daemon próprio, vazio no início do job. |
| **Box** | Daemon único compartilhado, 25 images / 106 volumes / 13 GB de build cache persistentes. |
| **Veredito** | **IMPOSSÍVEL** (vazio por job) → **PARCIAL** (prune disciplinado) |

**Evidência:** `docker info` → 1 daemon (`overlayfs`, containerd snapshotter), live-restore desabilitado.
`docker images` = 25 (mix de tags do acme por run-ID + bases vendor); `docker volume ls` = 106.

**O perigo central já corrigido nesta sessão** (a razão deste doc): o `cleanup` da box rodava
`docker image prune -a --filter until=168h` no branch host-busy. `until` casa a **data de build do VENDOR**,
não o pull — então uma image vendor-antiga recém-baixada (redis/minio/postgres) era apagada **debaixo de um
sibling em `compose up --build`** → "No such image", derrubando o tenant-isolation-smoke. Removido
(`cleanup.go:208-214`). O job cleanup agora só poda **dangling** (`-f`, nunca `-a`) e nunca
`system prune --volumes` durante job (corrompe `docker pull` concorrente — `hook.go:350-366`).

**Por que continua IMPOSSÍVEL/PARCIAL:** o daemon **nunca** está vazio para um job da box — ele sempre vê o
acúmulo dos vizinhos. Isso é fidelidade-zero na dimensão "estado inicial do daemon", mas é **benigno** desde
que os prunes sejam disciplinados (já são) e a serialização exista (§2, ainda não adotada pelo peer).

**AÇÃO p/ fechar:** daemon-por-runner via rootless dockerd / `DOCKER_HOST` (o SPEC marca como NO-GO sob RAM/disco
atuais — isolamento "fake" porque o content store ainda é compartilhado fisicamente). Curto prazo: lock-serialização
+ `down -v` por-run. Médio prazo: gate de expansão de disco/RAM que o SPEC já prevê.

---

## 5. RAM / CPU dedicados

| | |
| --- | --- |
| **Pago** | 16 GB RAM / 4 vCPU dedicados ao job. |
| **Box** | 9.85 GiB RAM / 12 vCPU divididos por 8 runners; load 7.87 medido. |
| **Veredito** | **IMPOSSÍVEL** (dedicado) → **PARCIAL** (admission por slot) |

**Evidência:** `free -h` = `Mem 9.9Gi total, 2.4Gi free, swap 4.0Gi (559Mi usado)`. `nproc` = 12.
`uptime` load `7.87, 10.38, 9.27` — a box JÁ está sob contenção real.

**Mitigação real:** `internal/admit` (`civmctl admit`) limita jobs **heavy** a `DefaultAdmitMaxHeavy = 2` slots
de flock concorrentes (`admit.go` doc + `civm.go:159`); light flui sem slot. Liveness = o próprio flock
(liberado pelo kernel na morte do holder), sem heartbeat. `CheckFn` fail-closed: watchdog Critical/Warn → backoff,
nunca admite.

**Por que IMPOSSÍVEL:** 9.85 GiB **totais** para a box inteira < 16 GB **por job** do pago. Mesmo com MaxHeavy=2,
dois jobs heavy + 6 runners light disputam <10 GiB. Swap em uso já indica pressão. Um job que precise de >5 GiB
sozinho pode OOM onde o pago não OOMaria. **Sem expansão de RAM, não há paridade aqui.**

**AÇÃO p/ fechar:** (a) `admit --exclusive` para os jobs realmente pesados (E2E) — serializa de fato;
(b) expandir RAM da VM (único caminho para o teto do pago); (c) calibrar `HeavyMaxMB` por perfil real medido
(`capacity --json`), não por adjetivo (Kahneman #3).

---

## 6. Cleanup / herança de estado quebrado

| | |
| --- | --- |
| **Pago** | VM incinerada; herança = 0; cleanup grátis e perfeito. |
| **Box** | Cleanup best-effort; estado quebrado **é** herdável. |
| **Veredito** | **IMPOSSÍVEL** (incineração) → **PARCIAL** (backstops fortes) |

**Backstops reais (todos verificados no código):**
- **Hook nunca falha o job que segue**: `job-completed` com cleanup falho → `DecisionCleanupDegraded`, erro
  visível em `hooks.jsonl`, **exit 0** (`hook.go:42-47,259-270`). Supersede o "stays fatal" antigo após o
  incidente 2026-06-10 (work_root leftover deixava Web CI vermelho).
- **Hook só toca o PRÓPRIO `_work`**: `workRoots` sem fallback global (`hook.go:636-667`). Em 2026-06-10 o
  fallback global deletou o checkout de um sibling MID-JOB; removido.
- **Mata órfão antes de deletar root**: `killWorkRootContainers` (`hook.go:462-519`) mata container que ainda
  bind-monta `_work` (stack de job cancelado) — match por path-segment, nunca substring.
- **Cleanup adia sob lock docker-heavy fresco** e reclama só lock **vencido** (`cleanup.go:191-196`).
- **Cleanup não trima cache que um sibling lê** (`hook.go:289-310,342-348`): `cache_trim_deferred_sibling_build`.

**Por que continua IMPOSSÍVEL:** todos os backstops reduzem a probabilidade de herança ruim, mas a herança é
**estrutural** — não há incineração. A própria existência deste doc nasceu de um estado herdado ("No such image").

**AÇÃO p/ fechar:** manter a disciplina; cada novo path destrutivo precisa do mesmo par positivo+recusa
(Kahneman #13 do acme). Único fechamento real = efemeridade (§1), com o custo do cache.

---

## 7. Rede / egress

| | |
| --- | --- |
| **Pago** | Egress Azure limpo, IP datacenter, sem firewall do host, sem NAT doméstico. |
| **Box** | LAN doméstica `<LAN_IP>/24` + NAT + `tailscale0` + `FORWARD policy DROP`. |
| **Veredito** | **INFIEL** (e parcialmente IMPOSSÍVEL) |

**Evidência:** `ip -br addr` → `eth0 <LAN_IP>/24`, `tailscale0 <TAILSCALE_IP>/32`; `iptables` → `FORWARD policy DROP`.
Há um `registry-mirror` configurado (`/etc/docker/daemon.json` → `http://127.0.0.1:5000`) **mas o mirror NÃO está
rodando** (`ss -ltnp` → nada em `:5000`; `curl :5000/v2/_catalog` vazio). Ou seja: pulls saem pela LAN doméstica
para o Docker Hub, sujeitos a rate-limit/latência/saúde da conexão de casa — o pago não tem isso.

**Impacto na fidelidade:** testes sensíveis a IP de origem, geolocalização, rate-limit de provider externo, ou
latência de rede divergem. Pulls podem falhar por rate-limit do Hub (o pago tem cache/peering do GitHub).

**AÇÃO p/ fechar:** (a) **subir o registry-mirror `:5000`** que já está configurado mas morto — fecha a maior
lacuna (pulls estáveis, imunes a rate-limit do Hub, mais rápidos); (b) aceitar que IP/egress doméstico é
**IMPOSSÍVEL** de igualar ao datacenter — documentar como divergência conhecida para testes IP-sensíveis.

---

## 8. Secrets

| | |
| --- | --- |
| **Pago** | Injetados no runner efêmero, somem com a VM. |
| **Box** | Entregues ao runner persistente; `.env` por runner em disco; home compartilhado. |
| **Veredito** | **PARCIAL** (funcional, mas superfície maior) |

**Evidência:** secrets de Actions são entregues pelo serviço do runner em cada job (não persistidos em texto pela
infra do GitHub), mas a **box é persistente e o `/home/emdev` é compartilhado pelos 8 runners** — qualquer step
que vaze um secret para disco/cache/`_work` tem janela de exposição cross-runner maior que no pago (onde a VM some).
`.credentials`/`.credentials_rsaparams` do runner ficam em disco (modo `0600`/`rw-rw-r--`).

**Por que PARCIAL e não FIEL:** o mecanismo de injeção é equivalente, mas o **raio de exposição** de um vazamento
acidental é maior (persistência + vizinhos). Não é uma falha do civm; é consequência do share-everything.

**AÇÃO p/ fechar:** (a) garantir `_work`/cache wipe por-job dos paths que possam reter secret (o hook já limpa
`_work`); (b) nunca compartilhar secret entre runners via env global; (c) preferir `civm-self`/runner dedicado para
workflows que tocam secret sensível.

---

## 9. Versões de tool / OS (drift de paridade)

| | |
| --- | --- |
| **Pago** | Image `runner-images` versionada, idêntica entre jobs do mesmo dia. |
| **Box** | Pins em `internal/specs/specs.go`; box real **diverge** dos pins. |
| **Veredito** | **PARCIAL** (drift medido, alguns BEHIND) |

**Evidência — pin (`specs.go`) vs box real (`ssh ... <tool> --version`):**

| Tool | Pin (`specs.go`) | Box real | Status |
| --- | --- | --- | --- |
| go | 1.26.3 (pref) | `1.26.3` | **FIEL** |
| node | 24.15.0 (pref); `24.14.1` na lista | `v24.14.1` | FIEL (na lista) |
| python | 3.12.13 (pref) | `3.12.3` (system) | **BEHIND** (patch) |
| docker | 28.0.4 | `29.1.3` | **AHEAD** (pin desatualizado) |
| docker-compose | 2.38.2 | `2.40.3` | **AHEAD** (pin desatualizado) |
| gh | 2.89.0 | `2.89.0` | **FIEL** |
| **git** | **2.53.0** | **`2.43.0`** | **BEHIND** (10 minors) |
| jq | 1.7 | `jq-1.7` | **FIEL** |

`ImageVersion` pinada = `20260413.86.1`, `OSVersion` = `24.04.4 LTS` (box confirma `Ubuntu 24.04.4 LTS`).

**Achados adversariais:**
- **`git 2.43.0` na box vs pin `2.53.0`**: a box está **atrás do próprio pin** E provavelmente atrás do
  GitHub-hosted (que ships git recente). `git 2.43` é o default do Ubuntu 24.04 — ou seja, **o git nunca foi
  atualizado para o pin**. Comportamento de `git` sensível a versão (sparse-checkout, `safe.directory`, etc.)
  pode divergir do pago. **INFIEL.**
- **`docker`/`compose` AHEAD**: a box roda 29.1.3 / 2.40.3, mais novo que o pin (28.0.4 / 2.38.2) e mais novo que
  o ubuntu-latest. `parity.go` classifica isso como `StatusAhead` (não falha), mas **ahead também é drift** — um
  bug que só existe no 29.x apareceria na box e não no pago.
- **`python 3.12.3` vs pin `3.12.13`**: patch atrás; a `parity.go` aceita por prefixo de 2 partes
  (`matchesPrefixParts(...,2)` em `compatibleVersion`) → marca `compatible`, mascarando o drift de patch.

**Por que PARCIAL:** existe um mecanismo de paridade (`civmctl parity` / `parity.go` / `specs.go`) que MEDE o drift —
isso já é melhor que nada. Mas (a) os pins estão **desatualizados** vs a box real (docker/compose) e vs o pago (git),
e (b) o classificador `compatible`/`ahead` **tolera** drift que pode mudar comportamento.

**AÇÃO p/ fechar:**
1. **Atualizar `git` na box** para o pin (`2.53.0`) — é a divergência BEHIND mais provável de morder.
2. **Re-sincronizar os pins** (`specs.go`: docker→29.x, compose→2.40.x, git→o que de fato instalar) contra a
   `runner-images` real do dia, e re-rodar `civmctl parity` até `exit 0` honesto.
3. Endurecer o classificador: tratar `StatusAhead` e patch-`compatible` como **warning visível**, não verde mudo,
   para drift não passar despercebido (Kahneman #13: "compatible" ≠ "idêntico ao pago").

---

## 10. Concorrência / modelo de escalonamento

| | |
| --- | --- |
| **Pago** | Cada job = sua VM. Concorrência ilimitada, sem contenção. |
| **Box** | 8 runners disputam recursos; `cancel-in-progress` + flock-slots. |
| **Veredito** | **IMPOSSÍVEL** (ilimitada sem contenção) → **PARCIAL** (admissão + cancel) |

**Evidência:** acme usa `concurrency: cancel-in-progress: true` nos workflows pesados (`ci-router.yml:28-30`,
`e2e-tenant-isolation.yml:33-35`, `security-scans.yml`, etc.). `admit` limita heavy a 2. `dockerlock` serializa
docker-heavy box-wide. Load real 7.87 confirma que a contenção **acontece**.

**Por que IMPOSSÍVEL:** o pago paraleliza criando N VMs; a box tem teto físico (12 vCPU / 9.85 GiB / 1 daemon).
Acima do teto, ou serializa (mais lento que o pago) ou contende (flaky). Não há como ter "concorrência ilimitada
sem contenção" em hardware finito compartilhado.

**AÇÃO p/ fechar:** afinar `MaxHeavy` e `--exclusive` ao perfil medido; rotear o E2E docker-heavy ao runner
dedicado `civm-e2e` (§2) para tirá-lo da disputa do pool genérico. Aceitar latência maior como o custo honesto de
não pagar VMs.

---

## 11. Self-upgrade / consistência do binário de controle

| | |
| --- | --- |
| **Pago** | N/A (sem control-plane próprio; o GitHub gerencia a image). |
| **Box** | `civmctl self-upgrade` rebuild + swap atômico (`os.Rename`), verificado por `version-pins`. |
| **Veredito** | **FIEL** (para o que é — não tem análogo no pago; é a forma de manter a box correta) |

**Evidência:** `internal/selfupgrade/selfupgrade.go` — build verificado por um subcomando determinístico real
(`version-pins`, não `--help`), swap atômico no mesmo dir, binário-alvo intocado em qualquer falha. É o mecanismo
que propaga correções como a deste branch (`fix/busy-branch-image-prune-race`) para a box sem cerimônia de scp/dpkg.

**Sem ação** — é uma força da box, não uma lacuna. Citado para completude.

---

## Resumo executivo — placar de fidelidade

| # | Dimensão | Veredito | Fechável? |
| --- | --- | --- | --- |
| 1 | Efemeridade / clean slate | INFIEL→PARCIAL | Só com runner efêmero (perde cache) OU `down -v` por-run |
| 2 | Isolamento entre jobs | IMPOSSÍVEL→PARCIAL | Lock-serialização + `civm-e2e` (não adotados pelo peer) |
| 3 | Disco | INFIEL→PARCIAL | `down -v` por-run + expandir disco |
| 4 | Daemon limpo | IMPOSSÍVEL→PARCIAL | Prune disciplinado (feito) + serialização |
| 5 | RAM/CPU dedicados | IMPOSSÍVEL→PARCIAL | `--exclusive` + expandir RAM |
| 6 | Cleanup / herança | IMPOSSÍVEL→PARCIAL | Backstops fortes (feitos); fechamento real = efemeridade |
| 7 | Rede / egress | INFIEL/IMPOSSÍVEL | Subir registry-mirror `:5000` (configurado, morto); IP doméstico não-fechável |
| 8 | Secrets | PARCIAL | Wipe por-job + runner dedicado p/ secret sensível |
| 9 | Versões de tool/OS | PARCIAL | Atualizar git, re-sync pins, endurecer classificador |
| 10 | Concorrência | IMPOSSÍVEL→PARCIAL | Afinar admission + runner dedicado |
| 11 | Self-upgrade | FIEL | — |

**Verdade de fundo (sem rubber-stamp):** "100% fiel" é **estruturalmente impossível** enquanto a box for
1 VM / 1 daemon / 1 disco / 8 runners — as dimensões 2, 4, 5, 6 e parte da 3/7 derivam de share-everything e só
fecham de verdade com **efemeridade** (runner descartável por job) ou **expansão de hardware** (mais RAM/disco) ou
**isolamento de daemon** (gate explícito do SPEC). O **máximo atingível hoje, sem hardware novo**, é:

1. **Adotar a admissão docker-heavy no peer** (`admit --weight heavy --exclusive docker`) — fecha a janela de corrida do content store (§2/§4).
2. **`docker compose -p $COMPOSE_PROJECT_NAME down -v` por-run** no peer — recupera disco/volumes na hora (§1/§3).
3. **Subir o registry-mirror `:5000`** já configurado mas morto — estabiliza pulls (§7).
4. **Atualizar `git` + re-sincronizar os pins** e endurecer o classificador de paridade (§9).
5. **Rotear o E2E ao runner `civm-e2e`** (flip `CIVM_E2E_RUNNER_AVAILABLE`, ainda só em docs) — tira o job mais
   pesado da disputa do pool genérico (§2/§10).

Isso leva a box de "verde-mudo enganoso" para "PARCIAL honesto com janela residual conhecida" — o melhor que a
topologia atual permite. As lacunas IMPOSSÍVEL ficam documentadas como divergências aceitas, não como bugs ocultos.
