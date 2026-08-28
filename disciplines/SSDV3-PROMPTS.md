# SSDV3 — Spec-Driven Development: Prompts Base

> **Metodologia civm-native (PRD → SPEC → IMPL).** Os prompts abaixo são os do próprio `civm` — repo **Go de infra** (Go 1.26 stdlib-first, `cmd/civmctl/` + `internal/**`, mais a camada host PowerShell em `deploy/windows/*.ps1` que dirige a VM/VHDX Hyper-V). Sem frontend, sem HTTP API, sem multi-tenant, sem banco. O pipeline (PRD → SPEC → Passo 2.5 Red-Team → SPECv2/SPECv3) é independente de stack; um peer repo que o adote troca o exemplo e os gates pelos seus.

Metodologia em 3 passos: **PRD → SPEC → IMPL**

Exemplo trabalhado ao longo deste doc: o redesign de reclaim do `civm` (`docs/specs/host-volume-reclamation/`, slug: `host-reclaim-admission-gate`). Caso SSDV3 real — um **deadlock de headroom** (quando `V: < 8 GB` todo reclaimer aborta, então um disco cheio não consegue rodar a própria limpeza) que virou PRD → SPEC; o SPEC levou **no-go** no red-team (`Stop-Job` NÃO aborta `Optimize-VHD`; a evidência N=1 nunca mediu scratch) e foi reescrito em SPECv3 (medir-primeiro, optimize ininterruptível, lock canônico). História completa nas snippets ao longo deste arquivo.

Objetivo desta versão:

- preservar a descoberta útil antes de decidir
- reduzir ambiguidade entre fatos do repo e propostas
- produzir PRD e SPEC mais executáveis
- melhorar a passagem do PRD para o SPEC e do SPEC para a implementação
- incorporar guardrails cognitivos que forcem Sistema 2 nas etapas críticas

## Como usar

1. **Passo 1** gera `docs/specs/{feature-slug}/PRD.md`
2. **Passo 2** transforma o PRD em `docs/specs/{feature-slug}/SPEC.md`
3. **Passo 2.5** quando houver risco estrutural, operacional ou de segurança (no `civm`: mexe na VM/VHDX, no contrato de reclaim/cleanup, em subcomando `civmctl` que muta, na superfície de privilégio, ou quebra invariante documentado)
4. **Passo 3** implementa estritamente a partir do SPEC

Se um passo encontrar ambiguidade que pertence ao passo anterior, volte um passo.

## Quando SSDV3 é obrigatório (cf. `rules/ssdv3.md`)

SSDV3 é **mandatório** para mudanças em:

1. **Camada host que muta VM/VHDX** — `deploy/windows/*.ps1` (`Stop-VM`, `Optimize-VHD`, `Start-VM`, reclaim, maintenance): risco de deixar a VM Off ou estourar o `V:`.
2. **Contrato de cleanup/reclaim** — o que `civmctl cleanup --execute` apaga, os guards de headroom/admissão do reclaim, a ordem fail-closed.
3. **Subcomando `civmctl` que muta** — `runner restart/remove/upgrade`, `bootstrap`, `maintenance enter/exit`, `hook install`.
4. **Superfície de privilégio** — `deploy/sudoers.d/*`, Scheduled Tasks SYSTEM, o wrapper `civm-safedelete`.
5. **Constantes que gateiam comportamento** em `internal/civm/civm.go` (headroom, faixas de porta, pre-cleanup %, `ScratchBudget`).
6. **Quebra de invariante documentado** (`disciplines/INVARIANTS.md`): sync rule, anti-skynet, cobertura ≥80%.

Tudo o mais é opcional — para UI/refactor sem mudança de contrato, bugfix localizado, docs/deps, o pipeline completo é overhead.

## Organização dos arquivos

**Todos os artefatos SDD (PRD.md, SPEC.md, IMPL.md) devem ficar em pastas semânticas em `docs/specs/`**, nunca soltos na raiz do repositório.

**Convenção de nomes:**

```text
docs/specs/{feature-slug}/
├── PRD.md
├── SPEC.md
├── SPECv2.md  # opcional, criado somente quando o Passo 2.5 der no-go
├── SPECv3.md  # opcional, criado quando uma 2ª rodada de red-team der no-go
└── IMPL.md    # registro do que foi feito (commits, arquivos, decisões pequenas, métricas)
```

- `{feature-slug}`: kebab-case inglês, curto e descritivo (ex.: `host-reclaim-admission-gate`, `multi-project-isolation`)
- `SPEC.md`: primeira versão gerada pelo Passo 2; preservar como baseline histórico
- `SPECv2.md`: versão melhorada criada pelo Passo 2 quando a auditoria do Passo 2.5 resultar em `no-go`
- Não sobrescreva `SPEC.md` para incorporar correções de auditoria; gere `SPECv2.md`, salvo pedido explícito do usuário para edição in-place
- Se `SPECv2.md` já existir e uma nova auditoria ainda der `no-go`, atualize `SPECv2.md` in-place, salvo pedido explícito do usuário para criar `SPECv3.md` (foi o que aconteceu no caso reclaim: SPECv2 fechou robustez operacional, SPECv3 fechou o deadlock de headroom)
- Se a feature já tem pasta em `docs/specs/`, reutilize-a
- Se for evolução incremental, reutilize a pasta existente ou crie uma subpasta semântica

## Frontmatter obrigatório no PRD.md

Toda PRD.md começa com frontmatter YAML:

```yaml
---
slug: host-reclaim-admission-gate
title: Resiliência do reclaim — quebrar o deadlock de headroom com admissão por folga provada
milestone: —
issues: []
---
# PRD — Reclamação de volume do host (VHDX): guest-free vira host-free com segurança
```

- `slug`: igual ao nome da pasta (`<issue>-<descricao>` para issues, ou `<descricao>` sem issue).
- `title`: humano, descreve a mudança.
- `milestone`: identificador (M14, v1.0.0, …). Use `—` se não associada.
- `issues`: array de números de issue do GitHub (`[]` se não tem).

Status derivado da presença de arquivos: `PRD` → só PRD.md; `SPEC` → SPEC.md (ou SPECv2/SPECv3) também presente; `DONE` → IMPL.md presente.

## Referência cognitiva

Quando a mudança envolver risco estrutural, operacional, de segurança, rollout, rollback, migração de estado, concorrência, contrato de reclaim/cleanup, secret, privilégio do host ou isolamento/concorrência (se aplicável), o SPEC deve apontar explicitamente para `disciplines/KAHNEMAN-DISCIPLINES.md`.

O objetivo não é teorizar, mas forçar cada etapa crítica a responder:

- qual viés está sendo combatido
- qual pergunta obrigatória de Sistema 2 precisa ser respondida
- qual evidência mínima autoriza avançar
- qual condição objetiva exige abortar, voltar um passo ou fazer rollback

## Política Day-0 (sem produção legada obrigatória)

> O `civm` **já roda em produção** (runner self-hosted), mas **não tem dados legados persistidos** que precisem de migração/backfill — o estado é arquivos efêmeros (`/var/lib/civm/*.json`, locks, métricas do host). Logo a política Day-0 se aplica ao **modelo de dados/estado** (sem shim, sem dual-reader, sem backfill para estado inexistente), enquanto mudança em **comportamento operacional** (reclaim, drain, headroom) segue rollback trigger numérico e teste, não a política Day-0 isoladamente.

Sem estado persistido legado obrigatório, toda mudança deve ser especificada e implementada como solução principal e única, no formato final correto para Day-0.

Por padrão, é proibido criar workaround, shim, dual-reader, dual-write, camada de compatibilidade com formato antigo, backfill para estado inexistente, migração incremental corretiva desnecessária ou código morto.

Exceções só são permitidas com requisito explícito e documentado para manter duas versões, integração externa real, estado persistido que não possa ser resetado, ou rollout coordenado aprovado. A exceção deve registrar motivo, prazo de remoção, rollback e evidência.

## Princípios da versão 3

1. **Discovery antes de convergência**
   A investigação pode ser ampla, mas o documento final deve convergir para uma única direção recomendada.
