---
name: testing
description: Regras de teste Go para civmctl e packages internal.
paths:
  - "**/*_test.go"
---

# Testing rules

## Coverage mínimo atual

**80% por package em `./internal/...` é o gate atual do CI.** Aumentar para
90/98 só após testes cobrirem o delta.

- **Threshold em CI:** 80% por package interno.
- **Exclusões explícitas:** documentar em `disciplines/COVERAGE-EXCLUSIONS-template.md` se um peer copiar este padrão; no `civm`, preferir testes focados em vez de exclusões.
- **O que conta como produção no civm:** `internal/**` e fluxo CLI em `cmd/civmctl/**`; `main()`/dispatch puro pode ficar sem teste se o comportamento estiver coberto pelos packages.

### Por quê 100%

- Threshold sem parser real vira no-op. O CI deve falhar quando qualquer package interno ficar abaixo do piso.
- Bug em código não-coberto é detectado em produção, não em CI. Caro.
- Testar sempre força melhor design — função impossível de testar geralmente é função mal-projetada.
- "Cobrir tudo 100% com os testes" — decisão do usuário em 2026-04-28.

### Como ramp do estado atual

Código pré-existente abaixo de 80% deve ganhar teste antes do aumento do piso.

## Go

### Go testing + testify

- Arquivos `*_test.go` ao lado do código.
- `testify/assert` e `testify/require`. Preferir `require` para fail-fast em setup.
- Tabela de cenários para >2 casos — pattern `tests := []struct { name, ... }{...}` + `for range; t.Run(...)`.

### Comandos

```bash
go test -race -count=1 ./...
go test -count=1 -cover ./internal/...
go test -count=1 -coverprofile=/tmp/civm-coverage.out ./...
go tool cover -func=/tmp/civm-coverage.out
```

## Disciplina (Kahneman)

1. **Test cobre positivo E negativo.** Ex.: "reclaim roda quando gap ≥ threshold" + "reclaim aborta com exit 2 quando headroom < floor".
2. **Tabela-driven para >2 cenários.** Reference class evita bias.
3. **Sem `t.Skip`** sem motivo documentado e issue rastreável.
4. **Sem `Sleep`** para sincronizar — use channels/contextos/eventos.
5. **Injeção hermética** (`RunFn`/`ReadFileFn`/`NowFn`) em vez de mock pesado — padrão dos packages `internal/**`; lint Go guarda a camada host (ex.: `internal/hostdisk`).
6. **Cleanup/runner automático prova efeito.** Named volumes: allowlist,
   rechecagem de idle, pós-condição e idempotência 2x. Queue stall: labels
   elegíveis, dwell, Worker aparecendo antes da mutação, marker inválido,
   exatamente 1 restart, estado `unresolved`, paginação e limpeza do incidente
   quando API/Worker comprovam consumo.

## Don't

- ❌ Mock dependências internas quando a função aceita injeção simples de `RunFn`, `WalkFn`, `StatfsFn` ou equivalente.
- ❌ Test que depende de ordem de execução.
- ❌ Snapshots de visual regression sem revisar diff (gera ruído).
- ❌ `t.Skip("flaky")` sem issue assignada e prazo.
- ❌ **Cobertura <80% em package interno** no estado atual do `civm`.
- ❌ Skip de teste em CI via `if (process.env.CI)` — flaky test deve ser fixado, não silenciado.
- ❌ Adicionar exclusão de coverage sem razão concreta + condição de remoção.
- ❌ `// nocov` ou `/* c8 ignore */` sem comentário adjacente explicando.
- ❌ Submeter shell scripts ou Makefile ao repo quando uma função Go testável resolve.
