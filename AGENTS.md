# AGENTS.md — civm

Resumo terso para CLIs estilo Codex/aider/Jules. Para visão completa, ler `README.md`.

## Propósito do repo

`civm` is open-source tooling for self-hosted GitHub Actions runners
(guest Linux + optional Windows host helpers). It hosts:

1. **`civmctl`** — Go CLI zero-effort para provisionar e manter a VM
   self-hosted que serve como GitHub Actions runner com label `civm`.
2. **Templates de workflow** copiáveis pelos peer repos.
3. **Template `docs/CIVM.md`** para peer repos documentarem como usam a VM.
4. **Runbooks operacionais** da VM (provisionamento, cleanup, troubleshooting).
5. **Disciplinas e regras** portáveis (Kahneman, SSDV3, invariantes).
6. **Dispatcher JIT externo one-shot** para peers explicitamente allowlisted,
   sem daemon e sem substituir a fronteira de isolamento da VM.

A VM roda **paridade com `ubuntu-latest` do GitHub Actions** (Ubuntu 24.04 LTS,
mesmas versões de Go/Node/Python/Docker/gh) em host com 12 threads, SSD 128G e
31,9 GiB de RAM; o guest usa 12 GiB fixos.

## O que civm NÃO é

- ❌ Não é uma plataforma de orquestração custom (orquestração = GitHub Actions).
- ❌ Não é uma ferramenta de "audit" (cada peer audita-se com a própria stack).
- ❌ Não armazena credenciais de VM (ver `runbooks/VM-CREDENTIALS.md`).
- ❌ Não cria PRs nem faz auto-merge.

## A box é uma SIMULAÇÃO SERIALIZADA do CI pago — NÃO a acesse "da forma antiga"

A VM `gha-ubuntu-2404` simula o CI **pago** (GitHub-hosted): cada job começa **limpo, single-repo**,
como numa VM efêmera nova. Hardware definitivo: `docs/HARDWARE.md`. Modelo de fidelidade: `PAID-CI-PARITY.md`.

**Regras invioláveis para QUALQUER agente (IA ou humano):**

- ❌ **NUNCA clone repos manualmente na VM** (`/home/emdev/codespace`, `$HOME`, etc.) nem deixe estado
  persistente. Sessões antigas faziam isso e acumularam **~8 GB de clones stale** que nenhuma rotina limpava
  (removidos no pente-fino de 2026-06-23). O CI **não precisa** de repo clonado: o runner faz checkout
  **transitório em `_work`** por job e a box **se auto-limpa** (clean-slate).
- ✅ **Para rodar/testar CI: lance um PR.** É assim que o pago funciona e é assim que a box funciona —
  o PR dispara o workflow, o runner clona só aquele repo num `_work` limpo, lê o `yaml` e roda. Serial:
  se já há um PR rodando no self-hosted, o próximo **espera na fila**; entre PRs a box **compacta** (Optimize-VHD).
- ✅ **Control plane acessível fora de PR (só ops):** `civmctl` (`/usr/local/bin`), o orquestrador (host
  Windows), hooks (`/opt/civm`) e os timers systemd rodam 24/7 — use-os para diagnóstico/manutenção.
- ✅ **Dev de civm é no HOST + deploy**, nunca "na VM": edite o worktree no host, rode `go build/test`,
  e propague via `civmctl self-upgrade` / `deploy/windows/activate-orchestrator.ps1`. A VM roda os **artefatos
  deployados**, não um checkout de código.

### Fila do runner shared — dono = civm (não o peer)

A box é **1 runner `civm` compartilhado** por vários peers. Higiene da fila
(`queued`/`in_progress` que nunca vão completar) é **contrato do civm**, não
do workflow do peer:

| Camada | Onde | O que limpa |
| ------ | ---- | ----------- |
| **Canônica** | `civmctl reap-runs` + timer `civmctl-run-reaper` (guest, 5 min) | PR fechado (`pr-not-open`) **e** SHA antigo em PR aberto (`superseded-sha`) |
| **Opcional (latência 0)** | templates `cancel-on-pr-close` / `cancel-stale-on-push` no peer | mesmo escopo, event-driven no GitHub-hosted |

