# Runbook — GitHub App com Plan: Read (rota de upgrade do detector billing)

> **Quando usar:** se a heuristica via `gh run list` (atual) gerar >1 falso
> negativo por mes (i.e., classificar BillingOK quando billing esta de fato
> bloqueado), considerar upgrade para detecao via API direta de billing
> usando GitHub App com escopo `Plan: Read`.
>
> **Status:** rota documentada, **nao implementada**. Implementar apenas se
> rollback trigger da heuristica disparar (ver
> [`CI-BILLING-FALLBACK.md`](./CI-BILLING-FALLBACK.md) §Rollback trigger).

## Por que GitHub App em vez de PAT classico

Comparado a Classic PAT com escopo `read:billing`:

| Dimensao | GitHub App | Classic PAT |
|---|---|---|
| **Token expiry** | Efemero (1h, gerado por JWT) | Indefinido ou 90d max |
| **Rotacao** | Automatica via JWT exchange | Manual (item de calendario) |
| **Escopo** | Per-org, per-permission | Amplo (usuario inteiro) |
| **Audit trail** | Logs por install + per-call | Logs ao nivel do user |
| **Vazamento blast radius** | Limitado a 1h e ao escopo do install | Acesso completo aos repos do user |
| **Setup** | Medio (criar app + install + JWT no workflow) | Trivial (gerar token + secret) |
| **Conta solo (1 repo)** | Overkill mas defensavel | Aceitavel com cuidado |
| **Org com 5+ repos compartilhando** | Recomendado | Anti-pattern |

Para repo solo, ambos sao defensaveis. Para conta com varios repos
sob o mesmo dono compartilhando infra de billing detection, GitHub
App escala melhor.

## Setup passo a passo

### 1. Criar GitHub App

- Acessar `https://github.com/settings/apps/new` (user-level) OU
  `https://github.com/organizations/<org>/settings/apps/new` (org-level).
- Preencher:
  - **GitHub App name:** `civm-billing-detector` (ou similar)
  - **Homepage URL:** URL do repo (cosmetico)
  - **Webhook:** desabilitar (este App nao recebe events)
  - **Permissions:**
    - `Plan: Read` — necessario para billing API
    - `Actions: Read` — necessario se quiser tambem listar runs (compatibilidade
      com heuristica fallback)
    - Demais: nenhum (principle of least privilege)
  - **Where can this GitHub App be installed?:** "Only on this account"
- **Create GitHub App.**

### 2. Gerar private key

- Na pagina do App criado, scroll ate "Private keys" > "Generate a private key".
- Download do arquivo `.pem`. Armazenar em local seguro (nao commitar).
- Anotar o **App ID** (numero proximo ao topo da pagina).

### 3. Instalar o App nos repositorios

- Pagina do App > "Install App" no menu lateral.
- Selecionar conta/org > "Only select repositories" > marcar este repo
  (e demais que compartilharem a infra de billing detection, conforme
  decisao do admin da VM).
- **Install.** Anotar o **Installation ID** (visivel na URL apos install:
  `https://github.com/settings/installations/<INSTALLATION_ID>`).

### 4. Adicionar secrets ao workflow

Em cada repo > Settings > Secrets and variables > Actions:

- `BILLING_APP_ID`: o App ID (numero)
- `BILLING_APP_PRIVATE_KEY`: conteudo do `.pem` (multi-line, comecando com
  `-----BEGIN RSA PRIVATE KEY-----`)
- `BILLING_APP_INSTALLATION_ID`: o Installation ID (numero)

### 5. Refatorar `ci-router` step "Decide runner"

Substituir o passo atual (chamada a `civmctl billing-status`) por:

