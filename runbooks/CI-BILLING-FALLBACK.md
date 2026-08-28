# Runbook — CI billing fallback (router automatico + fallback manual)

> **Ver tambem:**
> - [`MULTI-PROJECT-RUNNER.md`](./MULTI-PROJECT-RUNNER.md) — runner
>   self-hosted compartilhado entre N repos; setup multi-runner;
>   isolamento por job. **Doc do admin da VM**.
> - [`ORG-RUNNER-ADOPTION.md`](./ORG-RUNNER-ADOPTION.md) — passo-a-passo "1
>   comando" pra adotar civm em peer novo (template acme).
> - `civmctl billing-status` — detector Go canonico no proprio civm
>   (zero dep cross-repo, zero PAT, usa GITHUB_TOKEN auto-injetado).
>   Cada peer pode chamar diretamente sem importar nada externo.
>
> **Modelo conceitual:** o gate de verdade de cada peer roda no laptop
> ANTES de push (cada projeto define o seu — npm script, devctl Go,
> etc). Este runbook NAO descreve gate alternativo de
> validacao. Descreve **como manter o checkmark verde no PR** quando
> GitHub Actions billing esta bloqueado, porque branch protection
> precisa de algo verde mas a validacao real ja aconteceu antes do
> push.
>
> Em outras palavras: o runner self-hosted (label `civm`) e'
> **mirror visivel no GitHub**, nao "oficina alternativa de teste".
> O codigo ja foi testado local — o mirror so existe pra postar o
> resultado onde branch protection olha.
>
> **Duas camadas de mirror:**
>
> **Camada 1 — automatica (workflow router pattern):**
> Job `ci-router` em runner self-hosted `civm` consulta o detector
> heuristico (`civmctl billing-status --repo=<owner>/<repo>`) e decide
> entre `ubuntu-latest` (GitHub-hosted, custa minutos) ou `civm`
> (mirror sem custo). Job final aggregator (`Gates ...`) consolida
> resultado em civm e e' o check canonico para branch protection.
> Zero-touch: PR se beneficia automaticamente. Zero-PAT: usa
> `secrets.GITHUB_TOKEN` auto-injetado pelo Actions.
>
> **Camada 2 — manual:** quando a Camada 1 nao bastar (ex.: civm
> offline OU peer novo sem workflow refatorado), o admin roda o gate
> local do peer e posta check manual informativo na PR via gh api.
> Cada peer mantem seu "manual reporter" (acme:
> `devctl ci local --report-pr <N>`; peer: script proprio). Camada 2
> NAO e' uniforme entre peers.
>
> **Camada 3 — CI pago com aprovacao:** quando o plano GitHub permitir
> `required_reviewers` em Environments de repo privado, o peer pode usar
> o template `templates/ci-paid-approval.yml.template`: primeiro roda
> tudo em `[self-hosted, civm]`; se passar e a variavel
> `ENABLE_PAID_GITHUB_HOSTED_CI=true` estiver ligada, um preflight local
> verifica se o Environment `paid-github-hosted-ci` exige reviewers e
> impede self-review. So depois disso o job `ubuntu-latest` entra em
> estado `Waiting` para aprovacao dos admins. Sem o Environment protegido,
> o job pago nao e' agendado.
>
> **O que este runbook NAO entrega:** rotacao do GITHUB_TOKEN
> (gerenciado pelo GitHub), provisioning do runner civm (ver
> [`MULTI-PROJECT-RUNNER.md`](./MULTI-PROJECT-RUNNER.md)).

## Decisao de implementacao (Camada 1)

A primeira proposta usava PAT classico com escopo `read:billing` para
chamar `GET /users/{user}/settings/billing/actions` e ler minutos
disponiveis diretamente. Foi **rejeitada** com base em disciplina
Kahneman (`disciplines/KAHNEMAN-DISCIPLINES.md`):

1. **WYSIATI:** billing API reporta `total_minutes_used vs included_minutes`.
   Nao reporta diretamente "payment failed" — o caso de uso real. Quando
   payment trava, billing pode dizer "available > 0" enquanto runs ja sao
   rejeitados.
2. **Numero nao adjetivo:** o sinal empirico do block (3 runs failure
   em <10s) foi validado em produção 2026-05-09 contra o incidente real.
   Heuristica e' tao precisa quanto API para o gatilho que importa.
3. **Debito e divida com juros:** PAT classico exige rotacao 90d, vira
   item de calendario. GitHub App exige JWT, private key, install,
   maintenance. GITHUB_TOKEN padrao = zero debt.