2. **Reuso antes de criação**
   Antes de propor novo subcomando, constante, arquivo de estado, script `.ps1`, Scheduled Task ou env var, prove que o padrão existente não atende (`internal/**`, `cmd/civmctl/`, `deploy/windows/`).
3. **Separar fato de proposta**
   Diferencie explicitamente:
   - `Confirmado no codebase`
   - `Confirmado na documentação oficial`
   - `Inferência / proposta`
4. **Rastreabilidade obrigatória**
   Cada requisito do PRD deve aparecer no SPEC e cada bloco da implementação deve apontar para itens do SPEC.
5. **Sem criatividade estrutural no Passo 3**
   Se a implementação exigir decisão nova, ela volta para o SPEC antes de virar código.
6. **Sistema 2 explícito nas etapas críticas**
   Nos passos com risco estrutural, operacional ou de segurança, o SPEC deve apontar qual disciplina de `disciplines/KAHNEMAN-DISCIPLINES.md` reduz o viés, quais evidências mínimas são exigidas e qual condição objetiva dispara abortar, voltar um passo ou rollback.
7. **Passos críticos devem levar ao documento de disciplina**
   Nenhum item crítico do SPEC fica autocontido só em execução; ele precisa apontar para `disciplines/KAHNEMAN-DISCIPLINES.md` e registrar como a disciplina afeta a decisão local.

---

## PASSO 1 — Geração do PRD.md

### Prompt

Preciso gerar o PRD técnico para a seguinte mudança:

**[DESCREVA A FEATURE/MUDANÇA EM 1-2 FRASES]**

Objetivo:

- [qual resultado de negócio/técnico deve existir ao final]

Camada(s) envolvida(s):

- [ ] Guest Go — package(s) em `internal/**` / subcomando(s) em `cmd/civmctl/`
- [ ] Host PowerShell — `deploy/windows/*.ps1` (Stop-VM/Optimize-VHD/Start-VM) + Scheduled Task
- [ ] Estado / arquivos (`/var/lib/civm/*.json`, locks, métricas do host)
- [ ] Constantes que gateiam comportamento (`internal/civm/civm.go`)
- [ ] Systemd (timers/units do guest) / `schtasks` (host)
- [ ] Superfície de privilégio (`deploy/sudoers.d/*`, SYSTEM task, `civm-safedelete`)
- [ ] Documentação / runbook / ADR

### Processo obrigatório

Antes de escrever o PRD final, siga estas fases:

#### Fase 1 — Discovery

- Levante o contexto real no codebase
- Identifique o que já existe e pode ser reutilizado
- Liste opções de implementação viáveis
- Levante edge cases, riscos, dependências e impactos cross-camada (guest↔host)

#### Fase 2 — Convergência

- Escolha uma opção principal
- Explique por que é a recomendada no contexto do `civm`
- Liste alternativas descartadas e por quê
- Registre lacunas de contexto abertas

#### Fase 3 — PRD final

- Escreva o `PRD.md` refletindo apenas a opção recomendada
- Não escreva PRD com múltiplas arquiteturas concorrentes
- Se houver incerteza real, registre-a em riscos, dependências ou fora de escopo

### Pesquisa obrigatória antes de gerar o PRD

#### 1. Codebase do `civm` — contexto interno

- Leia as regras do repo: `rules/*.md` (`coding-style.md`, `testing.md`, `security.md`, `observability.md`, `ssdv3.md`, `governance.md`, …) e os docs autoritativos `AGENTS.md` / `README.md` / `CODEX.md` / `CLAUDE.md`
- Leia `.github/workflows/ci.yml` e `tools/ci/detect-changes.mjs` para entender os gates (jobs `validate-templates`, `build-civmctl`, `self-hosted-smoke`)
- Identifique o(s) package(s)/subcomando(s) alvo e leia o código existente: dispatch em `cmd/civmctl/main.go` (`switch`), os runners de subcomando (`cmd/civmctl/*.go`) e a lógica em `internal/**`
- Mapeie o estado atual: arquivos JSON em `/var/lib/civm/`, locks, logs estruturados (`slog`), métricas do host (`V:\civm-host-metrics.json`)
- Identifique constantes, timers systemd, Scheduled Tasks e invariantes que tocam nessa feature
- Verifique a camada host: `deploy/windows/*.ps1` e o registro das tasks (`register-*.ps1`); confira o lint guard de `.ps1` (`internal/hostdisk/ps1_safety_test.go`)
- Leia `disciplines/KAHNEMAN-DISCIPLINES.md` quando a mudança envolver risco estrutural, operacional ou de segurança
- Verifique runbooks relevantes em `runbooks/` (ex.: `MULTI-PROJECT-RUNNER.md`, `RUNBOOK-HOST-VHDX-MAINTENANCE.md`) e ADRs/specs anteriores em `docs/specs/`
- Se tocar a VM/VHDX/disco: confirme topologia (Windows host → Hyper-V → VM Linux guest; `V:` NTFS hospedando o VHDX dinâmico) e o pipeline de discard (controlador SCSI vs IDE, `fstrim`, `Optimize-VHD`)
- Se tocar privilégio: verifique `deploy/sudoers.d/*`, o wrapper `civm-safedelete` e o direito Hyper-V das tasks SYSTEM
- Identifique explicitamente:
  - o que já existe e pode ser reutilizado
  - o que precisa ser estendido
  - o que realmente precisa ser criado do zero

#### 2. Documentação oficial e compatibilidade

Valide contra a documentação oficial das tecnologias realmente envolvidas:

**Guest (Go):**

- Go 1.26 (stdlib-first: `os`, `os/exec` com `exec.CommandContext`, `encoding/json`, `log/slog`, `syscall`/`golang.org/x/sys/unix` para `Statfs`)
- `golangci-lint` (config `.golangci.yml`), `govulncheck`, `go test -race`

**Host (Hyper-V / Windows):**

- Hyper-V cmdlets: `Get-VM` / `Stop-VM` / `Start-VM`, `Get-VHD` / `Optimize-VHD` / `Resize-VHD`, `Get-Volume` / `Get-PSDrive`
- `Optimize-VHD -Mode Full` (compactação offline; semântica de scratch; ininterruptibilidade — `CompactVirtualDisk` em `virtdisk`)
- Controladores Hyper-V: SCSI repassa UNMAP/TRIM, IDE não
- `schtasks` (`/create`, `/run`, `/RU SYSTEM /RL HIGHEST`), `Start-Job`/`Wait-Job`, `[System.IO.FileStream]` `FileShare::None` (lock de arquivo)

**Guest Linux / runner:**

- `fstrim` / montagem com `discard`, `lsblk -D` (DISC-GRAN/DISC-MAX), `/proc/mounts`, `findmnt --json`
- systemd timers/units, `systemctl`, `gh api` (label dance do runner)

#### 3. Padrões de mercado e edge cases

- Pesquise implementações de referência (GitHub code search, docs oficiais) para reclaim de VHDX dinâmico, compactação offline segura e admission gates de disco antes de propor algo novo
- Identifique edge cases comuns desse tipo de mudança em host single-VM (host a poucos GB livres, optimize pendurado, VM ocupada, controlador sem UNMAP)
- Avalie trade-offs de disponibilidade (deixar a VM Off é o pior caso) vs. recuperação de espaço

### Regras de qualidade do PRD

- Não invente arquitetura nova se o codebase já tiver padrão equivalente
- Diferencie explicitamente:
  - **Confirmado no codebase**
  - **Confirmado na documentação oficial**
  - **Inferência / proposta**
