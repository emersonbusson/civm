# COMMUNICATION-STYLE.md — snippet portátil de regra de estilo

> **Como adotar:** copiar o bloco markdown abaixo (entre os marcadores
> `<!-- COMMUNICATION-STYLE:BEGIN -->` e `<!-- COMMUNICATION-STYLE:END -->`)
> para dentro de `CLAUDE.md`, `AGENTS.md` e `CODEX.md` do seu repo.
> Manter os marcadores intactos para um auditor do repo consumidor conseguir
> verificar.
>
> **Por quê este snippet existe:** força agentes (Claude, Codex, Aider,
> Jules) a gerarem relatórios de fim de sessão, MEMORY entries e respostas
> a "explica o que isso quer dizer" no formato Tech Lead pragmático em vez
> de listagem crua de commits. Qualquer repo do mesmo dono pode adotar
> esta regra para experiência consistente entre projetos.
>
> **Onde NÃO colocar:** `MEMORY.md`, `conversa.md`, ou qualquer arquivo
> append-only de log. Esses são lidos para contexto histórico, não como
> instrução. A regra deve viver em arquivos de instrução autoritativos
> (CLAUDE/AGENTS/CODEX).

---

<!-- COMMUNICATION-STYLE:BEGIN -->
## Communication & report style (output guidelines)

Ao gerar relatório de fim de sessão, changelog, explicação de implementação OU resposta a "o que isso quer dizer / como funciona", **priorize clareza e impacto**, não listagem crua de commits. Estrutura obrigatória:

1. **TL;DR (1-2 parágrafos):** qual era o maior problema e como a solução o resolveu. Foco no **por quê** antes do como.
2. **Impacto prático:** traduza sopa de letrinhas (arquivos, funções) em consequência real. Em vez de "refatorei docker.go", diga "isolei containers para projetos X e Y não conflitarem porta".
3. **Divisão por tópicos:** agrupe mudanças em categorias lógicas (Resiliência, Cobertura, Banco, etc.) com bullets, não bloco único.
4. **O que ficou para trás / próximos passos:** o que falta, o que precisa ação humana (ex.: "configurar branch protection no GitHub"), o que está bloqueando.

**Tom de voz:** Tech Lead pragmático repassando a branch — direto, focado em valor entregue, sem repetir cada hash de commit.

**Aplica-se a:** mensagens user-facing pós-sessão, MEMORY.md entries, descrições de PR, respostas a "explica o que isso quer dizer", auto-reports.

**Não aplica-se a:** commit messages individuais (essas seguem Conventional Commits + body factual), output literal de tools (logs de gh/git).

Esta regra tem precedência sobre verbosity preferences. Se conflitar com instrução de "respostas curtas", manter a estrutura mas comprimir cada seção.
<!-- COMMUNICATION-STYLE:END -->

---

## Como auditar (todos os repos)

O comando de auditoria é responsabilidade do repo consumidor. No `civm`,
os marcadores são checados pelo CI em `.github/workflows/ci.yml`.

Exit codes:
- `0` — todos os 3 arquivos (CLAUDE/AGENTS/CODEX) contêm a seção
- `1` — pelo menos 1 arquivo está sem a seção (output lista quais)
- `2` — pré-requisitos faltando (ex.: arquivos não existem)

Adoção sugerida: rodar o auditor no pre-commit hook do repo OU em CI step
informativo (sem bloquear merge inicialmente).

## Histórico

- **2026-05-10** — primeira versão. Criada como template portátil para
  garantir que múltiplos repos do mesmo dono adotem a mesma regra de
  estilo de comunicação.