4. **Lib nova exige LIBRARIES.md com rollback trigger:** PAT/App = nova
   dep operacional. Sem entrada formal, vaza com blast radius alto
   (leitura de dados financeiros).
5. **Anti-skynet:** token financeiro vaza = exposicao de billing data.
   Heuristica usa apenas leitura publica de runs do proprio repo.

Trade-off aceito: se o repo e' completamente novo (sem historico de
runs), heuristica retorna `BillingUnknown` e roteamento e' default-remote
(ubuntu-latest). Em payment failure no primeiro PR, o ci-router roda em
civm, ubuntu-latest tenta e falha em <10s, e o aggregator canonico
`Gates (typecheck, test, build, invariants)` falha. Operador entao roda
`devctl ci local --report-pr <N>` (Camada 2). Aceitavel — caso
edge, primeira sessao.

Se o trade-off virar problema (>1 falso negativo por mes), reavaliar
para GitHub App (nao PAT classico) — ver secao Rollback trigger.

## Sintomas do bloqueio de billing

1. `gh run list --workflow=ci.yml --limit=5` mostra 3+ runs consecutivos
   com `failure` em <10 segundos cada (típico 3-5s).
2. `gh run view <id>` exibe annotation:
   `"The job was not started because recent account payments have failed
   or your spending limit needs to be increased."`
3. Nenhum job dentro do run executou steps; todos `skipped` ou
   `failure` com `steps_count=0`.

## Detecção automática

Comando dedicado:

```bash
civmctl billing-status --repo=<owner>/<repo> --workflow=ci.yml
```

Output e exit code:

- `[billing] ok` → exit 0, GitHub Actions executando normalmente.
- `[billing] blocked` → exit 1, padrão de billing detectado nos 3 runs
  mais recentes.
- `[billing] unknown` → exit 2, sem dados suficientes (gh ausente,
  workflow novo sem histórico, JSON corrompido).

Heurística (em `internal/billing/billing.go`): considera
apenas runs com `startedAt` não-zero (efetivamente despachados pelo
GitHub) e classifica `blocked` quando os 3 mais recentes têm
`conclusion=failure` E `updatedAt - startedAt < 10 segundos`. Qualquer
run com duração ≥10s ou conclusão diferente de `failure` quebra o
padrão e devolve `ok`.

## Fallback manual: rodar local + reportar para PR

Quando billing está bloqueado, o gate de merge passa a ser
`devctl ci local --report-pr <N>`:

```bash
go run ./tools/devctl ci local --report-pr 42
```

Comportamento:

1. Captura stdout/stderr completo do RunLocal num buffer.
2. Roda os 5 gates fail-fast (lint, test, invariants, build, contracts).
3. Em sucesso, posta check run manual no head commit da PR #42 com
   `conclusion=success` e o output capturado como `text`.
4. Em falha, posta check run com `conclusion=failure` e o erro como
   `summary`.

Pré-requisitos:

- `gh` CLI instalado e logado (`gh auth status` retorna logged in).
- Token tem escopo `checks:write` (default do `gh auth login` web flow).
- Repo atual reconhecido por `gh repo view` (estar dentro do worktree
  do repo certo).

A check run aparece na PR igual a um job de Actions, com nome definido
pelo reporter do peer, conclusion `success` e summary dos gates.

## Fallback automático: detect + run + report

Quando o agente roda em modo autônomo e quer fluxo zero-touch:

```bash
go run ./tools/devctl ci local --auto-fallback --report-pr 42
```

Comportamento:

1. Chama `DetectBillingBlock`. Se retornar `BillingOK`, **não roda** local
   (assume que o CI remoto vai cobrir) e devolve sucesso silencioso.
2. Se retornar `BillingBlocked` ou `BillingUnknown`, prossegue com
   RunLocal e ReportLocalCIToPR como o fluxo manual.

`--auto-fallback` exige `--report-pr <N>`; sem PR número, comando aborta
com erro de validação.

## Pre-requisitos do runner civm (Camada 1)

O workflow refatorado depende do runner self-hosted `civm` estar
registrado e online. **Setup multi-runner** (varios repos do mesmo
dono da VM compartilhando o label `civm`) e' detalhe do admin
da VM, documentado em [`MULTI-PROJECT-RUNNER.md`](./MULTI-PROJECT-RUNNER.md)
secao "Setup operacional". Este repo e' agnostico de quem mais usa
o label.

Setup minimo (single-runner, single-repo):

1. **VM Linux (Ubuntu 24.04 LTS recomendado para paridade).** Pode ser
   laptop, NUC, servidor on-prem ou VM cloud — qualquer host com saida de
   internet.
