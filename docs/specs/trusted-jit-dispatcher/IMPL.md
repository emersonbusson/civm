# IMPL — Dispatcher JIT com isolamento descartável

Data: 2026-08-23

## Veredito

- **Implementação local:** concluída e validada hermeticamente.
- **Infraestrutura live:** **NO-GO**.

Nenhum GitHub real, Docker, VM, runner, serviço ou secret foi usado. Nenhum
commit/push foi feito.

## Implementação no CIVM

### Autoridade e API

- `civmctl jit-dispatch` one-shot Linux, token por stdin.
- Config JSON 0600 com repo/ref/workflow/digest/job/group exatos.
- API pinada `2026-03-10`; dispatch aceita exclusivamente HTTP 200 com run ID e
  URLs válidos.
- Run é consultado por ID e revalidado por trusted SHA/path/event/title.
- Job ID é persistido e o resultado só vale no runner ID/nome/grupo gerado.
- Generate JIT e remoção de runner são repository-scoped.

### Admissão global

- `/run/civm/jit-dispatch.lock` é fixo e serializa somente startup/recovery.
- Slot pesado é adquirido por `guard exec -- civmctl __jit-lease-hold`.
- Holder persiste lease/admission ID/PID/start ticks em path fixo.
- `release\n` remove marker; EOF mantém holder/Guard vivos.
- Recovery sem holder readquire Guard antes de qualquer limpeza.
- Ativação exige que o Guard desconte `12 GiB` de commit da VM, piso Windows e
  margem numa leitura Windows pós-reclaim; `20+12 GiB` já excedem os
  `31,90 GiB` físicos e não constituem capacidade disponível.

### Estado e recovery

- Ledger v2 persiste run ID, job ID, runner ID, lease, PID/start ticks,
  process group, cgroup, isolation ID/base e três postconditions.
- Startup lista ledgers antes de novo efeito e falha com múltiplos incompletos,
  marker sem ledger ou identidade divergente.
- Dispatch ambíguo sem run ID permanece bloqueado; não existe busca por
  “mais recente”.
- Generate JIT ambíguo sem runner ID permanece bloqueado mesmo após timeout
  sem label; rejeição HTTP 4xx autoritativa é a única ausência inferida.
- Run terminal e runner ausente são observados antes de release.

### Isolamento descartável

- Execução direta do runner no host foi removida.
- Driver externo é validado por metadata+SHA-256 e executado pelo fd aberto.
- Driver nasce no cgroup v2 do lease via `CLONE_INTO_CGROUP`, com process group
  e `PDEATHSIG`; não existe fallback de attach pós-start.
- Receipt `ready` exige base pinada, disposable e ausência de host mounts,
  host Docker e product secrets; JIT só é enviado após persistência.
- Receipt final exige `destroyed=true` e `reset_verified=true`.
- Recovery do cgroup funciona mesmo no crash anterior ao callback de PID;
  encerramento também aguarda o `Wait()` do driver.
- Marker Guard v2 inclui `admission_id` por tentativa; timeout antes da
  admissão termina/reap o broker e não pode produzir holder tardio aceito.
- Logs do driver são redigidos; recovery log é append-only/repetível.

### Config e segurança local

- `state_dir` e `runner_directory` não podem sobrepor.
- Guard, driver, `civmctl`, runner source, runtime dir e lock têm validações de
  path/metadata/ownership.
- Token está alinhado em 20..8192 bytes.
- O preflight local acontece antes da primeira chamada GitHub.

## Implementação no consumidor

No `peer-site`, `.github/workflows/civm-jit.yml` mantém:

- somente `workflow_dispatch` e um job;
- run name/inputs/label nonce exatos;
- checkout candidate SHA pinado, sem credencial persistida;
- `contents: read` e zero secret de produto;
- comandos pesados sequenciais;
- cleanup Docker conhecido com postconditions e sem prune global.

O Docker é o daemon interno da VM descartável. A destruição da VM pelo driver é
a postcondition contra recursos adicionais criados pelo candidato.

## Arquivos removidos da fonte

- `deploy/bin/civm-ephemeral-runner.sh`
- `deploy/systemd/civm-ephemeral-runner@.service`

A remoção não desativa cópias eventualmente instaladas; inventário live é
pré-requisito futuro e não foi executado.

## Evidência hermética

Executado no estado final durante a remediação:

```text
go test -p 1 -count=1 ./...                                      PASS
go test -p 1 -race -count=1 ./internal/jitdispatcher             PASS
go vet ./internal/jitdispatcher ./cmd/civmctl                     PASS
go test -p 1 -count=1 -coverprofile=<tmp> ./internal/jitdispatcher
  coverage: 80,1% de statements; gate do package: 80%             PASS
node scripts/civm-jit-workflow.test.mjs                           PASS
git diff --check (civm e peer-site)                              PASS
```

As execuções amplas intermediárias encontraram duas asserções estáticas antigas:
cleanup de work folder legado e a chamada literal anterior ao seam hermético de
membership cgroup. Ambas foram atualizadas para exigir, respectivamente,
isolamento descartável e `processInCgroup` como implementação real default.
O profile temporário de coverage foi removido; `coverage.out` preexistente não
foi alterado.

## Validação final

- [x] zero compilador/suíte pesada concorrente confirmado antes de cada suíte;
- [x] `go test -p 1 -count=1 ./...`;
- [x] `go test -p 1 -race -count=1 ./internal/jitdispatcher`;
- [x] `go vet ./internal/jitdispatcher ./cmd/civmctl`;
- [x] coverage focal em `80,1%`, acima do threshold de `80%`;
- [x] teste estático Node do workflow;
- [x] `git diff --check` nos dois repos.

## Pré-requisitos de ativação

1. driver real auditado;
2. base imutável real e runner copiado;
3. config/digest/token live;
4. Guard/runtime dir instalados;
5. canários success/theft/timeout/crash/reboot;
6. observabilidade e procedimento manual para estado ambíguo;
7. Guard com sensor Windows, piso/margem aprovados e reclaim bounded
   pós-verificado.

Até isso existir, **não executar o comando em produção**.