```yaml
- name: Decide runner (GitHub App billing API)
  id: decide
  env:
    BILLING_APP_ID: ${{ secrets.BILLING_APP_ID }}
    BILLING_APP_PRIVATE_KEY: ${{ secrets.BILLING_APP_PRIVATE_KEY }}
    BILLING_APP_INSTALLATION_ID: ${{ secrets.BILLING_APP_INSTALLATION_ID }}
    OWNER: ${{ github.repository_owner }}
  run: |
    set -uo pipefail

    # Generate JWT (short-lived) signed with the App private key.
    # Requires `openssl` (default Ubuntu).
    header='{"alg":"RS256","typ":"JWT"}'
    now=$(date +%s)
    exp=$((now + 540)) # 9 min, GitHub max is 10
    payload=$(printf '{"iat":%d,"exp":%d,"iss":%s}' "$now" "$exp" "$BILLING_APP_ID")

    b64() { openssl base64 -A | tr -- '+/' '-_' | tr -d '='; }
    header_b64=$(printf '%s' "$header" | b64)
    payload_b64=$(printf '%s' "$payload" | b64)
    unsigned="${header_b64}.${payload_b64}"
    signature=$(printf '%s' "$unsigned" | openssl dgst -sha256 -sign <(printf '%s' "$BILLING_APP_PRIVATE_KEY") | b64)
    jwt="${unsigned}.${signature}"

    # Exchange JWT for installation token (1h validity).
    install_token=$(curl -sS -X POST \
      -H "Authorization: Bearer $jwt" \
      -H "Accept: application/vnd.github+json" \
      "https://api.github.com/app/installations/$BILLING_APP_INSTALLATION_ID/access_tokens" \
      | jq -r .token)

    if [ -z "$install_token" ] || [ "$install_token" = "null" ]; then
      echo "::warning::JWT exchange failed; defaulting to remote"
      echo "use_local=false" >> $GITHUB_OUTPUT
      exit 0
    fi

    # Detecta tipo do owner.
    owner_type=$(curl -sS \
      -H "Authorization: Bearer $install_token" \
      "https://api.github.com/users/$OWNER" | jq -r .type)
    if [ "$owner_type" = "Organization" ]; then
      url="https://api.github.com/orgs/$OWNER/settings/billing/actions"
    else
      url="https://api.github.com/users/$OWNER/settings/billing/actions"
    fi

    # Read billing.
    body=$(curl -sS -H "Authorization: Bearer $install_token" \
      -H "Accept: application/vnd.github+json" "$url")
    included=$(echo "$body" | jq -r '.included_minutes // 0')
    used=$(echo "$body" | jq -r '.total_minutes_used // 0')
    paid=$(echo "$body" | jq -r '.total_paid_minutes_used // 0')
    available=$(( included - used ))

    if [ "$available" -ge 10 ] || [ "$paid" -gt 0 ]; then
      echo "use_local=false" >> $GITHUB_OUTPUT
      echo "route_reason=billing-app-ok-available-$available" >> $GITHUB_OUTPUT
    else
      echo "use_local=true" >> $GITHUB_OUTPUT
      echo "route_reason=billing-app-exhausted" >> $GITHUB_OUTPUT
    fi
```

### 6. Verificar

```bash
# Manual smoke (fora do CI):
# Substitua APP_ID, INSTALLATION_ID, PEM_FILE pelos seus.
APP_ID=12345
INSTALLATION_ID=67890
PEM_FILE=/path/to/key.pem
OWNER=other

# Generate JWT (use uma lib ou comando openssl como no workflow)
# ...

# Trocar JWT por install token e chamar billing
curl -sS -H "Authorization: Bearer $INSTALL_TOKEN" \
  https://api.github.com/users/$OWNER/settings/billing/actions
```

Esperado: JSON com `included_minutes`, `total_minutes_used`, etc.

## Quando NAO migrar

- Heuristica atual passou nos ultimos 90d sem falso negativo
- Volume de runs e' baixo (<100/mes)
- Repo unico (sem motivacao de escala)

Nesses casos, manter heuristica via `gh run list` + `GITHUB_TOKEN`.

## Rollback

Se a integracao do App causar mais ruido que valor (ex.: JWT exchange
falhando, App revogado, secrets vazados), reverter para heuristica em
1 commit:

```bash
git revert <commit-do-app-migration>
# Verifica que ci-router voltou a chamar civmctl billing-status
civmctl billing-status --repo=<owner>/<repo> --workflow=ci.yml
```

Custo de rollback: zero. Heuristica continua funcional como fallback ate
o App ser fixado.

## Historico

- **2026-05-10** — Primeira versao do runbook. Documentado mas **nao
  implementado** (heuristica atual e suficiente). Rota de upgrade pronta
  para quando rollback trigger da heuristica disparar.
