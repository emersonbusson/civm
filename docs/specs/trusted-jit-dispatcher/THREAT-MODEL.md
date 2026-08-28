# Threat model — Dispatcher JIT com isolamento descartável

Status live: **NO-GO**.

## 1. Ativos

- host persistente e seus dados;
- slot global de CPU/RAM/disco administrado pelo Guard;
- token GitHub do dispatcher e JIT config efêmera;
- integridade do workflow confiável e da imagem base;
- ledger, logs e identidade de run/job/runner;
- disponibilidade de outros builds da máquina.

## 2. Adversários

- autor de branch/código candidato same-repo;
- workflow concorrente que tenta consumir labels padrão do runner;
- processo local não privilegiado tentando ler arquivos/logs;
- resposta GitHub truncada, atrasada ou inconsistente;
- crash/kill/reboot entre qualquer efeito remoto e persistência;
- driver/imagem alterado por drift local;
- processo PID-reuse ou descendant órfão;
- job candidato com controle do Docker **da VM**.

Root e kernel comprometidos no host estão fora do controle do processo, mas
root-owned executables/configs são tratados como autoridade explícita.

## 3. Fronteiras

### Confiáveis

- `civmctl` e config host-local owner-only;
- Guard e seu broker machine-wide;
- driver de isolamento pinado por SHA-256;
- imagem base pinada e auditada;
- default branch/workflow no trusted SHA conferido por digest;
- APIs GitHub acessadas por token temporário de menor privilégio.

### Não confiáveis

- candidate ref/SHA e todo conteúdo checkout;
- scripts de package/build/test/Dockerfile;
- stdout/stderr do driver/runner;
- labels padrão como mecanismo de exclusividade;
- status remoto até ser validado contra identidade persistida;
- qualquer estado dentro da VM após começar o job.

## 4. Ameaças e controles

### T1 — runner theft por falta de bind atômico

**Ameaça:** outro job elegível consome o JIT antes do job esperado.

**Controle:** não declarar exclusividade. O job esperado só é aceito com
runner ID/nome/grupo gerado. Qualquer job roubador roda na VM descartável sem
host mounts, host Docker ou secrets de produto. O run esperado expira/cancela.

**Residual:** disponibilidade; um atacante pode causar timeout/consumo de
recursos dentro do slot. Guard limita a uma geração pesada por máquina.

### T2 — execução candidata no host persistente

**Ameaça:** persistência em runner install, kernel, Docker ou disco.

**Controle:** o host executa somente o driver pinado. O runner é copiado para a
VM; nenhum diretório host é montado. Destruição/reset da VM é obrigatória.

**Abort trigger:** qualquer caminho que execute `run.sh` diretamente no host ou
exponha socket Docker/mount host.

### T3 — driver ou base adulterados

**Ameaça:** path é trocado depois do hash ou imagem diferente é iniciada.

**Controle:** driver aberto com `O_NOFOLLOW`, validado por `fstat` e SHA-256 e
executado pelo fd herdado. Receipt precisa repetir o digest da base esperado.

**Residual:** a implementação real do driver/base ainda não existe nesta
entrega; por isso ativação permanece NO-GO.

### T4 — vazamento de token/JIT config

**Ameaça:** argv, env, log, ledger, erro HTTP ou processo concorrente lê segredo.

**Controle:** token por stdin e buffers zerados best-effort; env child reduzido;
body remoto nunca entra no erro; JIT só vai por stdin após receipt ready
persistido; redaction em stdout/stderr; VM sem outros tenants.

**Residual:** memória do processo não é enclave. O host de control plane precisa
ser confiável e sem processo same-UID hostil.

### T5 — crash libera recurso cedo

**Ameaça:** dispatcher morre e Guard libera enquanto VM/runner/descendentes
continuam.

**Controle:** holder separado permanece vivo em EOF e guarda marker com
PID/start ticks. Startup reconcilia antes de novo dispatch. Se holder morreu,
readquire Guard antes da limpeza.

**Residual:** kill simultâneo do broker/holder pode criar janela sem lease; o
próximo recovery readquire o slot, mas disponibilidade do Guard é requisito
operacional.

### T6 — PID reuse/descendente órfão

**Ameaça:** matar processo errado ou deixar child vivo.

**Controle:** `(PID,start_ticks)`, process group, nascimento direto no cgroup v2
do lease via `CLONE_INTO_CGROUP`, `PDEATHSIG`, TERM→KILL e `cgroup.kill`;
release só após PID morto, `Wait()` reap e cgroup vazio.

### T7 — efeito remoto parcial

**Ameaça:** dispatch/JIT ocorreu, mas resposta/persistência falhou.

**Controle:** intent durável antes do POST, zero retry e estado `ambiguous`.
Run ID desconhecido não é substituído por “mais recente”; Guard fica retido.

### T8 — cancelar/deletar identidade alheia

**Ameaça:** drift da API ou ledger adulterado leva a cancelar run/runner errado.

**Controle:** run revalidado por workflow, trusted SHA, evento e título; runner
por ID/nome/label custom; arquivos owner-only e JSON estrito.

### T9 — cleanup Docker enganoso

**Ameaça:** candidato cria recursos fora dos nomes conhecidos ou impede cleanup.

**Controle:** workflow verifica remoção dos recursos conhecidos e falha se
Docker/postcondition não puder ser consultado. A fronteira real é destruir toda
a VM; não tentar “sanitizar” host reutilizado.

### T10 — concorrência pesada

**Ameaça:** duas suítes/builds esgotam 31,9 GiB e travam host/WSL.

**Controle:** somente `guard exec` concede o slot global. O flock é fixo e
serializa recovery, mas não é apresentado como resource gate. Workflows têm um
job e comandos pesados sequenciais. Reclaim de file cache, quando necessário,
usa `memory.reclaim` bounded+timeout; a admissão depende da leitura posterior
do sensor Windows, não dos bytes solicitados. Sensor desconhecido falha fechado
e `drop_caches` cego é proibido. Com `20 GiB` permitidos ao WSL e `12 GiB`
fixos no guest, os `32 GiB` teóricos já excedem os `31,90 GiB` físicos: o Guard
desconta o commit pendente da VM, um piso Windows aprovado e margem, sob o lease
global, antes de permitir start/job.

### T11 — GitHub Free sem branch protection

**Ameaça:** ausência de controls pagos é confundida com segurança inexistente.

**Controle:** autoridade fica no dispatcher/config/digest e no isolamento
descartável. Não depender de upgrade, runner group pago ou required status.

## 5. Postconditions de segurança

O holder Guard só pode sair quando:

1. receipt `destroyed+reset_verified` válido foi persistido;
2. driver PID exato morreu e cgroup está vazio/removido;
3. run ID exato está terminal;
4. runner ID exato foi observado ausente;
5. ledger marca `cleanup_complete=true`.

Se uma única observação faltar, o estado é ambíguo e a admissão continua
bloqueada.

## 6. Riscos de ativação ainda abertos

- driver real e idempotência de destroy/recover não auditados;
- imagem base real e ausência de mounts/Docker host não medidas;
- comportamento live de runner theft não exercitado;
- token/permissões/config/digest live ausentes;
- nenhum canário de crash, timeout ou reboot;
- logs externos e alerta para lease preso não provisionados.

Esses riscos são de ativação e mantêm **NO-GO**; não são apagados por testes
unitários.
