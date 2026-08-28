# SPECv2 — Orçamento holístico de disco do V:

> SSDV3 PASSO 2.5 (auditoria adversarial) → SPEC ativo para o IMPL. Refina o
> `SPEC.md` (preservado como baseline). Três achados; um deu **NO-GO** parcial.

## Auditoria 2.5 — achados

### B1 (NO-GO parcial) — RF-2 pode remover recurso de job ativo se o filtro de idade falhar

**Risco:** o SPEC afirma que `image prune -a -f --filter until=168h` é seguro
durante job porque (i) `-a` exclui imagens em uso e (ii) o filtro `until=168h`
casa por CREATED-date (só vendor images >7d). O ponto (i) é garantia do docker
(uma imagem referenciada por container vivo nunca é removida por `image prune`).
Mas o ponto (ii) tem um caso de borda: uma imagem **construída localmente pelo
job** (não puxada de vendor) com `compose build` pode herdar uma CREATED-date
antiga da base se o Dockerfile faz `FROM <base antiga>` e o build é incremental —
o docker reporta a CREATED da camada final, não da base, então o caso é raro, mas
um `docker commit` ou um build reproducível com timestamp fixado (`SOURCE_DATE_EPOCH`)
pode gerar uma imagem com CREATED >7d que o job ATUAL acabou de criar e ainda não
tagueou a um container.

→ **Resolução:** RF-2 PERMANECE, com dois guard-rails que fecham o caso de borda
sem custo:

1. **Rodar só no caminho BUSY que NÃO é o build ativo.** `dockerPruneSafe` já é
   chamado a partir do branch host-busy de `Run` (cleanup.go:213) — mas o lock
   docker-heavy (`LockActiveFn`, cleanup.go:192) já causa early-return ANTES do
   prune quando um job docker-heavy SEGURA o lock. Logo o `dockerPruneSafe`
   endurecido só roda quando há atividade de runner mas NENHUM job docker-heavy
   ativo (o lock não está segurado). O caso de borda (job construindo imagem >7d)
   exige um build docker ativo → segura o lock → o prune nem chega a rodar.
   **Confirmado:** cleanup.go:192-194 retorna `deferred-by-docker-heavy-lock`
   antes de qualquer prune quando o lock está ativo.
2. **A imagem só é removível por `image prune -a` se NÃO referenciada por NENHUM
   container** (nem o do build em curso). Se o job a construiu e vai usá-la, ela
   está referenciada (ou o build a segura) → excluída. A janela de risco é
   "imagem construída, build terminou, container ainda não subiu, CREATED>7d,
   nenhum job docker-heavy segura o lock" — interseção vazia na prática (se o
   build terminou e liberou o lock, a imagem ou foi usada ou é leftover).

A combinação lock-gate (já existente) + filtro-idade + exclusão-em-uso fecha B1.
**Kahneman #13:** validar por integration que uma imagem EM USO sobrevive (par
positivo), não só que dangling some.

### B2 (aceito, vira DT-2) — cap apertado demais re-introduziria o trim no working-set

O SPEC (DT-2) e a auditoria #2 das auditorias divergem: a auditoria sugeriu
apertar os caps 34→20GB para "fechar o orçamento". **Rejeitado.** Apertar yarn
12→8 ou go-build 12→8 aproxima o cap do working-set medido (yarn 6.4 em 2 dirs
ativos, go-build 2.4) — uma rajada de PR que adicione deps pode fazer o
working-set tocar o cap, e o disk-watchdog (timer 8min + EmergencyBypass ≥75%)
dispara o trim NO MEIO de um `yarn install` → ENOENT (o incidente exato que
cachetrim-yarn-atomic A2 curou). O orçamento agregado (RF-1) já fecha com cache@34
e ≥29GB livres no guest — **não precisa do aperto**. A fonte de alívio é Docker
(RF-2), não cache.