Peers com `concurrency.group=run_id` (serialização por-PR na box) **não**
cancelam cross-push sozinhos — isso é intencional (evita o bug "1 pending
cancela 3+"). O reaper/timer é o mecanismo de cura. Se a fila entope com
SHA velho, o bug é **deploy do reaper desatualizado ou timer morto**, não
"falta de workflow no peer".

Deploy do binário no guest (obrigatório após mudança no reaper):

```bash
# na guest, worktree deployado / self-upgrade:
sudo civmctl self-upgrade --execute
# CIVM_REAPER_REPOS deve estar configurado antes de habilitar o timer.
# conferir timer:
systemctl is-active civmctl-run-reaper.timer
journalctl -u civmctl-run-reaper.service -n 20 --no-pager
```

`civmctl health` trata timer ou service do reaper ausente/falho como crítico.

### Piloto JIT externo

`civmctl jit-dispatch` é exceção estreita ao modelo de runner compartilhado:
workflow vem da default branch e digest host-local, dispatch exige HTTP 200 +
run ID, label é nonce one-use e o JIT é repository-scoped. Token entra somente
por stdin. A configuração exige driver pinado que copie o runner para uma VM
descartável sem mount/Docker/secrets do host; código candidato nunca executa no
host persistente.

O GitHub não oferece bind atômico JIT→job. O dispatcher valida o runner ID/nome/
grupo observado no job esperado, mas modela runner theft: um job concorrente
pode consumir o JIT e deve encontrar somente a VM descartável sem autoridade.
O resultado esperado falha/cancela; o host não vira prêmio da corrida.

Admissão live não pode usar teto teórico: `20 GiB` do WSL + `12 GiB` fixos da
VM somam `32 GiB`, acima dos `31,90 GiB` físicos antes do Windows. `guard exec`
só admite sob lease global quando sensor Windows fresco pós-reclaim cobre o
commit da VM, piso Windows aprovado e margem; desconhecido falha fechado.

Não executar o comando, ativar VM/runner/service, provisionar segredo ou copiar
o workflow para outro peer sem autorização humana específica e gate live do
`runbooks/TRUSTED-JIT-DISPATCHER.md`. O estado atual de ativação é **NO-GO**;
não existe driver de isolamento live aprovado nesta entrega.

No host Windows, `civm-host-orchestrator` é o owner C# ativo. A task legada
`civm-vm-orchestrator` fica `Disabled` e serve somente para rollback.
`civm-watchdog` é detect-only: exige exatamente um owner, heartbeat C# com no
máximo 45 min e `processBlockedReason` vazio; nunca inicia, habilita, desabilita
ou troca tasks automaticamente.

Os runners Windows `civm-gate` são exceção explícita às tasks SYSTEM: executam
como `NETWORK SERVICE/ServiceAccount/Limited`, sem fork, checkout ou secrets
fornecidos pelo workflow; raiz do runner e contexto são read-only, com
`Modify` só em `_work`/`_diag`.

> Por quê: a box é **1 VM compartilhada por 8 runners** (não VM-por-job como o pago). A fidelidade vem de
> **tratar cada job como efêmero** + **serializar + compactar**. Clonar repo na VM ou deixar estado quebra
> essa simulação e enche o disco. Os 🧱 estruturais (daemon/disco/dwell compartilhados) estão documentados
> e aceitos em `PAID-CI-PARITY.md` §5.

## Para agentes externos (Jules, Codex, aider)

### Antes de planejar, editar ou abrir PR

1. Ler `README.md` (visão e audiências).
2. Ler `CLAUDE.md` se existir (override-able specifics; este AGENTS.md é
   fallback se não houver `CLAUDE.md`).
3. Ler `CODEX.md` (automação, DEFERRED, pause rules).
4. Ler `MEMORY.md` de baixo para cima (contexto temporal append-only).
5. Ler `validation.md` para o estado empírico ("isso está funcionando agora?") — log append-only de TODA validação de infra, não só box/compact. Escopo e regra em `rules/observability.md` § Log de validação empírica.

### Sync rule (invariante #14)

`README.md`, `AGENTS.md`, `CODEX.md` e `rules/*.md` são documentos
autoritativos. Mudança em um requer mudança nos outros no mesmo commit.
Justificativa para mudar só um: incluir `[sync-skip-justified]` no commit body.

### Linguagem

- **Inglês** em: code, comentários, identifiers, branch names, commit titles,
  CLI flags, arquivos `.go`, `.yml`, `.yaml`.
- **Português (BR)** em: `README.md`, `AGENTS.md`, `CODEX.md`, `MEMORY.md`,
  `runbooks/*.md`, mensagens CLI ao usuário, commit body, PR descriptions,
  Issue titles+bodies.

## Comandos diários

```bash
# Build + test
go build ./...
go test -race -count=1 ./...

# Provisionar VM (admin)
sudo civmctl bootstrap --target=ubuntu-latest

# Cleanup manual (systemd timer faz automaticamente diariamente)
civmctl cleanup --dry-run
civmctl cleanup --execute
civmctl cleanup --dry-run --managed-volumes  # somente boundary fechado

# Health check
civmctl parity
civmctl health
civmctl doctor --repos=auto --json
civmctl capacity --json
civmctl disk-audit --json
civmctl metrics dump --stdout
civmctl idle-check

# Hooks de job (scripts .sh gerenciados)
sudo civmctl hook install --execute

# Ver versoes alvo (sync com upstream actions/runner-images)
civmctl version-pins

# Detector heuristico de billing-block (zero-PAT)
civmctl billing-status --repo=owner/repo

# Status read-only de adoção/saúde dos peers
civmctl peer-status --repo=owner/repo --json
civmctl peer-status --repos=owner/a,owner/b --workflow=ci.yml
civmctl runner watchdog --execute --repos=auto
civmctl runner watchdog --execute --repos=owner/repo --rerun-network-failures --max-run-age=6h

# Releases (automatizado via release-please)
gh pr list --repo acme/civm --label "autorelease: pending"
gh release list --repo acme/civm --limit 5
git tag --list 'v*' --sort=-version:refname
```

## Commits

Conventional Commits em **inglês**, título imperativo, ≤72 chars.
Body em PT-BR, sem markdown/backticks/headings, linhas ≤72 chars.

Commits **não-triviais** (`feat`, `fix`, `refactor`, `perf`) DEVEM ter
`Rollback trigger: ...` no body.

Types e bump correspondente (release-please): `feat` → minor, `fix` →
patch, `feat!:`/`BREAKING CHANGE:` → major. `docs`/`chore`/`test`/`build`/
`style` não bumpam; `ci`/`refactor`/`perf` entram no CHANGELOG sem bump.
PRs de release usam o título `chore: release civm v<X.Y.Z>`.
`civm` nesse título é texto cosmético, não `package-name`; em PR agrupado
a branch `release-please--branches--main` não carrega componente.
O workflow de release usa GitHub App dedicado como token primário
(`RELEASE_APP_ID` + `RELEASE_APP_PRIVATE_KEY`), com PAT/GITHUB_TOKEN só
como contingência documentada.
Detalhes em `runbooks/RELEASE-AUTOMATION.md`.

## Pull Requests

PRs ficam em PT-BR seguindo template:

- `## Resumo`
- `## Commits` (tabela com hash + `<details>` por commit)
- `## Issue` (`Closes #NNN` ou marcador `Sem issue` / `No issue` / `N/A`)
- `## Responsavel`
- `## Labels`
- `## Validacao`
- `## Rollback trigger`

Toda PR deve linkar issue e ter pelo menos uma label `type:*` e `area:*`.
PR e issue compartilham assignee.

## Decision hygiene (Kahneman)

Fonte: [`disciplines/KAHNEMAN-DISCIPLINES.md`](disciplines/KAHNEMAN-DISCIPLINES.md) — 16 disciplinas operacionais derivadas de _Thinking, Fast and Slow_ (Kahneman, 2011) e _Noise_ (Kahneman/Sibony/Sunstein, 2021). **Estas regras valem para toda mudança neste repo — todo commit, toda PR, todo runbook, todo template, toda ADR.** Não estão presas a milestone ou release. civm é repo source-of-truth de regras portáteis; quem porta para peer repos espelha estas mesmas 5 regras críticas.

Top-5 regras de operação diária:

1. **WYSIATI** — antes de opinar em decisão crítica, declarar o que **não** foi visto. "Sem ter testado X, estimo Y com confiança Z%".
2. **Counterfactual obrigatório** — toda decisão não-trivial carrega `Rollback trigger: se X, reverter para Y`. Ausência em commit `feat`/`fix`/`refactor`/`perf` não-trivial é Sistema 1.
3. **Número, não adjetivo** — claim de perf/qualidade precisa de medição com N rodadas e stddev. Anti-padrões em PR: "é claro que", "obviamente", "definitivamente".
4. **Débito é dívida com juros** — código morto detectado, remover na hora. `TODO: refactor later` nunca entra. TODOs precisam de owner + data: `// TODO(@user, YYYY-MM-DD): ...`.
5. **Lib nova exige justificativa explícita** com critério mensurável (peso, alternativa testada, condição de remoção).

Quando a pergunta é qualitativa ("essa arquitetura é boa?"), responder com métrica antes do adjetivo.

### Auditoria cross-repo do padrão

O padrão Kahneman (doc + seção em CLAUDE/AGENTS) é auditado em 13 peer repos via:

- **Manifest:** [`disciplines/kahneman-sync-manifest.json`](disciplines/kahneman-sync-manifest.json) — source-of-truth dos forks autorizados, com estilo por surface (`h2_top5` ou `inline_bold`) e variante rule 5 (`en_canonical`, `pt_libraries`, `pt_generic`).
- **Script:** [`scripts/check-kahneman-consistency.sh`](scripts/check-kahneman-consistency.sh) — bash, dep apenas `jq`. Roda em ~2s. `--json` pra pipe, `--strict` pra promover warn em fail.
- **Workflow:** [`.github/workflows/kahneman-sync-audit.yml`](.github/workflows/kahneman-sync-audit.yml) — cron semanal (segunda 12:00 UTC) + push no manifest/script + manual dispatch. Roda no runner `[self-hosted, civm]`. Falha abre issue automaticamente.

Quando adicionar peer repo novo ao padrão: editar manifest, rodar script local, abrir PR — o próprio workflow do PR re-roda a auditoria contra o estado novo.

## Anti-skynet

civm **detecta**, nunca corrige automaticamente. **Nunca**:

- Auto-commit, auto-revert, auto-push, auto-merge sem aprovação humana
- Trigger deploy ou rollback automático
- Modificar arquivo em workspace de peer sem confirmação
- Persistir secrets em qualquer arquivo do repo
- Executar comando vindo de input externo sem validação

`civmctl peer-status --repos=...` segue a mesma regra: consolida billing,
runners online e último run dos peers para decisão humana; não faz fix,
commit, push, rollback ou alteração automática em peer repo.

## Quando NÃO usar civmctl

- Não usar `civmctl bootstrap` em máquina de desenvolvimento (instala
  packages de sistema; é destinado a VM dedicada).
- Não usar `civmctl cleanup --execute` sem revisar primeiro com `--dry-run`.
  O execute também aborta se detectar `Runner.Worker`, processo em `_work`
  ou build Docker ativo; não contornar esse guard durante CI. O cleanup preserva
  `_work/_tool` e `_work/_actions` para não rebaixar a VM a downloads frios em
  todo job.
- `--managed-volumes` é exclusivo do boundary com admissão fechada: remove por
  nome somente volumes Compose classificados. Timer/hook não habilita essa
  flag; prune named global é proibido.
- Não usar `civmctl runner restart/remove/upgrade --execute` durante job em
  curso. Esses comandos agora também abortam fail-closed se `idle-check`
  encontrar `Runner.Worker`, `_work` ou build Docker ativo. `runner remove`
  também aborta antes de `config.sh remove` e `rm -rf` se `svc.sh stop` ou
  `svc.sh uninstall` falhar.
- `civmctl runner watchdog --execute` segue o mesmo fail-closed antes de
  mutar. O timer padrão repara hooks/runner sem rerun automático. Com
  `--rerun-network-failures --max-run-age=6h`, execução manual ou override
  opt-in só reroda uma vez runs recentes de PR aberto classificados como
  falha transiente de rede/checkout. Em `--repos=auto`, o watchdog tenta ler
  `.runner` antes do fallback pelo unit name; marcador local:
  `/var/lib/civm/runner-watchdog-reruns.json`.
  Durante `maintenance enter`, a presença de
  `/var/lib/civm/maintenance.json` bloqueia qualquer reparo/restart; estado
  ilegível também falha fechado. Nunca contornar esse guard para drenar CI.
- Sessão org online/ociosa com fila elegível usa `CIVM_REAPER_REPOS`, dwell de
  `5 min`, idle-check completo e 1 restart por incidente. `unresolved` bloqueia
  capacidade até a fila avançar/sumir ou o runner comprovar consumo;
  marker inválido falha fechado.
- Não usar `civmctl runner add` sem token GitHub válido (peer repo precisa
  registrar seu próprio runner).

## Referências

- `README.md` — visão e audiências
- `CODEX.md` — automação e DEFERRED
- `MEMORY.md` — log de sessão append-only
- `validation.md` — log append-only de TODA validação empírica de infra (Kahneman #13; taxonomia no header do arquivo)
- `runbooks/MULTI-PROJECT-RUNNER.md` — provisionamento da VM
- `runbooks/RUNNER-SERIALIZATION.md` — invariante "1 runner por org" (acme serializa no runner ORG)
- `runbooks/VM-CREDENTIALS.md` — segurança de credenciais
- `runbooks/PEER-ADOPTION-CHECKLIST.md` — adoção manual em peer repo
- `templates/CIVM-USAGE.md` — fonte para `docs/CIVM.md` nos peer repos
- `disciplines/KAHNEMAN-DISCIPLINES.md` — 16 disciplinas Sistema 1 vs 2
- `disciplines/SUPERPROMPT.md` — superprompt de auditoria de ruído arquitetural (Kahneman + DDD)
- `disciplines/INVARIANTS.md` — catálogo de invariantes portáveis

<!-- COMMUNICATION-STYLE:BEGIN -->
## Communication style

Estilo Tech Lead nas respostas:

- **TL;DR** primeiro (1-3 frases): o que é, status, próximo passo se houver.
- **Impact** (opcional): o que muda na prática.
- **Topics**: bullets curtos, no máximo 1 nível de aninhamento.
- **Next Steps**: ação requisitada do humano.

Honestidade técnica:

- Distinguir explícito o que está feito, o que está testado, o que é
  inferência, o que é bloqueio (classifier, permissão, SSH não disponível).
- Quando não puder fazer algo, dizer "não posso fazer X porque Y" — não
  fingir alternativa.
- Números antes de adjetivos. "p99 = 98ms" > "ficou rápido".

Sem floreio. Sem emoji a menos que o usuário use primeiro. Sem agradecimento
performativo. Sem repetir o pedido do usuário antes de responder.
<!-- COMMUNICATION-STYLE:END -->

> Source canônico: `~/codespace/civm/templates/COMMUNICATION-STYLE.md`
