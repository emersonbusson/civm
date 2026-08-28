# SPEC — disk-watchdog: limpeza segura mesmo com host ocupado

Issue: acme/civm#70
Status: implementação
Precedente: `docs/specs/civm-runner-reliability/SPEC.md` RF-6/ITEM-10 (runner-watchdog host-busy -> exit 0).

## Problema

`internal/cleanup/cleanup.go` `Run()` usa o `ensureIdle` como **gate duro**: se o
host não está ocioso, retorna uma única action `host_idle` **com `Err`** e não
executa nada. `internal/diskwatchdog/diskwatchdog.go` classifica esse `Err` como
`DecisionError` -> `ExitCode()=2` -> `civmctl-disk-watchdog.service` em FAILED.

Dois efeitos ruins num box compartilhado (8 runners) que vive ocupado:

1. Falsa-falha: o serviço aparece em `systemctl --failed` a cada tick, mascarando
   falha real. Inconsistente com o defer por `deferred-by-docker-heavy-lock`, que
   já retorna sem `Err` (exit 0).
2. Operacional: a limpeza agressiva (disparada acima do threshold) **nunca roda**,
   então o disco enche (observado 70% -> 88%) durante rajadas de CI.

## Decisão

Host ocupado deixa de ser um gate duro do tipo tudo-ou-nada. Passa a ser um
**deferral parcial**:

- A limpeza de **arquivos** (`tmp_old`, `work_old`) e o **prune agressivo**
  (`docker system prune -af --volumes`, `apt-get clean/autoremove`) **continuam
  atrás do gate de ocioso** — nada muda no caminho do delete privilegiado.
- Roda, **mesmo com host ocupado**, apenas um **docker prune seguro por
  construção**: `docker image prune -f` (somente imagens dangling, não
  referenciadas) + `docker builder prune -f --filter until=24h` (somente cache de
  build NÃO usado nas últimas 24h — o filtro `until` do BuildKit é baseado em
  último-uso, não em idade de criação). Esses comandos, por definição do Docker,
  nunca removem recurso em uso por container/build ativo.
- O retorno em host-busy não carrega `Err`: emite o `dockerPruneSafe` (com bytes
  liberados) + uma action `deferred-by-host-busy` (sem `Err`). `diskwatchdog`
  então classifica como `DecisionCleanupTriggered` -> `ExitCode()=0`.

Não-zero fica reservado para falha real: statfs/threshold inválido (exit 2),
erro de delete/prune (exit 2). O `deferred-by-docker-heavy-lock` (build pesado
segurando o lock) permanece como defer total benigno (exit 0), inalterado.

## Por que é seguro (análise WYSIATI)

- `docker image prune -f` remove só imagens dangling não referenciadas; imagens
  tagueadas e em uso por containers em execução são preservadas.
- `docker builder prune -f --filter until=24h` remove só cache de build NÃO usado
  nas últimas 24h (último-uso, não idade); o grafo de cache de um build ativo é "in use" e não é
  removido.
- Evidência empírica: prune manual equivalente liberou ~10 GB no box com jobs
  ativos, sem quebrar nenhum build (88% -> 73%).
- O caminho que apaga arquivos (`safedelete`/`_work`/`/tmp`) NÃO é tocado:
  continua 100% atrás do gate de ocioso + proteções finas existentes
  (`mtime<2h`, threshold `_work`>24h, `protectedWorkCacheDirs`, guard do
  `safedelete`). Não inspecionado nesta mudança: o comportamento sob
  docker-heavy-lock ativo (permanece defer total, fora de escopo).

## Mudanças

- `internal/cleanup/cleanup.go`:
  - `const deferredByHostBusy = "deferred-by-host-busy"`.
  - `dockerPruneSafe(ctx, opts)`: `image prune -f` + `builder prune -f --filter
    until=24h`; soma bytes liberados; sem gate de ocioso. Dry-run estima via
    `docker system df`.
  - `Run`: no ramo host-busy, em vez de `{host_idle, Err}`, retorna
    `[dockerPruneSafe?, deferred-by-host-busy]` (sem `Err`). Caminho de ocioso
    inalterado.
  - `parseTotalReclaimed` passa a aceitar também a linha `Total:` do
    `docker builder prune`.
- `internal/diskwatchdog/diskwatchdog.go`: nenhuma mudança de lógica
  (já dá exit 0 quando nenhuma action tem `Err`).

## Testes (Kahneman #13 — validar o PROPÓSITO)

- `cleanup`: host ocupado -> `dockerPruneSafe` EXECUTOU (liberou bytes) e NENHUM
  delete de arquivo ocorreu (`safedelete` não chamado); nenhuma action com `Err`;
  `deferred-by-host-busy` presente. (Substitui o antigo teste que afirmava
  `host_idle` com `Err` — o oposto do propósito.)
- `cleanup`: host ocioso -> comportamento integral inalterado (tmp_old, work_old,
  docker agressivo, apt) — caso positivo pareado.
- `cleanup`: re-check interno antes de deletar continua bloqueando delete quando
  um job começa no meio (inalterado).
- `diskwatchdog`: acima do threshold + host ocupado -> `DecisionCleanupTriggered`,
  `ExitCode()==0`, action `docker_prune_safe` presente, sem `Err`.
- `diskwatchdog`: erro real (statfs/threshold) -> `DecisionError`, exit 2
  (inalterado).

## Adendo — recusa do safedelete não é fatal (idle path)

Mesmo padrão "recusa segura ≠ falha fatal", agora no caminho de ocioso. Quando o
disk-watchdog (root, uid 0) limpa `/tmp`/`_work` e encontra um arquivo de OUTRO
uid (ex.: um leftover uid-1000 de CI em `/tmp`), o `safedelete` recusa com
`ErrUnsafePath` (sem nunca chamar sudo). Antes, essa recusa virava `a.Err` →
`DecisionError` → exit 2 → serviço FAILED a cada janela ociosa com um arquivo
cross-user antigo.

Agora `scanAndMaybeDelete` trata `errors.Is(err, safedelete.ErrUnsafePath)` como
**skip não-fatal**: pula o candidato, libera o resto, e o watchdog sai 0. Apenas
falha GENUÍNA permanece fatal — incluindo a escalação `_work` falha (a sentinela
de runner quebrado do ITEM-10), que NÃO é `ErrUnsafePath` e segue fatal. O fix
reduz deleção (pula mais), nunca aumenta. Teste:
`TestScanAndMaybeDelete_RefusalIsNonFatalSkip`.

## Rollback trigger

Se um build ativo passar a falhar por imagem/cache removido pelo prune seguro,
reverter este commit e voltar o ramo host-busy ao defer total (`host_idle` sem
`Err`, sem prune). O caminho de delete de arquivos não muda, então não é fonte de
rollback.

## Fora de escopo (follow-up)

Permitir limpeza de arquivos (`_work`/`tmp`) com host ocupado apoiando-se só nas
proteções finas (sem gate de ocioso) — mudança no caminho do delete privilegiado,
exige seu próprio SPEC + gates de teste de isolamento.
