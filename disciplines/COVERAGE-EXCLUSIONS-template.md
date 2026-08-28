# COVERAGE-EXCLUSIONS — template civm-native

> Template portátil mantido pelo `civm`. No próprio `civm`, o gate de cobertura
> é **≥80% por package em `internal/**`** (step "Coverage threshold" do job
> `build-civmctl` em `.github/workflows/ci.yml`; ver também `rules/testing.md`).
> A política do `civm` é **preferir teste focado a exclusão** — este doc existe
> para o caso (raro) em que um peer copie o padrão e precise registrar exclusões
> explícitas. Os exemplos abaixo usam paths genéricos/Go-stdlib.

> **Append-only.** Exclusões explícitas do gate de cobertura.
>
> **Quando usar:** quando código de produção genuinamente não pode ser coberto por teste (boilerplate de bootstrap, defensive panic em path impossível, etc.).
> **Quando NÃO usar:** quando a função é difícil de testar — refatore. Ou quando o teste seria "trivial" — escreva mesmo assim. Excluir é última opção.

## Regras

1. **Toda exclusão tem razão concreta** — citação de `path:linha` + por quê não é testável + alternativa avaliada.
2. **Toda exclusão tem condição de remoção** — data ou observable trigger.
3. **Append-only.** Exclusões removidas ganham nota `(removed: YYYY-MM-DD, motivo: ...)` na entrada original.
4. **Auditoria periódica.** Revisão de exclusões expiradas no ciclo de manutenção.
5. **Soma das exclusões pequena.** O threshold (80% no civm) já é piso, não teto; exclusão é exceção, não rotina. Excesso de exclusões = revisar a abordagem de teste.

## Categorias aceitas

- **Boot/bootstrap**: `cmd/<binary>/main.go` (entry point: `os.Exit(run(...))`, lógica real em `run()` testado).
- **Defensive panic/error em programmer-error path**: error path da stdlib inalcançável quando o input já foi validado upstream (ex.: `aes.NewCipher(key)` após validar `len(key) == 32`).
- **Generated code**: `*.gen.go` — excluído por categoria, não linha-a-linha.
- **External dependencies wiring**: bootstrap de exporters/coletores em `init()` — testável só em integração, não unit.

## Categorias NÃO aceitas

- "Difícil de mockar" — refatore para depender de interface (ou injete `RunFn`/`WalkFn`/`StatfsFn` como o `civm` faz) ao invés de struct concreta.
- "Não tenho tempo" — débito, não exclusão. Vai para issue, não para este doc.
- "Cobertura virá depois" — só com data de remoção observable.

## Template por entrada

```markdown
### `<path>:<line>` — `<descrição curta>`

- **Adicionado em:** YYYY-MM-DD por @author no PR #NNN
- **Razão:** descrição de por quê não é testável (concreta, não vaga)
- **Alternativa avaliada:** o que tentou primeiro? (refactor X, injetar fn Y, integration test Z)
- **Categoria:** boot / panic-defensive / generated / external-wiring / outra (justificar)
- **Tipo:** file-level OU line-range
- **Condição de remoção:** data observable (ex.: "remover em 2026-08-31 quando refactor X chegar") OU evento ("remover quando issue #NNN fechar")
- **Owner:** @username (responsável pela remoção)
```

---

## Exclusões atuais

Nenhuma. O `civm` cobre `cmd/civmctl/main.go` via injeção de dependências
(`RunFn`/`GlobFn`/`ReadFileFn` em `internal/**`) e testa o fluxo CLI por
`main_test.go`/`integration_test.go`, então não há exclusão ativa. As entradas
abaixo são **exemplos do formato** para um peer que adote o padrão.

### `cmd/<binary>/main.go:NN` — main() boot wrapper (exemplo)

- **Adicionado em:** YYYY-MM-DD por @author no PR #NNN
- **Razão:** `func main()` é trivialmente `os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))`. A lógica real está em `run()` (coberto via `main_test.go`). Testar `main()` direto exige `go test -c` + subprocess — complexidade desproporcional ao 1 statement testado.
- **Alternativa avaliada:** subprocess test via `os/exec` chamando o binário compilado. Custo (~30 linhas + teste lento) maior que o benefício (1 statement).
- **Categoria:** boot/bootstrap (entry point canônico).
- **Tipo:** file-level (corpo de `func main()`, 1 linha).
- **Condição de remoção:** se `main()` ganhar lógica não-trivial (>3 linhas), extrair para `run()` ou adicionar subprocess test e remover esta exclusão.
- **Owner:** @author.

### `internal/<pkg>/crypto.go:NN-MM` — defensive panic-impossible paths (exemplo)

- **Adicionado em:** YYYY-MM-DD por @author no PR #NNN
- **Razão:** `aes.NewCipher(key)` da stdlib só falha se `len(key) ∉ {16,24,32}`; o construtor já valida `len(key) == 32` antes. `cipher.NewGCM(block)` só falha se `block.BlockSize() != 16`; AES sempre devolve 16. Esses error paths existem como defesa contra refactor futuro; não há input válido em runtime que os dispare.
- **Alternativa avaliada:** refatorar com interface `cipher.Block` injetável + mock de erro. Custo (boilerplate não-idiomático em Go) maior que o benefício (2 linhas a menos descobertas).
- **Categoria:** panic-defensive (programmer-error paths).
- **Tipo:** line-range.
- **Condição de remoção:** se um refactor futuro deixar o método chamável sem passar pela validação upstream, adicionar teste que dispare o path e remover esta exclusão.
- **Owner:** @author.

---

## Histórico

(append-only)

- **YYYY-MM-DD** — primeira versão do template (exemplo). Gate declarado; sem exclusões ativas.
- **2026-06-04** — reescrito como civm-native. Removidas as entradas históricas importadas de um peer descontinuado (`services/api/internal/platform/secrets/aesgcm.go`, `services/shared/crypto/aesgcm.go`, parser Go em `tools/<peer>ctl/...`, threshold 98%). O `civm` usa gate ≥80% por package em `internal/**` (`ci.yml`) e prefere teste focado a exclusão — não há exclusão ativa; as entradas acima são exemplos do formato.
