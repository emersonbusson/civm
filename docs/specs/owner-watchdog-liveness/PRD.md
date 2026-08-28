# PRD — watchdog do owner ativo

## Problema

Em 02/08/2026, o PR `peer#1609` manteve `4/4` runners gate ocupados por
`2h03m` e um quinto gate queued. O owner C# ficou preso em `ssh.exe`; como a
task usa `IgnoreNew`, nenhum tick posterior publicou o novo SHA.

O watchdog existente não detectou o owner preso. Seu script existia somente em
`C:\civm-deploy` e consultava exclusivamente `civm-vm-orchestrator`, o owner
PowerShell desativado no cutover F4. Por isso cada execução produzia DRIFT falso
por `orchestrator nao Ready (Disabled)` e não media o heartbeat do owner C#.

## Resultado esperado

- Exatamente um owner fica ativo: C# ou PowerShell de rollback.
- O owner C# exige action ativa, `LastTaskResult=0` quando Ready e heartbeat
  concluído há no máximo `45 min`.
- `processBlockedReason` no estado da fila produz DRIFT observável.
- O watchdog detecta owner ausente/dual, mas não inicia nem habilita owner.
- O artefato e o registro da task ficam versionados no `civm`.

## Critérios de aceite

1. C# Ready/Running + PowerShell Disabled = um owner válido.
2. PowerShell Ready/Running + C# Disabled = rollback válido.
3. Zero ou dois owners ativos = DRIFT.
4. Heartbeat C# com idade maior que `45 min` = DRIFT.
5. Latch de processo preenchido = DRIFT.
6. A task roda a cada `20 min`, no startup e tem limite de `5 min`.

## Rollback trigger

Reverter se o watchdog produzir DRIFT em 2 execuções consecutivas com
exatamente um owner ativo, heartbeat menor que `45 min`, latch vazio e
`LastTaskResult=0`.
