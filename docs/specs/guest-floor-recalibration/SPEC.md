# SPEC — recalibração do piso de disco do guest

> SSDV3 passos 2 e 2.5. Estado: **NO-GO para alterar números**.

## Medições obrigatórias

1. Entregar a limpeza scoped de `emersonbusson/civm#181`.
2. Em boundary fechado, medir `df -B1 /` antes, depois e após segunda execução.
3. Rodar o maior smoke autorizado e medir mínimo livre/high-water.
4. Repetir pelo menos 3 vezes sem cache e 3 vezes com cache.
5. Escolher `MinFreeGB` e `GuestFloorGb` com margem numérica documentada.

## Invariantes

- `0 < hard floor < floor de admissão < capacidade útil`.
- Floor de admissão é alcançável após limpeza 2x.
- High-water não cruza hard floor.
- Anti-deadlock não substitui configuração correta.
- Mudança coordenada nos 2 repos e nos vetores PowerShell/C#.

## Passo 2.5 — auditoria adversarial

- Reduzir para os `14 GiB` atuais cristalizaria um estado sujo.
- Usar `14 + 10,44` como resultado presume reclaim byte-a-byte e ignora
  filesystem/TRIM.
- Um único smoke pode subestimar o pior serviço.
- Expandir floor acima da capacidade recria loop de compactação.
- Alterar somente C# quebra paridade com o contrato guest/legado.

**Veredito:** NO-GO até limpeza live 2x e high-water repetido. A documentação
de hardware pode ser corrigida agora; os números de decisão permanecem
explicitamente marcados como legados inválidos.

## Rollback trigger

Reverter se os dados de repetição não sustentarem a margem ou se os 2 repos
divergirem.
