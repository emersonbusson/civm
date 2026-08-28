# DATA-REPORT — CI efêmero clean-slate (a raiz é o estado persistente compartilhado)

> SSDV3 fase Discovery. Slug: `ephemeral-clean-slate-ci`. Repo: `civm`.
> Dados medidos no guest `gha-ubuntu-2404` e no host `V:` em 2026-06-15
> (incidente #13). Separa **fato medido** de **constante do código** de
> **inferência**. Diagnóstico central, em uma frase: **a corrupção que custou 4
> camadas não veio do Docker (content-addressed, nunca corrompeu) — veio de
> caches de FILESYSTEM MUTÁVEL no `$HOME` COMPARTILHADO entre 8 runners
> persistentes, escritos/trimados concorrentemente. O estado persistente
> compartilhado é a raiz; o resto é sintoma.**

## 1. Topologia e capacidade (Confirmado nos dados)

UMA VM Hyper-V `gha-ubuntu-2404`, host residencial (não datacenter):

| Recurso | Medido |
| --- | --- |
| vCPU | 12 |
| RAM do SO (guest) | 7 GB + 4 GB swap |
| disco guest `/` (VHDX dinâmico) | 108 GB |
| host `V:` (NTFS, hospeda o VHDX) | 120 GB |
| runners self-hosted | 8, de 7 projetos, **todos user `emdev`, mesmo `$HOME`** |

| Volume | Total | Usado | Livre | %usado |
| --- | --- | --- | --- | --- |
| guest `/` (VHDX dinâmico) | 108 GB | 73 GB | 30 GB | 72% |
| host `V:` (NTFS) | 120 GB | 91 GB | 29 GB | 76% |

O VHDX é dinâmico: cresce com a escrita do guest e **só encolhe via
`Optimize-VHD` offline**. Logo o `/` do guest e o `V:` do host estão acoplados —
encher o guest infla o VHDX e drena o `V:`.

## 2. A raiz medida: 1 `$HOME` compartilhado por 8 runners (Confirmado nos dados)

A causa medida da saga é estrutural, não de um cache específico:

- 8 runners (`actions.runner.*`), 7 projetos, **mesmo user `emdev`, mesmo
  `$HOME`** → o working-set de cada job é escrito num filesystem **mutável,
  compartilhado e persistente** entre jobs concorrentes.
- O hook `internal/hook/hook.go` é prova viva disso: o `workRoots()` (linha
  590-621) **proíbe** o fallback global porque em 2026-06-10 um hook job-started
  com env degradado varreu TODOS os `/home/*/actions-runner*/_work` e **apagou o
  checkout de um runner sibling MID-JOB** (civm#117, 20:12:44Z). O comentário no
  código é a confissão: "Empty env → no roots → cleanup no-op (fail-safe)".
- O `cleanWorkRoot` (hook.go:335) **não** faz wipe em job-started — ele
  **preserva** o `GITHUB_WORKSPACE` ativo e o `_temp` justamente porque o `$HOME`
  é compartilhado e um wipe mataria o sibling. Esse compromisso só existe por
  causa do estado compartilhado.

## 3. Consumidores do guest `/` (Confirmado, `docker system df` + `du`)

| Consumidor | Tamanho | Reclamável | Detalhe medido |
| --- | --- | --- | --- |
| **Docker (total)** | **~27 GB** | **~18 GB** | imagens 14.4 GB (8.3 reclaimable) + volumes 4.0 GB (4.0) + build-cache 8.9 GB (5.6) |
| Cache CI (natural) | ~10 GB | — | yarn-tenant 3.2 + yarn-audit 3.2 + go-build 2.4 + ms-playwright 0.6 + yarn-gates 0.6 |
| OS + go/pkg/mod + _work | ~36 GB | — | piso fixo (número MENOS medido, ver §8) |
| **Total** | **~73 GB** | **~18 GB** | bate com o `df` (73 GB usado) |

## 4. A saga das 4 camadas reconciliada com a raiz (Confirmado no código)

As 4 camadas e o per-PR lock existem TODAS para conter a consequência de um
working-set mutável compartilhado. Cada uma aponta para a mesma raiz:

| Camada | Arquivo | O que protege | Por que existe (a raiz) |
| --- | --- | --- | --- |
| backstop cap family-wide | `internal/cachetrim` + `civm.go:74-85` | cap por-família dos caches nomeados | os workflows apontam GOCACHE/yarn p/ dirs nomeados (`go-build-acme-services` chegou a 13 GB num cap de 5) que cresciam SEM LIMITE no `$HOME` compartilhado |
| trim atômico | `cachetrim` PackageDepth/WipeWhole | trim que não corrompe pacote parcial | yarn v1 (dir-pacote) e go-build (par `-a`/`-d` ref-cruzado) **corromperam sob trim concorrente** no FS mutável |
| hooks job-started/completed | `internal/hook/hook.go` | chown não-destrutivo + wipe gated por disco | leftover root-owned de Docker-as-root no `_work` compartilhado; wipe não pode ser total senão mata sibling |
| gate self-heal | `host-volume-reclaim-liveness` | reclaim não morre/starva | VHDX inflado pelo working-set compartilhado drena o `V:` |
| per-PR lock | `internal/dockerlock` | serializa docker-heavy box-wide | daemon Docker compartilhado entre runners; lock "só para contornar o box PERSISTENTE" |

**Conclusão:** todas as 4 camadas + lock são tratamento de sintoma. A raiz única
é o `$HOME`/FS/daemon compartilhado e persistente entre 8 runners.

## 5. Docker NÃO está na lista de corrupção (Confirmado no código + docs)

O Docker é content-addressed (layers por digest SHA, refcount no daemon).
Compartilhar a store entre runners é **seguro por construção** — nunca corrompeu.
A prova no próprio código:

- `cleanup.go:520` (`dockerPruneSafe`) documenta que prune `-f` (sem `-a`, sem
  `--volumes`) **nunca remove um recurso que um build vivo segura** — é
  refcount-safe, roda HOJE no ramo host-busy sem idle guard (issue #70).
- `setup-registry-cache.sh:11` já prova o padrão certo localmente: o `registry:2`
  pull-through é content-addressed e **sobrevive a `docker prune`** (volume
  nomeado + restart=always + imagem tagged em uso).

Logo: **incluir Docker no clean-slate é decisão de ESPAÇO (apaga ~18 GB
efêmeros), não correção de corrupção.** Misturar os dois faria um futuro operador
"curar" um problema de Docker que nunca existiu.

## 6. O custo de egress que mata o `actions/cache` puro (Confirmado em docs)

O insight do user ("CI pago = managed cache content-addressed") está **certo no
mecanismo**: `actions/cache` trata cache como BLOB content-addressed
(chave = hash do lockfile), verificado por integridade, baixado no início do job
e re-subido no fim — cada job recebe working-set FRESCO e VERIFICADO, nunca um
diretório mutável compartilhado. Isso elimina por construção as 2 fontes de
corrupção. **PORÉM o backend do GitHub é inviável aqui:**

- `actions/cache` num runner self-hosted **não** usa disco local — sobe/baixa
  para o Azure Blob hospedado do GitHub **pela REDE a cada job** (GH community
  #18549 + docs).
- Working-set medido a subir/baixar por job: yarn ~6.4 GB (tenant 3.2 + audit
  3.2) + go mod/build ~5.7 GB + playwright ~0.6 GB ≈ **~13 GB de rede por job**.
- Um uplink residencial **não** tem os ~145-200 MiB/s de DC. À ~145 MiB/s
  (otimista, só DC) seriam ~90 s só de I/O de cache/job; no uplink de casa,
  minutos — **mais lento que recompilar**. O egress, não o storage, é o matador.

## 7. Aritmética de RAM — o teto de concorrência é RAM-bound, não lock-bound (Confirmado no código)

| Número | Valor | Fonte |
| --- | --- | --- |
| RAM total do SO | 7 GB (+4 GB swap) | medido #13 |
| `DefaultAdmitMaxHeavy` | 2 | `civm.go:152` |
| `DefaultAdmitHostReserveMB` | 2048 | `civm.go:155` |
| MemoryMax efetivo/heavy | (7168−2048)/2 = **2560 MB** | derivado |
| pico RSS de job pesado acme | ~2.0-3.5 GB | inferido (go test -race testcontainers / compose up) |
| 8 runners × ~0.3 GB RSS | ~2.4 GB | inferido |

**Consequência dura:** o gargalo deste box é RAM (7 GB), não lock nem disco.
Remover o per-PR lock deixa 2 jobs heavy rodarem concorrentes SEM corromper, mas
**continua 2** — só a RAM sobe esse teto. O efêmero resolve **corrupção**
(estado), não **pressão** (RAM). São eixos ortogonais; o cap de `admit` fica.

**Por isso a VM-snapshot-reset por job é matematicamente inviável:** 8 jobs
concorrentes ⇒ 8 VMs; mesmo num piso punitivo de 1 GB/VM (que não roda Go+Docker
CI), 8×1 GB = 8 GB > 7 GB de RAM total. Realista 2-4 GB/VM ⇒ 16-32 GB ⇒ falta
2-4×. Descartada por ARITMÉTICA, não por preferência.

## 8. Limites honestos dos números (disciplina #13)

1. **OS+_work+go-mod = 36 GB é o número MENOS medido.** Vem da subtração
   (73 − 27 docker − 10 cache ≈ 36), não de um `du -sh` direto por categoria.
   Trava com `du` real antes de creditar.
2. **18 GB reclaimable é UMA medição (2026-06-15), não série.** É a ESTIMATIVA do
   `docker system df`, não o efeito; o efeito real só se prova lendo
   `parseTotalReclaimed` (cleanup.go:610) após o prune. O working-set ATIVO de uma
   rajada `compose up --build` de N serviços pode ser maior (F3, §9).
3. **Egress residencial: número de banda não medido neste box.** Os ~145 MiB/s
   vêm de docs de DC Azure; o uplink real de casa não foi cronometrado. A
   conclusão "egress mata" vale pelo MECANISMO (re-subir ~13 GB/job × N PRs × 8
   runners), não por um benchmark local.
4. **Este checkout roda numa VM de 15 GB/1 TB (WSL2 dev), NÃO no box-alvo de
   7 GB/120 GB.** Todos os números de RAM/disco do alvo vêm do prompt medido (#13)
   e do código (`civm.go`), não desta máquina. A feasibilidade final tem de ser
   confirmada NO guest `gha-ubuntu-2404` (POC de 1 PR antes de aposentar o lock).

## 9. F3 — o limite estrutural que o efêmero NÃO resolve (Confirmado em docs)

O working-set ATIVO de uma rajada concorrente pesada (imagens em uso + build de N
serviços) pode exceder os 108 GB do guest / 120 GB do `V:`. Nenhum reclaim
compacta dado em uso; nenhum efêmero faz caber um working-set oversized. Esse é o
limite de **hardware** (F3) que `host-volume-reclaim-liveness/PRD.md` declara fora
de escopo. O efêmero impede a MORTE (corrupção, death-spiral), não a PRESSÃO.
Abaixo de F3, a recusa de job no piso crítico (`hook.go` exit 75) permanece o
fail-safe correto (#15/#16).
