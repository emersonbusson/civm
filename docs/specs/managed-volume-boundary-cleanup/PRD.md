# PRD — limpeza de volumes gerenciados na fronteira

> Issue: `emersonbusson/civm#181`. SSDV3 passo 1.

## Problema

O `civm` afirma remover volumes Docker sem uso, mas executa
`docker volume prune -f`. Na versão atual do daemon, o comando remove somente
volumes anônimos. Em 30/07/2026, a box continha:

- `174` volumes locais;
- `0` volumes ativos;
- `10,44 GB` recuperáveis;
- ao menos `171` volumes Docker Compose nomeados `acme-org-<run-id>_*`;
- `3` volumes anônimos sem labels.

Assim, hooks e cleanup terminam com sucesso sem entregar o efeito documentado.
O crescimento atravessa gerações e reduz a consistência com um runner pago
descartável.

## Resultado esperado

- O boundary ocioso remove volumes nomeados gerenciados por runs concluídos.
- Volumes ativos, não gerenciados, sem identidade conclusiva ou de driver não
  permitido são preservados.
- Nenhuma poda global de volumes nomeados ocorre enquanto existe job/build.
- A ação registra inventário before/after, contagem e bytes recuperados.
- Falha parcial retorna erro; o host não pode interpretar a limpeza como
  concluída.
- A segunda execução não remove nada adicional nem falha.

## Fora de escopo

- Remover volumes arbitrários criados pelo operador.
- Confiar somente em prefixo controlável pelo workflow como fronteira de
  autorização.
- Alterar a compactação/energia da VM; isso pertence ao `civm-host`.
- Tornar o Docker compartilhado estruturalmente idêntico a uma VM descartável.

## Rollback trigger

Reverter se 1 volume ativo ou não gerenciado for removido, se a limpeza named
rodar com atividade comprovada ou se 3 boundaries ociosos consecutivos falharem
por classificação incompatível com volumes criados pelos workflows oficiais.
