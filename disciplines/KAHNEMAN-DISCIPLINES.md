# Kahneman disciplines — template portátil de higiene de decisão

> Template portátil mantido pelo `civm`. Os exemplos são do próprio `civm`
> (repo Go de infra: `cmd/civmctl/`, `internal/**`, `.github/workflows/ci.yml`).
> Um peer repo que adote esta metodologia troca os exemplos pelo próprio
> contexto — a metodologia (Sistema 2, counterfactual, anti-noise) é o que se
> transporta.

Disciplinas operacionais derivadas de **Thinking, Fast and Slow** (Kahneman, 2011) e **Noise** (Kahneman, Sibony, Sunstein, 2021). Doc autocontido — não depende de nenhum outro repositório.

LLMs falam Sistema 1 com fluência altíssima — articulam bem até quando estão errados. O humano (ou o agente AI quando assume papel humano) precisa ser o Sistema 2: verificar, medir, questionar o que parece óbvio. Este doc codifica os atritos que evitam que decisão fluente vire decisão errada.

**Quem deve ler:** dev antes de iniciar PRD em SSDV3, agente AI antes de propor mudança não-trivial em `internal/**` (ex.: watchdogs, runner restart, safedelete), revisor de PR.

---

## Arquitetura — 4 camadas

```
Disciplinas (do livro Kahneman/Sibony/Sunstein)
  ↓ instanciam
Rubricas (SSDV3, PR review, rules/*.md)
  ↓ aplicam-se a
Arquitetura (decisões com rollback trigger numérico no commit body)
  ↓ executam-se em
Operacional (gates de ci.yml: go vet/lint/govulncheck/test -race/cobertura ≥80%)
```

---

## As 16 disciplinas

