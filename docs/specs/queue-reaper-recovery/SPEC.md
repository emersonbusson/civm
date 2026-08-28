# SPEC — recuperação da fila shared após fechamento em massa

## Contratos

1. `health` consulta:
   - `civmctl-run-reaper.timer`: `enabled` e `active`;
   - `civmctl-run-reaper.service`: `systemctl show LoadState,Result`.
   - unit ausente ou `Result != success` é crítico mesmo com timer ativo.
2. `active-runs --repos=auto` resolve repos nesta ordem:
   - units repo-level;
   - `CIVM_REAPER_REPOS`, quando legível;
   - todos os repos não arquivados retornados por `gh api --paginate --slurp`
     para units org-level.
3. `Ensure-PrQueueState` adiciona somente propriedades ausentes:
   `contexts`, `currentPr`, `currentIdleSinceUtc`, `lastCompactHeadSha` e
   `lastCompactContext`.
4. A migração nunca substitui valor existente.
5. `bootstrap-everything` instala as units do reaper; o bootstrap documentado
   configura `CIVM_REAPER_REPOS` antes de habilitar o timer.

## Fail-safe

- Falha ao listar repos da organização aborta `active-runs --auto` com erro,
  sem produzir uma fleet vazia falsamente saudável.
- Config ausente ou root-only não é erro quando há runner org-level que possa
  ser expandido pela API.
- O reaper continua sendo o único mecanismo que cancela runs; health e
  active-runs são read-only.

## Testes

- Go race: serviço falho com timer ativo; fleet configurada; expansão org.
- PowerShell: estado mínimo migra para o shape completo sem perder contexto.
- Suite completa: `go test -race -count=1 ./...`.

## Rollback trigger

Reverter se houver divergência entre repos inferidos e a API paginada, falso
crítico com `Result=success`, ou mudança do contexto FIFO durante migração.
