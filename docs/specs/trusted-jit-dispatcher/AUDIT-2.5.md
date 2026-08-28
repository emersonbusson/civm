# Auditoria SSDV3 2.5 — Dispatcher JIT descartável

Data: 2026-08-23  
Origem: remediação do veredito independente Maxwell **NO-GO**.

## Veredito

- **Código local:** pronto para validação hermética final.
- **Ativação:** **NO-GO**.
- **Dependência paga:** nenhuma.

## Findings recebidos e disposição

### P0 — JIT não possui bind atômico run/job

**Confirmado.** O generate-jitconfig aceita labels; não recebe run/job ID. Job
API expõe o runner usado apenas como observação.

**Remediação:** remover qualquer promessa de exclusividade. Código candidato
executa somente em VM descartável sem host mounts/Docker/secrets. Sucesso exige
o runner ID/nome/grupo gerado no job esperado. Runner theft vira falha de
disponibilidade, não comprometimento do host.

**Status:** fechado no desenho/código; driver live continua pré-requisito.

### P1 — crash deixa runner/processos e libera slot

**Confirmado.** Um flock do processo principal é insuficiente.

**Remediação:** holder separado dentro de `guard exec`; EOF não o encerra.
Marker v2 identifica lease, admissão aleatória, PID e start ticks, evitando
confundir broker tardio com uma nova tentativa do mesmo lease. Cancelamento
pré-admissão termina e colhe o broker. Ledger persiste lease, PID/start ticks,
process group, cgroup, run/job/runner e isolamento. Startup reconcilia todos os
estados; sem holder, readquire Guard antes de cleanup. TERM/KILL+cgroup e
desaparecimento remoto são postconditions.

**Status:** fechado por unit tests; live crash/reboot pendente.

### P1 — cleanup de host/Docker não é fronteira adversarial

**Confirmado.** Remover work folder ou recursos Docker conhecidos não sanitiza
host reutilizado.

**Remediação:** zero execução candidata no host e zero acesso ao Docker host.
Driver pinado cria ambiente descartável e precisa provar destroy/reset. Cleanup
Docker do workflow é fail-closed, mas apenas defesa em profundidade dentro da
VM.

**Status:** fechado no contrato; implementação real do driver pendente.

### P1 — admissão ligada a `state_dir`

**Confirmado.** Paths configuráveis poderiam criar múltiplos domínios de lock.

**Remediação:** lock fixo `/run/civm/jit-dispatch.lock` somente para recovery;
marker fixo `/run/civm/jit-dispatch-lease.json`; admissão pesada pertence ao
broker Guard por `guard exec`, sem API nova.

**Status:** fechado e testado com fakes/holder local.

### P2 — overlap, token e checklist

**Confirmado.** Documentação/código divergiam e `state_dir` podia coincidir com
runner source.

**Remediação:** rejeição de overlap em ambas as direções; token 20..8192 bytes
em código/docs; config inclui Guard/driver/digests/recovery; checklist e threat
model atualizados.

**Status:** fechado.

## Controles preservados

- endpoint JIT repository-scoped;
- dispatch HTTP 200 com run ID/URLs exatos;
- workflow na default branch, trusted SHA e digest;
- candidate ref/SHA exatos e revalidação final;
- nonce 256-bit e custom label único;
- token somente por stdin;
- nenhuma descoberta por run recente;
- zero dependência de GitHub Pro/branch protection/runner groups pagos;
- workflow consumidor com `contents: read`, zero secret e um job.

## Counterfactual obrigatório

Se o runner theft fosse impossível, bind atômico seria parte da chamada JIT e
testável antes de executar o runner. A API não oferece esse parâmetro. Portanto,
qualquer design que execute candidato no host e diga “o nonce garante o job” é
factualmente falso. A fronteira descartável é necessária, não cosmética.

## Evidência local atual

- testes focais do dispatcher e SPEC passaram durante a remediação;
- teste estático do workflow passou;
- nenhuma chamada GitHub real, VM, Docker, runner, secret ou serviço;
- validações amplas/race/vet são registradas no `IMPL.md` somente após execução
  final sob a regra de um único teste/build pesado por host.

## Blockers de ativação

1. driver real de VM não implementado/auditado;
2. imagem base e digest reais ausentes;
3. Guard/runtime paths/config live não provisionados;
4. token de menor privilégio e workflow digest live ausentes;
5. canários de success/theft/timeout/crash/reboot não executados;
6. monitoramento de lease preso e logs externos não instalado.
7. headroom Windows pós-reclaim ainda não integrado/provado no Guard live.

Até fechar os 7 itens com evidência, ativação permanece **NO-GO**.
