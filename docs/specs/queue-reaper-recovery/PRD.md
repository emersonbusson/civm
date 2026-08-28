# PRD — recuperação da fila shared após fechamento em massa

## Problema

Em 2026-07-25, 38 PRs foram fechados em sequência no `acme/app`.
O timer `civmctl-run-reaper.timer` permanecia `active`, mas o serviço falhava
a cada cinco minutos porque `CIVM_REAPER_REPOS` não estava configurado. A fila
chegou a 71 runs ativos e a VM continuou executando checks de PRs fechados.

O runner de organização também fazia `active-runs --repos=auto` retornar zero:
a unit expõe apenas `acme`, enquanto a API de runs exige `owner/repo`.

## Resultado esperado

- `health` deve ficar crítico quando o timer está ativo, mas o último resultado
  do serviço do reaper não é `success`.
- `active-runs --repos=auto` deve descobrir a fleet por configuração explícita
  ou expandir o runner de organização via API paginada do GitHub.
- O PowerShell legado deve aceitar o estado FIFO mínimo gravado pelo
  `civm-host`, preservando rollback sem remoção manual do arquivo.

## Fora de escopo

- Cancelar runs de `push` da `main`.
- Forçar Stop-VM ou Optimize enquanto `idle-check` reportar atividade.
- Alterar thresholds de disco, grace ou compactação.

## Rollback trigger

Reverter se `health` marcar `Result=success` como falha, se um repo não
arquivado do runner de organização deixar de ser consultado, ou se a migração
do estado alterar `currentPr`.
