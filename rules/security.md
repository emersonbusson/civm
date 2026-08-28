---
name: security
description: Segurança do civm — gitleaks, segredos do runner/host, runner self-hosted, privilégio mínimo, anti-skynet.
paths:
  - "**/*"
---

# Security rules

civm é infra operacional (runner self-hosted + camada host Hyper-V). A superfície
de segurança é **segredo, privilégio do host e código de PR não-confiável em
runner self-hosted** — não há web/HTTP/tenant/DB. Detalhe em `SECURITY.md` e
`disciplines/INVARIANTS.md`.

## Invariantes (CI gates)

1. **Sem secrets hardcoded** — gitleaks em pre-commit + pre-push + CI.
2. **`govulncheck` limpo** + `go vet` + `go test -race` verdes.
3. **Nunca logar token/chave/secret raw** (ver `rules/observability.md`).

## Segredos do civm

- **Nunca** commitar segredo; `.env` no `.gitignore`.
- Os segredos do civm são: o **GitHub App de release** (`RELEASE_APP_ID` +
  `RELEASE_APP_PRIVATE_KEY`, no Secrets do repo), a **chave SSH host→guest**
  (`C:\ProgramData\civm\ssh`, dona/legível só por SYSTEM) e o **token `gh`**
  (`GH_CONFIG_DIR`/PAT de contingência). Nenhum vive no repo.
- Validar na inicialização que o segredo requerido existe; rotacionar qualquer
  um exposto.

## Runner self-hosted

- Jobs em `runs-on: [self-hosted, civm]` devem rodar apenas PR confiável ou
  same-repo.
- Evitar `pull_request_target` quando qualquer step faz checkout ou executa
  código da branch do PR.
- **Nunca** expor secrets a código vindo de fork em runner self-hosted.
- Runners legacy/offline são removidos **manualmente** via `gh api -X DELETE`
  após revisão humana; `civmctl doctor` apenas reporta.

## Dispatcher JIT repository-scoped

- Autoridade vem de config 0600 fora do Git: repo, default ref, workflow,
  SHA-256, refs candidatas, runner group e job exatos.
- Token GitHub App installation/fine-grained entra somente por stdin e fica em
  memória; nunca flag, config, env child, ledger ou log.
- API pinada `2026-03-10`: dispatch aceita somente HTTP 200 com run ID/URLs
  válidos e JSON sem chave duplicada. 204/empty/partial é ambíguo, sem retry e
  sem consulta por run “mais recente”.
- Nonce usa 32 bytes de `crypto/rand`; label é repository-scoped, one-use e
  verificado antes do dispatch e novamente antes do JIT.
- O GitHub não fornece bind atômico JIT→run/job. Labels padrão podem permitir
  runner theft; sucesso só vale para o runner ID/nome/grupo exatos no job
  esperado. O roubo deve ser inofensivo dentro de VM descartável sem mounts,
  Docker do host ou secrets de produto.
- Workflow JIT só usa `workflow_dispatch` na default branch, checkout do SHA
  input, `persist-credentials: false`, zero secret de produto e zero write
  token. Fork e ref fora da allowlist causam zero efeito.
- O driver de isolamento é executável host-local pinado por SHA-256, recebe o
  segredo JIT somente após receipt `ready` durável e precisa provar
  `destroyed+reset_verified`. O runner directory nunca é montado na VM.
- Cleanup de Docker conhecido dentro da VM é fail-closed; a postcondition real
  é destruir/resetar a VM inteira. `docker system prune`, prune global de
  volumes e fstrim são proibidos no dispatcher e no host.
- O slot pesado pertence ao Guard via `guard exec`; state directory não define
  admissão. O lock fixo em `/run/civm` existe só para recovery/serialização.
- No host de `31,90 GiB`, teto WSL `20 GiB` + VM fixa `12 GiB` já excedem a
  RAM física. O Guard mantém lease global e só admite start/job após sensor
  Windows fresco pós-reclaim cobrir commit da VM, piso Windows e margem.
  Capacidade teórica, sensor stale ou reclaim solicitado falham fechado.

## Privilégio mínimo (host)

- Tasks do control plane de Hyper-V rodam como **SYSTEM** com
  Optimize-VHD/Start-VM/Get-VM, acesso a `V:` e SSH ao guest — sem segredo de
  repo embutido. Já os runners `civm-gate` rodam como
  `NETWORK SERVICE/ServiceAccount/Limited`: binários read-only, `Modify` só em
  `_work`/`_diag` e apenas `Read` no arquivo de admissão, sempre sem herança.
- Gate Windows aceita somente workflow same-repo confiável, sem fork, checkout
  ou secrets fornecidos pelo workflow. As credenciais internas do próprio
  runner continuam legíveis pelo listener; alteração de workflow por autor
  não-confiável fica fora do threat model de qualquer runner self-hosted.
- No guest, o único wrapper NOPASSWD com caminho validado é
  `civm-safedelete` (`deploy/sudoers.d/civm-cleanup`) — preferido a `NOPASSWD`
  em `rm`/`chown` crus.

## Validação de entrada

- Todo `owner/repo` vindo de input externo passa por `ValidateRepo`
  (`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`) antes de virar
  argumento de `gh api`. Nunca interpolar input não-validado em comando.

## Anti-skynet

civm **detecta, nunca corrige automaticamente**. Nunca: auto-commit/revert/push/
merge sem aprovação humana; trigger de deploy/rollback; mutar workspace de peer
sem confirmação; persistir secret em qualquer arquivo do repo; executar comando
de input externo sem validação.

## Reportar vulnerabilidade

Ver `SECURITY.md`: reportar **privadamente ao mantenedor** (canal privado, não
issue pública) antes de divulgar.

## Don't

- ❌ Hardcode de secret em qualquer arquivo do repo.
- ❌ Logar token/chave/secret raw.
- ❌ Rodar fork não-confiável em self-hosted com secrets acessíveis.
- ❌ `pull_request_target` executando código de PR de fork.
- ❌ `NOPASSWD` em comando cru destrutivo (use wrapper validado).
- ❌ Interpolar `owner/repo` não-validado em `gh api`.
- ❌ Pular gitleaks via `--no-verify`.
- ❌ Auto-mutar peer repo / VM sem aprovação humana.
