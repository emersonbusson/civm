---
slug: orphan-port-reaper
title: Reaper de container de CI órfão na fronteira job-started
milestone: —
issues: []
---

# SPEC — Reaper de container de CI órfão (job-started)

> Disciplinas: `disciplines/KAHNEMAN-DISCIPLINES.md` (#13 existe≠função, #14 retry
> calibrado, #15 fail-safe + curador independente, #16 idempotência).
> Validação: `go test -race ./internal/hook/...`,
> `go test -tags=integration -run TestIntegrationReapOrphan ./internal/hook/`,
> `golangci-lint run -c .golangci.yml`.

## Problema (incidente 2026-06-19)

A box civm tem 1 runner self-hosted servindo o acme. Jobs de CI sobem um stack
docker-compose (`acme: infra/docker-compose.yml` + override) com **portas FIXAS
de host** publicadas em `127.0.0.1` (minio API em `:9020`, nginx em `:81`,
postgres em `:5443`, etc.).

Quando um stack anterior não é derrubado — job **cancelado**, OU o **runner que o
subiu foi REMOVIDO** — o container vira **órfão** e segura a porta fixa. O próximo
job morre no step "Start local backend stack":

```
Bind for 127.0.0.1:9020 failed: port is already allocated
make up-local  (exit 2)
```

Isso matou tenant-isolation no #1184 e #1186.

### Por que os mecanismos existentes não pegavam

1. `internal/hook/hook.go::killWorkRootContainers` (hook job-started/completed) só
   reapa containers cujo bind-mount cai sob o `_work` root do **PRÓPRIO** runner
   (match por path-segment). Um órfão de **OUTRO** runner (ex.: o repo-runner que
   foi removido) tem `_work` root diferente — e se o runner foi removido, esse
   root nem existe mais — então **nenhum** hook vivo o reapa.
2. `docker container prune -f` (no cleanup do hook) só remove containers
   **PARADOS**. O órfão do incidente estava **RODANDO**, segurando a porta.
3. `acme/infra/ci/preclean-stack.sh` remove só containers com o prefixo de
   project **do slot atual** (`<slot>-`). O órfão de outro slot/runner, ou o stack
   com project `acme` puro, escapa desse filtro.

## Decisão

Codificar um **reaper de órfão NÃO escopado a um único `_work` root**, na fronteira
**job-started** (ANTES de o job subir o stack):
`internal/hook/hook.go::reapOrphanCIContainers`.

Numa box de 1 runner, na fronteira job-started, qualquer container **RODANDO** que:

- **(a)** publique uma host port FIXA de CI conhecida (`civm.DefaultCIFixedHostPorts`), OU
- **(b)** tenha `com.docker.compose.project` começando com `civm.DefaultCIOrphanProjectPrefix` (`acme`)

é **órfão por definição** → `docker stop --time 5` + `docker rm -f`.

O sinal **(b) é o primário** (pega o stack inteiro independente das portas, e é o
único que pega o órfão de outro runner); **(a) é defesa em profundidade** para um
container que segure a porta sem o label esperado. A lista de portas fixas espelha
os defaults do compose committed do acme (mesma disciplina do schema-contract: se
um default mudar lá, atualiza-se a lista aqui).

### Invariante de segurança (não mata o stack do JOB ATUAL)

O GitHub Actions dispara o hook `ACTIONS_RUNNER_HOOK_JOB_STARTED` **ANTES de
qualquer step do job rodar** — e o stack do job só sobe num step posterior
("Start local backend stack"). Logo, na fronteira job-started o stack do job atual
**AINDA NÃO EXISTE**: todo container que case o critério é, por construção, resíduo
de um run/runner **ANTERIOR**. Numa box de 1 runner que executa um job por vez, não
há peer concorrente cujo stack pudéssemos matar por engano.

### Best-effort (job-started não pode falhar por higiene)

Toda falha do reaper (docker fora do ar, inspect quebrado, rm que não conseguiu
remover) é **Warning**, nunca **Error** — higiene de job-started nunca rejeita o
job. É o mesmo contrato de `killWorkRootContainers`/`reclaimWorkspaceOwnership`. Se
o reap falhar e a porta seguir presa, o bring-up subsequente reexpõe a colisão; o
reaper não inventa um sucesso (#13 existe≠função). Idempotente (#16): rodar o
reaper 2× = rodar 1× (nada para remover na 2ª).

## Classificador de falha (peer acme — `tools/devctl/internal/ci/failure.go`)

Com o reaper no lugar, a colisão de porta **não deve mais chegar** ao classificador
de falha do acme. Decisão (#14 retry calibrado): **NÃO** adicionar assinatura
transitória de "port is already allocated" / "Bind for". Um retry de colisão de
porta sem reap entre as tentativas roda o MESMO `make up-local` e re-encontra a
mesma porta presa (retry-sem-reap, #16). Se a colisão AINDA aparecer pós-reaper, é
bug real → fica **determinística** (`SigUnknown → KindDeterministic`, falha rápido)
para o problema aparecer, não ser mascarado como flake. O raciocínio fica
documentado num comentário no catálogo do `failure.go` (mudança no repo acme,
commit/PR separado).

## Validação por efeito (#13)

- **Unit (hermético):** `isCIOrphan` (predicado puro, table-driven),
  `orphanIDsFromInspect` (parser puro), `reapOrphanCIContainers` (sequência docker
  via `RunFn` injetado), best-effort (ps/inspect/stop/rm falham → warning, nunca
  error), integração no `Run` job-started.
- **Integration (`//go:build integration`, docker real):**
  - `TestIntegrationReapOrphanFreesRealPort` — sobe um container REAL publicando
    uma porta fixa de CI, prova que a porta está PRESA, roda o reaper, e afirma que
    a porta foi **LIBERADA** (o efeito real). Self-skip quando o bridge/iptables do
    host não suporta publicar porta (ex.: alguns WSL2) → gate real no self-hosted.
  - `TestIntegrationReapOrphanRemovesRealLabeledContainer` — sobe um container REAL
    com label `com.docker.compose.project=acme-*` (sem porta, `--network none`),
    roda o reaper contra o daemon REAL, e afirma que o container **sumiu**. Cobre o
    sinal primário (label) onde o bridge é indisponível.

Os dois afirmam o EFEITO (porta livre / container removido), nunca "uma função foi
chamada".
