# Dispatcher JIT com isolamento descartável

## Veredito atual

**Código local: implementado. Ativação: NO-GO.** Este runbook não autoriza
instalar driver, provisionar token, iniciar VM/runner/Docker, habilitar serviço
ou executar canário.

O GitHub não fornece bind atômico entre JIT e run/job. O desenho aceita runner
theft e o torna inofensivo: qualquer job que consumir o runner executa somente
numa VM descartável sem mounts do host, Docker do host ou secrets de produto.

## Fluxo

```text
preflight local
  -> flock fixo de recovery
  -> guard exec (slot pesado machine-wide)
  -> workflow trusted + run/job IDs
  -> JIT repository-scoped
  -> driver pinado cria VM e atesta ready
  -> JIT por stdin, runner copiado, um job
  -> driver destrói/reset VM
  -> run terminal + runner remoto ausente
  -> release explícito do Guard
```

## Config host-local

Exemplo estrutural, sem valor real:

```json
{
  "api_base_url": "https://api.github.com",
  "api_version": "2026-03-10",
  "state_dir": "/var/lib/civm/jit-dispatcher",
  "runner_directory": "/opt/actions-runner-source",
  "guard_executable": "/usr/local/bin/guard",
  "isolation_driver": "/usr/local/libexec/civm-jit-isolation",
  "isolation_driver_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
  "base_image_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
  "queue_wait": "30s",
  "queue_poll": "100ms",
  "http_timeout": "15s",
  "job_poll_interval": "2s",
  "job_bind_timeout": "2m",
  "run_timeout": "2h",
  "shutdown_grace": "10s",
  "recovery_timeout": "20m",
  "repositories": [
    {
      "repository": "owner/repository",
      "trusted_ref": "refs/heads/main",
      "workflow": ".github/workflows/civm-jit.yml",
      "workflow_sha256": "0000000000000000000000000000000000000000000000000000000000000000",
      "candidate_refs": ["refs/heads/reviewed-branch"],
      "runner_group_id": 1,
      "job_name": "trusted-jit"
    }
  ]
}
```

Requisitos:

- arquivo regular 0600, owner do processo, path absoluto e fora do Git;
- state e runner source sem igualdade/nesting;
- driver/base/workflow com digests reais, nunca placeholders;
- `/run/civm` real, sem symlink e sem write de group/outros;
- kernel/delegação cgroup v2 aceitam `CLONE_INTO_CGROUP`; não existe fallback
  para attach depois do start;
- runner source confiável; driver copia arquivos para VM, não monta o path;
- config do segundo repo não cria outro slot: Guard continua global.

## Contrato obrigatório do driver

O executável implementa:

```text
civm-jit-isolation run ... --control-fd=3
civm-jit-isolation recover ... --control-fd=3
```

Para `run`:

1. criar VM/disco descartável por lease;
2. validar imagem base contra `--expected-base-sha256`;
3. garantir zero host mount, zero host Docker e zero product secret;
4. emitir receipt `ready` em fd 3;
5. ler um único JSON JIT por stdin;
6. copiar o runner source e iniciar JIT dentro da VM;
7. nunca persistir/logar JIT config;
8. destruir VM e disco mesmo em erro/timeout;
9. verificar ausência/reset e emitir receipt `destroyed`.

Para `recover`, destruir idempotentemente qualquer isolamento do lease, mesmo
se isolation ID estiver ausente, e emitir um receipt `destroyed`.

Receipt v1 contém exatamente:

```json
{
  "protocol": 1,
  "event": "ready|destroyed",
  "lease_id": "64 lowercase hex",
  "isolation_id": "opaque-safe-id",
  "base_sha256": "64 lowercase hex",
  "disposable": true,
  "host_mounts": false,
  "host_docker": false,
  "product_secrets": false,
  "destroyed": false,
  "reset_verified": false
}
```

No evento final, `destroyed` e `reset_verified` são `true`. Qualquer divergência
mantém o Guard preso.

## Credencial

Aceitos futuramente:

- GitHub App installation token; ou
- fine-grained token temporário.

Permissões mínimas devem cobrir Contents read, Actions dispatch/read/cancel e
Administration de runner apenas no repo allowlisted. GitHub Free é suportado;
não usar branch protection paga como requisito.

O token entra por stdin, 20..8192 bytes ASCII. A forma de invocação futura é:

```bash
token-provider | civmctl jit-dispatch \
  --config=/etc/civm/jit-dispatcher.json \
  --repo=owner/repository \
  --candidate-ref=refs/heads/reviewed-branch \
  --candidate-sha=40-or-64-lowercase-hex \
  --idempotency-key=opaque-request-key
```

Não colocar token em variável exportada, flag, arquivo ou histórico.

## Headroom Windows antes da admissão

