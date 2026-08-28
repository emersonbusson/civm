# PR-DESCRIPTION-STYLE.md — regra portátil de visibilidade de commits

> **Como adotar:** copiar o bloco entre os marcadores
> `<!-- PR-COMMIT-VISIBILITY:BEGIN -->` e `<!-- PR-COMMIT-VISIBILITY:END -->`
> para o `.github/pull_request_template.md` do repo logo abaixo do
> heading `## Commits`. Manter os marcadores intactos para o auditor
> futuro do repo consumidor conseguir verificar.
>
> **Por quê este snippet existe:** vários repos do mesmo dono adotaram
> o template civm com seção `## Commits` em formato tabela
> (`Commit | O que fez | Por que fez | Detalhes`). Em PRs com 15+ commits
> agentes começaram a usar `<details>` agrupador para "esconder" commits
> considerados secundários, deixando só uma sub-categoria visível na
> tabela e o restante atrás de "clique para expandir". Isso quebra o
> contrato visual do reviewer — quem abre o PR espera ver toda a lista
> de commits no preview, sem precisar clicar.
>
> Per-row `<details>` no campo `Detalhes` continua sendo o padrão e a
> base do guard `tools/check-pr.ts` (que exige `<details>` na seção).
> O proibido é `<details>` que envolve múltiplas linhas de tabela.
>
> **Onde colocar:** dentro de `.github/pull_request_template.md`, logo
> abaixo do heading `## Commits` e antes da tabela de exemplo.

---

<!-- PR-COMMIT-VISIBILITY:BEGIN -->
> **Regra de visibilidade dos commits:** todos os commits do PR aparecem
> como linhas visíveis na tabela `## Commits`, mesmo quando o PR tem 20+
> commits. **Nunca** envolver múltiplas linhas dentro de um `<details>`
> agrupador — o reviewer precisa ver a lista completa no preview inicial
> do PR, sem precisar clicar para expandir. O `<details>` por linha (no
> campo `Detalhes`) é obrigatório e cumpre o papel de esconder o contexto
> profundo (arquivos, validação, rollback). Agrupamento por categoria
> editorial (ex.: "core" vs "fechamento", "frontend" vs "backend") vai
> no `summary` da própria linha ou em texto curto no campo `Detalhes`,
> nunca em `<details>` que oculte commits.
<!-- PR-COMMIT-VISIBILITY:END -->

---

## Como auditar (futuro, todos os repos)

Implementação proposta para um auditor `pr-style` do repo consumidor:

1. Ler último PR aberto via `gh pr view --json body,number`.
2. Em `## Commits`, identificar quantas linhas de tabela existem dentro de cada `<details>`.
3. Falhar se algum `<details>` contém mais de uma linha começando com `|` (sintaxe de tabela markdown).
4. Per-row `<details>` continua passando — só falha se o conteúdo do `<details>` contém múltiplas linhas de tabela ou um pipe `|` em posição de começo de linha.

Até o auditor existir, regra fica documentada via:

- `.github/pull_request_template.md` (instrução visível para o autor do PR)
- `.claude/rules/governance.md` (regra para agentes que escrevem o PR)
- Memory de cada agente (feedback do dono)

## Histórico

- **2026-05-10** — primeira versão. Criada após PR #4 do peer apresentar 16 commits dobrados em `<details>` agrupador, frustrando o reviewer ao ver só 5 linhas no preview. Regra retroativamente aplicada em peer no mesmo PR.
