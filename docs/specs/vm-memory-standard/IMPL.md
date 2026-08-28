# IMPL — Padrão fixo de memória da VM civm

## Entrega

- Helper PowerShell dry-run por padrão, com aplicação explícita por `-Execute`.
- Alvo default de `12 GiB` fixos.
- Guard fail-closed que exige VM `Off` antes de qualquer mutação.
- Pós-condição que relê VM e memória e exige que a VM permaneça `Off`.
- Idempotência: configuração já correta retorna `noop` sem chamar
  `Set-VMMemory`.

## Testes automatizados

O teste injeta os cmdlets de Hyper-V e cobre 13 contratos sem exigir Hyper-V:

- dry-run e zero mutações;
- recusa de VM ligada antes de mutar;
- mudança única para 12 GiB;
- no-op idempotente;
- pós-condição divergente falha;
- ausência de start/stop no source.

## Operação

O helper é executado somente após o owner encerrar uma geração, limpar,
compactar e deixar a VM `Off`. Ele não disputa power-state com o owner C#.
Após a aplicação, `Get-VMMemory`, `Get-VM` e `free -h` fornecem a evidência
empírica; ela é registrada no `validation.md` local append-only.

## Rollback

Restaurar Dynamic Memory `7/7,5/12 GiB` com a VM `Off` se qualquer um dos 3
primeiros jobs terminar por OOM/exit 137, ou se o host permanecer com menos de
1 GiB livre por 5 minutos.
