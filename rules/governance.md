---
name: governance
description: Regras para issues, PRs, labels, descrições em PT-BR e publicação segura.
paths:
  - ".github/**"
  - "CLAUDE.md"
  - "AGENTS.md"
  - "CODEX.md"
  - ".claude/rules/**"
---

# Governance rules

## Jules / agentes GitHub externos

- Jules deve usar `AGENTS.md` na raiz como arquivo de instruções do repo.
- Antes de publicar PR, Jules deve ler `.github/pull_request_template.md` e esta regra.
- Respostas ao humano, issues, PR descriptions, comentários user-facing e evidências de validação ficam em PT-BR.
- Identifiers, comentários de código, nomes de branch e títulos de commit/PR ficam em inglês; PR title usa Conventional Commits.
- Se só a metadata do PR falhar, corrigir título, corpo, labels ou assignee no GitHub e aguardar apenas `PR Governance / PR metadata guard`; não empurrar commit de código para corrigir descrição.
- Remover texto automático em inglês do corpo do PR quando conflitar com o template do repo.
- Nunca fazer auto-merge, auto-push direto para `origin/main`, `--no-verify`, deploy, rollback ou correção automática fora do escopo aprovado.

## Pull Requests

- Título do PR em inglês, formato Conventional Commits.
- Descrição do PR em Português (BR), clara para revisor fora da conversa.
- Usar `.github/pull_request_template.md` como base.
- Seções obrigatórias: `## Resumo`, `## Commits`, `## Issue`, `## Responsavel`, `## Labels`, `## Validacao`, `## Rollback trigger`.
- `## Commits` lista cada commit em tabela e usa `<details>` clicável para explicar contexto, impacto, arquivos principais e validação.
- O guard bloqueia `## Commits` vazio ou sem tabela, hash de commit e `<details>`.
- Linkar issue com keyword do GitHub: `Closes #NNN`, `Fixes #NNN` ou `Resolves #NNN`. Quando o PR de fato não tem issue (operacional, CI, docs sem rastreio formal), usar o marcador explícito `Sem issue`, `No issue` ou `N/A` na seção. Pior do que ausência é referência falsa.
- Aplicar pelo menos uma label `type:*` e uma label `area:*`.
- Atribuir o PR e a issue linkada ao responsável pela entrega.
- PR e issue linkada devem compartilhar pelo menos um assignee. Quem fecha o problema assina a entrega.
- Não deixar placeholders do template (`#NNN`, marcador pendente, `<descreva>`) no PR final.

## Proteção automática

O `civm` ainda não tem `pr-governance.yml` ativo. Enquanto esse guard não
existir, a governança de PR é regra operacional revisada por humano.

Se o guard for implementado, deve ficar neste repo, ser stdlib-only quando
possível e ter testes focados. Evitar `pull_request_target`; se ele for
inevitavel, o job nao pode fazer checkout nem executar codigo vindo da branch
do PR e nao pode expor secrets a fork.

## Documentação civm em peer repos

- Peer repo que usa a VM deve ter `docs/CIVM.md` baseado em
  `templates/CIVM-USAGE.md`.
- Docs antigas como `docs/CI-VM.md`, `docs/LOCAL-VM-CI.md` ou
  `docs/CI-LOCAL-RUNNER.md` devem virar ponte curta para `docs/CIVM.md`
  ou ficar marcadas como historicas.
- Documentacao operacional ativa nao deve usar `legacy-ci`, `ci-result`,
  `make ci-vm`, `CI_VM_*`, `acme-ci-vm-autoclean.timer` ou wrappers
  `.sh` customizados de hook como estado atual. O estado atual usa scripts
  `.sh` gerenciados por `civmctl hook install`.

## Issues

- Criar issue antes ou junto do PR quando o trabalho merece tracking (feature, bug, refactor não-trivial).
- Usar o link no PR (`Closes #NNN`, `Fixes #NNN`, `Resolves #NNN`) para manter
  rastreabilidade e fechamento automático correto.
- Para PRs puramente operacionais (CI, infra, docs sem rastreio formal), o
  marcador explícito `Sem issue`, `No issue` ou `N/A` é alternativa válida.
  Não force criação de issue artificial só para satisfazer o guard.
- Issues também devem ser compreensíveis em PT-BR, receber labels coerentes e
  estar atribuídas a quem assume a entrega.
