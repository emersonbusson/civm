# SPEC — Padrão fixo de memória da VM civm

## Escopo aprovado

Implementar RF-1..RF-6 e RNF-1..RNF-4 do PRD sem transferir power-state para
o novo helper. O owner C# permanece exclusivo para start/stop/compact.

## Arquivos

- Criar `deploy/windows/configure-civm-vm-memory.ps1`.
- Criar `deploy/windows/configure-civm-vm-memory.test.ps1`.
- Atualizar `docs/HARDWARE.md`, `vm.md`, `PAID-CI-PARITY.md`, as superfícies
  de setup/instrução e marcar os checklists anteriores como históricos.
- Criar `docs/specs/vm-memory-standard/IMPL.md` após a implementação.

## Contrato do helper

Parâmetros:

- `VMName`: default `gha-ubuntu-2404`, não vazio.
- `MemoryGiB`: inteiro de 4 a 64, default 12.
- `Execute`: switch; ausente significa dry-run.

Fluxo:

1. Ler `Get-VM` e `Get-VMMemory` com `-ErrorAction Stop`.
2. Produzir objeto de plano com estado observado e alvo.
3. Se sem `-Execute`, emitir plano e retornar 0 sem mutação.
4. Se configuração já for fixa e igual ao alvo, emitir `noop` e retornar 0.
5. Se VM não estiver Off, lançar erro antes de `Set-VMMemory`.
6. Executar `Set-VMMemory -DynamicMemoryEnabled $false -StartupBytes <alvo>`.
7. Relê VM e memória; falhar se estado mudou, Dynamic Memory continuar ativo
   ou startup divergir do alvo.
8. Emitir `changed` em JSON compacto.

## Testes

O teste deve usar funções puras/injeção de scriptblocks para provar:

- dry-run faz zero mutações;
- Running + `-Execute` faz zero mutações e falha;
- Off + configuração divergente executa exatamente uma mutação;
- Off + configuração correta executa zero mutações;
- pós-condição divergente falha;
- o source contém default 12 e não chama `Start-VM`/`Stop-VM`.

## Segurança e observabilidade

- Não executar texto externo; `VMName` vai como argumento tipado de cmdlet.
- Não elevar dentro do helper; o operador escolhe o contexto administrativo.
- Output não contém secrets e usa os estados `plan`, `noop`, `changed`.
- Guard fail-closed segue Kahneman #13 e #16: existência do script não prova
  proteção; os testes cobrem caminho legítimo e recusa.

## Rollback

Com a VM Off:

```powershell
Set-VMMemory -VMName gha-ubuntu-2404 `
  -DynamicMemoryEnabled $true `
  -MinimumBytes 7GB -StartupBytes 7.5GB -MaximumBytes 12GB
```

Rollback trigger: se qualquer um dos 3 primeiros jobs pós-mudança terminar por
OOM/exit 137, ou se o host permanecer com menos de 1 GiB livre por 5 minutos,
restaurar Dynamic Memory e registrar a medição.