Este controle pertence ao Guard; o dispatcher não duplica sua API nem usa
`MemAvailable` do Linux como substituto do host.

Evidência operacional fornecida em 2026-08-23, não repetida nesta remediação:
com aproximadamente 7,2 GiB de file cache e zero build ativo, um reclaim
bounded de 6 GiB em `/sys/fs/cgroup/memory.reclaim` reduziu `buff/cache` de
aproximadamente 8,6 para 3,5 GiB. Em cerca de 15 s, `vmmemWSL` WorkingSet caiu
de 11,12 para 7,80 GiB, Private de 12,08 para 8,33 GiB e Windows
FreePhysical subiu de 10,13 para 12,22 GiB.

Isso prova possibilidade, não bytes exatos. Contrato de ativação:

1. adquirir/serializar a decisão no lease global e confirmar zero
   compilador/build/suíte pesada concorrente;
2. ler sensor Windows fresco; `20 GiB` de teto WSL + `12 GiB` fixos da VM
   somam `32 GiB`, acima dos `31,90 GiB` físicos, portanto teto configurado não
   é capacidade disponível;
3. se necessário e se `memory.reclaim` existir, solicitar apenas reclaim
   bounded de file cache sob timeout;
4. aguardar propagação por janela limitada;
5. reler o sensor Windows e admitir somente se o saldo cobrir o commit ainda
   pendente de `12 GiB` da VM, o piso Windows aprovado e a margem de segurança;
6. se o ganho for menor, sensor falhar ou timeout expirar, aguardar/falhar
   fechado.

Nunca usar `drop_caches` cego, loop ilimitado ou afirmar que o valor escrito em
`memory.reclaim` foi integralmente devolvido ao Windows.

## Recovery

O holder Guard grava `/run/civm/jit-dispatch-lease.json` v2 com lease,
`admission_id` aleatório por tentativa, PID e start ticks. Em shutdown limpo,
recebe `release\n`; em EOF fica vivo. Timeout antes da admissão termina e colhe
o broker; uma tentativa nunca aceita marker de outra admissão do mesmo lease.

Ao iniciar uma nova solicitação, o dispatcher:

1. bloqueia o flock fixo;
2. lista todos os ledgers;
3. compara marker/lease;
4. mantém holder existente ou readquire Guard se ele morreu;
5. mata processo/cgroup exatos;
6. chama driver `recover`;
7. espera run terminal;
8. remove/confirma ausência do runner;
9. persiste cleanup completo;
10. libera Guard.

Não apagar marker/ledger manualmente para “destravar”. Run ID desconhecido,
mais de um ledger incompleto ou marker divergente exigem investigação humana.
Se Generate JIT teve resultado ambíguo e o runner ID não foi recebido, a busca
por label exato pode remover um runner que apareça; expirar a janela sem
encontrá-lo não comprova ausência e mantém o Guard preso. Somente uma rejeição
HTTP 4xx autoritativa permite concluir que esse POST não criou runner.

## Gate de ativação

Todos os itens precisam de evidência medida:

- [ ] loop/unit JIT legado instalado foi inventariado e está inativo;
- [ ] Guard real aceita `guard exec` e garante um slot para toda a máquina;
- [ ] Guard aplica o contrato de headroom Windows pós-reclaim bounded, com
      piso Windows aprovado, margem e commit de `12 GiB` da VM;
- [ ] runtime dir/marker/lock têm ownership e modes corretos;
- [ ] kernel/delegação cgroup v2 iniciam e recuperam child no lease exato;
- [ ] driver real confere digest e é root/current-owned sem write amplo;
- [ ] VM não tem mount host, Docker host, shared memory ou secret de produto;
- [ ] runner source é copiado, não montado;
- [ ] destroy/recover é idempotente 2x;
- [ ] token/config/workflow digest reais têm menor privilégio;
- [ ] canário de sucesso confirma runner ID/nome/grupo exatos;
- [ ] canário de theft confirma host inatingível e run esperado recusado;
- [ ] timeout/crash/reboot mantêm/reobtêm slot e limpam tudo;
- [ ] runner remoto desaparece antes do próximo heavy;
- [ ] logs externos não contêm token/JIT;
- [ ] alerta detecta lease preso.

Até 16/16 itens passarem, estado live é **NO-GO**.

## Abort e rollback

Abortar imediatamente se:

- candidato alcançar path/socket/processo do host;
- driver aceitar base divergente;
- Guard liberar com VM/processo/run/runner pendente;
- runner esperado completar em ID/nome/grupo diferente;
- houver retry de POST ambíguo ou descoberta por run recente;
- cleanup usar prune global.

Rollback é remover a invocação externa e preservar ledgers/logs. Cancelar apenas
run ID conhecido e remover apenas runner ID validado. Nunca restaurar o loop
legado ou apagar evidência para repetir a solicitação.
