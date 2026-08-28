# Runbook — adotar civm no acme (sem atrito)

> **Audiência**: você (admin) ou qualquer agente futuro que receba pedido
> "rodar acme na VM". Este runbook deixa adoção em 1 comando + cópia
> de template + push.

## Pré-requisitos

- `gh auth status` autenticado com escopo `repo` na conta dona do acme
- SSH funcional pra VM `gha-ubuntu-2404` (Tailscale ou rede local)
- `civmctl` >= versão com `runner add` e checksum pinado do
  actions/runner (sessão 2026-05-10+)

## Passo 1 — Runner do acme: usar o runner ORG (NAO registrar por-repo)

> **CRITICO (serializacao):** o acme e servido pelo runner ORG
> `civm-acme-org` (registrado contra `https://github.com/acme`), que atende
> `acme/app` E `acme/civm` num **unico processo**. **NAO** registre um runner
> POR-REPO `civm-app` para `acme/app` — dois runners civm-labeled servindo o
> mesmo repo = dois jobs concorrentes no mesmo disco -> `concurrent prune on
> shared civm runner` mata o `docker pull` de um deles (incidente #1184). Ver
> [`RUNNER-SERIALIZATION.md`](./RUNNER-SERIALIZATION.md).

### Registrar o runner ORG (manual, uma vez)

O `civmctl runner add` so cobre runners **por-repo** (`--repo=owner/repo`). O
runner org e registrado **manualmente** em GitHub Settings > Actions > Runners da
**organizacao** `acme`:

1. Em `https://github.com/organizations/acme/settings/actions/runners`, clicar
   **New runner** (Linux x64) e copiar o token de registro.
2. Na VM, instalar contra a URL da **org** (nao do repo), com label `civm` e nome
   `civm-acme-org`:

```bash
# Na VM gha-ubuntu-2404:
mkdir -p ~/actions-runner-acme-org && cd ~/actions-runner-acme-org
# (baixar/extrair o tarball actions/runner — mesma versao dos demais runners)
./config.sh --unattended --url https://github.com/acme \
  --token "<ORG_TOKEN>" --labels civm --name civm-acme-org --work _work --replace
sudo ./svc.sh install emdev
sudo ./svc.sh start
```

> Por que manual: a API publica de registration-token de **org** exige PAT com
> escopo de admin da org; o `civmctl runner add` foi desenhado para o caminho
> por-repo (token efemero por repo). Registrar o runner org e um passo
> operacional one-time, nao zero-effort.

### Validar online

```bash
# O runner ORG aparece em /orgs/acme, NAO em /repos/acme/app:
gh api orgs/acme/actions/runners --jq '.runners[]|"\(.name) \(.status) \(.labels[].name)"'
# Esperado: civm-acme-org online civm

# Garantir que NAO existe runner por-repo civm-app:
gh api /repos/acme/app/actions/runners --jq '.runners[]|"\(.name) \(.status)"'
# Esperado: vazio
```

E na VM:

```bash
ssh gha-ubuntu-2404 "systemctl is-active actions.runner.acme.civm-acme-org.service"
# Esperado: active
```

### Se um runner por-repo civm-app ja existir (heranca / re-provisao errada)

Remova-o (NAO so `systemctl disable`, que o watchdog ressuscita):

```bash
# Do host Windows:
powershell -NoProfile -ExecutionPolicy Bypass -File C:\civm-deploy\serialize-runner.ps1 -Execute
# ou no guest:
TOKEN=$(gh api -X POST /repos/acme/app/actions/runners/remove-token --jq .token)
sudo civmctl runner remove --short=acme --token="$TOKEN" --execute
```

## Passo 2 — Adotar workflow (quando você decidir push em acme)

O peer repo atualmente roda 100% em `ubuntu-latest` (workflows `go.yml`,
`web.yml`). Pra ativar fallback billing-block via civm, adicione um
job router que decide entre `ubuntu-latest` e `[self-hosted, civm]`.
Seguranca: usar o self-hosted apenas para PR confiavel/same-repo; evitar
`pull_request_target` e secrets em jobs que executem codigo de fork.

Template pronto:

```bash
cp ~/codespace/civm/templates/acme-ci-router.yml.template \
   ~/codespace/acme/.github/workflows/ci-router.yml
```

O template:
- Roda `ci-router` em `[self-hosted, civm]` (decide via `civmctl billing-status`)
- Output `use_local` (true se billing-blocked)
- Gates aggregator condicional (paralelo aos `go.yml`/`web.yml` existentes)

Push trigger natural (push no acme) executa o router. Não precisa
modificar `go.yml`/`web.yml` na primeira iteração — coexistem.

### Adoção minimal (sem alterar workflows existentes)

Só adicionar `ci-router.yml` no acme. Os workflows go.yml e web.yml
continuam rodando em ubuntu-latest (com risco de billing block matar
em <10s). O router vai postar `Gates (civm fallback)` como check
adicional, sempre verde quando billing-block ativa fallback.

### Adoção avançada (Tier 2: rotear go.yml e web.yml)

Editar `go.yml` e `web.yml` pra adicionar `runs-on:` dinâmico igual
peer faz:

```yaml
runs-on: ${{ needs.ci-router.outputs.use_local == 'true' && fromJSON('["self-hosted","civm"]') || 'ubuntu-latest' }}
```

Nota: jobs `services` matrix de go.yml requer ferramentas Go disponíveis
na VM (já estão: Go 1.26.3 instalado pelo bootstrap). Web job requer
Node 24 (já está: v24.15.0 LTS Krypton via nvm).

## Filosofia "sem atrito"

| Decisão | Razão |
|---|---|
| 1 comando registra runner | civmctl encapsula toda a sequência, dry-run default |
| Template separado em `ci-router.yml` | Não toca workflows existentes; coexiste; rollback trivial via `git rm` |
| Token efêmero gh api | Sem PAT persistente; sem rotação; sem secret novo |
| Label `civm` reuso | Mesmo padrão dos peers já adotados; zero divergência |
| Push trigger natural | Sem precisar `workflow_dispatch:` adicionado |

## Rollback

Remover o runner do acme da box (desfaz a adocao por completo). Isso para o
runner ORG — acme volta a depender 100% de `ubuntu-latest`:

```bash
# Remove o runner ORG (Settings > Actions > Runners da org > Remove) e na VM:
ssh gha-ubuntu-2404 "cd ~/actions-runner-acme-org && sudo ./svc.sh stop && \
  sudo ./svc.sh uninstall && ./config.sh remove --token '<ORG_REMOVE_TOKEN>' && \
  rm -rf ~/actions-runner-acme-org"
```

> **Nao** remova so o runner org e registre um por-repo no lugar achando que e
> "mais simples" — e a topologia que causou o #1184. Se o runner org for
> gargalo, ver `RUNNER-SERIALIZATION.md` §"Rollback trigger".

Se template `ci-router.yml` quebrar workflow acme:

```bash
cd ~/codespace/acme
git rm .github/workflows/ci-router.yml
git commit -m "revert: ci-router fallback"
git push
```

Workflows `go.yml`/`web.yml` continuam rodando 100% em ubuntu-latest
como antes (com risco de billing-block continuar matando jobs em <10s).

## Critério de sucesso

- `gh api orgs/acme/actions/runners` retorna `civm-acme-org online` (com label
  `civm`), e `gh api /repos/acme/app/actions/runners` retorna **vazio** (sem
  runner por-repo)
- `civmctl doctor --repos=auto --json` retorna o check `RUNNER_SERIALIZATION` em
  severidade `ok`
- Push em acme dispara `ci-router` que roda em `civm-acme-org`
  (verificar via `gh run view <id> --json jobs --jq '.jobs[] | "\(.name) runner=\(.runnerName)"'`)
- Em billing-block: jobs `gates-civm` rodam no `civm-acme-org` enquanto
  go.yml/web.yml morrem em ubuntu-latest; pelo menos UM job verde permite branch
  protection passar (após configurar required check)
- `gh api orgs/acme/actions/runners --jq '[.runners[]|select(.busy)]|length'`
  retorna `<= 1` sob carga (serializado)

## Histórico

- **2026-05-10** — Runbook criado. Sessão de migração peers para
  padrão civm/civmctl. acme decidiu adoção minimal (apenas runner +
  template, sem modificar workflows existentes do acme) por filosofia
  "senior, sem atrito" do usuário.
- **2026-06-18** — Passo 1 corrigido: acme usa o runner ORG `civm-acme-org`
  (serializa acme/app + acme/civm num processo), NÃO um runner por-repo
  `civm-app`. Origem: incidente #1184 (`concurrent prune` com 2 runners acme).
  Ver `RUNNER-SERIALIZATION.md`.
