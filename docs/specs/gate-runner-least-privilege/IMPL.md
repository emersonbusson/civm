# IMPL — menor privilégio dos gate runners Windows

## Arquivos

- `deploy/windows/civm-gate-runner-provision.ps1`: pausa publisher, remove a
  elegibilidade remota, drena e instala 2.336.0 com SHA-256 em staging
  SYSTEM/Administrators-only, sem service e com `--disableupdate`; após o
  drain, o walker restaura acesso somente em diretórios reais, audita aliases
  e permite retomar staging validado sem novo replace. A raiz compartilhada é
  criada uma vez ou somente validada; um gate não normaliza ACL da fleet. A
  contagem nativa de hardlinks precede toda reescrita de ACL em arquivos; o
  reparo de diretórios preserva o inventário raw de ACEs e usa escrita de DACL
  sem propagação aos filhos, com chaves Base64 comparadas ordinalmente.
- `deploy/windows/civm-gate-task-setup.ps1`: preflight, migração de task,
  DACLs protegidas, listener direto, probe efetivo e owner real do processo.
- `internal/hostdisk/ps1_safety_test.go`: contrato estrutural dos dois scripts.
- a travessia permite somente `bin`/`externals` apontando para siblings da
  versão pinada e nunca segue a junction;
- `deploy/windows/civm-vm-orchestrator.ps1`: caminho de rollback alinhado ao
  contexto canônico e cinco interpolações inválidas corrigidas.
- `rules/security.md` e `runbooks/PR-QUEUE-ENABLE.md`: confiança e rollout.

## Ordem de rollout

1. desabilitar o publisher; o provisionador remove `civm-gate`, confirma zero
   job remoto/local por 3 segundos e rejeita labels de geração residuais;
2. reprovisionar um gate com versão e SHA-256 pinadas e `--disableupdate`;
   staging parcial só pode usar `-ResumeStaged` após revisão do estado;
3. executar o setup elevado; ele remove todas as labels customizadas e o probe
   precisa negar write/create/delete/rename, DACL e escrita nos binários;
4. confirmar listener NETWORK SERVICE e runner online, ainda sem custom labels;
5. repetir uma segunda vez para provar idempotência;
6. repetir nos quatro gates; zero listener SYSTEM e zero `.rollback` ao final;
7. implantar o publisher ainda desabilitado e enviar ao peer os workflows que
   exigem label de geração;
8. só então ativar o publisher de labels do `civm-host`.

## Evidência local antes do rollout

- parser PowerShell: 21/21 scripts em `deploy/windows` sem erro;
- objeto DACL em memória: arquivo e diretório protegidos;
- `File.Replace` em NTFS: conteúdo trocado e DACL protegida preservada como
  `Read, Synchronize`;
- REST autenticado em memória: 4/4 gates encontrados sem expor o token;
- fixture NTFS local e no CI Windows pago cobre junction oficial, alvo
  externo, hard link, nível aninhado, nome/versão divergentes e cadeia de
  reparse, sem travessia; 5.184 itens live nas quatro árvores em cada
  implementação;
- `go build ./...`: verde;
- `go test -race -count=1 ./...`: verde.

Validação real 4/4, reboot e GitHub online pertence ao log append-only
`validation.md`; source verde não é evidência de deploy.
