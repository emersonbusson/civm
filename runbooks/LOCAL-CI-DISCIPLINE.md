# Runbook — Local CI é o gate de verdade

> **Princípio núcleo:** validação real acontece no laptop do dev ANTES
> de qualquer push. CI remoto (GitHub Actions ou civm self-hosted)
> é mirror informativo que posta resultado pro PR — não é onde o código
> é validado pela primeira vez.

## Modelo conceitual

```
1. Dev muda código no laptop.
2. Roda gate local do peer (ex.: script `ci-local`; acme usa `devctl ci local --clean`)
       │
       ├─ Falha → fix loop até passar local
       └─ Passa → autorizado a `git push`
3. git push + abrir PR
4. CI remoto roda os mesmos gates em GitHub Actions OU civm
       │
       ├─ Passa → check verde no PR (esperado, local já validou)
       ├─ Falha com local OK → contexto remoto (env diff, transient).
       │                       Investiga env, NÃO código.
       └─ Billing bloqueado → router roteia pra civm OU template
             optimistic-retry skipa, PR fica verde, merge libera.
5. Merge.
```

## Implicações operacionais

### O que isso significa na prática

- **Push sem rodar local primeiro = anti-pattern.** Sistema não te
  protege disso (e não deveria — overhead). Disciplina do dev.
- **Falha em CI remoto com local OK = quase sempre infra.** Não chama
  bug de código antes de re-rodar local. Em 95% dos casos: cache,
  rede, env diff, secret faltando.
- **Branch protection com aggregator (`Gates`) required = ainda faz
  sentido**, porque protege contra: (a) push sem local rodado e (b)
  regressão de outro merge entre local-pass e push.

### Por que CI remoto não é o gate primário

- **Latência:** local roda em ~2 min, CI remoto pode levar 5-20 min
  (queue, setup-go, npm ci, etc).
- **Custo:** GitHub Actions paga por minuto. Local é grátis.
- **Disponibilidade:** GitHub pode estar fora; civm pode estar
  offline. Local sempre funciona.
- **Iteração:** dev no fix loop precisa de feedback em segundos,
  não minutos.

### O que o CI remoto adiciona

- **Visibilidade pros reviewers** (check verde no PR)
- **Validação cross-platform** (se tiver matrix runners)
- **Gate contra dev distraído** que esqueceu de rodar local
- **Audit trail** de quando o código passou nos gates

## Implementação

Cada repo deve ter:

1. **Comando local com clean state** que roda os mesmos gates do CI:
   - Lint
   - Unit tests
   - Type check / vet
   - Invariantes/discipline checks
   - Build
   - Contracts/schema drift

   Exemplo: `devctl ci local --clean` (acme) OU
   `npm run ci:local` (Node) OU `make ci` (genérico).

2. **Pre-push hook husky** que pelo menos warns se tests não foram
   rodados (não bloqueia — disciplina é do dev).

3. **CI remoto que espelha** os mesmos gates em GitHub Actions
   (mesma ordem, mesmo comando, mesmo flag). Se diverge = bug.

4. **Documentação clara** que diz "rode local antes de push" no
   CLAUDE.md / AGENTS.md / CODEX.md / README.md.

## Anti-patterns comuns

| Anti-pattern | Por quê é ruim | Fix |
|---|---|---|
| Pular `ci local` "porque é rápido" | Quebra disciplina, push pode falhar com gates legítimos | Sempre rodar antes de push |
| Investigar bug em código quando CI remoto falha mas local passou | Tempo perdido — quase sempre é env | Re-rodar local primeiro; se também falha, ENTÃO bug |
| Confiar no GitHub Actions como "única fonte de verdade" | Quando billing trava, fica bloqueado sem alternativa | Local CI sempre funciona, é o gate primário |
| Branch protection com cada job individual como required | Qualquer flake em 1 job bloqueia merge | Aggregator único como required |

## Rollback trigger

Se em 30 dias houver dúvida operacional sobre qual é o gate real
(local vs GitHub Actions), reler este runbook. Se a dúvida persistir,
revisar:

1. Existe comando `ci local --clean` operacional? Está documentado?
2. CI remoto espelha exatamente os mesmos gates do local?
3. Devs sabem que `ci local` é o gate de verdade?
