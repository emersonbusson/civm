# CIVM-USAGE.md — template portatil para peer repos

> Como adotar: copiar este arquivo para `docs/CIVM.md` no peer repo e
> ajustar apenas o bloco "Gate local do projeto". Nao copiar runbook de
> administracao da VM para o peer; essa fonte permanece em
> `<civm>/runbooks/`.

# civm — CI compartilhado deste repo

Este repo usa `civm` como runner self-hosted do GitHub Actions para postar
o check visivel no PR. A validacao de produto continua sendo o gate local
do proprio repo, rodado antes de push.

## Padrao atual

- Workflow GitHub Actions: definido pelo peer repo em `.github/workflows/`
- Runner label: `runs-on: [self-hosted, civm]`
- Check required em branch protection: nome do gate agregador do peer
- Fonte operacional da VM: checkout local do repo `civm`
- Runbook admin da VM: `<civm>/runbooks/MULTI-PROJECT-RUNNER.md`

## Gate local do projeto

Substituir este bloco pelo comando real do peer antes de commitar:

```bash
# Exemplo; nao copiar literalmente se o repo tiver outro gate.
npm run lint
npm test
npm run build
```

## Segurança

- Self-hosted runner deve executar apenas PR confiavel/same-repo.
- Nao usar `pull_request_target` com jobs self-hosted.
- Nao expor secrets a codigo vindo de fork externo.
- `civm` executa o workflow do peer; ele nao audita codigo sozinho.
- Hooks de job usam scripts `.sh` gerenciados por
  `civmctl hook install --execute`; nao usar wrappers customizados nem
  symlinks diretos para o binario Go.

## Operacao da VM

Comandos read-only uteis para diagnostico:

```bash
civmctl parity
civmctl health
civmctl capacity --json
civmctl doctor --repos=auto --json
civmctl idle-check --json
civmctl metrics dump --stdout
civmctl actions-metrics --org=<org> --period=month --repos=auto --json
```

Comandos mutaveis (`bootstrap`, `cleanup --execute`, `hook install --execute`,
`runner watchdog --execute`, `runner restart/remove/upgrade --execute` e
`self-upgrade --execute`) devem seguir os runbooks do repo `civm` e os guards
de host idle.

## Termos obsoletos em docs ativas

Nao usar estes nomes em documentacao operacional nova:

- `ci-vm`
- `legacy-ci`
- `ci-result`
- `make ci-vm`
- `CI_VM_HOST`, `CI_VM_USER`, `CI_VM_PASSWORD`
- `~/.config/acme/ci-vm.env`
- `acme-ci-vm-autoclean.timer`
- wrappers shell customizados em hooks de job; o contrato atual usa
  scripts `.sh` gerenciados por `civmctl hook install`

Se algum termo acima aparecer, ele deve estar em arquivo historico,
arquivo de migracao ou bloco explicitamente marcado como legado.

## Como verificar

```bash
rg -n "legacy-ci|ci-result|make ci-vm|CI_VM_|acme-ci-vm|ci-vm" \
  README.md AGENTS.md CLAUDE.md CODEX.md docs .github/workflows
```

Resultado esperado: nenhum match em docs ativas, exceto contexto historico
explicitamente marcado.

## Histórico

- 2026-05-17 — template criado para padronizar a documentacao de VM/civm
  entre os repos que compartilham a VM.
