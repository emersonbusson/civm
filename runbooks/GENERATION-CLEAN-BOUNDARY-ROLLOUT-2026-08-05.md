# Validacao live da fronteira limpa — 2026-08-05

## Escopo e limite da evidencia

Esta entrada registra o rollout guest-first do commit `59cb68e` do `civm` e
do controlador C# correspondente. Antes deste documento ser publicado, ainda
nao havia sido executado um PR canario posterior ao rollout; build e teste
local nao substituem essa prova.

## Pre-condicoes medidas

- filas `peer/app` e `owner/civm`: `0` queued e `0` in progress;
- VM `gha-ubuntu-2404`: `Off`, memoria atribuida `0` e checkpoints
  automaticos desabilitados;
- `V:`: `83,85 GiB` livres;
- `V:\civm-current-context`: vazio;
- `V:\civm-reclaim.lock`: ausente;
- `civm-host-orchestrator`: owner unico; owner PowerShell legado `Disabled`.

Dois runs de maio apareciam como `queued`, mas o endpoint de cancelamento os
classificava como concluidos. O PR, a branch e o SHA ja nao existiam. Os IDs
`26423751663` e `26423751642` foram removidos de forma exata pela API; a fila
voltou a `[]`.

## Rollout do guest

O primeiro `self-upgrade` recompilou o source staged em `/opt/civm`, mas esse
source estava em `c3bf40e`, anterior ao merge. A divergencia foi detectada
antes de promover o host. O worktree de deploy existente, sem mudancas
tracked, foi atualizado para `59cb68e`; nenhum clone novo foi criado.

Depois da recompilacao e do `hook install --no-restart`:

- `civmctl capability generation-clean-boundary` retornou
  `civm-generation-boundary/v1`;
- o wrapper root-owned retornou o mesmo marcador;
- `civmctl doctor --repos=auto --json` retornou `exit=0`;
- `civmctl health --json` retornou `exit=0`;
- reaper timer ficou `enabled+active` e service com resultado `success`;
- runner serial ficou `1/1 active/running` e `idle-check` retornou `0`.

## Rollout do host e compactacao

O binario do host foi trocado com a task owner desabilitada e backup
recuperavel. Um tick shadow decidiu `stop_and_compact` com `queued=0`,
`running=0` e `V:=76 GiB` enquanto a VM estava ligada.

O tick ativo manteve o contexto em `reclaim`, executou o wrapper do guest,
chegou a `Off` por poweroff gracioso e compactou o VHDX:

| Medida | Antes | Depois |
| --- | ---: | ---: |
| VHDX | `35,38 GiB` | `32,20 GiB` |
| `V:` livre | `76,00 GiB` | `86,94 GiB` |
| reclaim lock | presente durante o efeito | ausente |
| contexto publicado | `reclaim` durante o efeito | `0 bytes` |

Um trigger recorrente recebeu `0x800710e0` porque
`MultipleInstances=IgnoreNew` recusou sobreposicao enquanto o processo
original ainda possuia o lock. O processo original nao foi interrompido. O
heartbeat posterior terminou com `LastTaskResult=0`, task `Ready`, VM `Off`,
zero processo `civm-host`/SSH orfao e owner legado ainda `Disabled`.

## Veredito

PASS para rollout guest-first, owner unico, cleanup, poweroff gracioso,
compactacao, piso fisico de 80 GiB e canario self-hosted. O canario final
esta registrado abaixo.

## Canario do PR 230

O SHA `4fa5424` disparou o workflow completo. Na primeira tentativa, antes da
variavel opt-in existir no contexto do run, os tres jobs GitHub-hosted e o
agregador passaram; o smoke self-hosted foi `skipped`. A tentativa 2 do mesmo
run, depois de habilitar `CIVM_SELF_HOSTED_SMOKE=true`, criou o job real com
labels `[self-hosted, civm]`.

Antes de publicar esse contexto, o controlador mediu e executou:

| Instante UTC | Acao | `V:` livre | Contexto publicado |
| --- | --- | ---: | --- |
| `09:39:48` | boot de manutencao | `86 GiB` | vazio |
| `09:43:40` | cleanup + compactacao | `79 -> 87 GiB` | vazio |
| `09:45:42` | boot separado da geracao | `87 GiB` | vazio |
| `09:47:52` | admissao/hold | `80 GiB` | `pr-230@4fa5424...` |

Na admissao, o guest reportou `32 GiB` livres. O runner Linux `2.336.0`
estava `online`, `idle`, com labels `self-hosted,civm`, e o grupo `Default`
autorizava explicitamente `owner/civm`. Mesmo assim, o endpoint do job da
tentativa rerun permaneceu `queued` e `runner_id=0`; nao houve atribuicao pelo
scheduler do GitHub. Este commit cria um SHA novo no mesmo PR para validar o
dispatch fresco, a cura do SHA anterior e a segunda geracao consecutiva.

O job de PR permaneceu sem atribuicao porque `owner/civm` e publico e o grupo
`Default` tinha `allows_public_repositories=false`. Um grupo temporario foi
criado com acesso somente a `owner/civm` e ao workflow
`ci.yml@refs/heads/main`. O POST de criacao nao aplicou o repositorio
selecionado; a pos-condicao mostrou `total_count=0`. Depois do PUT explicito,
a pos-condicao passou a `total_count=1`.

Por seguranca, o grupo nao foi aberto a todos os workflows de pull request.
O canario confiavel foi disparado por `workflow_dispatch` no `main`, pinado
ao workflow permitido. O primeiro job criado antes da associacao explicita
nao foi reavaliado; um run novo do mesmo SHA recebeu o runner em `7 s`.

## Resultado final do canario

Run `30997503263`, contexto
`branch-main@59cb68e257dabd014bfdd9d0ab5be9910a9a1f07`:

- cleanup e compactacao terminaram antes da admissao;
- `V:` passou de `80` para `87 GiB` na compactacao e tinha `80 GiB` com a
  VM ligada na admissao;
- guest reportou `32 GiB` livres;
- runner `civm-runner-org`, ID `71`, recebeu o job pelo grupo temporario;
- smoke self-hosted passou em `37 s`, incluindo health, build, escalacao
  controlada e cleanup do workspace;
- o tick `10:29:46Z` manteve `hold`, com `running=1`, depois do smoke acabar
  e enquanto o job GitHub-hosted ainda rodava;
- workflow terminou com `5/5` jobs aprovados.

Depois do canario, a variavel opt-in foi removida, o runner voltou ao grupo
privado `Default`, o grupo temporario vazio foi apagado e o listener voltou
`online`, `idle`. Nao ficou acesso self-hosted habilitado para repo publico.

WYSIATI: foi observado um canario real e duas fronteiras consecutivas. Nao
foi executada carga de aplicacao nesse smoke; carga pertence ao black-box do
peer, nao ao rollout do controlador.

Rollback trigger: restaurar o backup do controlador e manter o gate fechado
se um worker for interrompido, uma geracao publicar abaixo de 80 GiB, o lock
ficar sem owner ou um retry recuperavel deixar de ocorrer.
