# IMPL — limpeza de volumes gerenciados na fronteira

> Issue: `emersonbusson/civm#181`. Estado: implementação local, validação live pendente.

## Entrega

- `cleanup --managed-volumes` é opt-in; timers e hooks permanecem sem a flag.
- Inventário usa somente volumes `dangling=true` com labels Compose; o
  `volume rm` sem `--force` mantém a recusa do daemon se surgir referência.
- Classificador exige driver/scope `local`, config hash, versão, volume, projeto
  com sufixo numérico de run e consistência `nome = projeto_volume`.
- Remoção passa nomes explícitos a `docker volume rm`; prune named global é
  proibido.
- Idle-check e docker-heavy lock são revalidados imediatamente antes da
  mutação.
- Inventário posterior prova que os alvos sumiram.
- `docker system df` registra bytes recuperáveis antes/depois.
- Segunda execução sem alvos retorna sucesso e não muta.

## Evidência local

- RED: funções/flag ausentes falharam compilação.
- GREEN: testes focados de classificação, erro, idle race, pós-condição,
  opt-in e idempotência passaram.
- RED adversarial: inventário sem `dangling=true` falhou a prova de volumes sem
  referência; GREEN após estreitar o filtro.
- `go test -race -count=1 -coverprofile=... ./...`: PASS.
- Cobertura: `internal/cleanup=82,2%`; todos os `35` packages `internal/**`
  ficaram em `>=80%`.
- `golangci-lint v2.12.1`, `go vet`, `govulncheck v1.1.4`, gitleaks em árvore
  e `262` commits, build e smokes read-only: PASS.
- Binário stripped: `8.069.385` bytes (`<10.485.760`).

## Pendente antes do merge/deploy

- dry-run do binário candidato contra o daemon real;
- execução live somente com geração fechada e prova de box ociosa;
- medir `174 → 3/0` volumes conforme classificação e bytes;
- executar 2x e provar segunda remoção igual a `0`;
- usar o novo piso medido em `emersonbusson/civm#182`.

## Rollback trigger

Reverter se qualquer volume ativo/não gerenciado entrar nos alvos, se o branch
busy executar named cleanup ou se a pós-condição puder falhar sem exit não zero.