- Se faltar contexto no repo, declare a lacuna em vez de assumir como fato
- Prefira reaproveitar constantes, subcomandos, packages, scripts `.ps1`, Scheduled Tasks e arquivos de estado existentes
- Não proponha novos subcomandos, constantes, scripts ou env vars sem justificar por que os existentes não atendem
- Aponte breaking changes, rollout, rollback e migração de estado quando aplicável
- Sem estado legado obrigatório, aplique a política Day-0: proponha a solução correta principal, sem compatibilidade legada, shims, workarounds ou backfills para estado inexistente
- Se sugerir dual path, camada de compatibilidade ou migração corretiva de estado, declare a exceção Day-0 com motivo objetivo; caso contrário, consolide o desenho final
- Em **Alternativas descartadas**, descarte explicitamente soluções de compatibilidade que só existem para preservar versão antiga sem estado vivo
- Liste os documentos a atualizar no mesmo commit quando houver impacto estrutural (sync rule: README ≡ AGENTS ≡ CODEX ≡ rules)
- Se o risco for alto, antecipe no PRD quais etapas provavelmente exigirão disciplina explícita de `disciplines/KAHNEMAN-DISCIPLINES.md` no SPEC
- Mantenha o PRD específico e operacional; evite texto genérico

### Saída esperada

Gere o arquivo `docs/specs/{feature-slug}/PRD.md` com **EXATAMENTE** esta estrutura:

#### Resumo

- O que é, por que existe, qual problema resolve
- Valor operacional no contexto do runner

> Exemplo (reclaim): "O VHDX dinâmico cresce quando o guest escreve mas **não encolhe** quando o guest libera blocos; o `V:` chegou a 3 GB livres (98%) enquanto o guest tinha 44 GB livres. Pior: quando `V: < 8 GB`, **todo reclaimer aborta** por falta de headroom — então um disco cheio não consegue rodar a própria limpeza (deadlock). Valor: o runner para de morrer por disco do host de forma silenciosa, e a manutenção vira um comando seguro e auditável."

#### Contexto técnico

- Package(s)/subcomando(s)/script(s) envolvidos e papel de cada um na topologia (guest Go ↔ host PowerShell ↔ VM/VHDX)
- Estado atual: constantes, timers, Scheduled Tasks, arquivos de estado, fluxos já existentes que serão reutilizados ou estendidos
- Isolamento/concorrência — se aplicável (locks entre reclaimers, anti-reentrância de task, mutual exclusion de `Stop-VM`/`Optimize-VHD`)
- Dependências entre camadas (guest ↔ host via SSH; o que cada lado autoritativamente conhece)
- O que está **confirmado no codebase**
- O que está **confirmado na documentação oficial**
- O que está **sendo proposto**

#### Opção recomendada

- Solução escolhida
- Motivo da escolha
- Alternativas descartadas
- Trade-offs aceitos

> Exemplo (reclaim): "Admission gate (`EmergencyAdmits`) em vez de piso fixo: o caminho de emergência (`V: < HeadroomGB`) só é admitido quando `liveVFree − HardFloorGB >= ScratchBudgetGB` (o pior scratch high-water **medido**). Descartado: 'baixar o piso de 8 GB para um menor' — só **realoca** o deadlock; 'abortar o Optimize no meio via `Stop-Job`' — `Stop-Job` NÃO aborta a compactação nativa, que segue escrevendo o `V:`."

#### Requisitos funcionais

Para cada requisito:

- **RF-N**: descrição objetiva sem ambiguidade
- **Critério de aceite**: condição verificável
- **Isolamento/concorrência**: como esse requisito respeita exclusão mútua / fail-closed, se aplicável

#### Requisitos não-funcionais