2. **Software requerido na VM:**
   - `git` (>= 2.30)
   - `go` (1.26 — actions/setup-go pode instalar tambem se preferir)
   - `gh` CLI (>= 2.40) — usado pelo detector heuristico para listar
     runs. Install: `https://cli.github.com/manual/installation`
   - `curl`, `jq` (default em ubuntu)
3. **Registrar runner** em GitHub Settings > Actions > Runners > New
   self-hosted runner. Selecionar Linux x64. Seguir o script gerado
   (download + config + run). Adicionar label `civm` durante a
   config interativa.
4. **Manter online via systemd unit** (gerada pelo `./svc.sh install`
   apos config). Se cair, ci-router falha e branch protection trava
   merge — single point of failure aceitado (mitigado em multi-runner
   setup; ver MULTI-PROJECT-RUNNER.md).

**Nota de seguranca:** runner self-hosted deve executar apenas PR confiavel
ou same-repo. Para repo publico ou aceitando PRs externos, configurar
`Settings > Actions > General > Fork pull request workflows` para
"Require approval for all outside collaborators" ou desabilitar PRs externos
completamente. Evitar `pull_request_target` em workflows que possam tocar
codigo do PR e nunca expor secrets a codigo de fork em runner self-hosted.

## Configurar branch protection para aceitar Gates como required

Em GitHub Settings > Branches > Branch protection rule de `main`:

