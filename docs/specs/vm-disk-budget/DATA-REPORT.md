# DATA-REPORT — Orçamento de disco do V: (consumidores medidos)

> SSDV3 fase Discovery. Dados medidos no guest `gha-ubuntu-2404` e no host V:
> em 2026-06-15 (incidente #13). Separa **fato medido** de **constante do código**
> de **inferência**. Diagnóstico central: **Docker > cache** — o maior lever
> reclamável (18GB) não tem teto, enquanto o cache (34GB de cap) nunca morde.

## 1. Topologia e capacidade (Confirmado nos dados)

| Volume | Total | Usado | Livre | %usado |
| --- | --- | --- | --- | --- |
| guest `/` (VHDX dinâmico) | 108GB | 73GB | 30GB | 72% |
| host `V:` (NTFS, hospeda o VHDX) | 120GB | 91GB | 29GB | 76% |

O VHDX é dinâmico: cresce com a escrita do guest e **só encolhe via
`Optimize-VHD` offline** (Stop-VM → compacta → Start-VM), feito pela task
`civm-vhdx-autoreclaim`. Logo o `/` do guest e o `V:` do host estão acoplados —
encher o guest infla o VHDX e drena o `V:`.

## 2. Consumidores do guest `/` (Confirmado nos dados, `docker system df` + `du`)

| Consumidor | Tamanho | Reclamável | Detalhe medido |
| --- | --- | --- | --- |
| **Docker (total)** | **~27GB** | **~18GB** | imagens 14.4GB (8.3 reclaimable) + volumes 4.0GB (4.0) + build-cache 8.9GB (5.6) |
| Cache CI (natural) | ~10GB | — | yarn-tenant 3.2 + yarn-audit 3.2 + go-build 2.4 + ms-playwright 0.6 + yarn-gates 0.6 |
| OS + go/pkg/mod + _work | ~36GB | — | piso fixo (ver §6, número MENOS medido) |
| **Total** | **~73GB** | **~18GB** | bate com o `df` (73GB usado) |

No `V:` o VHDX (~73GB de conteúdo guest) + VMRS (~12GB, snapshot de save-state)
+ overhead NTFS ocupam os 91GB usados.

## 3. Diagnóstico: Docker > cache (o lever nº1)

A assimetria medida é o coração deste orçamento:

- **Docker = 18GB reclaimable, SEM teto.** Os 3 caminhos de prune têm
  agressividade DISTINTA, e nenhum solta as imagens TAGGED-unused fora do caminho
  idle:
  - **hook job-completed** (`internal/hook/hook.go:321-324`): `buildx prune
    --filter until=24h` + `image prune -f` (SÓ dangling, sem `-a`) +
    `container prune -f` + `volume prune -f`. **Nunca** remove imagem tagged
    unused — o comentário hook.go:309-317 prova que o `-a` foi removido de
    propósito (um `image prune -a --filter until=` casa por CREATED-date e
    deletaria uma vendor image recém-puxada por sibling job mid-`compose up`).
  - **disk-watchdog IDLE** (`internal/cleanup/cleanup.go:506` `dockerPrune`):
    `docker system prune -af --volumes` — o agressivo, mata imagens TAGGED unused
    + volumes + build-cache inteiro. Só roda após `ensureIdle` (cleanup.go:198) E
    used>60%.
  - **disk-watchdog BUSY** (`cleanup.go:525` `dockerPruneSafe`): `image prune -f`
    (dangling) + `builder prune -f --filter until=24h`. Pega uma FRAÇÃO dos 18GB.
- **Cache = 10GB natural, 34GB de cap.** Os caps backstop somam **34GB**
  (yarn 12 + go-build 12 + npm 3 + pnpm 5 + golangci 2,
  `internal/civm/civm.go:74-85`) contra um working-set natural de ~10GB. O trim
  atômico (cachetrim-yarn-atomic SPECv2) impede crescimento real → o cap **nunca
  morde** o working-set. Há **24GB de teto morto** (34−10) que o working-set nunca
  toca.

**Conclusão numérica:** o cache não é generoso demais isoladamente (o teto morto
é inerte por design). O problema é que **o maior consumidor reclamável — Docker,
18GB — é o único de 2 dígitos SEM teto**, e seus 8.3GB de imagens tagged-unused só
saem no caminho idle (2), que pode nunca disparar num box ocupado.

## 4. Os 3 caminhos de prune reconciliados com os 18GB (Confirmado no código)

| Fatia reclaimable | Tamanho | Quem solta | Quem NÃO solta |
| --- | --- | --- | --- |
| Imagens dangling | (parte dos 8.3) | hook + busy + idle | — |
| Imagens TAGGED unused | (maioria dos 8.3) | **só idle** `system prune -af` | hook, busy |
| Volumes anônimos | 4.0 | hook `volume prune -f` + idle | busy |
| Build-cache <24h | (parte dos 5.6) | ninguém (preservado) | todos |
| Build-cache ≥24h | (parte dos 5.6) | hook+busy `buildx until=24h` + idle | — |

A fatia presa no box ocupado = **imagens TAGGED unused** (a maioria dos 8.3GB):
o hook e o caminho busy só fazem `image prune -f` (dangling). Esse é o lever
exato que falta.

## 5. Constante órfã — o lever já nomeado e abandonado (Confirmado no código)

`internal/civm/civm.go:90`:

```go
DefaultDockerImagePruneFilter  = "until=168h"  // comentado "imagens unused < 7 dias"
```

`grep -rn "DefaultDockerImagePruneFilter" .` → **ZERO call-sites** (só a
definição). É **código morto**: a intenção era prunar imagens tagged-unused com
>7d via `image prune -a --filter until=168h`, mas nunca foi fiada. Esse é o lever
exato que o caminho busy precisa — um filtro por CREATED-date >7d só atinge vendor
images antigas, nunca uma recém-puxada por sibling job (resolve o risco de
hook.go:309-317 sem reintroduzi-lo).

## 6. Limites honestos dos números (disciplina #13)

1. **OS+_work+go-mod = 36GB é o número MENOS medido.** Vem da subtração
   (73GB usado − 27 docker − 10 cache ≈ 36 resto), não de um `du -sh
   ~/go/pkg/mod _work /usr` direto. Se a árvore do PR variar, esse piso flutua.
   Travar com um `du` real por categoria antes de creditar o piso fixo.
2. **18GB reclaimable é UMA medição (2026-06-15), não série.** O p100 de docker
   reclaimable de uma rajada que faz `compose up --build` de N serviços pode ter
   o working-set ATIVO (não-reclaimable) maior — aí o teto não pode ser imposto
   sem matar o job (é F3, §7).
3. **A maioria dos 8.3GB de imagens é tagged-unused>7d — inferido**, não medido
   por idade. O hook já roda `image prune -f` a cada job-completed, então o
   reclaimable residual tende a ser tagged, não dangling. Validar com
   `docker image ls --filter dangling=false --format '{{.Size}} {{.CreatedSince}}'`
   no guest antes de creditar o lever R1.
4. **VMRS=12GB é o snapshot de save-state; medido `vmrs_release=8.02GB`**
   (civm.go) sugere que parte é liberada no Stop-VM do reclaim → 12 é
   conservador, favorece a segurança do orçamento.

## 7. F3 — o limite estrutural (Confirmado em docs)

Mesmo com Docker no teto, o working-set ATIVO de uma rajada concorrente pesada
(imagens em uso por containers vivos + build de N serviços) pode exceder os 108GB
do guest. Nenhum prune compacta dado em uso (`dockerPruneSafe` documenta isso,
cleanup.go:520; `cacheTrim` blinda 24h via MinProtect, civm.go:69). Esse é o
limite de **hardware** (F3) que `host-volume-reclaim-liveness/PRD.md` declara
honestamente fora de escopo. Este orçamento não resolve F3 — só prova com número
ONDE F3 começa (§SPEC) e que abaixo disso a folga-alvo é garantida.
