# Release automation (release-please)

Status: ativo desde 2026-05-11. Mantido em `.github/workflows/release.yml`
+ `release-please-config.json` + `.release-please-manifest.json`.

## Como funciona

1. Cada push em `main` dispara `release.yml` (job `release-please`).
2. `googleapis/release-please-action@v4` lê os commits desde a tag mais
   recente (`v<X.Y.Z>` no manifest) e calcula o proximo bump por
   Conventional Commits:
   - `fix:` -> patch (1.0.0 -> 1.0.1)
   - `feat:` -> minor (1.0.0 -> 1.1.0)
   - `feat!:` / `BREAKING CHANGE:` em footer -> major (1.0.0 -> 2.0.0)
   - `docs:`, `chore:`, `test:`, `style:`, `build:` -> sem bump
   - `ci:`, `refactor:`, `perf:` -> sem bump (entram so no CHANGELOG)
3. Se ha pelo menos 1 commit bumpavel, abre/atualiza um PR
   `chore: release civm v<X.Y.Z>` com:
   - `.release-please-manifest.json` bumpado.
   - `CHANGELOG.md` regerado com as secoes configuradas.
4. Mergear esse PR cria a tag `v<version>` e publica o GitHub Release
   automaticamente. release-please nao escreve em `main` fora desse PR.

O repo usa manifest mode com `separate-pull-requests=false`; por isso o
titulo do PR agrupado e controlado por `group-pull-request-title-pattern`.
`pull-request-title-pattern` fica igual para manter parsing consistente de
PRs individuais caso a estrategia mude no futuro.
Os dois patterns usam `civm` como texto cosmetico no titulo, nao como
`${component}` nem como `package-name`. Em manifest mode agrupado com
`separate-pull-requests=false`, a branch gerada e
`release-please--branches--main`, sem componente. Se a config definir
`package-name: civm`, o release-please espera componente na branch e
aborta com `PR component: undefined does not match configured component:
civm` antes de criar a tag.
Esse contrato fica coberto por `TestReleasePleaseGroupedModeIsComponentless`
e `TestReleasePleaseTitlePatternsParseMergedGroupedPR` em
`internal/specs/release_please_config_test.go`.

## Anchor de bootstrap (`last-release-sha`)

`release-please-config.json` define
`packages[\".\"].last-release-sha` apontando para o merge commit
`7d987077...` que carrega a tag `v1.1.0`. Esse anchor existe porque o
release `v1.0.0` foi criado manualmente antes do release-please ser
adotado, e por isso nao tem PR `autorelease: tagged` associado. Sem o
anchor, release-please nao reconhecia `v1.1.0` como seu release e
abortava com `untagged, merged release PRs outstanding` a cada push.

Quando `v2.0.0` for criado pelo proprio release-please (ciclo limpo),
o anchor pode ser removido com seguranca; ele so existe para colar o
gap entre a era manual e a era automatizada. Trocar o SHA do anchor
exige issue + PR como qualquer outra mudanca de config (sync rule
nao se aplica pois nao toca docs autoritativos).

## Conventional Commits cheat-sheet

```
<type>(<scope opcional>): <descricao curta em ingles imperativo>

<corpo em PT-BR, linhas <=72 chars, sem markdown>

[BREAKING CHANGE: <descricao>] [opcional, somente major]
Rollback trigger: <gatilho objetivo>   # obrigatorio em feat/fix/refactor/perf
```

Types validos: `feat`, `fix`, `refactor`, `perf`, `docs`, `ci`, `test`,
`chore`, `build`, `style`.

## Token

O caminho primario usa GitHub App dedicado e token efemero gerado por
`actions/create-github-app-token@v3`.

Setup do App:

1. Criar GitHub App dedicado para release automation.
2. Instalar apenas em `acme/civm`.
3. Conceder permissoes minimas:
   - `contents: write`
   - `pull-requests: write`
   - `issues: write`
   - `metadata: read`
4. Adicionar em <https://github.com/acme/civm/settings/secrets/actions>:
   - `RELEASE_APP_ID` = App ID numerico.
   - `RELEASE_APP_PRIVATE_KEY` = private key PEM do App.

Nao colar a private key em runbook, commit, log ou issue. Validar apenas
nomes de secrets com `gh secret list --repo acme/civm`.

Fallbacks, em ordem:

1. `RELEASE_PLEASE_TOKEN` (PAT legado): funciona, mas exige token pessoal
   com escopos `repo` + `workflow` e rotacao manual.
2. `GITHUB_TOKEN`: contingencia final. Ele depende da permissao do repo
   "Allow GitHub Actions to create and approve pull requests" para abrir
   PR e, mesmo quando abre, PRs criados por `GITHUB_TOKEN` nao disparam
   outros workflows como `ci.yml`.
3. Re-run manual: quando release-please abrir o PR sem CI, `gh pr ready
   <num>` + close/reopen forca novo evento de PR.

## Operacao diaria

```bash
# Ver PRs de release pendentes
gh pr list --repo acme/civm --label "autorelease: pending"

# Inspecionar o PR de release antes do merge
gh pr view <num> --repo acme/civm

# Mergear (squash). Tag + release sao criados imediatamente.
gh pr merge <num> --repo acme/civm --squash
```

## Override de versao

Caso precise pular um bump sugerido (e.g. forcar major sem `feat!:`):

1. No PR aberto pelo release-please, comentar `release-as: 2.0.0`.
2. release-please reabre o PR com a versao forcada.

Ou ajustar `release-please-config.json` (campo `release-as`) e fazer
commit em `main` direto pela governance normal (issue + branch + PR).

## Validacao apos primeiro merge

```bash
gh release list --repo acme/civm
git fetch --tags origin
git tag --list 'v*'
```

Confirma que a primeira tag `v1.0.1` (ou superior) foi criada e
`CHANGELOG.md` aparece em `main`.

## Rollback trigger

Se release-please:

- Calcular bump incorreto persistentemente,
- Falhar criando PR/tag por permissao,
- Causar dois releases consecutivos sem changelog significativo,

desabilitar o workflow (`gh workflow disable release.yml`), reverter
o manifest pra ultima tag valida e revisar a config + token antes de
reativar.
