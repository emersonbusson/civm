# PRD — recalibração do piso de disco do guest

> Issues: `emersonbusson/civm#182` e `emersonbusson/civm-host#18`.

## Problema

O guest atual tem disco de `40 GiB` e filesystem `/` de `37,70 GiB`.
`DefaultMinFreeGB=58` e `GuestFloorGb=40` vieram da topologia anterior de
`108 GiB` e são fisicamente inalcançáveis.

Em 30/07/2026, mesmo com `V:` host em `80,50 GiB` após compactação, o
orquestrador repetiu `reclaim_before_admit` porque o guest reportava
`14 GiB`. A poda named pendente representa `10,44 GB`, mas ainda não existe
medição pós-limpeza 2x nem high-water do maior job na geometria atual.

## Resultado esperado

- Floors ficam menores que a capacidade útil e maiores que o high-water medido.
- Full clean saudável não dispara novamente sem nova alocação relevante.
- Civm, civm-host, vetores de paridade e docs usam os mesmos números.
- Configuração impossível falha alto no bootstrap/doctor.

## Fora de escopo

- Escolher número por estimativa antes de `emersonbusson/civm#181`.
- Expandir/mover o VHDX.
- Reduzir margem para apenas fazer checks verdes.

## Rollback trigger

Reverter se 3 admissões consecutivas repetirem reclaim sem ganho ou se um job
medido atingir o hard floor por falta de margem.
