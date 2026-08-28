# SPEC — gate runner Windows com menor privilégio

> Issues: `emersonbusson/civm#231`, `emersonbusson/civm#234`. SSDV3 passos 2 e 2.5.

## Invariantes

- A task usa SID `S-1-5-20`, `ServiceAccount` e `Limited`.
- A DACL do runner é protegida e contém somente SYSTEM, Administrators e
  NETWORK SERVICE. O gate recebe `ReadAndExecute` na raiz e `Modify` apenas em
  `_work` e `_diag`.
- As DACLs de `C:\ProgramData\civm\gate` e `current-context` são protegidas,
  removem herança e concedem ao gate `ReadAndExecute` no diretório e `Read` no
  arquivo; SYSTEM e Administrators preservam `FullControl`.
- O runner precisa ter `disableUpdate=true`; executáveis nunca ficam graváveis
  pelo job. Upgrade exige reprovisionamento com versão pinada.
- A enumeração não atravessa reparse points. As únicas exceções são as
  junctions oficiais `bin` e `externals`, cada uma com alvo único no diretório
  sibling `<nome>.2.336.0`, que precisa existir como diretório real e não pode
  ser outro reparse point. Hard links são sempre rejeitados.
- O publisher precisa estar `Disabled`; o provisionador remove `civm-gate`,
  prova 3 segundos estáveis sem `busy`/Worker e só então desabilita as fontes
  locais. O setup remove todas as labels customizadas, repete o dwell e deixa o
  listener online sem elegibilidade; apenas o publisher restaura as duas labels.
- O token administrativo entra como `SecureString`, cria o registration token
  via REST e nunca é gravado em arquivo, argumento nativo ou log.
- Antes do registro, a fonte de restart é desabilitada; o script aborta sem
  matar um `Runner.Worker`, encerra o listener ocioso e desregistra a task.
- Após o start, a definição e o owner do único `Runner.Listener.exe` precisam
  confirmar NETWORK SERVICE. Instância antiga ou owner divergente falha.
- Um probe efêmero como NETWORK SERVICE precisa ler o contexto e falhar ao
  escrever, criar sibling, apagar, renomear ou alterar DACL. Também precisa
  falhar ao escrever em `run.cmd`, `.runner`, credenciais e listener.
- Gates aceitam apenas workflows same-repo confiáveis, sem fork, checkout ou
  secrets fornecidos pelo workflow. A credencial interna do runner continua
  legível pelo listener; NETWORK SERVICE ainda possui capacidades de host e
  rede.
- O nome Windows permanece `civm-<owner>-gate-<index>` para que `--replace`
  substitua os quatro registros vivos. O detector `-gate` de `internal/runner`
  enumera somente units systemd do guest e não participa deste cohort.

## Auditoria adversarial

- ACE explícita `Read` não reduz uma ACE herdada `Modify`; por isso cada item
  do runner, o diretório de contexto e seu arquivo ficam protegidos, sem
  herança, e são verificados após `Set-Acl`.
- `Register-ScheduledTask -Force` não troca o token de uma instância em curso;
  por isso stop, wait e owner do processo são pós-condições separadas.
- `Modify` na raiz permitiria substituir `run.cmd`; por isso apenas diretórios
  de trabalho e diagnóstico são graváveis e auto-update fica desativado.
- `run.cmd` copia `run-helper.cmd` em todo start; a task executa diretamente
  `bin\Runner.Listener.exe run` para manter a raiz imutável.
- O provisionamento sempre usa diretório novo, runner 2.336.0, SHA-256 pinado,
  checagem do exit nativo e DACL SYSTEM/Administrators antes do download. Um
  único `.rollback`, também protegido, é removido após o setup verde.
- Raízes são validadas sem travessia antes da quarentena. Depois do drain e do
  stop, o walker recupera acesso administrativo somente em diretórios reais,
  sem herança, e rejeita links antes de tocá-los. Só após a auditoria completa
  o handoff reescreve DACLs e executa o swap.
- A recuperação preserva o inventário binário de ACEs. Uma regra administrativa
  FullControl já aplicável é reutilizada; regra Allow existente mas insuficiente
  falha fechada; na ausência dela, uma ACE raw não herdável é inserida com
  `SetFileSecurityW`, sem propagação aos filhos, sem ampliar nem remover ACE
  herdável. O multiset binário usa comparação ordinal case-sensitive.
- A raiz compartilhada da fleet nunca é reescrita por uma operação de gate
  individual. Quando existente, sua DACL protegida e sem herança deve estar
  canônica no preflight anterior à quarentena; drift falha sem derrubar o gate
  e sem propagar ACL para hardlinks de outro gate.
- `-ResumeStaged` retoma apenas staging existente e exige publisher desligado,
  zero processo, `.rollback` ausente, versão/config/agent ID pinados, runner
  remoto offline/idle e zero labels customizadas. Falha no segundo move restaura
  o diretório antigo; o staging nunca é removido automaticamente.
- Rejeitar todo reparse point também rejeita o layout oficial do runner. A
  exceção é estreita por nome, versão, cardinalidade, parent e tipo de alvo;
  junction externa, symlink ou cadeia de reparse continua falhando antes da
  quarentena.
- Para todo arquivo não-reparse, os dois walkers exigem exatamente um hardlink
  via metadata nativa do handle. Em `AccessDenied`, habilitam
  `SeBackupPrivilege` só durante a consulta e restauram o estado anterior;
  erro nativo ou contagem diferente de um falha antes de `Set-Acl` no arquivo
  ou em seu alvo externo.
- Falha após o start desabilita/desregistra a task e encerra somente o listener
  do diretório, após remover novamente as labels customizadas. `Runner.Worker`
  nunca é morto pelo rollout; se surgir, a compensação falha visivelmente e
  preserva o job.
- Falha intermediária prefere gate offline a restaurar automaticamente uma
  task SYSTEM. Rollback privilegiado continua decisão humana.
- O host e seus administradores são confiáveis. Árvores maliciosas deixadas por
  jobs são rejeitadas depois de provar zero Worker e parar o listener; mutação
  concorrente por administrador local está fora do modelo de ameaça.

## Testes

- contrato Go exige principal, DACL protegida, raiz read-only, subdiretórios
  graváveis, quarantine-before-stop, owner real, listener direto e runner com
  auto-update desativado;
- parser PowerShell valida os dois scripts;
- fixture NTFS local e no CI Windows pago aceita as junctions oficiais e
  rejeita alvo externo, hard link, nível aninhado, nome/versão divergentes e
  cadeia;
- fixture elevada parte de uma ACL legada não enumerável e prova recuperação
  diretório a diretório sem resíduo; execução não elevada falha antes de criar
  a fixture;
- fixture elevada restringe a DACL de um hardlink, comprova contagem nativa 2,
  rejeição nos dois walkers e SDDL externo idêntico;
- fixture elevada combina uma ACE administrativa herdável, uma ACE ausente e
  hardlink externo com DACL divergente não protegida; prova inserção raw,
  rejeição e SDDL externo idêntico;
- as quatro árvores live são enumeradas sem atravessar as oito junctions;
- probe real de deploy valida leitura aceita e escrita negada sob SID S-1-5-20;
- suíte Go com race detector permanece verde.

## Rollback trigger

Interromper o rollout se qualquer ACL contiver principal inesperado, se o probe
conseguir escrita, se mais de um listener sobreviver ou se o owner real não for
`NT AUTHORITY\NETWORK SERVICE`.
