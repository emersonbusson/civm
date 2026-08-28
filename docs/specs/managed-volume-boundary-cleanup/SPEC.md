# SPEC — limpeza de volumes gerenciados na fronteira

> Issue: `emersonbusson/civm#181`. SSDV3 passos 2 e 2.5.

## Propriedade e classificação

O `civm` é dono da higiene dentro do guest. O `civm-host` apenas solicita a
operação e valida o exit code.

Um volume named só é elegível quando todas as condições são verdadeiras:

1. host ocioso comprovado pelo mecanismo existente;
2. nenhum docker-heavy lock ativo;
3. driver e scope pertencem à allowlist local;
4. labels de Compose contêm projeto e volume;
5. o projeto corresponde ao formato gerenciado configurado para runners CI;
6. nenhum container referencia o volume;
7. o nome resolvido no inventário é passado explicitamente a `docker volume rm`.

Prefixo isolado não basta. Volumes anônimos continuam sob o prune conservador
existente. Estado, label ou referência ambígua preserva o volume.

## Execução

1. coletar `docker volume ls/inspect` em formato determinístico;
2. pedir ao daemon somente volumes `dangling=true` e classificar em
   `managed-unused`, `unmanaged` ou `unknown`;
3. revalidar o idle guard imediatamente antes da mutação;
4. remover por nomes explícitos somente `managed-unused`;
5. coletar inventário posterior e `docker system df`;
6. retornar erro se qualquer alvo elegível persistir ou se uma remoção falhar.

A classificação e a geração de alvos são lógica pura testável. O adapter
Docker executa comandos sem shell e com deadline do contexto.

## Integração com os caminhos existentes

- `cleanup --execute --managed-volumes`, somente no boundary fechado, inclui a
  nova ação; o cleanup rotineiro não habilita a flag;
- o branch busy não remove volumes named;
- hooks de job mantêm apenas operações compatíveis com concorrência e não
  assumem que `volume prune -f` remove named;
- o resultado da ação entra no render e no exit status já usados pelo CLI.

## Testes TDD

- negativo: volume ativo gerenciado é preservado;
- negativo: volume named sem labels é preservado;
- negativo: projeto fora da allowlist é preservado;
- negativo: driver/scope desconhecido é preservado;
- efeito: `171` volumes gerenciados inativos viram `171` alvos explícitos;
- retry: falha transitória não amplia os alvos;
- exaustão: falha persistente retorna erro, sem marcar limpeza concluída;
- idempotência 2x: inventário vazio na segunda execução gera `0` remoções;
- corrida: idle reprovado na segunda checagem aborta antes de `volume rm`;
- integração: branch busy nunca emite remoção named.

## Passo 2.5 — auditoria adversarial

- **Workflow falsifica prefixo/label:** nome e label são controláveis por quem
  acessa o daemon. A classificação limita o escopo, mas a fronteira real é
  também a box ociosa e dedicada; estado ambíguo preserva.
- **Container está prestes a anexar:** a rechecagem de idle imediatamente antes
  da mutação e o boundary do host fecham a admissão concorrente; além disso,
  `volume ls` exige `dangling=true` e `volume rm` sem `--force` recusa referência
  que surgir na corrida. Se o boundary não puder ser provado, NO-GO.
- **Container parado ainda guarda dado deliberado:** projeto de run concluído é
  regenerável pelo contrato CI; projetos fora da allowlist são preservados.
- **Driver remoto/plugin:** somente driver/scope local permitido; demais são
  `unknown`.
- **Remoção parcial:** não mascarar com comando posterior; cada alvo e a
  pós-condição entram no resultado.
- **`docker volume prune -af` global:** explicitamente proibido por blast
  radius.

**Veredito:** GO para TDD da classificação e integração, condicionado à prova
de exclusão entre admissão e mutação. Sem essa prova, manter named e retornar
evidência, não ampliar o prune.

## Rollback trigger

Reverter se a lista de alvos puder conter volume ativo/não gerenciado, se o
branch busy emitir `volume rm` ou se uma falha for convertida em sucesso.
