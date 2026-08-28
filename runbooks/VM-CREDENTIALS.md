# Runbook — VM credentials e acesso seguro

> **Princípio absoluto:** **nenhum** secret (SSH private key, senhas,
> tokens, env values reais) vai pra dentro deste repo. Mesmo sendo
> civm, mesmo private. Documentamos COMO acessar; valores ficam
> fora.

## O que vai NESTE repo (público, versionado)

- **Host pattern operacional:** ex.: "VM rodando Ubuntu 24.04 LTS, IP
  privado na rede do dono, acessível via VPN/Tailscale do operador"
- **User convention:** runner roda como usuário operacional dedicado
  (ex.: `emdev` ou `runner`) sob o próprio `$HOME`.
- **Onde ficam os work dirs:** `~/actions-runner-<short>/_work` quando
  criado por `civmctl runner add`.
- **Comandos exemplos** com placeholders: `ssh <YOUR_USER>@<VM_HOST>`
- **Permissões esperadas** em arquivos sensíveis: `~/.ssh/id_*` com
  `chmod 600`

## O que NÃO vai aqui (NUNCA)

- ❌ SSH private key (`id_ed25519`, `id_rsa`, etc)
- ❌ IPs/hostnames reais da VM
- ❌ Tokens de registro de runner GitHub (são ephemeral mas ainda
   secrets)
- ❌ PAT/GitHub App private key
- ❌ Qualquer variável de ambiente de produção
- ❌ Senhas de usuários do sistema
- ❌ Qualquer secret/token de serviço de terceiros

## Onde VALORES reais ficam

| Tipo | Onde guardar | Quem acessa |
|---|---|---|
| SSH private key do operador | `~/.ssh/` no laptop do operador | só o operador |
| IP/host da VM | env var local OU `~/.ssh/config` Host alias | operador |
| Runner registration token (GitHub) | gerado on-demand via `gh api` no momento de registrar | uma vez, descartável |
| GitHub PAT (se usar) | password manager OU GitHub Secret no peer repo | per-repo, scoped |
| GitHub App private key | password manager OU `/opt/secure/` na VM (root-only chmod 600) | runner via env |
| Runner watchdog env | `/etc/civm/runner-watchdog.env` na VM (root-only chmod 600) | `civmctl-runner-watchdog.service` |
| DATABASE_URL | GitHub Secret per-repo | workflow runtime |

## Setup de SSH para acesso à VM

No laptop do operador (NÃO no repo):

```bash
# ~/.ssh/config (NUNCA commitar; este arquivo NÃO vai pra civm)
Host civm
    HostName <SEU_IP_OU_DOMINIO>
    User <SEU_USER>
    IdentityFile ~/.ssh/<SUA_KEY>
    Port 22
```

Acesso:

```bash
ssh civm   # usa o alias do ssh/config
```

## Rotação se vazar

1. **SSH key vazada:** revogar no `~/.ssh/authorized_keys` da VM,
   gerar nova, distribuir
2. **GitHub PAT vazado:** revogar em GitHub Settings > Developer
   settings, criar novo, atualizar secrets dos repos que usavam
3. **GitHub App private key vazada:** rotacionar via página do App,
   atualizar secret nos repos que usavam
4. **Runner registration token vazado:** descartar; tokens são
   ephemeral mas se ativo, remover runner em
   `Settings > Actions > Runners > Remove`

## Histórico

- **2026-05-10** — primeira versão. Documenta princípio "credenciais
  fora do repo" + setup conventions sem expor valores.
