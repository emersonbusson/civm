# SPECv2 — Cache trim atômico por pacote (yarn)

> SSDV3 PASSO 2.5 (auditoria adversarial) → SPEC ativo. Refina o SPEC.

## Achados da auditoria 2.5

### A0 — Escopo: só o yarn v1? Análise por package manager

Trim por arquivo corrompe qual cache? Só os que guardam um pacote como **diretório
multi-arquivo** lido de forma estrita. Mapa:

| Cache                              | Unidade no disco        | Remoção parcial                                       |
| ---------------------------------- | ----------------------- | ---------------------------------------------------- |
| yarn v1 (`.cache/yarn/v6/npm-*/`)  | diretório multi-arquivo | **corrompe** — `--frozen-lockfile` dá ENOENT, não re-busca |
| yarn berry (`.yarn/cache/*.zip`)   | 1 zip                   | seguro (zip é atômico)                               |
| npm (`_cacache/content-v2/...`)    | 1 blob por hash         | seguro — content-addressed, re-busca o que falta     |
| pnpm (`.pnpm-store/v3/files/...`)  | 1 arquivo por hash      | seguro — content-addressed, re-busca                 |
| go-build / golangci                | par `-a`+`-d` ref cruzada | **corrompe** — `-a` órfão → `go vet` "can't import facts" |

**Dois** caches são vulneráveis, por razões diferentes (não só o yarn — correção do
incidente gates 2026-06-15). **yarn v1**: o pacote é um diretório e o yarn assume
que um diretório presente está completo → atômico por pacote (PackageDepth).
**go-build/golangci**: cada entrada é um par `<actionID>-a` + `<outputID>-d` ligados
por hash de conteúdo, e o `-d` pode viver em OUTRO diretório de prefixo — remover
qualquer um orfana a entrada e o `go vet` (modo "vet only, no build") quebra com
"can't import facts ... no such file or directory". Como as refs cruzam prefixos,
nem por arquivo nem por diretório é seguro → só o wipe do dir inteiro acima do cap é
atômico (WipeWhole). npm/pnpm são content-addressed (cada blob é uma unidade
completa) — uma ausência é detectada por integridade e re-baixada → seguros por
arquivo. (O acme usa yarn v1 + go-build + golangci; não há `_cacache`/`.pnpm-store`
no guest.)

**Generalização (robustez a outros managers):** três modos, escolhidos por cache —
por arquivo (default, content-addressed); `PackageDepth=N` (pacote = diretório,
agrupa nos N primeiros segmentos; yarn v1 = 2); e `WipeWhole` (refs cruzadas
opacas, wipe do dir inteiro como backstop; go-build/golangci, cap generoso para o
wipe ser raro). Um manager futuro entra escolhendo o modo: profundidade fixa →
PackageDepth; refs cruzadas ou profundidade variável (ex.: grupo Maven) → WipeWhole.

### A1 — `.yarn/cache` (yarn berry) é zip de arquivo único

A família yarn casa `.cache/yarn*` (v1, pacote = diretório) **e** `.yarn/cache`
(berry, pacote = `<pkg>.zip`, arquivo único). Atômico-por-dir num cache berry
seria errado.

**Resolução:** sem caso especial. O glob de pacotes é `<root>/*/*` (profundidade
2). No berry os zips estão em profundidade 1 → o glob acha zero dirs de pacote →
caem no coletor de **arquivos soltos** → trim por arquivo. Um zip é unidade
atômica, então trim por arquivo é seguro nele. `DirAtomic` degrada para modo
arquivo sozinho quando não há dir de pacote. Vale para v1 (dir) e berry (zip).

### A2 — disk-watchdog trima o working-set no meio de um install (corrige #13)

A auditoria 2.5 marcou isto como resíduo BENIGNO ("no pior caso re-baixa"). A
validação ao vivo (#1155: jobs `web`, `tenant-isolation-smoke`, `Yarn audit`)
provou o contrário — #13, subestimei. O disk-watchdog (timer 8min + o
EmergencyBypass do #117 a ≥75%) dispara NO MEIO de um `yarn install`: remove (mesmo
atômico, o pacote inteiro) o pacote que o yarn está escrevendo, o yarn re-fetcha,
e a corrida entre o re-fetch e a leitura seguinte deixa o `.yarn-metadata.json`
parcial → `ENOENT`. Sempre o mesmo pacote (`@ai-sdk/gateway`), porque o timer cai
num ponto determinístico da ordem do lockfile. O self-heal (clean+retry) **não
vence**: o disk-watchdog re-corrompe o retry.

**Resolução — backstop cap (a filosofia certa para cache regenerável).** O
cache-trim deixa de enforçar um cap apertado contínuo e vira BACKSTOP. Com cap
generoso (yarn 3→12GB; go-build já 12GB), o working-set (~0.84GB/dir yarn,
~2.2GB/dir go-build) fica sempre SOB o cap → `TrimByAge` é no-op durante o job
(`total <= MaxBytes`), inclusive sob o EmergencyBypass → o disk-watchdog NUNCA toca
o working-set em escrita → fim do race, em TODOS os jobs de uma vez. O trim
(atômico/WipeWhole) só age no crescimento descontrolado; o alívio de disco sob
pressão vem de docker/`/tmp` e, no piso, da recusa de job (exit 75). Conserta a
FONTE (não cada job — não-whack-a-mole) e preserva o EmergencyBypass do #117.

### A3 — Glob `*/*` em `.tmp`

`.cache/yarn-x/.tmp/<file>` casaria `*/*`. São transitórios de install; removê-los
atomicamente é inócuo. Sem ação.

## Decisão final (escopo do IMPL)

- **RF-1** (core, civm): `Cap.PackageDepth` (0 = por arquivo; N = por diretório de
  pacote, agrupando nos N primeiros segmentos) + `collectUnits` agrupando por
  pacote. yarn v1 = `PackageDepth=2`; berry/npm/pnpm = 0; dois-passes reaproveitado.
- **RF-3** (civm): `Cap.WipeWhole` para go-build/golangci (refs cruzadas): acima do
  cap, RemoveAll do dir inteiro (atômico). Cap do go-build subido 5→12GB para o
  wipe ser backstop, não o working-set normal (~2.2GB/dir).
- **RF-6** (core, civm — fix do A2): cap de cache regenerável é BACKSTOP, não cap
  contínuo. yarn 3→12GB (go-build já 12GB). O working-set fica sob o cap → o
  disk-watchdog/EmergencyBypass nunca o trima durante um job → elimina o race.
- **RF-5** (superseded por RF-6): reverter `yarn-acme-audit` virou desnecessário —
  com o backstop cap, 4 dirs yarn (3GB/dir) ficam todos sob o cap.
- **RF-4** (validação): rebuild + deploy civmctl → limpar caches → re-run dos jobs
  `web`/`gates`/`tenant-isolation`/`audit` do #1155 todos verdes.

## Resíduos aceitos (documentados, #16)

- Corrupção por escrita concorrente (2 runners no mesmo dir, mesmo `$HOME`) — fora
  do escopo do trim; mitigada por dirs per-workflow + `--mutex` e, em última rede,
  pelo self-heal do gate (clean+retry no devctl/ci-router). Prevenção total =
  cache per-runner (`CIVM_RUNNER_SLOT` no path) — follow-up.
