# SPEC — Dispatcher JIT com isolamento descartável

PRD: [`PRD.md`](PRD.md)  
Threat model: [`THREAT-MODEL.md`](THREAT-MODEL.md)  
Status: remediação local implementada; ativação **NO-GO**.

## 0. Invariantes

1. Não existe bind atômico JIT→run/job na API utilizada.
2. Runner theft é possível; deve ser inofensivo para o host.
3. Código candidato executa somente em isolamento descartável.
4. O slot pesado pertence ao Guard, global à máquina.
5. O slot não é liberado até três provas: isolamento destruído, run terminal e
   runner remoto ausente.
6. Falha parcial não autoriza retry de POST nem cleanup presumido.
7. Capacidade teórica não é headroom: `20 GiB` de teto WSL + `12 GiB` de guest
   fixo somam `32 GiB`, acima dos `31,90 GiB` físicos antes do piso Windows.

## 1. Arquitetura

```text
config 0600 + token stdin
          |
          v
local preflight (Guard, driver digest, runner source, /run/civm)
          |
          v
fixed flock /run/civm/jit-dispatch.lock   [recovery only]
          |
          v
guard exec -- civmctl __jit-lease-hold   [machine-wide heavy slot]
          |
          +--> trusted workflow dispatch on default branch
          +--> repository JIT registration
          +--> pinned isolation driver --> disposable VM --> one runner job
          |
          v
VM destroyed + run terminal + runner absent --> explicit holder release
```

O holder escreve `/run/civm/jit-dispatch-lease.json` com lease, PID e start
ticks. Ele remove o marker somente ao receber `release\n`. EOF significa crash
do dispatcher: o holder bloqueia e mantém o processo `guard exec` vivo.

### Contrato de headroom do Guard

O dispatcher não implementa um segundo resource gate. Antes de executar o
holder, o Guard precisa confirmar que não há outro compilador/suíte pesada e
que o sensor Windows possui headroom real para a carga aprovada.

A admissão é uma decisão serial sob o lease global. Para iniciar VM/job, a
última leitura Windows válida após eventual reclaim precisa cobrir a RAM ainda
não comprometida da VM (`12 GiB` no baseline atual), um piso Windows não zero
aprovado e margem de segurança. Se a VM já estiver ligada, a contabilidade não
duplica o commit, mas precisa provar que o piso permanece. O limite WSL de
`20 GiB`, `MemAvailable` Linux e a soma de máximos configurados não são provas
de capacidade física disponível.

`/sys/fs/cgroup/memory.reclaim` pode ser usado somente como reclaim bounded de
file cache, com valor máximo, timeout e verificação posterior. O número escrito
não é garantia de bytes devolvidos. Após o reclaim, o Guard espera a propagação
e exige nova leitura válida do sensor Windows; sensor stale/indisponível ou
headroom abaixo do limiar mantém fila/falha fechada. `drop_caches` cego é
proibido.

## 2. Config

Campos obrigatórios:

```json
{
  "api_base_url": "https://api.github.com",
  "api_version": "2026-03-10",
  "state_dir": "/var/lib/civm/jit-dispatcher",
  "runner_directory": "/opt/actions-runner-source",
  "guard_executable": "/usr/local/bin/guard",
  "isolation_driver": "/usr/local/libexec/civm-jit-isolation",
  "isolation_driver_sha256": "64 lowercase hex",
  "base_image_sha256": "64 lowercase hex",
  "queue_wait": "30s",
  "queue_poll": "100ms",
  "http_timeout": "15s",
  "job_poll_interval": "2s",
  "job_bind_timeout": "2m",
  "run_timeout": "2h",
  "shutdown_grace": "10s",
  "recovery_timeout": "20m",
  "repositories": []
}
```

Regras:

- JSON estrito, sem campos/chaves duplicadas;
- config regular 0600, owner atual, sem symlink;
- paths absolutos, clean e não raiz;
- `state_dir` e `runner_directory` não podem ser iguais nem ancestrais um do
  outro;
- Guard e driver são paths distintos;
- driver e base exigem SHA-256 lowercase;
- token: 20..8192 bytes ASCII, uma linha por stdin;
- política por repo: trusted ref, workflow, digest, candidate refs, runner
  group e job name exatos.

## 3. Preflight local

Antes da primeira chamada GitHub:

- validar driver por file descriptor, metadata e SHA-256;
- validar runner source como diretório real, root/current-owned e sem write de
  group/outros;
- validar Guard e o próprio `civmctl` como executáveis confiáveis;
- validar `/run/civm` como diretório real sem write de group/outros;
- abrir o lock fixo com `O_NOFOLLOW`, 0600 e metadata confiável.

Qualquer falha produz zero dispatch, zero JIT e zero admissão Guard.

## 4. Estado durável

Ledger v2 em `<state_dir>/requests/<request_id>.json`, write atômico
temp→fsync→rename→fsync. Campos relevantes:

- request/repo/ref/candidate SHA;
- trusted SHA/workflow/digest;
- nonce, label, runner name e work folder;
- run ID, job ID e runner ID;
- lease ID no ledger; marker fixo v2 com lease, `admission_id` aleatório por
  tentativa e PID/start ticks do holder;
- PID, `/proc` start ticks, process group e cgroup;
- isolation ID e base SHA-256;
- `run_terminal`, `runner_gone`, `isolation_gone`, `cleanup_complete`;
- status/failure code/timestamps.

Token, JIT config e idempotency key raw não entram no ledger.

Status:

```text
prepared -> dispatching -> workflow_dispatched -> run_bound -> jit_created
         -> runner_started -> isolation_ready -> completed
                              \-> failed | stale | ambiguous
startup: qualquer incompleto -> reconciling -> ambiguous(cleanup completo)
```

Replay de `completed+cleanup_complete` retorna sem rede. Outros ledgers não
repetem efeito.

O `admission_id` impede uma tentativa nova de aceitar um holder tardio de outra
invocação do mesmo lease. Cancelamento anterior ao marker termina e colhe o
process group do broker; incerteza mantém o ledger incompleto/ambíguo.

## 5. Identidade GitHub

- Resolve default branch e exige `trusted_ref` igual a ela.
- Resolve trusted SHA e lê workflow naquele SHA; confere SHA-256.
- Resolve candidate ref e exige candidate SHA informado.
- Nonce: 32 bytes de `crypto/rand`; label `civm-jit-<64hex>`.
- Confere ausência do label antes do dispatch e novamente antes do JIT.
- Dispatch usa trusted ref e quatro inputs exatos.
- Aceita somente HTTP 200 com run ID/URLs válidos; 204, redirect, 5xx,
  transporte parcial ou JSON inválido são ambíguos.
- Consulta diretamente o run ID retornado e revalida evento, trusted SHA,
  path e display title em todo poll.
- Exige exatamente um job queued, com ID/nome/label exatos.
- Generate JIT é repository-scoped; resposta deve conter runner offline,
  non-busy, ID/nome exatos e exatamente um custom label nonce.
- Rejeição HTTP 4xx do Generate JIT prova que o runner não foi criado. Falha
  de transporte, 5xx ou resposta 201 inválida mantém a criação ambígua; não
  observar o label dentro de um timeout não prova ausência futura.
- Durante/ao fim, `GET job` precisa mostrar o runner ID/nome/grupo gerado.

Isso detecta theft; não o impede atomicamente.

## 6. Protocolo do driver de isolamento

Executável host-local pinado, iniciado como:

```text
civm-jit-isolation run --protocol=1 --lease-id=<64hex>
  --runner-directory=<source> --expected-base-sha256=<64hex> --control-fd=3
```

O executável é aberto com `O_NOFOLLOW`, validado por `fstat`+digest e executado
via `/proc/self/fd/4`, fechando troca de path entre validação e `exec`.

Sequência:

1. o cgroup v2 derivado do lease é criado antes do processo;
2. `clone3(CLONE_INTO_CGROUP)` via `UseCgroupFD` faz o child nascer nesse
   cgroup, com process group próprio e `PDEATHSIG=SIGKILL`; falta de suporte ou
   delegação falha fechado, sem fallback para escrita tardia em `cgroup.procs`;
3. PID/start ticks/group/cgroup são verificados e persistidos;
4. driver cria VM limpa a partir da base pinada e escreve em fd 3:

```json
{"protocol":1,"event":"ready","lease_id":"...","isolation_id":"...","base_sha256":"...","disposable":true,"host_mounts":false,"host_docker":false,"product_secrets":false,"destroyed":false,"reset_verified":false}
```

