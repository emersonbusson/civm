# PRD — menor privilégio dos gate runners Windows

> Issue: `emersonbusson/civm#231`. Relacionada a `emersonbusson/civm-host#38`.

## Problema

Os runners `civm-gate` executam o polling definido pelo peer no host Windows.
A task legada iniciava `run.cmd` como `SYSTEM/Highest`; código de workflow
recebia autoridade administrativa e podia escrever no arquivo de admissão.
Uma ACL aparentemente read-only também era anulada por ACE herdada de
`Authenticated Users: Modify`.

## Resultado esperado

- task e processo real executam como `NETWORK SERVICE/ServiceAccount/Limited`;
- `C:\ProgramData\civm\gate\current-context` permite leitura e nega escrita
  ao gate dentro de um diretório protegido que o principal não possui;
- binários, configuração e credenciais do runner são read-only para o job;
- somente `_work` e `_diag` permitem `Modify` ao principal do gate;
- raiz compartilhada é protegida sem propagar ACL a outros gates, enquanto o
  staging e o rollback ficam SYSTEM/Administrators-only;
- o registro remoto perde a label-base no provisionamento e todas as labels
  customizadas no setup; cada transição drena por 3 segundos estáveis antes de
  permitir parar o listener local;
- uma instância SYSTEM anterior precisa terminar antes do start novo;
- qualquer pós-condição divergente deixa o gate offline e falha visivelmente.

## Fora de escopo

- conceder confiança a forks ou workflows adversariais;
- mudar RAM, VHDX, Docker, cleanup ou o runner Linux;
- atualização automática do runner: ela é desativada e o upgrade é
  reprovisionamento administrativo supervisionado.

## Rollback trigger

Reverter o rollout e deixar o gate offline se 1/4 listener não confirmar
NETWORK SERVICE, se o publisher não puder ser pausado, se a quarentena remota
não drenar, se leitura falhar, se escrita no contexto for aceita ou se o runner
não conectar após reprovisionamento.
