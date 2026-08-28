# SPEC — watchdog do owner ativo

## Decisão

`deploy/windows/civm-watchdog.ps1` calcula dois predicados:

- `hostActive`: `civm-host-orchestrator` em Ready ou Running;
- `legacyActive`: `civm-vm-orchestrator` em Ready ou Running.

A soma deve ser `1`. O script aceita o PowerShell somente como rollback com
`-EnforceQueue` e sem `-Observe`. Para C#, a action precisa conter `--active`
ou `active.cmd`.

O heartbeat C# é o `LastWriteTimeUtc` de `V:\civm-host-shadow.jsonl`. O limite
de `45 min` cobre o budget padrão de limpeza/Optimize de `30 min` mais `5 min`
de encerramento e `10 min` de margem. O watchdog apenas detecta; não mata o
processo nem troca o owner.

`V:\civm-pr-queue.json.processBlockedReason` não vazio produz DRIFT. O latch
continua exigindo diagnóstico e limpeza operacional explícita; o watchdog não
o remove.

## Registro

`register-civm-watchdog.ps1`:

- valida o AST antes de copiar;
- copia para `C:\civm-deploy\civm-watchdog.ps1`;
- registra `civm-watchdog` como SYSTEM;
- agenda startup + repetição de `20 min`;
- usa `ExecutionTimeLimit=5 min` e `IgnoreNew`.

## Passo 2.5

- Owner dual é mais perigoso que owner ausente: ambos geram DRIFT, sem
  correção automática.
- Uma compactação válida pode durar `30 min`; usar limiar menor geraria falso
  positivo. O limite de `45 min` é numérico e versionado.
- `civm-vhdx-autoreclaim` ativo produz DRIFT, mas a desabilitação continua
  sendo uma ação operacional explícita; o watchdog não faz self-heal.
- Falha para ler V:, VM, fila ou heartbeat aparece em DRIFT; não vira OK.

## Testes

`internal/hostdisk/owner_watchdog_test.go` exige owner C#/rollback, heartbeat,
latch, registro limitado e ausência de `Enable-ScheduledTask`,
`Start-ScheduledTask` ou `Disable-ScheduledTask`.

## Rollback trigger

Mesmo gatilho do PRD. Rollback operacional: restaurar o script anterior sem
alterar a task do owner; não reativar autoreclaim/optimize legados.