→ **Resolução:** caps INALTERADOS (RF-4 do SPEC). O aperto fica documentado como
descartado, com o failure mode que o motiva (A2). Se um PR fizer o working-set
crescer materialmente, re-medir por efeito (#13) antes de qualquer mudança de cap.

### B3 (aceito, documentado) — alvo de folga inatingível sob F3

WARN=30GB livres no V: é **inatingível** se o working-set ATIVO de uma rajada
concorrente exceder a capacidade usável — ex. `compose up --build` de N serviços
cujas imagens+volumes EM USO somam >88GB num disco de 108G. Nesse caso nenhum
lever compacta dado em uso: `image prune -a` exclui imagens referenciadas,
`cacheTrim` blinda 24h (MinProtect), `system prune -af` só roda idle e não toca
container vivo. É F3 (hardware).

→ **Resolução:** F3 fica **fora de escopo** (alinhado ao
`host-volume-reclaim-liveness` PRD F3 e ao NO-GO B1 do seu SPECv2). O orçamento
NÃO promete folga sob F3 — promete que ABAIXO de F3 (working-set ativo < 88GB
usável) a folga de 30GB é determinística, e prova com número ONDE F3 começa
(working-set ativo > capacidade usável). Quando F3 torna o piso inevitável, a
recusa a exit 75 permanece o fail-safe correto (#15/#16), observável e raro
(host-volume-reclaim-liveness RF-4), não o orçamento de fato.

## Escopo ativo (pós-auditoria)

Todos os RF permanecem. Mudanças vs SPEC:

- **RF-2:** mantido, com o guard-rail B1 explicitado (o lock docker-heavy já
  gateia o prune; o caso de borda do build>7d cai no early-return do lock).
- **RF-4:** reforçado — caps INALTERADOS (B2), o aperto é descartado, não adiado.
- **F3:** explicitamente fora de escopo, com a fronteira numérica definida (B3).

## RF-1 — orçamento agregado — INALTERADO do SPEC

Folga-alvo WARN=30GB / CRIT=10GB (banda 20GB). Pior caso guest 34+9+36=79 → 29
livres; V: com Docker reclamado → 53 livres ≫ 30; sem reclamar → 35 livres (sob
WARN se +5GB de rajada). O teto de Docker (RF-2) é o que mantém a folga
determinística. Fronteira F3 (B3): working-set ativo > ~88GB usável.

## RF-2 — docker-prune endurecido — com guard-rail B1

`internal/cleanup/cleanup.go` `dockerPruneSafe` (~525):

```go
// Caminho host-busy SEM job docker-heavy ativo (o lock já causou early-return em
// Run se houvesse — cleanup.go:192). Solta imagens TAGGED-unused >7d por
// CREATED-date: só vendor images antigas, nunca uma recém-construída/puxada por
// job ativo (esse job segura o lock → o prune nem roda). Kahneman #13/#16.
images, err := opts.RunFn(ctx, "docker", "image", "prune", "-a", "-f",
    "--filter", civm.DefaultDockerImagePruneFilter) // "until=168h" (GAP-2, civm.go:90)
```

Garantias compostas (B1): (1) lock-gate já existente (cleanup.go:192) → não roda
sob job docker-heavy; (2) `image prune -a` exclui imagens em uso; (3) filtro
CREATED>7d → só vendor antigo. Hook job-completed e idle `system prune -af`
intocados.

## RF-3 — `threshold-pct=1` — INALTERADO do SPEC

`civm-vhdx-autoreclaim.ps1:412`: `--threshold-pct=0` → `=1` (escapa o reset 0→60
de diskwatchdog.go:135; `1` é o mínimo válido de diskwatchdog.go:138).

## RF-4 — caps INALTERADOS — reforçado (B2)

Nenhum cap muda. Prova: somatório 34GB respeita o teto agregado (RF-1) sem
apertar; o aperto re-introduziria o risco A2 (trim no working-set → ENOENT). O
teto morto (24GB) é inerte por design.

## Arquivos tocados (pós-auditoria) — igual ao SPEC

| Arquivo | Mudança | RF |
| --- | --- | --- |
| `docs/specs/vm-disk-budget/*.md` | orçamento documentado | RF-1, RF-4 |
| `internal/cleanup/cleanup.go` (dockerPruneSafe) | `image prune -a -f --filter until=168h` | RF-2 |
| `internal/cleanup/cleanup_test.go` | unit (arg) + integration (efeito + par #13) | RF-2 |
| `deploy/windows/civm-vhdx-autoreclaim.ps1` (412) | `threshold-pct` 0→1 | RF-3 |

## Validação (pós-auditoria)

1. **RF-2 integration (o crítico, #13):** daemon docker real, imagem tagged
   CREATED>7d, sem job docker-heavy segurando o lock → `dockerPruneSafe` →
   imagem SUMIU (`docker image ls` não lista). `//go:build integration`.
2. **RF-2 par #13 (positivo legítimo):** no mesmo run, imagem EM USO por
   container vivo OU CREATED<7d → **NÃO** removida. Prova RNF-1 (não mata recurso
   ativo).
3. **RF-2 B1 (lock-gate):** com o lock docker-heavy SEGURADO, `Run` retorna
   `deferred-by-docker-heavy-lock` ANTES de `dockerPruneSafe` — o prune nem roda
   (cleanup.go:192). Unit já existente em `cleanup_test.go` (reusar/estender).
4. **RF-3 (efeito):** coberto por host-volume-reclaim-liveness RF-3 host-effect.
5. **RF-1/RF-4 (numérico):** revisável no documento.
6. **Regressão:** `go test ./... -race` verde; `golangci-lint` 0; parse PS OK.

## Rastreabilidade

RF-1 → orçamento (DATA-REPORT + este doc). RF-2 → `dockerPruneSafe` + integration
(efeito + par #13) + lock-gate (B1). RF-3 → autoreclaim.ps1:412. RF-4 → caps
inalterados (B2). F3 → fora de escopo, fronteira numérica (B3).

## Links Kahneman

- RF-2: **#13** (o arg ≠ o efeito; integration + par positivo) + **#16** (prune
  antes do piso; recusa é o backstop).
- RF-1, RF-4: **#15** (WARN/CRIT distintos) + **#13** (números medidos; B2: A2 foi
  cura de incidente real, não apertar).
- B3/F3: **#15/#16** (piso fail-safe correto, não relaxar; fronteira numérica
  honesta).

## Limites honestos (do PRD + auditoria)

1. **OS+_work+go-mod=36GB é subtração, não `du` direto** — travar com
   `du -sh ~/go/pkg/mod _work /usr` real antes de creditar o piso fixo.
2. **18GB reclaimable é UMA medição (2026-06-15)** — sob rajada o working-set
   ATIVO docker pode passar do reclaimable; aí é F3 (B3).
3. **A maioria das 8.3GB ser tagged-unused>7d é inferido** — validar com
   `docker image ls --filter dangling=false --format '{{.Size}} {{.CreatedSince}}'`
   antes de creditar o lever R2.
4. **`pr-serial-queue` não existe** — o lever de footprint por-PR é
   `multi-project-isolation`; este orçamento o justifica numericamente (se mesmo
   com Docker no teto o working-set estoura, é F3 e a serialização é o lever).
5. **#13:** um teste hermético do arg `-a --filter` NÃO prova reclaim — só o
   integration contra daemon real com imagem tagged>7d prova o EFEITO.
