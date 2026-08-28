# SPEC — Orçamento holístico de disco do V:

> SSDV3 PASSO 2. Traduz `PRD.md` em: o orçamento agregado com prova numérica
> (RF-1), o docker-prune endurecido com flags concretas e seguro-durante-job
> (RF-2), a correção do sentinel `threshold-pct` (RF-3) e a reconciliação dos caps
> (RF-4). Links Kahneman nos passos críticos.

## Princípio de design

O orçamento é **documento + 2 patches mínimos**, não mecanismo novo. RF-2 ATIVA
uma constante já definida e abandonada (`DefaultDockerImagePruneFilter`,
civm.go:90) — zero conceito novo, só termina uma decisão já registrada. RF-3 muda
1 arg do invoke. RF-1/RF-4 são a prova numérica. Nenhum reclaim, trim ou lock
novo é criado.

## RF-1 — O orçamento agregado (prova numérica)

### Folga-alvo (thresholds distintos, #15)

| Threshold | GB livre V: | %usado (disco 120G) | Reusa | Ação |
| --- | --- | --- | --- | --- |
| WARN | ≥30GB | ~75% | `DefaultHostVolumeWarnFreeGB=30` (civm.go:97) | reclaim deve agir |
| CRIT | <10GB | ~92% | `DefaultHostVolumeCritFreeGB=10` (civm.go:98) | recusa job (exit 75) |

Banda WARN→CRIT = **20GB** = o espaço para o reclaim (autoreclaim host-side +
disk-watchdog guest-side) agir antes da recusa. Folga-alvo = WARN = **≥30GB**.

### Conta de pior caso (guest 108GB)

Teto agregado dos consumidores quando TODOS maximam (números do `DATA-REPORT.md`):

```
cache @cap (34) + docker steady-state pós-system-prune (~9 working images, 0 reclaimable)
  + OS/_work/go-mod (~36) + (VMRS vive no V:, não no guest /)
= 34 + 9 + 36 = ~79GB usado no guest /  →  ~29GB livres
```

Bate com os 30GB livres medidos. O guest tem folga; o gargalo é o **V:**, onde o
VHDX (conteúdo guest) + VMRS competem.

### Conta de pior caso (host V: 120GB) — o gargalo real

```
VHDX (conteúdo guest ~79 no pior caso pós-prune) + VMRS (12) + overhead NTFS (~+)
```

Se o guest crescer ao máximo SEM reclaim, o VHDX infla até ~108 (todo o `/`) +
VMRS 12 = 120 → 0 livre → CRIT. A folga-alvo de 30GB no V: exige que o reclaim
mantenha o conteúdo reclamável fora do VHDX. **É exatamente por isso que o lever
nº1 (Docker, 18GB reclamável) precisa de teto:** soltar os 18GB do guest encolhe
o VHDX em até 18GB no próximo `Optimize-VHD`, devolvendo a folga ao V:.

**Prova:** com Docker reclamado (caminho RF-2 + idle), o conteúdo guest cai de
~73 para ~55GB; VHDX ~55 + VMRS 12 = 67 → **53GB livres no V:** ≫ folga-alvo 30.
Sem reclamar (Docker preso a 18GB), o VHDX fica a ~73 + VMRS 12 = 85 → 35GB livres
— ainda acima do WARN, mas uma rajada que adicione >5GB cai sob 30. **O teto de
Docker é o que mantém a folga determinística.**

> **Kahneman #13/#15:** o orçamento é número medido (34 dos caps, 18 do docker df,
> 30/10 do WARN/CRIT), não adjetivo. WARN e CRIT são distintos por construção.

## RF-2 — Docker-prune endurecido (fia a constante órfã, seguro-durante-job)

### Patch (1 linha funcional em `dockerPruneSafe`, cleanup.go:525)

O caminho BUSY hoje só faz `image prune -f` (dangling). Endurecer para soltar
imagens TAGGED-unused >7d:

```go
// internal/cleanup/cleanup.go — dockerPruneSafe (caminho host-busy)
// ANTES:
//   images, err := opts.RunFn(ctx, "docker", "image", "prune", "-f")
// DEPOIS (fia DefaultDockerImagePruneFilter="until=168h", civm.go:90, GAP-2):
images, err := opts.RunFn(ctx, "docker", "image", "prune", "-a", "-f",
    "--filter", civm.DefaultDockerImagePruneFilter)
```

### Por que é seguro durante um job ativo (RNF-1, #13)

- `-a --filter until=168h` casa por **CREATED-date** (data de build da imagem),
  não pull-date. Só atinge vendor images com >7 dias de idade — nunca uma
  recém-puxada por sibling job mid-`compose up` (resolve o risco hook.go:309-317
  SEM reintroduzi-lo).
- `image prune -a` **exclui imagens em uso** por qualquer container (rodando ou
  parado referenciado) por definição do docker — não toca recurso de build/job
  ativo.
- `dockerPruneSafe` já roda sem idle-guard por design (cleanup.go:520, issue #70):
  o filtro de idade + a exclusão de imagens-em-uso são a garantia, não o idle.

### O que NÃO muda (RNF-1)

- **hook job-completed (hook.go:321-324) intocado** — permanece `image prune -f`
  (dangling) para ser sibling-safe (o comentário hook.go:309-317 fica válido).
- **caminho idle `system prune -af` (cleanup.go:506) intocado** — continua atrás
  de `ensureIdle` (cleanup.go:198) + lock docker-heavy (cleanup.go:192).
- **`builder prune until=24h` mantido** no `dockerPruneSafe` (build-cache ≥24h).

> **Kahneman #13:** o EFEITO (imagem tagged >7d sumiu) é a prova, não o arg. Ver
> validação por integration abaixo.

## RF-3 — `threshold-pct` semanticamente correto (1 arg no invoke)

### Patch (`civm-vhdx-autoreclaim.ps1:412`)

```powershell
# ANTES: $prune = Invoke-Guest -RemoteCommand 'civmctl disk-watchdog --threshold-pct=0 --execute'
# DEPOIS: 1 é o mínimo válido (diskwatchdog.go:138); força cleanup sempre que used>1%
$prune = Invoke-Guest -RemoteCommand 'civmctl disk-watchdog --threshold-pct=1 --execute'
```

`Check` reseta `0→DefaultWatchdogThresholdPct(60)` (diskwatchdog.go:135-136); `1`
escapa o reset e satisfaz o range `[1,99]` (diskwatchdog.go:138), forçando o prune
sempre que used>1% — o intent real do "pruna antes de desistir" do RF-3 do
host-volume-reclaim-liveness. Interação benigna hoje (a 72% used o cleanup já
roda), mas a semântica de `=0` era enganosa; `=1` a torna correta.

> **Nota:** este patch é COMPLEMENTAR ao guest-prune do
> `host-volume-reclaim-liveness` RF-3 — só corrige o arg do MESMO invoke, sem
> tocar a lógica de duas fases nem o Optimize-VHD.

## RF-4 — Reconciliação dos caps (preserva A2, não aperta nada)

Os caps backstop permanecem **inalterados** (a auditoria #2 das auditorias propôs
apertar 34→20GB, mas isto introduz risco de trim no working-set — ver SPECv2 §
resolução). Prova de que o somatório respeita o teto agregado SEM apertar:

| Cache | Cap (civm.go) | Working-set medido | Folga |
| --- | --- | --- | --- |
| yarn | 12GB | 3.2×2 dirs ativos = 6.4 | 5.6 |
| go-build | 12GB | 2.4 | 9.6 |
| npm | 3GB | ~0 (sem `_cacache` no guest) | 3 |
| pnpm | 5GB | 0 (sem `.pnpm-store`) | 5 |
| golangci | 2GB | <2 | >0 |
| **Total** | **34GB** | **~10GB** | **24GB de teto morto** |

O teto morto (24GB) é INERTE: o working-set sob o cap → trim no-op no job → sem
race ENOENT (invariante A2 da cachetrim-yarn-atomic preservado). O orçamento
agregado (RF-1) já fecha com cache@34 no pior caso e ≥29GB livres no guest — logo
**não há necessidade de apertar os caps**. A fonte de alívio sob pressão é o
Docker (RF-2), não o cache.

> **Kahneman #13:** A2 foi a cura de um incidente REAL (#1155 ENOENT); apertar o
> cap a ponto de o working-set tocá-lo re-introduz exatamente esse failure mode.
> O orçamento não precisa do aperto — Docker já fecha a conta.

## Arquivos tocados

| Arquivo | Mudança | RF |
| --- | --- | --- |
| `docs/specs/vm-disk-budget/{DATA-REPORT,PRD,SPEC}.md` | o orçamento documentado | RF-1, RF-4 |
| `internal/cleanup/cleanup.go` (dockerPruneSafe, ~525) | `image prune -a -f --filter until=168h` (fia GAP-2) | RF-2 |
| `internal/cleanup/cleanup_test.go` | unit (arg montado) + integration (efeito, daemon real) | RF-2 |
| `deploy/windows/civm-vhdx-autoreclaim.ps1` (412) | `--threshold-pct=0` → `=1` | RF-3 |
| `internal/civm/civm.go:90` | (nenhuma — a constante PASSA a ter call-site, deixa de ser morta) | RF-2 |

## Validação (critérios do PRD → testes)

1. **RF-1 (prova numérica):** o orçamento fecha no SPEC: pior caso guest
   34+9+36=79 → 29 livres; V: com Docker reclamado → 53 livres ≫ 30. Revisável
   no documento, não executável.
2. **RF-2 unit (Go):** mock `RunFn` afirma que `dockerPruneSafe` monta
   `image prune -a -f --filter until=168h`. **Insuficiente sozinho (#13).**
3. **RF-2 integration (Go, daemon real — o que prova o EFEITO, #13):** subir um
   daemon docker real (ou usar o do guest em test-tag), criar uma imagem tagged
   com CREATED >7d (via `docker build` + `--build-arg` ou re-tag de base antiga),
   rodar `dockerPruneSafe` → afirmar que a imagem SUMIU (`docker image ls` não a
   lista). Tag `//go:build integration`.
4. **RF-2 par #13 (o crítico — o positivo legítimo):** no mesmo integration,
   uma imagem (a) EM USO por um container vivo OU (b) com CREATED <7d **NÃO** é
   removida. Prova que o lever não mata recurso ativo (RNF-1).
5. **RF-3 (efeito):** com guest >1% usado, `disk-watchdog --threshold-pct=1
   --execute` dispara o prune (não pula como `=0`). Coberto pelo
   host-volume-reclaim-liveness RF-3 host-effect (firefight 2026-06-15).
6. **Regressão:** `go test ./... -race` (civm) verde; `golangci-lint` 0 issues;
   parse PS do autoreclaim OK.

## Links Kahneman (passos críticos)

- RF-2: **#13** (existência ≠ função — o arg montado ≠ imagem removida; exige
  integration por efeito) + **#16** (o prune agressivo age antes do piso, a recusa
  permanece o backstop).
- RF-1, RF-4: **#15** (WARN/CRIT distintos, determinísticos) + **#13** (todo
  número é medido).
- F3 (fora de escopo): **#15/#16** (o piso fail-safe é correto, não se relaxa).

## Decisões e trade-offs

- **DT-1: endurecer só o caminho BUSY, não o hook.** O hook é sibling-safe por
  design; mexer nele reintroduz hook.go:309-317. O `dockerPruneSafe` já roda
  sem idle-guard e o filtro de idade o torna seguro. Counterfactual (#2): se o
  churn for dominado por dangling recém-criado (não tagged>7d), R2 rende menos —
  validar com `docker image ls --filter dangling=false` antes de creditar o lever.
- **DT-2: NÃO apertar os caps (contra a proposta da auditoria #2).** Apertar
  34→20 re-introduz o risco A2 (trim no working-set). O Docker fecha a conta sem
  isso. Se um PR adicionar muitas deps e o working-set crescer, re-medir por
  efeito (#13) antes de qualquer aperto.
- **DT-3: `threshold-pct=1`, não tratar `0` como sentinel "sempre rodar".**
  Mudar a semântica de `0` em `Check` afetaria outros call-sites; `=1` é o
  mínimo válido, localizado e sem efeito colateral.
