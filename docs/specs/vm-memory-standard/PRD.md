# PRD — Padrão fixo de memória da VM civm

## 1. Resumo

- **Confirmado no codebase:** o civm não possui helper versionado para aplicar
  a memória da VM Hyper-V.
- **Confirmado empiricamente:** a VM `gha-ubuntu-2404` usa Dynamic Memory com
  mínimo de 7 GiB, startup de 7,5 GiB e máximo de 12 GiB.
- **Opção aprovada:** substituir esse intervalo por 12 GiB fixos, aplicados
  somente com a VM Off e por comando explícito do operador.

## 2. Contexto técnico

- **Confirmado empiricamente:** o host tem 31,9 GiB físicos; durante o E2E o
  Hyper-V reportou 12 GiB atribuídos e 3,12 GiB demandados, enquanto o guest
  expôs 6,76 GiB em `MemTotal`.
- **Confirmado em docs:** `docs/HARDWARE.md` descreve o VMRS como a reserva de
  runtime da VM, liberada quando a VM fica Off.
- **Confirmado no codebase:** o owner C# serializa gerações e desliga a VM no
  boundary; o ajuste de memória não pertence ao scheduler do GitHub.

## 3. Opção recomendada

Criar um helper PowerShell fino, dry-run por padrão, que aplique 12 GiB fixos
com `Set-VMMemory` apenas quando `Get-VM` comprovar estado Off. O helper valida
a pós-condição e não inicia a VM.

## 4. Requisitos funcionais

- **RF-1:** o default deve ser 12 GiB fixos.
- **RF-2:** sem `-Execute`, o helper só descreve o plano.
- **RF-3:** com `-Execute`, qualquer estado diferente de Off deve abortar antes
  de `Set-VMMemory`.
- **RF-4:** após a mutação, Dynamic Memory deve estar desabilitado e startup
  deve ser exatamente 12 GiB.
- **RF-5:** a VM deve permanecer Off após sucesso ou falha.
- **RF-6:** reaplicar a configuração já correta deve ser idempotente.

## 5. Requisitos não-funcionais

- **RNF-1:** zero dependências externas.
- **RNF-2:** parse e teste devem rodar sem Hyper-V disponível.
- **RNF-3:** output deve distinguir dry-run, alteração e no-op.
- **RNF-4:** nenhuma credencial, IP ou dado de usuário pode ser persistido.

## 6. Fluxos

1. O operador aguarda Worker = 0 e boundary compactado.
2. Confirma a VM Off.
3. Executa o helper primeiro em dry-run e depois com `-Execute`.
4. O helper aplica e relê a configuração.
5. O orquestrador continua sendo o único owner que liga a VM.

## 7. Modelo de dados

Não há banco nem estado novo. O estado relevante é a configuração Hyper-V:
`DynamicMemoryEnabled`, `Startup`, `Minimum` e `Maximum`.

## 8. API / Interfaces

```powershell
configure-civm-vm-memory.ps1 [-VMName <name>] [-MemoryGiB 12] [-Execute]
```

## 9. Dependências e riscos

- **Confirmado empiricamente:** fixar 12 GiB não excede os 12 GiB já atribuídos
  no snapshot vivo, mas reduz a elasticidade do host em futuros boots.
- Risco: executar durante job interromperia o contrato de isolamento.
- Mitigação: `Get-VM.State -eq Off` é pré-condição fail-closed.
- Rollback: restaurar Dynamic Memory 7/7,5/12 GiB com a VM Off.

## 10. Estratégia de implementação

1. Adicionar teste de contrato vermelho.
2. Implementar helper com dry-run, guard Off e verificação final.
3. Atualizar as fontes atuais de hardware/paridade e o runbook do host.
4. Validar parse, testes PowerShell e suite Go.
5. Aplicar no boundary real e registrar a medição em `validation.md` local.

## 11. Documentos a atualizar

- `docs/HARDWARE.md`
- `vm.md`
- `PAID-CI-PARITY.md`
- `README.md`
- `AGENTS.md`
- `CI-PARITY-CHECKLIST.md`
- `PAID-CI-PARITY-CHECKLIST.md`
- `runbooks/MULTI-PROJECT-RUNNER.md`
- `runbooks/HOST-ORCHESTRATOR-SETUP.md`
- `docs/specs/vm-memory-standard/IMPL.md`

## 12. Fora de escopo

- Aumentar RAM física do host.
- Alterar o limite de 16 GiB do WSL.
- Iniciar, parar ou compactar a VM pelo novo helper.
- Alterar CPU, swap ou disco.

## 13. Critérios de aceitação

- Teste negativo prova recusa quando a VM não está Off.
- Teste positivo prova o comando exato de 12 GiB fixos.
- Teste de idempotência prova no-op na segunda aplicação.
- Aplicação real mostra Dynamic Memory desabilitado e 12 GiB no guest.
- O primeiro job pós-mudança completa sem segundo Worker ou run órfão.

## 14. Validação

- Parser PowerShell em todos os scripts alterados.
- Teste de contrato do helper.
- `go build ./...`.
- `go test -race -count=1 ./...`.
- Verificação elevada de `Get-VM`, `Get-VMMemory` e `free -h` pós-boot.