5. dispatcher valida e persiste o receipt;
6. só então envia JSON com JIT config por stdin;
7. driver copia a instalação do runner para a VM; não monta
   `runner_directory`;
8. após o job, destrói VM/disco descartável e escreve receipt `destroyed` com
   `destroyed=true` e `reset_verified=true`;
9. dispatcher espera `Wait()` reap, PID exato morto e cgroup vazio/removido.

Recovery usa `recover` com o mesmo lease/isolation ID. O driver precisa tornar
essa operação idempotente e emitir um único receipt `destroyed` válido.

## 7. Contenção e crash

- Process identity é `(PID,start_ticks)`, nunca PID isolado.
- Processo nasce no cgroup v2 próprio; portanto nenhum descendente pode surgir
  na janela anterior a um attach pós-start.
- TERM é seguido de KILL no process group e `cgroup.kill`; retorno só ocorre
  quando PID exato morreu e cgroup está vazio/removido.
- Recovery deriva o cgroup pelo lease mesmo se o crash ocorreu antes de
  persistir o callback `OnStarted`.
- Se existe ledger incompleto sem holder, startup readquire Guard antes de
  recuperar.
- Se existe holder correspondente, startup mantém o lease enquanto recupera.
- Mais de um ledger incompleto, marker sem ledger ou identidade divergente
  falha fechado.
- Run ID desconhecido após dispatch ambíguo não pode ser cancelado por
  heurística; o lease permanece bloqueado para inspeção.
- Runner ID desconhecido após Generate JIT ambíguo pode ser reconciliado pelo
  label exato se aparecer, mas timeout sem aparição mantém o lease bloqueado.

## 8. Cleanup e release

Ordem obrigatória:

1. encerrar/reconciliar driver e provar VM destruída/resetada;
2. cancelar run conhecido se não terminal;
3. confirmar estado terminal no run de identidade exata;
4. localizar runner pelo ID persistido (ou label exato quando ID não chegou);
5. validar ID/nome/label, deletar se presente e observar 404/ausência;
6. persistir as três postconditions e `cleanup_complete=true`;
7. enviar `release\n` ao holder Guard.

Erro em qualquer etapa mantém `cleanup_complete=false` e não libera o slot.
Ausência por timeout na busca por label não satisfaz a etapa 5.

## 9. Workflow consumidor

- somente `workflow_dispatch` na default branch;
- quatro inputs exatos; um job `trusted-jit` e label nonce;
- `contents: read`, zero secret de produto, checkout SHA com credencial não
  persistida;
- comandos pesados sequenciais;
- Docker disponível somente dentro da VM;
- cleanup de container/image conhecidos com remoção e inspeção posterior;
- nenhum prune global; destruição da VM cobre recursos adversariais adicionais.

## 10. Testes obrigatórios

- config/path/token/JSON negativos;
- HTTP 200 exato e recusa 204/malformed/redirect/partial;
- run/job/JIT identity, duplicate job e runner theft;
- ready persistido antes do segredo e receipts host-safe;
- driver digest drift, fd pinning, redaction e timeout;
- recovery repetível, crash antes/depois de callbacks e cgroup;
- holder explícito versus EOF;
- readmissão Guard em startup e não-release em cleanup inconclusivo;
- runner DELETE seguido de ausência confirmada;
- workflow estático e `bash -n`;
- Go focal, race e vet, sempre sem infraestrutura live.

## 11. Checklist de implementação

- [x] HTTP 200 + identidade exata preservados.
- [x] runner ID/job ID/process identity/lease persistidos.
- [x] lock local fixo e admissão Guard machine-wide.
- [x] startup reconciliation fail-closed.
- [x] cgroup/PDEATHSIG/process group.
- [x] driver pinado e protocolo ready/destroyed.
- [x] zero execução candidata no host.
- [x] cleanup remoto com postconditions.
- [x] `state_dir` e runner source sem overlap.
- [x] token docs/código alinhados em 8192 bytes.
- [x] workflow Docker fail-closed.
- [ ] driver real de VM auditado e instalado.
- [ ] imagem base real pinada e medida.
- [ ] config/token/digest live provisionados.
- [ ] canário live supervisionado.
- [ ] Guard comprova headroom Windows pós-reclaim bounded.

Os cinco itens finais mantêm ativação **NO-GO**.
