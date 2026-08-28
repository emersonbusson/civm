# CLAUDE.md — civm

> **ATENÇÃO:** Mantenha este arquivo minúsculo. As regras específicas do projeto vivem em [`rules/*.md`](rules/) (na raiz do repo, convenção do civm). Não copie longos dossiers aqui.

## Fonte de verdade das regras de agente

`AGENTS.md`, `CODEX.md` e [`rules/*.md`](rules/) são os documentos autoritativos. Este `CLAUDE.md` é o override específico do Claude Code; `AGENTS.md` é o fallback quando não há `CLAUDE.md` (ver `AGENTS.md` § Para agentes externos).

Antes de planejar, editar ou abrir PR:

1. Leia este arquivo, `AGENTS.md`, `CODEX.md` e `MEMORY.md` (de baixo para cima).
2. Para "isso está funcionando agora?", consulte/atualize `validation.md` (log append-only de TODA validação de infra; escopo em [`rules/observability.md`](rules/observability.md) § Log de validação empírica).
3. Siga sempre [`rules/coding-style.md`](rules/coding-style.md) para formatação, naming e PRs.
4. Para segurança, testes e observabilidade, leia [`rules/security.md`](rules/security.md), [`rules/testing.md`](rules/testing.md), [`rules/observability.md`](rules/observability.md).
5. Em PRs, siga [`rules/governance.md`](rules/governance.md) (template de PR, tabela de commits, sync rule).
6. Mudança estrutural, novo comando `civmctl`, contrato de runner ou hook: siga **SSDV3** ([`rules/ssdv3.md`](rules/ssdv3.md)).

## Sync rule (invariante #14)

`README.md`, `AGENTS.md`, `CODEX.md` e `rules/*.md` são autoritativos. Mudança em um requer mudança nos outros no mesmo commit. Para alterar só um, inclua `[sync-skip-justified]` no body do commit.

## Metodologias Core

- **Kahneman Disciplines**: decisões de arquitetura/power-state/disk-safety seguem as disciplinas de Kahneman ([`disciplines/KAHNEMAN-DISCIPLINES.md`](disciplines/KAHNEMAN-DISCIPLINES.md)). Evite "Sistema 1"; registre counterfactuals e triggers de rollback.
- **SSDV3**: Spec-Driven Development. Pipeline PRD → SPEC → IMPL. Veja [`rules/ssdv3.md`](rules/ssdv3.md).

## Commits & PRs

- **Inglês** em: code, comentários, identifiers, branches, CLI flags, títulos de commit (Conventional Commits: `feat(scope): title`), arquivos `.go`/`.yml`/`.yaml`.
- **PT-BR** em: `README.md`, `AGENTS.md`, `CODEX.md`, `MEMORY.md`, `runbooks/*.md`, mensagens CLI ao usuário, body de commits, PRs e issues.
- Commits estruturais (power-state, scale-to-zero, disk-safety) requerem um `Rollback trigger:` no body.

## Visão geral da stack

- **CLI**: Go 1.26 — `civmctl` (`github.com/emersonbusson/civm`), provisiona e mantém a VM self-hosted (GitHub Actions runner, label `civm`).
- **VM**: Ubuntu 24.04 LTS em paridade de efeito com `ubuntu-latest`,
  gerenciada via Hyper-V no host; **scale-to-zero**. Geometria atual:
  disco guest `40 GiB`, `/` útil `37,70 GiB`, `V:` host `119,24 GiB` e
  baseline Off/compactado `80,50 GiB`. `GuestFloorGB=40` e
  `MinFreeGB=58` são legados inalcançáveis sob correção coordenada
  (`civm-host#18`, issues #181/#182).
- **Operação privilegiada é esperada**: `sudo civmctl bootstrap`, `systemctl`, units systemd e timers fazem parte do fluxo normal — diferente de repos de aplicação, aqui `sudo` é legítimo.

Consulte [`rules/*.md`](rules/) para as diretrizes profundas de cada tópico.