1. Habilitar `Require status checks to pass before merging`.
2. Adicionar **`Gates (typecheck, test, build, invariants)`** como check
   requerido (este e' o aggregator job no ci.yml refatorado).
3. **Remover** `lint`, `test`, `invariants`, `build`, `contracts-check`,
   `integration` da lista de required (ja sao consolidados pelo Gates).
4. Salvar.

Quando billing falhar, `ci-router` roteia para `civm`, gates rodam
la, `Gates` aggregator passa verde e merge desbloqueia automaticamente.
Quando billing voltar, mesmo workflow roteia para `ubuntu-latest`.

## Preparar CI pago com aprovacao dos admins (Camada 3)

Use esta camada quando o objetivo nao e' fallback de billing, mas sim
economizar minutos: todo PR passa primeiro no self-hosted; se e somente
se esse gate ficar verde, admins aprovam explicitamente o job pago.

O estado seguro por default e' **desligado**. Workflow preparado mas sem
variavel nao agenda `ubuntu-latest`.

### Requisitos

1. Repo privado em plano que suporte `required_reviewers` em
   Environments.
2. Workflow do peer no padrao `ci-paid-approval.yml.template` ou
   equivalente:
   - job `validate-civm` em `[self-hosted, civm]`;
   - job `paid-ci-preflight` tambem em `[self-hosted, civm]`;
   - job `paid-validate` em `ubuntu-latest` com:

     ```yaml
     environment:
       name: paid-github-hosted-ci
       deployment: false
     ```

3. Branch protection exige apenas o aggregator:
   `Gates (typecheck, test, build, invariants)`.

### Ativacao

Depois que o plano GitHub aceitar a regra de Environment, rodar:

```bash
cd /home/emdev/codespace/civm

for repo in acme/civm acme/app acme/harmya acme/menu-orders acme/barbershop acme/sample-repo acme/salon; do
  scripts/configure-paid-ci-environment.sh \
    --repo "$repo" \
    --reviewer-login other \
    --reviewer-login reviewer-two \
    --enable
done
```

O script cria/atualiza o Environment `paid-github-hosted-ci` com
`prevent_self_review=true`, valida que existe protection rule
`required_reviewers` e so entao seta:

```bash
ENABLE_PAID_GITHUB_HOSTED_CI=true
```

Se o plano ainda nao permitir required reviewers em repo privado, o
script falha e nao liga a variavel. Nao criar a variavel manualmente
antes do Environment protegido existir.

Para repos `other/*`, precisa existir outro admin/collaborator
com acesso ao repo para approval real, ou o repo precisa ser movido
para a org. Com `prevent_self_review=true`, um repo pessoal em que so
`other` e admin fica sem aprovador independente.

### Rollback

Para voltar ao modo 100% self-hosted sem remover workflow:

```bash
gh variable set ENABLE_PAID_GITHUB_HOSTED_CI --body false --repo owner/repo
```

Rollback trigger: se um job `paid-validate` iniciar em `ubuntu-latest`
sem mostrar `Waiting` para o Environment protegido, desligar a variavel
imediatamente, confirmar que `paid-github-hosted-ci` tem
`required_reviewers` + `prevent_self_review=true`, e so religar apos
um run de teste em PR descartavel.

## Fallback de emergencia (Camada 2): postar check manualmente

Se o ci-router nao conseguir rodar (ex.: civm offline) ou se o
workflow refatorado ainda nao esta presente, usar a Camada 2 manual:

1. Manter branch protection exigindo `Gates (typecheck, test, build,
   invariants)` quando o workflow existir.
2. Operador roda `devctl ci local --report-pr <N>`.
3. Check manual aparece na PR com conclusion=success/failure.
4. Se o workflow `Gates` estiver indisponivel, humano decide a excecao
   de merge com base no check manual e registra o motivo no PR.

Esta camada existe como rede de seguranca para casos onde a Camada 1
nao funciona. Se o civm voltar online, Camada 1 retoma o controle
no proximo push.

## Limitações conhecidas

- **Confiança no operador local.** O reporter posta `success` sem
  verificação independente. Operador malicioso poderia rodar uma
  versão local modificada e ainda postar success. Mitigação: log do
  output completo na check run permite review post-hoc.
- **Token escopo.** `gh auth login` default já dá `checks:write`, mas
  contas com PAT customizado precisam adicionar o escopo manualmente.
- **PR de fork não-owner.** Se a PR vem de fork e o operador não tem
  push access, posting check pode falhar com 403. Sem mitigação além
  de "owner roda o fallback".
- **Rate limit GitHub API.** 5000 req/h por user; cada `ci local
  --report-pr` consome ~3 calls (pr view + repo view + check post).
  Suficiente para uso normal.
- **Não substitui review.** Um check verde não substitui code review
  humano.

## Rollback trigger deste runbook

Se o detector marcar `blocked` quando billing está OK em mais de 1
ocorrência por semana, ajustar a heurística (relaxar threshold de 10s
para 30s ou exigir 4 runs em vez de 3).

Se o reporter postar check com `success` mas RunLocal teve falha
silenciosa (output capturado vazio), reverter a integração de
`outputCapture` e voltar ao reporter standalone manual.

Se branch protection aceitar um check manual alternativo mas merge passar
com check em estado `pending`/`neutral` (não `success`), revisar
`buildCheckRunBody` para garantir `status=completed` + `conclusion=success`
em todos os caminhos.

**Camada 1 (router workflow):** se `ci-router` mostrar latencia >30s em
mais de 3 runs consecutivos (deve ser <5s), inspecionar gh CLI no
civm (rede, auth, rate limit). Se civm ficar offline >1h, abrir
incidente — o gate `Gates` nao consegue rodar e merge fica travado.
Mitigacao temporaria: usar Camada 2 (`devctl ci local --report-pr`)
ate civm voltar, com excecao humana registrada no PR.

**Falso negativo da heuristica:** se a heuristica reportar `BillingOK`
quando billing esta de fato bloqueado em mais de 1 PR por mes, considerar
escalar para GitHub App com `read:billing` (NAO PAT classico). GitHub
App tem token efemero (1h), JWT exchange no workflow, escopo per-org,
sem rotacao manual. Setup descrito em https://docs.github.com/apps.
Migracao seria nova SPEC + ADR justificando o trade-off.

## Comandos de referência

```bash
# Detectar status
civmctl billing-status --repo=<owner>/<repo> --workflow=ci.yml

# Posting manual de check
go run ./tools/devctl ci local --report-pr 42

# Fluxo combinado (autonomy mode)
go run ./tools/devctl ci local --auto-fallback --report-pr 42

# Verificar checks postadas em uma PR (debug)
gh api repos/$(gh repo view --json nameWithOwner -q .nameWithOwner)/commits/$(gh pr view 42 --json headRefOid -q .headRefOid)/check-runs --jq '.check_runs[] | {name, status, conclusion}'
```

## Histórico

- **2026-05-10** — Primeira versão. Criada após billing block em
  2026-05-09 que bloqueou run 25611144720 e impediu janela de
  contagem do gate Tier-3 de M5. Camada 2 (manual `devctl ci
  local --report-pr`) entregue.
- **2026-05-10 (mesma sessao)** — Camada 1 entregue: refatoracao do
  ci.yml com job `ci-router` em civm usando heuristica via gh
  run list (sem PAT, sem GitHub App), conditional `runs-on` por
  job (ubuntu-latest vs self-hosted), e job aggregator `Gates
  (typecheck, test, build, invariants)` como check canonico para
  branch protection. Decisao de usar heuristica vs PAT documentada
  na secao "Decisao de implementacao".
