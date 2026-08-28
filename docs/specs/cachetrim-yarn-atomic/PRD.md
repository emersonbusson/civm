# PRD — Cache trim atômico por pacote (yarn)

> SSDV3 PASSO 1. Problema + requisitos. Day-0: solução definitiva, sem shim.

## Status

Proposto — 2026-06-15.

## Problema

O `internal/cachetrim.TrimByAge` impõe o cap de cada dir de cache removendo
**arquivos individuais** do mais antigo ao mais novo. Isso é seguro para caches
cujos arquivos são unidades independentes (Go build cache, golangci-lint), mas
**corrompe** caches cujo pacote é um **diretório multi-arquivo**: o cache do
yarn v1 (`<root>/v6/npm-<pkg>-<ver>-<hash>-integrity/...` + `.yarn-metadata.json`).

Remover um arquivo do meio de um pacote yarn (ex.: o `.yarn-metadata.json`,
geralmente o mais antigo) deixa o pacote **parcial**. O `yarn install
--frozen-lockfile` então falha com `ENOENT` ao abrir o arquivo que sumiu.

## Evidência (incidente 2026-06-15, #1155)

```
error Error: ENOENT: no such file or directory, open
'/home/emdev/.cache/yarn-acme-web/v6/npm-@adobe-css-tools-4.4.4-.../.yarn-metadata.json'
[x] yarn failed
error: yarn install --frozen-lockfile: exit status 1
```

O job `web` do CI consolidado falhou. `@adobe/css-tools` **não** é um dos 16
bumps — o pacote estava no cache e foi parcialmente trimado.

## Causa raiz

1. Há 4 dirs yarn (`yarn`, `yarn-acme-web`, `yarn-acme-tenant-isolation-smoke`,
   `yarn-acme-audit`). O cap de família (3GB) é dividido entre os dirs casados
   (`per = 3GB / 4 = 0.75GB/dir`).
2. Cada dir tem ~0.84GB > 0.75GB → `TrimByAge` dispara em cada um.
3. Trim por arquivo remove o arquivo mais antigo de dentro de um pacote → pacote
   parcial → `ENOENT` no install.

O cap/dir-count só decide **quando** dispara. O defeito real é **trim por
arquivo em cache de pacote multi-arquivo**.

## Requisitos

- **RF-1**: Trim de cache de pacote yarn DEVE ser **atômico por pacote** —
  remover diretórios de pacote inteiros, nunca arquivos parciais.
- **RF-2**: O disco DEVE continuar bounded (o cap de família ainda vale; o
  incidente original #124 — cache crescendo sem limite — não pode regredir).
- **RF-3**: Caches de arquivo independente (Go build, golangci-lint) PERMANECEM
  em trim por arquivo (já são seguros; mudar seria churn sem ganho).
- **RF-4**: A correção DEVE ser validada **por efeito** (#13): o job `web` do
  #1155 passa após deploy, não só "o código existe".

## Não-objetivos

- Corrupção por **escrita concorrente** (2 jobs no mesmo dir yarn) — mitigada por
  dirs per-workflow + `--mutex network`. Fora de escopo.
- npm `_cacache` e pnpm store são content-addressed (resilientes a remoção
  parcial — re-fetcham o que falta). Não precisam de atômico.

## Disciplinas Kahneman

- **#13** (existência ≠ função): validar por efeito — o `web` verde, não o build.
- **#16** (fail-safe não morre com o recurso): o cap protege o disco, mas a cura
  (trim) não pode destruir o recurso que protege (o cache). Trim atômico cura
  sem corromper.
