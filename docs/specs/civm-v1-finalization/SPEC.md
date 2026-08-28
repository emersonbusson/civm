---
slug: civm-v1-finalization
title: civm v1 Operational Finalization SPEC
milestone: v1.0.0
issues: [3]
---

# SPEC — civm v1 Operational Finalization

**PRD:** `docs/specs/civm-v1-finalization/PRD.md`
**Status:** approved
**Discipline links:** Kahneman #1 (WYSIATI), #3 (numero antes de adjetivo),
#6 (rollback trigger objetivo).

## 1. Principio de implementacao

Esta SPEC e de governanca/versionamento. O implementador nao deve mudar
codigo Go, workflows, systemd units ou comportamento operacional.

O unico output de repo deve ser documentacao SSDV3 e append em `MEMORY.md`.

## 2. Issue de rastreabilidade

- Usar issue `#3`: `Formalizar civm v1.0.0 operacional`.
- Labels aceitos no estado atual do repo: `documentation`, `area:civmctl`.
- A issue permanece aberta ate a formalizacao local estar concluida.
- Fechar a issue somente depois de tag/release remotas existirem ou depois
  do humano confirmar que a publicacao remota nao sera feita nesta rodada.

## 3. Validacoes obrigatorias

Executar e registrar no `IMPL.md`:

```bash
git status --short --branch
git branch --merged main
git branch -r --merged origin/main
gh pr list --state open --json number,title,url
gh issue list --state open --json number,title,labels,url
go vet ./...
go build ./...
go test -race -count=1 ./...
go test -count=1 -cover ./internal/...
git diff --check
gh run list --branch main --limit 1 --json databaseId,status,conclusion,workflowName,headSha,url
```

Validar VM por comandos read-only quando SSH estiver disponivel:

```bash
ssh gha-ubuntu-2404 'command -v civmctl && sha256sum /usr/local/bin/civmctl'
ssh gha-ubuntu-2404 'civmctl health'
ssh gha-ubuntu-2404 'civmctl doctor --json'
ssh gha-ubuntu-2404 'civmctl idle-check'
```

Nao rodar `civmctl cleanup --execute` nesta SPEC. A evidencia de cleanup real
vem do rollout do PR `#2`, onde `_work/_tool` e `_work/_actions` foram
preservados.

## 4. Passos de implementacao

1. Criar `PRD.md`, `SPEC.md` e `IMPL.md` em
   `docs/specs/civm-v1-finalization/`.
2. Executar as validacoes obrigatorias.
3. Preencher `IMPL.md` com:
   - commit base antes da formalizacao;
   - issue `#3`;
   - resultados dos comandos;
   - ultimo run CI em `main`;
   - evidencias VM;
   - escopo fora da v1.
4. Fazer append em `MEMORY.md` com resumo da formalizacao.
5. Criar commit local:

```text
docs: formalize civm v1 operational completion
```

Body em PT-BR, sem markdown, com:

```text
Rollback trigger: se a tag v1.0.0 apontar para commit sem CI verde ou sem VM validada, remover release/tag e reabrir a issue.
```

6. Criar tag anotada local:

```bash
git tag -a v1.0.0 -m "v1.0.0 - civm operational baseline"
```

7. Preparar release notes em PT-BR. Publicar release remota somente se houver
   autorizacao humana explicita para publicar tag/release.

## 5. Abort triggers

- Abortar antes do commit se qualquer validacao local falhar.
- Abortar antes da tag se `git status` nao estiver limpo apos o commit.
- Abortar publicacao remota se o ultimo CI de `main` nao estiver verde.
- Abortar publicacao remota se SSH/VM nao puder provar estado operacional e
  nao houver evidencia operacional recente suficiente no `IMPL.md`.

## 6. Rollback

Se a tag/release forem publicadas incorretamente:

```bash
gh release delete v1.0.0 --cleanup-tag --yes
git tag -d v1.0.0
git fetch --prune --tags origin
```

Depois, reabrir issue `#3` com o motivo e repetir os gates.