| # | Disciplina | Regra (1 linha) | Exemplo civm | Status no civm |
|---|---|---|---|---|
| 1 | WYSIATI | Declarar o não-visto antes de opinar | "Sem ter medido o disk-watchdog sob 60-90% real, estimo que cleanup libera X GB com confiança Y%" | Manual (PR review) |
| 2 | Counterfactual obrigatório | Rollback trigger numérico em commit não-trivial | "se p99 do restart do runner passar de 30s em 3 rodadas, reverter o sentinel de auto-restart" no body | **Gate de commit** (`AGENTS.md` §Commits; INVARIANTS #15) |
| 3 | Número não adjetivo | Métrica antes de adjetivo | "binário 8.2MB stripped (<10MB RNF-3)" não "leve". Anti-padrões: "obviamente", "claramente" | Manual (PR review); RNF-3 medido em CI (INVARIANTS #7) |
| 4 | Anchoring em estimativas | Reference class antes de bottom-up | Custo de uma nova feature de `civmctl` estimado pela reference class de comandos já entregues (doctor, capacity, cleanup) | Manual (PRD/SPEC) |
| 5 | Availability heuristic | Worst-case, não happy path | `capacity` hard-fail a 90% de disco e cleanup a 60% (`internal/civm` `DefaultPreCleanupPct`) desenhados para o runner cheio, não o happy path | Confirmado em `internal/capacity`, `internal/diskwatchdog` |
| 6 | Confiança calibrada | Intervalo, não ponto | "drift `--timeout=15` ±jitter" não "instantâneo"; "TTL com jitter" em watchdogs | Manual |
| 7 | Hindsight bias | Postmortem separa processo de outcome | Postmortem do wedge do VHDX (Int32 clamp) separa "decisão com info disponível" de "o que aconteceu" — virou o gate `ps1_safety_test.go` | Manual; o gate #17 nasceu de um postmortem |
| 8 | Planning fallacy | Multiplicador de reference class | Estimativa inside view × multiplicador baseado em PRs já fechados; "tempo é alarme, não meta" | Manual |
| 9 | Substituição de pergunta | Qualitativa vira métrica | "o cleanup é seguro?" → cobertura ≥80% por package, `go test -race` verde, teste de integração de `safedelete` contra fixture root-owned real | **Parcial** (cobertura = INVARIANTS #6; race = #5) |
| 10 | Hyperbolic discounting | TODO sem owner+date é débito explícito | `// TODO(@user, YYYY-MM-DD): ...` ou issue rastreável; `rules/testing.md` proíbe `t.Skip("flaky")` sem issue + prazo | Manual (PR review) |
| 11 | Halo effect em libs | Dep nova exige justificativa explícita | civm é stdlib-first (`go.mod` mínimo, só `testify` em teste); toda dep nova precisa de razão registrada no PR (alternativas, custo, rollback) | Manual (PR review); `govulncheck` cobre CVE (INVARIANTS #4) |
| 12 | Priming em prompts | Framing adversarial em PR review e SSDV3 | "qual problema esse código tem?" / "liste 3 cenários de falha do watchdog" — não "está OK?" | Manual (cultural) |
| 13 | Ilusão de validade | Valide o propósito, não a existência (existe ≠ funciona); pareie recusa com "legítimo passa?" | safedelete recusava root-owned e teste verde afirmava a recusa (#59); fix = teste de integração contra o modo de falha real | **Teste de integração obrigatório** (`internal/safedelete`, INVARIANTS #9) |
| 14 | Retry calibrado | Re-tentar só com assinatura transitória PROVADA (rate-limit, lease, rede); falha determinística (migrate, build, teste) falha-rápido — nunca mascarada por re-tentativa. Um retry que "às vezes passa" esconde bug determinístico (corolário de #13) e reage à explicação disponível "é flake de infra" sem investigar (#1/#5) | `admit.go` re-tenta heavy só quando `tryAcquireHeavy` devolve `retry=true` (contenção de slot), não em erro real; `dockerlock` faz backoff só em lock contido. Anti-padrão evitado: loop `for 1 2 3` que trata QUALQUER `exit≠0` como flake (visto em `acme/.github/workflows/web.yml` re-tentando `migrate exit 1` 3× como "transient" — corrigido 2026-06: `web.yml` passou a chamar `devctl ci classify` (classificador tipado, acme ADR-088); `go.yml` ainda usa grep das mesmas assinaturas, follow-up) | Confirmado em `internal/admit`, `internal/dockerlock` (classificam); gate de CI no peer (`go.yml`/`web.yml`) |
| 15 | Fail-safe default + curador independente | Modo de falha seguro é o DEFAULT (esquecer = falhar barulhento, nunca mascarar); o curador (reclaim/admissão/breaker) não morre junto com o recurso que cura nem decide por medição stale. "Código existe" ≠ "proteção ativa": o artefato corrente tem que estar de fato rodando no alvo | death-spiral do disco (#106): o autoreclaim abortava a `V:`<8GB — o curador morria com o recurso — e o `.ps1` corrigido no repo rodava STALE em `C:\civm-deploy`; fix = gate de duas fases (`autoreclaim_post_off_remeasure`; abort-threshold ≠ trigger-threshold) + re-medição VIVA pós-`Stop-VM` (`Get-PSDrive V`, não JSON de 10 min) + `EmergencyAdmits` fail-closed; `admit.go` fail-closed (CheckFn err → backoff, nunca admite); `fstrim` best-effort não bloqueia o reclaim | Confirmado em `deploy/windows/civm-vhdx-autoreclaim.ps1` (gate 2 fases), `internal/civm` (`EmergencyAdmits`), `internal/admit` (fail-closed); lint `internal/hostdisk/specv3_reclaim_test.go` + `internal/civm/reclaim_test.go` (INVARIANTS #5/#6) |
| 16 | Idempotência de efeitos replayáveis | Todo efeito atrás de retry/replay/re-registro é idempotente (aplicar 2× = aplicar 1×); só assim retry (#14) e fail-safe (#15) são SEGUROS | `deploy/bin/setup-registry-cache.sh` reconcilia o estado ("rodar de novo nunca duplica"); `register-*.ps1` re-registra via `schtasks /delete /f` + `/create` (idempotente); reclaim re-disparado acima do threshold = `autoreclaim_skip_threshold` no-op. Peer (acme): `idempotency_keys`, outbox, goose versionado + validação de efeito em `run-migrations.sh` | Confirmado em `deploy/bin/setup-registry-cache.sh`, `deploy/windows/register-*.ps1`; Manual (PR review) |

---

## Top-5 operacionais (espelha `AGENTS.md` §"Decision hygiene")

1. **WYSIATI (#1)** — declarar o NÃO-visto antes de opinar.
2. **Counterfactual (#2)** — `Rollback trigger:` no body de commits não-triviais (`feat/fix/refactor/perf`).
3. **Número não adjetivo (#3)** — claim de perf precisa de medição com unidade, intervalo e número de rodadas.
4. **Débito é dívida com juros (#10)** — TODO sem owner+date é débito; `t.Skip("flaky")` sem issue + prazo é proibido (`rules/testing.md`).
5. **Lib nova exige justificativa explícita (#11)** com critério mensurável — civm é stdlib-first.

Quando a pergunta é qualitativa, responder com métrica antes do adjetivo (#9): cobertura ≥80% por package em `internal/**`, arquivo ≤800 linhas, `go test -race` verde, binário <10MB.

---

## Rubricas ativas

| Procedimento | Onde vive | Disciplinas que opera |
|---|---|---|
| SSDV3 (PRD → SPEC → IMPL) | `docs/specs/<slug>/`, `disciplines/SSDV3-PROMPTS.md`, `rules/ssdv3.md` | #1, #2, #3, #5, #9 |
| Gates de CI | `.github/workflows/ci.yml` (jobs `build-civmctl`, `validate-templates`, `self-hosted-smoke`) | #2 (rollback no commit), #3 (RNF-3 medido), #9/#13 (cobertura + integração safedelete) |
| Sync rule (README ≡ AGENTS ≡ CODEX ≡ rules) | `AGENTS.md` §"Sync rule"; skip via `[sync-skip-justified]` | #12 (cultural) |
| Postmortem operacional | issues + commit body (ex.: wedge do VHDX → gate `ps1_safety_test.go`) | #5, #7, #8 |
| Reference class para estimativas | PRs/comandos `civmctl` já entregues | #4, #8 |
| Justificativa de dep nova | PR description (alternativas, custo, rollback); `go.mod` mínimo | #11 |

Rules granulares por domínio em `rules/`:

- **Governança:** `governance.md` (issues, PRs, labels, PT-BR)
- **Teste:** `testing.md` (cobertura ≥80% por package, Go + testify, sem `t.Skip` órfão)
- **Segurança:** `security.md` (secrets, gitleaks, self-hosted runner)
- **Observabilidade:** `observability.md` (slog estruturado, doctor/capacity/disk-audit read-only)
- **Metodologia:** `ssdv3.md`

---

## Mapeamento disciplina → gate civm

| Disciplina | Gate civm (INVARIANTS.md) | Forma de check | Bloqueio? |
|---|---|---|---|
| #2 Counterfactual | #15 Rollback trigger | `Rollback trigger:` no body para `feat\|fix\|refactor\|perf` (`AGENTS.md` §Commits) | Sim (PR review / merge) |
| #3 Número (proxy RNF-3) | #7 Binário <10MB | `go build -ldflags='-s -w'` + `stat -c%s` no job `build-civmctl` | Sim (CI) |
| #9 Substituição (cobertura como métrica) | #6 Cobertura | `go test -count=1 -cover ./internal/...` ≥80% por package | Sim (CI) |
| #5/#9 Worst-case (race) | #5 `go test -race` | detector de race no job `build-civmctl` | Sim (CI) |
| #13 Ilusão de validade | #9 Integração safedelete | `go test -tags=integration` contra fixture root-owned real | Sim (CI self-hosted) |
| #7 Hindsight (postmortem → gate) | #17 Int32 clamp ps1 | `ps1_safety_test.go` varre `deploy/windows/*.ps1` | Sim (CI) |
| #15 Fail-safe + curador | reclaim gate de duas fases | `internal/hostdisk/specv3_reclaim_test.go` (tokens do gate pós-Off) + `internal/civm/reclaim_test.go` (`EmergencyAdmits` + valor medido travado) | Sim (CI `build-civmctl`) |
| #1, #4, #6, #8, #10, #11, #12, #16 | — | manual em PR review + SSDV3 | Manual |

**Várias disciplinas com gate CI hard; o resto exige humano consciente.** Não é teatro.

---

## Contra-exemplos por disciplina

Toda disciplina vira **comportamento de Sistema 1 disfarçado** quando aplicada sem juízo. Os 12 modos de falha agrupam em 4 padrões:

| Padrão | Disciplinas | Sintoma comum |
|---|---|---|
| Forma sem conteúdo | #2, #3, #6 | compliance formal, valor zero |
| Over-engineering | #5, #8 | custo presente para risco hipotético |
| Atrito injustificado | #10, #11, #12 | fricção sem ganho de qualidade |
| Eliminação de nuance | #1, #4, #7, #9 | regra substitui pensamento em vez de guiar |

| # | Vira... | Sintoma concreto | Mitigação |
|---|---|---|---|
| 1 | Paralisia | listar TODA ignorância em todo PRD trava SSDV3 PASSO 1 | PRD agrupa "Inferências" em seção separada, capada em 30% do total |
| 2 | Teatro | "Rollback trigger: se der errado, reverter" passa no formato mas é vazio | PR reviewer recusa frase sem unidade/janela; INVARIANTS #15 |
| 3 | Métrica sem contexto | "restart 30% mais rápido" inútil se medido sem `-race` em máquina diferente | SSDV3 SPEC §"Validação" exige descrever ambiente do bench |
| 4 | Anchor errado | reference class de comandos read-only subestima um comando que muta disco | citar também a complexidade real do path destrutivo (ex.: safedelete) |
| 5 | Paranoia | watchdog desenhado pra 1M repos quando o runner serve um punhado de peers | gate por carga real: `capacity` hard-fail a 90%, não por número hipotético |
| 6 | Escape | "p99 220ms ±50%" cobre tanto "passa" quanto "falha" | stddev >25% trigga investigação, não aceitação |
| 7 | Desculpa | "Foi sorte genuína" vira racionalização do bug | postmortem exige causa raiz técnica precisa (ex.: o Int32 clamp do VHDX) |
| 8 | Inflação | multiplier 5x vira justificativa pra qualquer estimativa inflada | "tempo é alarme, não meta" — postmortem em vez de inflar |
| 9 | Eliminação de pergunta válida | forçar tudo em métrica destrói nuance operacional | RNFs aceitam julgamento humano onde a métrica não captura (ex.: legibilidade de output CLI) |
| 10 | Scope creep | PR fix-only removendo TODOs antigos vira PR de 50 arquivos | TODO novo é débito do diff atual; débito legado vira issue própria |
| 11 | Not-Invented-Here | recusar lib boa "porque civm é stdlib-first" sem avaliar | a justificativa custa 5min no PR; stdlib-first é default, não dogma |
| 12 | Degradação de colaboração | "Qual problema tem?" em pair programming dá ar de desconfiança | framing rota — em PR review usar; em pair programming não |

**Meta-contra-exemplo: cargo cult.** Kahneman aplicado sem skin-in-the-game vira ritual: autor commita rollback trigger numérico mas nunca executa quando a condição dispara — vive só no commit, não na mente. Mitigação: PR review walks rollback triggers de mudanças não-triviais e exige que sejam acionáveis.

---

## Meta-regra: counterfactual como gatekeeper

Se você (humano ou IA) não consegue responder "**O que me faria mudar de opinião?**" com algo específico (numérico, observável), **pare**. Está em Sistema 1 — opinião fluente sem condição de revisão.

Resposta válida: "se o restart do runner via sentinel passar de 30s em 3 rodadas, reverter o auto-restart" ou "se o cleanup a 60% liberar < 2 GB em medição real, ajustar `DefaultPreCleanupPct`".

Resposta inválida (Sistema 1 disfarçado): "se der errado", "se piorar", "se não funcionar". Sem unidade, sem janela, sem trigger — é wishful.

O `civm` aplica isso a si mesmo:

- Commits não-triviais (`feat/fix/refactor/perf`) exigem `Rollback trigger:` no body (`AGENTS.md` §Commits).
- PRs trazem seção `## Rollback trigger` no template (`rules/governance.md`).
- Dep nova exige justificativa com condição de reversão no PR.
- **Este doc tem rollback trigger:** se em 6 meses <30% dos PRs não-triviais citarem alguma disciplina, simplificar para o Top-5 + 1-pager.

---

## Como a IA/dev usa este doc

1. Antes de opinar em escolha não-trivial: declarar o NÃO-visto (#1).
2. Com a opinião: número, não adjetivo (#3). Se métrica não disponível, declarar "sem medir, estimo X com confiança Y%".
3. Depois: counterfactual específico no commit body (#2 — INVARIANTS #15).
4. Adicionar dep: justificar no PR (alternativas, custo, rollback) — civm é stdlib-first (#11).
5. Achar TODO órfão: remover ou abrir issue dedicada (#10).
6. Mudar comportamento destrutivo (cleanup/safedelete) ou contrato de hook/runner: SSDV3 + teste de integração (#1, #2, #9, #13).
7. Postmortem: separar processo de resultado (#7).
8. Estimativa de feature: reference class + multiplicador honesto (#4, #8).
9. Prompt para outro agente: framing adversarial (#12).

---

## Princípio de Noise

Mesmo código, reviewer diferente, feedback diferente — isso é ruído. Os gates do `civm` (`go vet`, `golangci-lint`, `govulncheck`, `go test -race`, cobertura ≥80%) são **rubricas fixas**: não variam com o humor do dia. São anti-noise estrutural. O formato canônico de PR (`rules/governance.md`) reduz variância na narrativa de decisão.

---

## Leituras cruzadas

- `disciplines/INVARIANTS.md` — gates reais do `civm`
- `disciplines/SSDV3-PROMPTS.md`, `disciplines/COVERAGE-EXCLUSIONS-template.md`
- `rules/ssdv3.md`, `rules/governance.md`, `rules/testing.md`, `rules/security.md`, `rules/observability.md`
- `AGENTS.md` §"Decision hygiene", §"Commits", §"Sync rule"
- `.github/workflows/ci.yml` — onde os gates rodam
- Kahneman, _Thinking, Fast and Slow_ (2011)
- Kahneman, Sibony, Sunstein, _Noise_ (2021)
