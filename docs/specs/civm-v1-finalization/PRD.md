---
slug: civm-v1-finalization
title: civm v1 Operational Finalization
milestone: v1.0.0
issues: [3]
---

# PRD — civm v1 Operational Finalization

**Status:** approved
**Author:** sessao 2026-05-10
**Discipline links:** Kahneman #1 (WYSIATI), #3 (numero antes de adjetivo),
#6 (rollback trigger objetivo).

## 1. Resumo

Formalizar o estado atual do `civm` como **v1 operacional finalizada**.
Esta frente nao adiciona feature nova: ela registra evidencia, fecha
rastreabilidade e prepara a publicacao `v1.0.0`.

Finalizado significa: pronto para uso operacional e manutencao incremental,
nao "nunca mais mexe".

## 2. Contexto confirmado

| Gate | Estado esperado | Evidencia |
|---|---|---|
| Repo | `main` sincronizada, sem PR/issue bloqueadora, worktree limpo | `git`, `gh pr`, `gh issue` |
| Codigo | build, vet, race tests e cobertura interna passam | comandos Go locais |
| CI | ultimo run de `main` verde | GitHub Actions |
| VM | `civmctl` instalado e runner idle/saudavel | comandos read-only na VM |
| Escopo | futuras ideias ficam em `DEFERRED` | `CODEX.md` |

## 3. Objetivo

Criar uma trilha SSDV3 pequena e auditavel que permita dizer:

- qual commit representa a v1;
- quais gates foram checados;
- qual issue rastreia a formalizacao;
- qual tag/release publica o estado final;
- quais itens seguem explicitamente fora da v1.

## 4. Requisitos

- **RF-1** Criar `docs/specs/civm-v1-finalization/PRD.md`, `SPEC.md` e
  `IMPL.md`.
- **RF-2** Linkar a frente a issue GitHub `#3`.
- **RF-3** Registrar no `IMPL.md` as validacoes locais, CI e VM.
- **RF-4** Criar commit local dedicado de formalizacao.
- **RF-5** Criar tag anotada `v1.0.0` apontando para o commit de
  formalizacao.
- **RF-6** Preparar release `v1.0.0` em PT-BR com resumo, validacao e
  rollback trigger.

## 5. Fora de escopo

- Nao alterar comportamento do `civmctl`.
- Nao adicionar subcomandos.
- Nao modificar repos peer.
- Nao promover itens `DEFERRED` do `CODEX.md`.
- Nao executar cleanup destrutivo novo como parte da formalizacao.

## 6. Riscos e rollback

| Risco | Mitigacao | Rollback |
|---|---|---|
| Tag apontar para commit errado | checar `git rev-parse HEAD` antes da tag | apagar tag local/remota e release |
| Release publicada sem CI verde | checar ultimo run de `main` antes de publicar | remover release e reabrir issue |
| VM nao comprovada operacional | usar apenas comandos read-only; abortar se falhar | manter issue aberta |
| Escopo futuro virar bloqueio falso | manter `DEFERRED` documentado como fora da v1 | abrir issue separada quando gate disparar |

## 7. Criterio de aceite

O produto pode ser considerado v1 finalizado quando:

1. artefatos SSDV3 existem e citam a issue `#3`;
2. validacoes locais e CI estao registradas;
3. VM foi validada por comandos read-only ou evidencia operacional recente;
4. commit local de formalizacao existe;
5. tag `v1.0.0` existe localmente;
6. publicacao remota so ocorre com autorizacao humana explicita.
