# Invariantes do civm

> Documento civm-native. As invariantes abaixo são os **gates reais** do
> repositório `civm` (repo Go de infra para runner self-hosted). O enforcement
> vive em `.github/workflows/ci.yml`, nas regras de `rules/*.md` e em testes
> `*_test.go` — não em nenhum binário `checkinvariants` nem em `apps/web`.
> Um peer repo que adote esta metodologia substitui a tabela pelos próprios
> gates.

As invariantes do `civm` são **testáveis em CI** e **bloqueiam merge** quando
violadas. O gate agregado é o job `CI` em `.github/workflows/ci.yml`, que exige
sucesso de `validate-templates`, `build-civmctl` e `self-hosted-smoke` (com a
lógica docs-only/full em `tools/ci/detect-changes.mjs`).

Não há binário dedicado de invariantes: cada linha aponta para o job, step ou
teste Go que a faz cumprir. Toda exceção exige justificativa rastreável (entrada
em `rules/*.md`, comentário no código, ou `[sync-skip-justified]` no commit).

| # | Invariante | Por quê | Como é enforçada no civm | Quando |
|---|------------|---------|--------------------------|--------|
| 1 | Sem secrets hardcoded | Vazamento via `git push` é irreversível: atacante extrai a key do histórico mesmo após revert. | Step "Secret pattern scan" do job `build-civmctl`: `grep -RInE` por tokens `ghp_`/`github_pat_`/`sk-proj-`/`AKIA…` e por blocos `-----BEGIN … PRIVATE KEY-----`. `gosec` (via `golangci-lint`) reforça. | CI (`build-civmctl`) |
| 2 | `go vet` limpo | Vet pega bugs estáticos (printf mismatch, locks copiados, shadowing perigoso) antes do runtime. | Step "go vet" (`go vet ./...`) do job `build-civmctl`. Reproduzível local com `go vet ./...`. | CI (`build-civmctl`) |
| 3 | `golangci-lint` limpo | Lint estrutural: `errcheck`, `staticcheck`, `gosec`, `gocritic`, `errorlint`, `misspell`, etc. (config em `.golangci.yml`). Erro silencioso ignorado é bug em produção. | Step "golangci-lint" do job `build-civmctl` (`golangci-lint run ./...`, versão pinada). | CI (`build-civmctl`) |
| 4 | Sem vulnerabilidades conhecidas (govulncheck) | Dep com CVE conhecida em runner compartilhado expõe todos os peers. | Step "govulncheck" do job `build-civmctl` (`govulncheck ./...`, versão pinada). | CI (`build-civmctl`) |
| 5 | `go test -race` verde | Data race em código concorrente (watchdogs, locks, runner restart) é não-determinístico em produção; só o detector pega cedo. | Step "go test (race + cover)" do job `build-civmctl` (`go test -race -count=1 -coverprofile=coverage.out ./...`). | CI (`build-civmctl`) |
| 6 | Cobertura ≥80% por package em `internal/**` | Threshold sem parser real vira no-op. Bug em código não-coberto é detectado em produção, não em CI. `rules/testing.md` §"Coverage mínimo atual". | Step "Coverage threshold" do job `build-civmctl`: itera `go list ./internal/...` e falha se qualquer package ficar `< 80%`. Local: `go test -count=1 -cover ./internal/...`. | CI (`build-civmctl`) |
| 7 | Binário `civmctl` < 10MB stripped (RNF-3) | Zero-effort: o binário é baixado/instalado na VM; bloat indica dep indevida ou debug embarcado. | Step "go build" do job `build-civmctl`: compila `-ldflags='-s -w'` e falha se `size > 10485760`. | CI (`build-civmctl`) |
| 8 | Comandos read-only não mutam (smoke) | `civmctl --help`, `version-pins`, `health`, `cleanup --dry-run` precisam ser seguros para rodar em qualquer estado sem efeito colateral. | Step "Smoke tests (read-only commands)" do job `build-civmctl` + job `self-hosted-smoke` (parity, `version-pins`, `health --json`, `drift`, `billing-status`). | CI (`build-civmctl`, `self-hosted-smoke`) |
| 9 | Capacidade destrutiva validada pelo propósito (não pela existência) | Existe ≠ funciona: `safedelete` removia leftovers root-owned do `_work`; #59 shipou teste que afirmava o **oposto** (recusa). Teste hermético pode codificar a premissa errada e passar (disciplina Kahneman #13). | Step "Privileged cleanup escalation" do `self-hosted-smoke`: `go test -tags=integration -run TestIntegration ./internal/safedelete/` contra fixture root-owned real. Self-skip sem sudo NOPASSWD (no-op em fork). | CI (`self-hosted-smoke`) |
| 10 | Paridade com `ubuntu-latest` | A VM existe para reproduzir `ubuntu-latest` com mais hardware; drift de versão (Go/Node/Docker/gh) quebra a promessa "roda igual". | Step "Tool parity check" do `self-hosted-smoke` (`go/node/docker/gh/git/jq --version` + `civmctl parity`). | CI (`self-hosted-smoke`) |
| 11 | Templates e systemd válidos | Template `.yml.template` ou unit systemd quebrado só falha quando um peer copia — tarde demais. | Job `validate-templates`: YAML lint em `templates/*.yml.template` + `ci.yml`, presença não-vazia das units `deploy/systemd/*`. | CI (`validate-templates`) |
| 12 | Links locais nos docs não quebram | Doc referenciando arquivo inexistente apodrece a documentação operacional. | Step "Markdown links basico" do `validate-templates`: resolve cada link relativo em `README/AGENTS/CODEX/runbooks/disciplines/rules/templates` e falha se faltar alvo. | CI (`validate-templates`) |
| 13 | Marcadores COMMUNICATION-STYLE íntegros | `templates/COMMUNICATION-STYLE.md` é fonte sincronizada em `AGENTS.md`/`CODEX.md`; perder os marcadores `BEGIN/END` desincroniza o bloco. | Step "Communication style template integrity" do `validate-templates`. | CI (`validate-templates`) |
| 14 | Sync rule (README ≡ AGENTS ≡ CODEX ≡ rules) | Docs autoritativos desincronizados → agentes seguem regras conflitantes. `AGENTS.md` §"Sync rule (invariante #14)". | Regra operacional revisada por humano (não há `pr-governance.yml` ativo ainda — ver `rules/governance.md`). Skip explícito via `[sync-skip-justified]` no commit body. | PR review |
| 15 | Conventional Commits + Rollback trigger | Histórico legível alimenta release-please; commit não-trivial sem condição de reversão é Sistema 1 (Kahneman #2). `AGENTS.md` §"Commits". | Regra operacional: título em inglês ≤72 chars; `feat/fix/refactor/perf` exigem `Rollback trigger:` no body. release-please valida o formato no merge. | PR review / merge |
| 16 | PII scrubbing em logs (`slog`) | LGPD/GDPR + exposição via log files. `rules/observability.md` + `rules/security.md`. | Regra de código revisada em PR: nunca logar email/cpf/phone/password/token raw; `slog` estruturado, nunca `fmt.Println`/`log.Printf` em produção. | PR review |
| 17 | Scripts Windows sem clamp Int32 `[math]::Max(0, …)` | Um `0` literal é Int32; fixa overload `Max(int,int)` e estoura em bytes > ~2 GiB. Esse bug travou todo reclaim do VHDX dinâmico até wedgear o runner. | Teste Go `internal/hostdisk/ps1_safety_test.go`: regex `\[math\]::(Max|Min)\(\s*0\s*,` varre `deploy/windows/*.ps1`. Roda em `go test -race ./...`. | CI (`build-civmctl`) |

## Como rodar localmente

Não há subcomando de invariantes. Os gates de código reproduzem-se com as
mesmas ferramentas do CI:

```bash
export GOTOOLCHAIN=auto

go vet ./...                                   # invariante #2
golangci-lint run ./...                        # invariante #3 (gosec inclui #1)
govulncheck ./...                              # invariante #4
go test -race -count=1 ./...                   # invariantes #5, #17
go test -count=1 -cover ./internal/...         # invariante #6 (≥80% por package)
go build -ldflags='-s -w' -o /tmp/civmctl ./cmd/civmctl && \
  stat -c%s /tmp/civmctl                       # invariante #7 (<10MB)

# invariante #9 (só na VM com sudo NOPASSWD para o wrapper):
go test -tags=integration -run TestIntegration ./internal/safedelete/

# invariante #10 (paridade), na VM:
civmctl parity
```

Os gates de documentação/governança (#11–#16) são revistos no PR ou pelos jobs
`validate-templates`/`self-hosted-smoke`; o passo "Markdown links basico" e o
de integridade do COMMUNICATION-STYLE rodam exatamente como em `ci.yml`.

## Como adicionar exceção

Não há mecanismo de waiver inline com binário próprio. As exceções aceitas hoje:

1. **Lint:** justificativa no `.golangci.yml` (ex.: as exclusões `gosec` G204/G304/G115 já documentadas inline com rationale) ou comentário `//nolint:<linter> // <razão>` adjacente.
2. **Cobertura:** preferir teste focado a exclusão. Se um peer copiar este padrão e precisar excluir, documentar em `disciplines/COVERAGE-EXCLUSIONS-template.md` (ver lá). No próprio `civm`, abaixo de 80% em package interno **bloqueia** — sem override silencioso.
3. **Sync rule:** `[sync-skip-justified]` no commit body com a razão.
4. **Smoke billing-aware:** falha por quota/billing de serviço externo pode virar warning amarelo (não bloqueia) — ver "Exceções de skip em CI" abaixo.

Toda exceção é **auditável** por grep (`grep -rn nolint .`, `git log --grep`).

## Exceções de skip em CI

Diferente dos gates de qualidade do código, há condições operacionais em que um
step pode falhar por motivo **fora do controle do PR** — tipicamente
billing/quota esgotada em serviço externo (GitHub Actions billing). Para esses,
o `civm` usa detecção heurística zero-PAT em `civmctl billing-status` e o smoke
do `self-hosted-smoke` aceita exit code não-zero desses steps (`|| true`) em vez
de bloquear merge por causa de billing.

### Markers reconhecidos (case-insensitive)

`civmctl billing-status` classifica como `blocked` quando a saída contém, entre
outros: `payment required`/`http 402`, `quota exceeded`, `insufficient quota`,
`insufficient credits`, `billing disabled`, `out of credits`. Lista append-only
na implementação (`internal/billing/`); adições documentadas no PR que as
introduz.

### Steps NÃO protegidos (e por quê)

- `go vet`, `golangci-lint`, `govulncheck`, `go test`, coverage, build, validate-templates: determinísticos. Qualquer falha aqui é bug real — não há vetor billing.

## Histórico

(append-only)

- 2026-06-04: Documento reescrito como civm-native. Substituída a tabela de 13 invariantes de frontend (importada de um peer descontinuado: `apps/web`, `console.log`, `NEXT_PUBLIC_E2E_AUTH_BYPASS`, `@<peer>/api-client`, `tools/<peer>ctl/cmd/checkinvariants`) pelos gates reais do `civm` extraídos de `.github/workflows/ci.yml` + `rules/*.md` + testes Go. O `civm` é repo Go de infra: não tem frontend, multi-tenant nem binário de invariantes; o enforcement é CI workflow + `go test -race` + gate de cobertura ≥80% em `internal/**` + lints.