- **Performance**: tempo do caminho online (sem downtime) vs. offline (`Optimize-VHD`, minutos com a VM off); meta de `V:` livre sustentado
- **Segurança / privilégio**: o que roda como SYSTEM, qual direito mínimo do host (Hyper-V), nada de `pull_request_target`/código de PR no host, sem segredo em `deploy/windows/`
- **Observabilidade**: logs estruturados (`slog`/JSON no guest, log da task no host), métricas do host (`V:` free/size + VHDX file/min/max + gap), alarmes nos pisos
- **Escalabilidade**: comportamento por-host/por-VM; o gap guest×host como sinal de saúde
- **Resiliência (worst-case — Kahneman #5)**: host a 3 GB livres, `Optimize-VHD` pendura/falha, VM ocupada, controlador sem UNMAP — cada um com mitigação

#### Fluxos

**Happy path**

- Passo a passo numerado
- Qual componente/camada executa cada passo (guest Go, host `.ps1`, Hyper-V)
- Qual mecanismo (subcomando `civmctl`, SSH guest↔host, `fstrim`, `Optimize-VHD`, lock de arquivo)
- Como o estado flui (ex.: drain grava `/var/lib/civm/maintenance.json`; métricas do host entregues ao guest)

**Fluxos alternativos**

- Variações válidas do happy path (ex.: caminho online auto-shrink vs. fallback offline)

**Fluxos de erro**

Para cada erro:

- Condição de trigger
- Resultado / exit code / mensagem (ex.: `abort_insufficient_slack` exit 2; nunca deixar a VM Off)
- Log level e campos contextuais
- Impacto na consistência do estado (VHDX/VM/disco)

#### Modelo de dados

> **N/A — sem banco.** Para o `civm`, "modelo de dados" = **estado em arquivos**. Liste os arquivos de estado novos/alterados, o shape JSON e a semântica de escrita.

**Estado novo (arquivos)**

Para cada arquivo, descrever shape e gravação atômica:

```text
/var/lib/civm/maintenance.json (guest):
  { "drained_at": "<iso>", "runners": [ { "name": "...", "repo": "...",
    "stopped": true, "label_removed": true } ] }
  Escrita: os.WriteFile substitui o arquivo (atômico por arquivo).
```

```text
V:\civm-host-metrics.json (host) + cópia em /var/lib/civm/host-metrics.json (guest):
  { "v_free_gb": N, "v_size_gb": N, "vhdx_file_size_gb": N,
    "vhdx_min_size_gb": N, "vhdx_max_size_gb": N, "guest_free_gb": N,
    "gap_gb": N, "timestamp": "<iso>", "delivery_status": "ok|failed" }
```

**Alterações em estado/constantes existentes**

- Constante exata adicionada/alterada em `internal/civm/civm.go`
- Justificativa
- Impacto em comportamento (quem lê a constante)
- Necessidade de migração de estado, se houver

**Estado scope**

- Guest-local (`/var/lib/civm/*.json`) vs. host (`V:\*.json` / `V:\*.log`)
- Backfill = **N/A — Day-0** por padrão (estado efêmero, sem dado legado)

#### API / Interfaces

> **Sem endpoint HTTP/OpenAPI/SDK.** Para o `civm`, "interfaces" = **CLI `civmctl` + componente host + arquivos/locks**.

**Subcomandos `civmctl` (guest) novos ou modificados**

| Campo            | Valor                                                   |
| ---------------- | ------------------------------------------------------- |
| Subcomando       | `disk-doctor` / `maintenance enter\|exit` / `host-disk` |
| Read-only?       | sim/não (`disk-doctor`/`host-disk` read-only; `maintenance` muta com `--execute`) |
| Exit codes       | `0` ok / `1` erro ou `crit` / `2` abort / `64` flag inválida |
| Privilégio       | usuário do runner / `sudo` via `civm-safedelete` / SSH→host SYSTEM |
| Idempotência     | sim/não e por quê (`maintenance enter/exit` é idempotente) |

**Contrato (shape)**

```text
civmctl disk-doctor --json  →  { "device": "...", "controller": "scsi|ide|virtio",
  "mount_discard": bool, "disc_max_bytes": N, "trim_effective": bool,
  "root_cause": "IDE controller does not propagate UNMAP | TRIM supported, ..." }
```

**Componente host (Windows, `deploy/windows/`)**

| Artefato                                              | Função                                              |
| ---------------------------------------------------- | --------------------------------------------------- |
| `civm-host-metrics.ps1` + Scheduled Task             | emite JSON de `V:`/VHDX e entrega ao guest          |
| `civm-vhdx-optimize.ps1` + Scheduled Task (SYSTEM)   | compactação offline segura, não-interativa          |
| `civm-vhdx-autoreclaim.ps1` + Scheduled Task (SYSTEM)| reclaim por pressão com admission gate + lock canônico |
| `register-*.ps1`                                     | `schtasks /create /RU SYSTEM /RL HIGHEST` versionado |

**Erros / abort triggers**

| Exit | Condição                                  | Log                          |
| ---- | ----------------------------------------- | ---------------------------- |
| 2    | `V: < Headroom` e budget=0 (gate off)     | `abort_headroom`             |
| 2    | folga não cobre `ScratchBudget` medido    | `abort_insufficient_slack`   |
| 0    | outro reclaimer ativo (lock canônico)     | `reclaim_skip_other_active`  |
| 1    | `Start-VM` falhou 3× — VM Off             | `CRITICAL vm_left_off`       |

**Impacto em contrato / docs**

- Subcomandos novos no `printHelp` (`cmd/civmctl/main.go`)
- Constantes novas em `internal/civm/civm.go` (sync rule se mudar contrato)
- Runbook (`runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md`) e `MULTI-PROJECT-RUNNER.md` §Disk
- `deploy/windows/*` versionado; lint guard de `.ps1`

#### Dependências e riscos

- Pré-requisitos
- Riscos técnicos com mitigação concreta
- Impacto em componentes existentes (guest cleanup/hook, watchdogs, timers, tasks)
- Breaking changes, se houver
- Estratégia de rollout (slices)
- Estratégia de rollback (app / host / estado / proibições)
- Hipóteses que precisarão de disciplina explícita no SPEC, quando aplicável

#### Estratégia de implementação

- Ordem recomendada das fatias de implementação
- Dependências entre fatias
- O que pode ser validado cedo (ex.: Slice 0 = diagnóstico/baseline sem código)
- O que exige janela de manutenção, troca one-time (SCSI) ou rollout supervisionado

#### Fora de escopo

- O que explicitamente NÃO faz parte desta implementação
- Motivo de exclusão

---

## PASSO 2 — Geração do SPEC.md / SPECv2.md / SPECv3.md (a partir do PRD ou auditoria)

> **Leia `docs/specs/{feature-slug}/PRD.md` e produza um `docs/specs/{feature-slug}/SPEC.md` cirúrgico para implementação.**
> Se este Passo 2 estiver sendo reexecutado após um `no-go` do Passo 2.5, preserve o `SPEC.md` original e crie/atualize `docs/specs/{feature-slug}/SPECv2.md` (e `SPECv3.md` numa 2ª rodada).
> O SPEC não replica o PRD; fecha decisões, remove ambiguidade e traduz requisitos em mudanças exatas no repo.

### Objetivo do Passo 2

- transformar requisitos em tarefas de código com ordem e dependências
- resolver ambiguidades do PRD antes do código
- explicitar impactos em contrato, estado, docs, testes e rollout
- ligar etapas críticas às disciplinas de `disciplines/KAHNEMAN-DISCIPLINES.md`

### Prompt

Leia `docs/specs/{feature-slug}/PRD.md` e gere `docs/specs/{feature-slug}/SPEC.md` com decisões fechadas, rastreabilidade por requisito e instruções implementáveis sem interpretação.

Voltando do Passo 2.5 com decisão `no-go`, leia também o relatório da auditoria e gere `docs/specs/{feature-slug}/SPECv2.md` (ou atualize-o / crie `SPECv3.md`) como versão melhorada, sem alterar o `SPEC.md` original.

### Regras

1. Só inclua o que será realmente implementado agora
2. Cada arquivo listado deve ter caminho completo a partir da raiz do repo
3. Cada mudança deve explicar **o que muda**, **como muda** e **por que muda**
4. Referências a código existente devem usar nome exato de função, struct, tipo, interface, constante ou caminho de `.ps1`
5. Ordem dos itens = ordem de implementação
6. Se o PRD estiver ambíguo, resolva aqui com uma decisão explícita e justificativa
   - Na primeira execução do Passo 2, grave o resultado em `docs/specs/{feature-slug}/SPEC.md`
   - Na execução após `no-go` do Passo 2.5, grave o resultado em `docs/specs/{feature-slug}/SPECv2.md`, preservando `SPEC.md`
   - Se `SPECv2.md` já existir, atualize `SPECv2.md` in-place (ou crie `SPECv3.md` numa 2ª rodada de red-team), salvo pedido explícito
7. Todo subcomando `civmctl` que executa comando externo deve usar `exec.CommandContext` (sem shell), com timeout
8. Todo subcomando que muta deve validar flags e estado antes de tocar a VM/VHDX/disco ou o estado em `/var/lib/civm`
9. Todo caminho de reclaim que pode disparar `Stop-VM`/`Optimize-VHD` deve passar por guard de admissão (headroom/folga provada) + lock canônico, fail-closed, quando aplicável
10. Não deixe pseudocódigo estrutural em assinaturas Go, esqueletos `.ps1`, contratos JSON ou ordem de operações do host
11. Todo requisito funcional do PRD deve ser rastreado por ID no SPEC
12. Toda mudança estrutural deve dizer quais documentos serão atualizados no mesmo commit (sync rule)
13. Em etapas com risco estrutural, operacional, de segurança, rollout, rollback, migração de estado, concorrência, privilégio do host, contrato de reclaim/cleanup, secret ou indisponibilidade, o SPEC deve apontar explicitamente a disciplina correspondente em `disciplines/KAHNEMAN-DISCIPLINES.md`
14. Nenhum passo crítico pode ficar só com instrução operacional; ele deve registrar também pergunta obrigatória, evidência mínima e abort trigger
15. Em qualquer mudança com sequência multi-passo no host (drain→shutdown→optimize→start→restore), múltiplos writes de estado ou dois reclaimers concorrentes, o SPEC deve declarar explicitamente a fronteira de atomicidade: o que fica atômico nesta issue e o que continua fora dessa garantia (ex.: cada `os.WriteFile` é atômico; o ciclo de compactação não é)
16. Toda evidência mínima de etapa crítica deve dizer como será produzida no repo de forma executável, observável e reproduzível (`go test -race`, lint host `ps1_safety_test.go`, log esperado do `.ps1`, janela supervisionada documentada, medição VHDX antes/depois). Não vale evidência implícita, presumida ou sem caminho de execução descrito
17. Em qualquer mudança com migração de estado, troca one-time (ex.: re-anexar VHDX a SCSI), rollout ou risco de indisponibilidade, o SPEC deve definir separadamente:
    - rollback de aplicação (binário)
    - rollback de host (Scheduled Task / config Hyper-V)
    - rollback de estado (arquivos)
      e dizer explicitamente o que é permitido, proibido ou `forward-only` por ambiente
18. Aplique a política Day-0: o SPEC deve escolher a solução principal e única. Não liste shims, compatibilidade legada, dual-reader, dual-write, backfill ou código morto, salvo exceção explícita e justificada
19. Para estruturas de estado ainda não vivas, prefira consolidar o formato correto desde já em vez de criar caminho corretivo incremental
20. Se qualquer arquivo existir apenas para compatibilidade temporária, o SPEC deve marcar o arquivo para não criar ou para deletar, salvo exceção Day-0 documentada

### Guardrail cognitivo obrigatório

Em qualquer ITEM do SPEC que envolva migração de estado, privilégio do host, concorrência/exclusão mútua, rollout, rollback, contrato de reclaim/cleanup, secret, retry ou risco de indisponibilidade (deixar a VM Off, estourar o `V:`), inclua um bloco `Disciplina Kahneman` com:

- **Disciplina**: nome exato da disciplina em `disciplines/KAHNEMAN-DISCIPLINES.md` (ex.: #2 Counterfactual, #3 Número não adjetivo, #5 Availability/worst-case)
- **Link**: caminho do documento e, quando possível, âncora da seção correspondente
- **Pergunta obrigatória**: pergunta de Sistema 2 que precisa ser respondida antes de avançar
- **Evidência mínima**: métrica, teste, log, diff, output ou validação objetiva exigida
- **Abort trigger**: condição objetiva que impede avanço ou exige rollback

Nenhum passo crítico pode ficar apenas com instrução operacional.

> Exemplo real (SPECv3 do reclaim), DT-v3-1 (admission gate):
> **Disciplina** #5 Availability · **Pergunta** "O Optimize pode estourar o `V:`?" · **Evidência** gate pré-flight `liveVFree − HardFloor >= ScratchBudget`; Optimize ininterruptível (sem `Stop-Job`) · **Abort trigger** folga não cobrir o `ScratchBudget` medido → recusa começar.

Regras adicionais para etapas críticas:

- A evidência mínima deve ser reproduzível no estado real do repo; se depender de medição no host (ex.: scratch high-water do `Optimize-VHD`), o SPEC deve descrever a campanha (poll de 1 s, ≥5 runs supervisionados) que produz esse número antes de habilitar o gate
- Se houver rollback, o SPEC deve dizer se é de aplicação, de host (task/config) ou de estado
- Se algum rollback não for seguro fora de janela (ex.: reverter SCSI→IDE, rodar durante Windows Update), o SPEC deve registrar como política `forward-only`/janela explícita, com abort trigger correspondente

### Saída esperada

Quando gerar `SPECv2.md`/`SPECv3.md`, manter a mesma estrutura abaixo e adicionar logo após o H1:

```markdown
> Versão melhorada após auditoria do Passo 2.5.
> Baseline preservado: `SPEC.md` (e camada anterior `SPECv2.md`, se houver).
> Motivo: {resumo objetivo dos blockers corrigidos}.
> Onde houver conflito, **esta versão prevalece**.
```

#### Escopo fechado desta implementação

- O que entra agora
- O que fica explicitamente fora agora
- Dependências já assumidas como prontas

#### Matriz de rastreabilidade PRD → SPEC

| PRD  | Implementação no SPEC  |
| ---- | ---------------------- |
| RF-1 | ITEM-3, ITEM-4, ITEM-7 |

#### Decisões técnicas

Decisões tomadas que não estavam explícitas no PRD:

| #    | Decisão | Justificativa |
| ---- | ------- | ------------- |
| DT-1 | ...     | ...           |

> No caso reclaim, as decisões viraram a coluna vertebral do red-team: `DT-v3-1` (admission gate em vez de piso fixo), `DT-v3-2` (campanha de medição ANTES de baixar o piso), `DT-v3-3` (lock canônico único entre os dois reclaimers).

#### Fronteira de atomicidade e política de rollback

- **Fronteira de atomicidade desta implementação**:
  - o que esta issue garante atomicamente (ex.: cada `os.WriteFile` de estado; cada `Optimize-VHD` é uma operação Hyper-V única)
  - o que fica fora da atomicidade (ex.: o ciclo drain→shutdown→optimize→start→restore; a entrega SSH de métricas best-effort)
  - quais estados parciais continuam aceitos nesta fase (ex.: VM drenada mas não compactada; métricas stale → degrada para guest-only)
- **Política de rollback**:
  - rollback de app (`civmctl self-upgrade` anterior; subcomandos novos viram no-op)
  - rollback de host (`schtasks /delete`; reverter config Hyper-V em janela)
  - rollback de estado (N/A — Day-0; arquivos efêmeros)
  - o que é **proibido** (zero-fill sob baixo headroom; deixar a VM Off ao fim de qualquer caminho)
  - se algo é `forward-only`/exige janela

#### Mapa Kahneman por etapa crítica

Para cada etapa crítica da implementação, rollout, validação ou rollback, preencher:

| Etapa / ITEM | Disciplina Kahneman | Link                                  | Pergunta obrigatória | Evidência mínima | Abort trigger |
| ------------ | ------------------- | ------------------------------------- | -------------------- | ---------------- | ------------- |
| ITEM-3       | #5 Availability     | `disciplines/KAHNEMAN-DISCIPLINES.md` | ...                  | ...              | ...           |

#### Checklist de segurança (pré-implementação)

- [ ] Isolamento/concorrência: caminhos de reclaim concorrentes adquirem o lock canônico (`V:\civm-reclaim.lock`, `FileShare::None`) antes de qualquer `Stop-VM`; quem não obtém → skip exit 0
- [ ] Exec safety: `civmctl` usa `exec.CommandContext` sem shell; `.ps1` sem `Invoke-Expression` de input externo
- [ ] Privilégio do host: tasks que rodam como SYSTEM têm só o direito Hyper-V mínimo; sem rede; documentado
- [ ] Sudoers / safedelete: nenhuma ampliação de `deploy/sudoers.d/*` sem justificativa; capacidade destrutiva validada pelo propósito (não pela existência)
- [ ] Input validation: flags, JSON de métricas (+ timestamp/stale) e números de headroom validados antes de agir
- [ ] Fail-closed: sob incerteza (budget=0, métrica stale, folga insuficiente) o caminho perigoso **recusa começar**, não tenta
- [ ] Secrets: nenhuma credencial hardcoded; nada de segredo em `deploy/windows/`; usa `gh`/SSH já presentes
- [ ] Logs: `slog` estruturado / JSON; sem PII; nunca `fmt.Println`/`log.Printf` em produção; a task nunca deixa a VM Off em silêncio
- [ ] Int32 clamp: nenhum `[math]::Max(0, …)` literal nos `.ps1` (bug Int32 que travou todo reclaim do VHDX — invariante #17)

#### Mudanças de estado / constantes

Para cada constante ou arquivo de estado, na ordem de execução:

**Arquivo:** `internal/civm/civm.go` (bloco `const (...)`)

```go
// Reclamação de volume do host (docs/specs/host-reclaim-admission-gate).
DefaultHostVolumeHeadroomGB      = 8  // mínimo de V: livre ANTES do Optimize (caminho normal)
DefaultHostVolumeHardFloorGB     = 1  // piso DURO absoluto; nunca operar abaixo (Optimize é ininterruptível)
DefaultHostVolumeScratchBudgetGB = 0  // pior scratch high-water MEDIDO + margem; 0 = emergência DESABILITADA
DefaultAutoreclaimPressureGB     = 25 // abaixo disso, cadência de DETECÇÃO curta (não de ação)
DefaultReclaimMinIntervalMin     = 30 // mínimo entre eventos reais de Stop-VM+Optimize
```

- **Quem lê:** `internal/hostdisk`, o helper de admissão `EmergencyAdmits`, os `.ps1` de reclaim (via parâmetro)
- **Invariante:** `HardFloor(1) < Headroom(8) < Pressure(25)`; `ScratchBudget >= 0`; o gate de emergência só habilita quando `ScratchBudget > 0`
- **Política Day-0:** consolidar a constante correta desde já; **a constante de emergência (`ScratchBudget`) é commit explícito com as ≥5 medições anexadas** (Número, não adjetivo)
- **Migração de estado:** **N/A — Day-0** por padrão (estado efêmero). Só descrever migração quando houver arquivo de estado vivo que precise ser convertido
- **Disciplina Kahneman** quando a constante for crítica:
  - **Disciplina**: #2 Counterfactual / #3 Número não adjetivo
  - **Link**:
  - **Pergunta obrigatória**:
  - **Evidência mínima**:
  - **Abort trigger**:

#### Arquivos a CRIAR

Para cada arquivo novo:

**`caminho/completo/desde/raiz/arquivo.ext`**

- **Propósito**: uma frase
- **Requisitos cobertos**: `RF-N`, `DT-N`
- **Structs/Types/Interfaces** (Go) ou **esqueleto vinculante** (`.ps1`): assinatura/ordem de operações exata
- **Funções**: assinatura exata + lógica resumida em passos
- **Dependências internas**: imports do projeto (`internal/**`, `internal/civm`)
- **Dependências externas**: stdlib / `golang.org/x/sys`; cmdlets Hyper-V no host
- **Padrão de referência**: arquivo existente no repo (ex.: `internal/capacity/capacity.go` para `Options{...Fn}` injetável; `deploy/windows/civm-vhdx-optimize.ps1` para a estrutura `try/catch/finally`)
- **Testes requeridos**: arquivo de teste e cenários mínimos (Go `*_test.go`; ou assertion de lint host em `internal/hostdisk/ps1_safety_test.go`)
- **Disciplina Kahneman** se o arquivo suportar etapa crítica:
  - **Disciplina**:
  - **Link**:
  - **Pergunta obrigatória**:
  - **Evidência mínima**:
  - **Abort trigger**:

> Esqueleto real (SPECv3, `civm-vhdx-autoreclaim.ps1`):
>
> ```powershell
> # Lock canonico compartilhado ANTES de tudo (DT-v3-3):
> #   abrir V:\civm-reclaim.lock FileShare::None; se falhar -> reclaim_skip_other_active; exit 0
> # Rate-limit (DT-v3-4): if (now - lastReclaim < MinIntervalMin) { skip_ratelimited; exit 0 }
> #   $live = Get-VFreeGB                                  # SEMPRE ao vivo (Get-PSDrive)
> #   if ($live -ge ThresholdGB) { skip_threshold; exit 0 }
> #   if ($live -lt HeadroomGB) {
> #       if (ScratchBudgetGB -le 0) { abort_headroom (gate disabled); exit 2 }   # sem medicao -> sem emergencia
> #       if ($live - HardFloorGB -lt ScratchBudgetGB) { abort_insufficient_slack; exit 2 }
> #       $emergency = $true
> #   }
> #   ... fstrim; Stop-VM; wait Off; Mount-VHD -ReadOnly ...
> #   Optimize-VHD -Path $VhdxPath -Mode Full -ErrorAction Stop   # ININTERRUPTIVEL: sem Stop-Job
> #   ... finally Start-VM (3x); liberar locks; gravar civm-reclaim-last.txt ...
> ```

#### Arquivos a MODIFICAR

Para cada arquivo existente:

**`caminho/completo/desde/raiz/arquivo.ext`**

- **O que muda**: descrição cirúrgica
- **Requisitos cobertos**: `RF-N`, `DT-N`
- **Função/bloco/constante afetada**: nome exato
- **Antes**: trecho ou shape atual relevante
- **Depois**: shape novo esperado
- **Por quê**: vínculo ao PRD
- **Impacto**: quebra assinatura? exige ajuste em callers? afeta docs? afeta a sync rule? afeta o contrato de reclaim/cleanup?
- **Testes requeridos**: quais cenários precisam ser cobertos
- **Disciplina Kahneman** se a mudança for crítica:
  - **Disciplina**:
  - **Link**:
  - **Pergunta obrigatória**:
  - **Evidência mínima**:
  - **Abort trigger**:

#### Arquivos a DELETAR (se houver)

| Arquivo        | Motivo                       |
| -------------- | ---------------------------- |
| `path/to/file` | substituído por X / removido |

#### Observabilidade

**Métricas / contadores do host** (se aplicável)

- shape JSON exato (`V:\civm-host-metrics.json`): `v_free_gb`, `v_size_gb`, `vhdx_file_size_gb`, `vhdx_min_size_gb`, `vhdx_max_size_gb`, `guest_free_gb`, `gap_gb`, `timestamp`, `delivery_status`
- como o guest consome (`civmctl host-disk` → `level` ok/warn/crit, `stale`, `headroom_violation`)
- quais fluxos emitem/observam (task `civm-host-metrics`; alarme nos pisos 30/10 GB)

**Logs estruturados**

| Evento           | Level | Campos                                          |
| ---------------- | ----- | ----------------------------------------------- |
| `optimize_start` | Info  | `vhdx_path`, `v_free_before_gb`                 |
| `optimize_end`   | Info  | `v_free_after_gb`, `scratch_high_water_gb`, `duration_ms` |
| `abort_headroom` | Error | `v_free_gb`, `headroom_gb`                       |
| `vm_left_off`    | Error | `attempts`, `last_error`                         |

> Guest = `slog`/JSON (nunca `fmt.Println` em produção); host = log estruturado em `V:\civm-hyperv-maintenance.log`. Sem PII, sem segredo, sem label de alta cardinalidade.

#### Contratos e documentação viva

Preencha explicitamente (sync rule: o que muda contrato/convenção entra no **mesmo commit**):

| Documento                                       | Atualização necessária | Motivo                              |
| ----------------------------------------------- | ---------------------- | ----------------------------------- |
| `cmd/civmctl/main.go` (`printHelp`)             | Alterar / N/A          | subcomando novo?                    |
| `internal/civm/civm.go`                         | Alterar / N/A          | constante que gateia comportamento? |
| `deploy/windows/*.ps1` + `register-*.ps1`       | Criar / Alterar / N/A  | script host / Scheduled Task?       |
| `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md`     | Criar / Alterar / N/A  | procedimento operacional mudou?     |
| `runbooks/MULTI-PROJECT-RUNNER.md` §Disk        | Alterar / N/A          | headroom/observabilidade/rollback?  |
| `README.md` / `AGENTS.md` / `CODEX.md` / `rules/*.md` | Alterar / N/A     | sync rule (contrato/convenção)?     |
| `docs/specs/{slug}/IMPL.md`                      | Criar                  | registro do que foi feito           |
| `disciplines/KAHNEMAN-DISCIPLINES.md`           | Alterar / N/A          | nova disciplina, link ou anchor?    |
| `disciplines/INVARIANTS.md`                      | Alterar / N/A          | novo gate/invariante?               |

#### Ordem de implementação

Lista numerada, verificável e sem gaps:

1. Diagnóstico / baseline (Slice 0, sem código — mede o root cause)
2. Constantes (`internal/civm/civm.go`) + helper de admissão (`EmergencyAdmits`)
3. Lógica de domínio (`internal/{diskdoctor,maintenance,hostdisk}`)
4. Subcomandos `cmd/civmctl/*.go` + dispatch/`printHelp`
5. Componente host observabilidade (`deploy/windows/civm-host-metrics.ps1` + register)
6. Troca one-time / pipeline (ex.: SCSI/discard) em janela + verificação
7. Componente host de ação (`civm-vhdx-optimize.ps1` / `civm-vhdx-autoreclaim.ps1` + register SYSTEM)
8. Logs / contadores / lint host
9. Testes unitários (Go `-race`)
10. Validação em janela supervisionada (medição VHDX antes/depois)
11. Documentação viva (runbook + sync rule)

#### Plano de testes

**Guest (Go)**

- unitários: casos (mock `os/exec`, `ReadFileFn`/`RunFn` injetáveis)
- helper de admissão: `EmergencyAdmits(liveFree, hardFloor, budget)` (folga insuficiente, budget=0 desabilita, folga exata)
- concorrência / idempotência: enter/exit re-run no-op; `flock` serializa; estado parcial restaura correto
- integração (`-race`, na VM): `disk-doctor` real (lsblk/proc), `maintenance` com gh/systemctl mock

**Host (PowerShell, lint + janela)**

- lint `internal/hostdisk/ps1_safety_test.go`: ausência de `Stop-Job` no caminho do Optimize; presença do lock canônico e do gate `abort_insufficient_slack`; sample `scratch_high_water_gb`; sem `[math]::Max(0, …)` Int32
- janela supervisionada: 5 ciclos de medição; `V: < 8` com `ScratchBudget=0` ainda aborta (sem regressão); com budget setado, folga entre `HardFloor+Budget` e `8` admite e completa; dois reclaimers simultâneos → o 2º `reclaim_skip_other_active`; `Start-VM` falha simulada → 3 tentativas → CRITICAL exit; VM nunca fica Off

**Manuais (evidência das etapas críticas)**

- `disk-doctor --json` colado no IMPL (root cause)
- log `optimize_start/end` com before/after FileSize e `scratch_high_water_gb`
- `host-disk --json` mostrando `level` cruzando 30/10 GB
- evidências objetivas exigidas pelo mapa Kahneman

#### Checklist de validação

**Guest (Go)**

- [ ] `gofmt -w ./...`
- [ ] `golangci-lint run -c .golangci.yml ./...`
- [ ] `go vet ./...`
- [ ] `go test ./... -race -count=1`
- [ ] `go test -count=1 -cover ./internal/...` (≥80% por package)
- [ ] `govulncheck ./...`
- [ ] `go build -ldflags='-s -w' -o /tmp/civmctl ./cmd/civmctl` (compila + < 10MB)

**Host (PowerShell)**

- [ ] lint host (`ps1_safety_test.go`): sem `Stop-Job` no Optimize; lock canônico; sample de scratch; sem clamp Int32
- [ ] PSScriptAnalyzer nos `.ps1` (se disponível)
- [ ] `schtasks /run /tn civm-vhdx-optimize` em janela: aborta sob baixo headroom; religa em erro; nunca deixa VM Off
- [ ] Janela: ≥5 medições coletadas e `ScratchBudgetGB` definido por commit com evidência

**Docs**

- [ ] Links locais resolvem (job `validate-templates`)
- [ ] Sync rule: README ≡ AGENTS ≡ CODEX ≡ rules no mesmo commit se contrato/convenção mudou

**Gates cognitivos**

- [ ] Cada etapa crítica aponta para `disciplines/KAHNEMAN-DISCIPLINES.md`
- [ ] Cada etapa crítica registra pergunta obrigatória, evidência mínima e abort trigger
- [ ] Não há linguagem vaga em pontos críticos sem critério observável

---

## PASSO 2.5 — Auditoria do SPEC (red-team, opcional por risco)

> **Use quando a implementação tiver risco estrutural, operacional ou de segurança.**
> Existe para reduzir ambiguidade antes do código, não para burocratizar mudanças pequenas.
> No caso reclaim, **rodou duas vezes**: SPEC → SPECv2 (4 CRÍTICOS de robustez operacional do host) e SPECv2 → SPECv3 (2 CRÍTICOS: evidência N=1 não-medida + `Stop-Job` não aborta `Optimize-VHD`). Os no-go ficaram registrados no próprio SPEC para não reincidir.

### Quando usar

Use o Passo 2.5 quando a mudança envolver um ou mais destes pontos:

- camada host que muta VM/VHDX (`Stop-VM`/`Optimize-VHD`/`Start-VM`) — risco de deixar a VM Off ou estourar o `V:`
- contrato de reclaim/cleanup, guards de headroom/admissão, ordem fail-closed
- subcomando `civmctl` que muta (`runner restart/remove/upgrade`, `bootstrap`, `maintenance`, `hook install`)
- superfície de privilégio (`deploy/sudoers.d/*`, Scheduled Task SYSTEM, `civm-safedelete`)
- concorrência / exclusão mútua entre reclaimers, watchdogs ou tasks; locks
- constantes que gateiam comportamento; janela operacional / restart ordenado
- quebra de invariante documentado; risco alto de indisponibilidade ou drift de configuração

### Quando pode pular

Quando a mudança for pequena, local e sem risco estrutural relevante:

- poucos arquivos
- sem mutação de VM/VHDX nem do contrato de reclaim/cleanup
- sem impacto em privilégio, concorrência, constantes ou infra do host
- sem necessidade de rollout/janela especial

### Prompt

Revise `docs/specs/{feature-slug}/SPEC.md` como auditoria pré-implementação.

Se `docs/specs/{feature-slug}/SPECv2.md` (ou `SPECv3.md`) existir e tiver sido criado como resposta a um `no-go` anterior, revise a versão mais recente como candidato ativo.

Revisão de lacunas com foco em:

- ambiguidades técnicas ainda não resolvidas
- fronteira de atomicidade implícita, ambígua ou incompatível com o código/host real
- riscos operacionais de rollout, shutdown/restart da VM e rollback
- evidência mínima que não tenha caminho executável claro no repo (ex.: "não estourou o `V:`" inferido de "não deu erro", sem poll de `V:` durante o Optimize)
- rollback descrito de forma genérica sem separar app, host (task/config) e estado
- uso de mecanismo tecnicamente existente mas operacionalmente inseguro (ex.: `Stop-Job` "abortando" um `Optimize-VHD` que na verdade é ininterruptível e segue escrevendo o `V:`)
- dependências não mapeadas entre guest, host, constantes, docs e automação
- gaps de privilégio, fail-closed, exclusão mútua e consistência do estado (VM/VHDX/disco)
- inconsistências entre requisitos, decisões técnicas, arquivos listados, testes e validação final
- qualquer item do SPEC que ainda exija interpretação durante a implementação
- ausência de disciplina cognitiva explícita nas etapas críticas
- passos críticos sem pergunta obrigatória, evidência mínima ou abort trigger
- uso de linguagem vaga (`validar`, `garantir`, `confirmar`, `se necessário`) sem critério observável
- piso/threshold rígido que apenas **realoca** um deadlock para um valor menor em vez de resolvê-lo
- gaps entre etapas críticas do SPEC e as disciplinas documentadas em `disciplines/KAHNEMAN-DISCIPLINES.md`
- presença de workaround, shim, compatibilidade legada, dual-reader, dual-write ou migração corretiva de estado sem exceção Day-0 explícita
- código novo mantendo versão antiga sem estado vivo

### Formato da resposta

1. Liste os findings primeiro, ordenados por severidade
2. Para cada finding, cite a seção/ITEM exato do SPEC afetado
3. Depois liste `Open questions`
4. Depois diga se o SPEC está pronto para implementação
5. Feche com `go` ou `no-go`

### Regra de saída

- Se houver finding que exija decisão nova, volte ao **Passo 2**
- Se o arquivo auditado foi `SPEC.md` e o resultado foi `no-go`, **não pare apenas na auditoria**: volte ao **Passo 2 no mesmo turno**, crie `SPECv2.md` corrigindo os findings bloqueantes e preserve o `SPEC.md` original
- Se o arquivo auditado foi `SPECv2.md` e o resultado foi `no-go`, **não pare apenas na auditoria**: volte ao **Passo 2 no mesmo turno** e atualize `SPECv2.md` in-place ou crie `SPECv3.md`, conforme o pedido
- Ao criar ou atualizar o SPEC corrigido depois de um `no-go`, registre no arquivo:
  - qual SPEC foi auditado
  - quais findings bloqueantes foram endereçados (no caso reclaim: "`Stop-Job` não aborta `Optimize-VHD`", "evidência N=1 não mediu scratch")
  - que a versão corrigida é o candidato ativo para nova auditoria
- Se houver etapa crítica sem link para disciplina Kahneman aplicável, sem evidência mínima ou sem abort trigger, o resultado deve ser `no-go`
- Se houver mecanismo de segurança que não funciona como descrito (abort que não aborta, guard só point-in-time numa corrida real), o resultado deve ser `no-go`
- Se houver violação da política Day-0 sem exceção documentada, o resultado deve ser `no-go`
- Se a auditoria resultar em `go`, siga para o **Passo 3**

---

## PASSO 3 — Implementação (a partir do SPEC ativo)

> **Leia o SPEC ativo e execute passo a passo.**
> O Passo 3 não fecha lacunas arquiteturais; implementa o que já foi decidido.
> O SPEC ativo é a última versão auditada com `go`: `SPECv3.md` quando existir e aprovado pelo Passo 2.5; senão `SPECv2.md`; senão `SPEC.md`.

### Prompt

Implemente a feature do SPEC ativo de `docs/specs/{feature-slug}/`.

Use a versão mais recente que recebeu `go` no Passo 2.5 (`SPECv3.md` > `SPECv2.md` > `SPEC.md`). Não altere as versões baseline preservadas.

### Regras de execução

1. Siga a ordem de implementação do SPEC (no caso reclaim, a ordem é OBRIGATÓRIA: medição → lock canônico → admission gate → cadência/footprint)
2. Use os trechos, assinaturas, esqueletos `.ps1` e contratos do SPEC como base
3. Não adicione funcionalidade fora do escopo fechado
4. Se encontrar um gap, volte ao Passo 2 antes de continuar
5. Toda alteração em contrato/convenção deve ser refletida em docs e na sync rule no mesmo ciclo
6. Toda alteração em estado, privilégio, concorrência ou comportamento de reclaim/cleanup deve ser validada com teste (Go `-race` e/ou lint host) — e número, não adjetivo, para qualquer claim de comportamento
7. Não refatore código adjacente sem necessidade funcional
8. Em qualquer item crítico, execute também o bloco `Disciplina Kahneman` antes de avançar para a próxima fatia
9. Implemente a solução Day-0 limpa definida no SPEC. Não adicione shims, fallbacks, compatibilidade com versões antigas, migração corretiva de estado ou dead code
10. Se durante a implementação parecer necessário manter duas versões, pare e volte ao Passo 2 para registrar a exceção Day-0
11. Quando o SPEC consolidar um formato de estado/constante, reescreva/remova o código antigo necessário em vez de preservar caminhos mortos
12. Registre o resultado em `docs/specs/{feature-slug}/IMPL.md` (commits, arquivos, decisões pequenas que não pediram nova ADR, métricas de validação) e cite os requirement IDs cobertos em cada commit/PR

### Ritual de execução por fatia

Para cada fatia do SPEC:

1. implementar apenas o item atual
2. validar compilação (`go build`)
3. validar testes relacionados (`go test -race`; lint host se tocou `.ps1`)
4. validar a pergunta obrigatória, a evidência mínima e o abort trigger quando o item tiver disciplina Kahneman
5. comparar com o SPEC
6. só então avançar

> **Micro-slicing (anti-falácia do planejamento):** nunca implemente múltiplos padrões / dezenas de arquivos de uma vez — uma fatia ortogonal por vez → commit atômico → valida e testa → próxima. O Sistema 1 é otimista demais.

### Checklist durante a implementação

A cada camada concluída:

- [ ] Código compila sem erros
- [ ] Lint passa (`golangci-lint`; lint host de `.ps1` se aplicável)
- [ ] Testes existentes continuam passando (`-race`)
- [ ] Novos testes da fatia foram adicionados (cobertura ≥80% no package)
- [ ] Fail-closed mantido (sob incerteza, recusa começar o caminho perigoso)
- [ ] Contrato/constantes atualizados quando necessário (sync rule)
- [ ] Docs/runbook atualizados quando o item exige
- [ ] Etapas críticas continuam coerentes com `disciplines/KAHNEMAN-DISCIPLINES.md`

### Quando voltar ao SPEC

- Descobriu necessidade de constante/arquivo de estado não previsto
- Subcomando precisa de flag/campo extra
- Apareceu edge case do host não coberto (optimize pendurado, controlador inesperado, métrica stale)
- Ordem de implementação não fecha
- Mudou shape de saída JSON, exit code ou contrato de reclaim/cleanup
- Surgiu necessidade de janela, troca one-time ou rollback não descritos
- A etapa crítica exige decisão que o mapa Kahneman do SPEC ainda não fechou
- Surgiu necessidade de manter versão antiga, shim, dual path ou compatibilidade não documentada como exceção Day-0

> **Regra absoluta:** se o código precisar decidir algo que o SPEC não decidiu, a implementação deve parar e o SPEC deve ser atualizado primeiro.
> Se o SPEC ativo for `SPECv3.md`, atualize `SPECv3.md`; não altere as versões baseline.

### Validação final

Ao terminar toda a implementação, execute o checklist do SPEC:

**Guest (Go)**

```bash
gofmt -w ./...
golangci-lint run -c .golangci.yml ./...
go vet ./...
go test ./... -race -count=1
go test -count=1 -cover ./internal/...                # ≥80% por package
govulncheck ./...
go build -ldflags='-s -w' -o /tmp/civmctl ./cmd/civmctl && stat -c%s /tmp/civmctl
```

**Host (PowerShell)**

```bash
# lint host (parte do go test -race): varre deploy/windows/*.ps1
go test ./internal/hostdisk/
# em janela supervisionada:
schtasks /run /tn civm-vhdx-optimize        # aborta sob baixo headroom; religa em erro; nunca deixa VM Off
```

**Integração (na VM, com sudo NOPASSWD para o wrapper)**

```bash
go test -tags=integration -run TestIntegration ./internal/safedelete/
civmctl parity                              # paridade com ubuntu-latest
```

---

## Critérios de saída entre passos

### PRD → SPEC

Só avance se:

- houver uma opção recomendada clara
- os requisitos funcionais estiverem fechados
- os riscos estruturais (VM/VHDX, headroom, privilégio, concorrência) estiverem explícitos
- o fora de escopo estiver definido

### SPEC → Implementação

Só avance se:

- cada requisito do PRD estiver rastreado
- a ordem de implementação estiver fechada
- arquivos a criar/modificar (Go + `.ps1` + constantes) estiverem explícitos
- plano de testes e docs estiverem definidos
- etapas críticas estiverem mapeadas para `disciplines/KAHNEMAN-DISCIPLINES.md`
- cada etapa crítica tiver pergunta obrigatória, evidência mínima e abort trigger
- se o risco for alto, o Passo 2.5 tiver resultado em `go` (e os no-go anteriores estiverem registrados)

### Implementação → Commit

Só avance se:

- código, testes e docs estiverem consistentes com o SPEC
- validações finais (Go `-race`/cobertura/lint/govulncheck/build + lint host) tiverem sido executadas
- não houver drift entre contrato, constantes, subcomandos, `.ps1` e runbook (sync rule)
- não houver drift entre o que foi implementado e os guardrails cognitivos descritos no SPEC
- cada commit não-trivial (`feat`/`fix`/`refactor`/`perf`) carregue `Rollback trigger:` numérico e cite os requirement IDs cobertos

---

## Referência rápida — Stack do `civm`

| Camada                  | Tecnologia                                            | Versão / detalhe     |
| ----------------------- | ----------------------------------------------------- | -------------------- |
| Linguagem (guest)       | Go (stdlib-first)                                     | 1.26                 |
| CLI                     | `cmd/civmctl/` + dispatch por `switch`                | —                    |
| Domínio                 | `internal/**` (capacity, hostdisk, diskdoctor, maintenance, idle, cleanup, …) | — |
| Exec externo            | `os/exec` (`exec.CommandContext`, sem shell)          | stdlib               |
| Logging                 | `log/slog` (JSON estruturado)                         | stdlib               |
| Estado                  | arquivos JSON (`/var/lib/civm/*.json`) + locks (`flock`/`FileShare::None`) | — |
| Disco / `Statfs`        | `golang.org/x/sys/unix`                               | —                    |
| Camada host             | PowerShell em `deploy/windows/*.ps1` + `register-*.ps1` | —                  |
| Hypervisor              | Hyper-V (`Get-VM`/`Stop-VM`/`Start-VM`, `Get-VHD`/`Optimize-VHD`/`Resize-VHD`) | — |
| Agendamento (guest)     | systemd timers/units (`deploy/systemd/`)              | —                    |
| Agendamento (host)      | `schtasks` Scheduled Tasks (SYSTEM, `/RL HIGHEST`)    | —                    |
| Discard / reclaim       | `fstrim` / `discard`, `lsblk -D`, controlador SCSI vs IDE | —                |
| Runner                  | GitHub Actions self-hosted runner (`actions.runner.*`, `gh api`) | 2.336.0   |
| Lint / segurança        | `golangci-lint` (`.golangci.yml`), `gosec`, `govulncheck`, `go vet` | —      |
| Testes                  | `testing` stdlib + `go test -race`; lint host `internal/hostdisk/ps1_safety_test.go` | — |
| CI                      | `.github/workflows/ci.yml` (`validate-templates`, `build-civmctl`, `self-hosted-smoke`) | — |

---

## Regra de ouro

> O **PRD** decide o que e por quê.
> O **SPEC** fecha como, onde, em que ordem e com quais guardrails.
> A **implementação** executa sem reinventar a decisão.

## Quando iterar

- Se o Passo 3 achar um gap real, volte ao Passo 2
- Se o Passo 2 achar ambiguidade insolúvel, volte ao Passo 1
- Se o Passo 2.5 achar mecanismo de segurança que não funciona como descrito, é `no-go` → volte ao Passo 2 e gere a próxima versão (SPECv2/SPECv3)
- Nunca resolva um gap estrutural só no código
- Nunca crie `PRD.md` ou `SPEC.md` fora de `docs/specs/{feature-slug}/`
