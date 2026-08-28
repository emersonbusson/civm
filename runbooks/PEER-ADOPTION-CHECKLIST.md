# Runbook — peer adoption checklist (manual, per-repo)

> Use este checklist quando um peer repo (peer, acme, futuro repo X)
> for adotar o padrão civm. Cada peer commita independentemente em
> branch isolada.

## Pré-requisitos

- [ ] civm repo presente localmente (ex.: `~/codespace/civm/`; ajuste o path
      para a máquina do operador)
- [ ] Runner GitHub do peer registrado e online com label `civm`
- [ ] Workflow usa `runs-on: [self-hosted, civm]` nos jobs que devem cair
      no runner local
- [ ] Self-hosted roda apenas PR confiavel/same-repo; forks externos,
      `pull_request_target` e secrets em jobs self-hosted foram revisados
- [ ] Working tree do peer está clean OU você sabe o que está
      uncommitted (não vai perder nada)
- [ ] git branch atual é segura (não main de produção)

## Padrão documental obrigatório

Cada peer repo deve ter uma doc curta e atual em `docs/CIVM.md`. Ela
explica como o repo consome a VM, mas não duplica o runbook de
administração da VM.

Fonte canônica para copiar:

```bash
mkdir -p docs
cp "${CIVM_REPO:-$HOME/codespace/civm}/templates/CIVM-USAGE.md" docs/CIVM.md
```

Depois de copiar, substituir apenas o bloco "Gate local do projeto" pelo
comando real do peer. Se o repo já tiver `docs/CI-VM.md`,
`docs/LOCAL-VM-CI.md` ou `docs/CI-LOCAL-RUNNER.md`, manter esse arquivo
como ponte curta para `docs/CIVM.md` ou remover em PR dedicado. Não
deixar conteúdo operacional duplicado.

Termos que não podem sobreviver em docs ativas, exceto em arquivo
histórico/migração explicitamente marcado:

- `ci-vm`
- `legacy-ci`
- `ci-result`
- `make ci-vm`
- `CI_VM_HOST`, `CI_VM_USER`, `CI_VM_PASSWORD`
- `~/.config/acme/ci-vm.env`
- `acme-ci-vm-autoclean.timer`
- wrappers shell customizados em hooks de job; o contrato atual usa
  scripts `.sh` gerenciados por `civmctl hook install`

## Passo 0 — Backup do estado atual

```bash
cd ~/codespace/<peer-repo>
git status --short
# Se WIP: commit em branch atual OU stash com cuidado (risco de
# perder untracked se -u). Avalia caso a caso.
```

## Passo 1 — Criar branch isolada

```bash
git checkout -b chore/adopt-civm
```

## Passo 2 — Decidir destino de MEMORY.md vs conversa.md

```bash
# Se peer tem AMBOS, deletar conversa.md e manter MEMORY.md:
[ -f conversa.md ] && [ -f MEMORY.md ] && git rm conversa.md

# Se peer tem só conversa.md, renomear:
[ -f conversa.md ] && [ ! -f MEMORY.md ] && git mv conversa.md MEMORY.md

# Se peer tem só MEMORY.md, nada a fazer.
# Se não tem nenhum, criar MEMORY.md (Passo 5).
```

## Passo 3 — Criar/atualizar docs/CIVM.md

```bash
mkdir -p docs
cp "${CIVM_REPO:-$HOME/codespace/civm}/templates/CIVM-USAGE.md" docs/CIVM.md
```

Editar `docs/CIVM.md` para trocar o gate de exemplo pelo comando real
do projeto. Adicionar um link curto para `docs/CIVM.md` em `README.md`
e nos arquivos de instrução (`AGENTS.md`, `CLAUDE.md`, `CODEX.md`) que
existirem.

## Passo 4 — Criar CODEX.md se não existir

Conteúdo minimal (header + cross-refs + snippet COMMUNICATION-STYLE
do passo seguinte + histórico).

## Passo 5 — Inserir snippet COMMUNICATION-STYLE em CLAUDE/AGENTS/CODEX

Copiar bloco entre marcadores `<!-- COMMUNICATION-STYLE:BEGIN -->`
e `<!-- COMMUNICATION-STYLE:END -->` de
`<civm>/templates/COMMUNICATION-STYLE.md` para o final
de cada arquivo `CLAUDE.md`, `AGENTS.md`, `CODEX.md`.

Adicionar logo abaixo: `> Source canônico: <civm>/templates/COMMUNICATION-STYLE.md`

## Passo 6 — Criar MEMORY.md se não existir

Header com formato append-only canônico (campos branch/scope/goal/
actions/validations/commits/open-items/next-step).

Primeira entrada documenta esta adoção.

## Passo 7 — Adotar workflow CI (escolher tier)

```bash
mkdir -p .github/workflows

# Tier 3 (recomendado para repos novos — zero auth, self-healing):
cp "${CIVM_REPO:-$HOME/codespace/civm}/templates/ci-optimistic.yml.template" \
   .github/workflows/ci.yml

# OU Tier 1 (router pattern):
cp "${CIVM_REPO:-$HOME/codespace/civm}/templates/ci-router.yml.template" \
   .github/workflows/ci.yml
```

EDITAR: substituir steps "echo PLACEHOLDER" pelos gates reais do peer.
Conferir que todo job local usa `runs-on: [self-hosted, civm]` ou o
conditional equivalente `fromJSON('["self-hosted","civm"]')`.

## Passo 8 — Commit

```bash
git add README.md CLAUDE.md AGENTS.md CODEX.md MEMORY.md docs/CIVM.md .github/workflows/ci.yml
git commit -m "chore: adopt civm pattern (docs + style + ci workflow)"
```

## Passo 9 — Restaurar WIP (se houve stash)

```bash
git checkout <branch-original>
git stash pop
```

## Passo 10 — Branch protection no GitHub (admin)

Settings → Branches → main → Require status checks:

- [ ] Adicionar `Gates (typecheck, test, build, invariants)` como required
- [ ] Remover jobs individuais antigos (lint, test, etc)

## Passo 11 — Verificar adoção/saúde da fleet

Antes de publicar PR de adoção ou investigar CI quebrado, consolidar os
sinais operacionais dos peers:

```bash
civmctl peer-status --repos=owner/a,owner/b --workflow=ci.yml
civmctl peer-status --repos=owner/a,owner/b --workflow=ci.yml --json
```

Exit codes: `0=ok`, `1=warn`, `2=critical`. O comando é read-only:
mostra billing, runners online e último run por peer, mas não corrige
workflow, runner, branch protection ou workspace automaticamente.

## Verificação

```bash
for f in CLAUDE.md AGENTS.md CODEX.md; do
  grep -qF "<!-- COMMUNICATION-STYLE:BEGIN -->" "$f" && echo "OK: $f" || echo "FAIL: $f"
done
[ -f .github/workflows/ci.yml ] && python3 -c "import yaml; yaml.safe_load(open('.github/workflows/ci.yml'))" && echo "ci.yml OK"
rg -n "legacy-ci|ci-result|make ci-vm|CI_VM_|acme-ci-vm|ci-vm" README.md AGENTS.md CLAUDE.md CODEX.md docs .github/workflows
civmctl peer-status --repos=owner/a,owner/b --workflow=ci.yml
```

## Histórico

- **2026-05-10** — primeira versão. Criada após acme adoption ser
  bloqueada por classifier de auto-mode (73 uncommitted files). Este
  checklist permite adoção manual sem risco de perder WIP.
