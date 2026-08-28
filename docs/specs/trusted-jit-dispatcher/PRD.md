---
slug: trusted-jit-dispatcher
title: Dispatcher JIT com isolamento descartável
milestone: —
issues: []
---

# PRD — Dispatcher JIT com isolamento descartável

> Mudança R4 em scheduler, credencial e execução de código candidato.
> Implementação e testes herméticos não autorizam ativação. Estado live:
> **NO-GO**.

## 1. Problema

O GitHub gera runners JIT de um job, mas não oferece uma operação atômica que
vincule aquele runner a um run/job específico. Labels custom reduzem colisão;
labels padrão (`self-hosted`, SO e arquitetura) ainda podem tornar o runner
elegível para outro job same-repo. Portanto, exclusividade de scheduler não
pode ser prometida.

Executar código candidato num host persistente tornaria esse roubo relevante:
o job poderia alcançar disco, `/proc`, kernel, Docker, instalação do runner ou
segredos residuais. Limpar um host adversarial depois do job não prova que ele
voltou ao estado confiável.

## 2. Resultado esperado

Criar `civmctl jit-dispatch`, um comando one-shot externo ao workflow que:

1. valida autoridade local e identidade GitHub antes de qualquer execução;
2. obtém o único slot pesado machine-wide exclusivamente por `guard exec`;
3. despacha o workflow confiável e registra runner JIT repository-scoped;
4. entrega o segredo JIT somente a uma VM descartável, sem mounts do host,
   Docker do host ou secrets de produto;
5. aceita sucesso apenas quando o job esperado usou o runner ID/nome/grupo
   gerado;
6. libera o slot somente após VM destruída/resetada, run terminal e runner
   remoto ausente.

Se outro job roubar o runner, o job esperado expira ou é cancelado. O código
roubador encontra apenas o ambiente descartável sem autoridade sobre o host.

## 3. Requisitos funcionais

- **RF-1 — autoridade local:** config JSON absoluta, owner-only, fora do Git,
  com allowlist exata de repo, refs, workflow, digest, job e runner group.
- **RF-2 — segredo:** token por stdin; token e JIT config nunca em argv, env
  child, ledger, log ou mensagem remota.
- **RF-3 — dispatch identificável:** API `2026-03-10`; somente HTTP 200 com
  `workflow_run_id`, `run_url` e `html_url` exatos. Sem descoberta heurística.
- **RF-4 — identidade:** default branch, trusted SHA, workflow digest,
  candidate ref/SHA, evento/path/título do run e job único precisam coincidir.
- **RF-5 — JIT:** endpoint repository-scoped, nonce CSPRNG de 256 bits, runner
  ID/nome/labels persistidos e conferidos no job executado.
- **RF-6 — admissão:** o slot pesado é global da máquina e pertence ao Guard.
  `state_dir` não cria domínio de concorrência. O Guard só admite após sensor
  Windows fresco pós-reclaim contabilizar RAM da VM, piso do Windows e margem.
- **RF-7 — recuperação:** ledger persiste lease, run/job/runner, PID+start time,
  process group, cgroup e isolamento. Startup reconcilia antes de novo efeito.
- **RF-8 — contenção:** driver nasce no cgroup v2 do lease via
  `CLONE_INTO_CGROUP`, em process group próprio, com `PDEATHSIG`; executável é
  pinado por SHA-256 e aberto por file descriptor antes de `exec`.
- **RF-9 — handoff:** o driver envia receipt `ready`; somente depois de
  persistido recebe o JIT por stdin.
- **RF-10 — cleanup:** driver envia `destroyed+reset_verified`; run deve estar
  terminal e runner remoto ausente antes de liberar Guard.
- **RF-11 — Docker:** candidato não acessa Docker do host. Recursos conhecidos
  dentro da VM têm cleanup fail-closed; destruir a VM é a postcondition final.
- **RF-12 — replay:** nenhum POST ambíguo é repetido. Estado não terminal exige
  reconciliação ou inspeção.
- **RF-13 — compatibilidade:** não depende de GitHub Pro, branch protection,
  rulesets ou runner groups pagos.

## 4. Requisitos não funcionais

- Linux e Go stdlib no control plane; sem daemon/webhook novo.
- Uma única execução pesada por máquina; Guard é a autoridade de admissão.
- Logs estruturados e redigidos; arquivos de estado 0600 e diretórios 0700.
- Nenhuma ação live, credencial, runner, VM, Docker ou serviço nos testes
  herméticos desta entrega.
- Falha de prova é falha fechada, mesmo que exija intervenção humana.

## 5. Fora de escopo

- Implementar ou instalar o driver de VM real nesta remediação.
- Provisionar token, config live, imagem base, runner ou serviço.
- Resolver a corrida do scheduler do GitHub; ela é modelada, não ocultada.
- Usar upgrade pago como controle compensatório.
- Sanitizar e reutilizar host/VM que executou código candidato adversarial.

## 6. Critérios de aceitação de código

- Zero execução direta de `run.sh` no host persistente.
- `state_dir` e `runner_directory` não se sobrepõem.
- Lock local e marker têm caminhos fixos em `/run/civm`.
- Guard permanece preso em crash/EOF até a próxima reconciliação.
- Recovery sem holder readquire Guard antes de limpar.
- PID sem start identity ou cgroup exato é recusado.
- Receipt com host mount, host Docker, product secret ou base divergente é
  recusado antes de enviar JIT.
- Job executado em runner divergente nunca produz sucesso.
- Cleanup remoto exige confirmação de ausência por ID exato.
- Workflow consumidor tem um job serial, SHA exato, zero secret de produto e
  cleanup Docker com postconditions.
- Testes focais, race, vet e estáticos passam sem infraestrutura live.

## 7. Gate de ativação

A ativação permanece **NO-GO** até existirem, fora deste commit:

1. driver real auditado que cria/destrói VM por lease;
2. imagem base imutável com digest real e runner copiado, nunca montado;
3. prova de `host_mounts=false`, `host_docker=false` e
   `product_secrets=false`;
4. Guard e diretório `/run/civm` instalados com ownership/permissões corretos;
5. config 0600, digest do workflow e token temporário de menor privilégio;
6. canário live supervisionado para sucesso, theft, timeout, crash e cleanup;
7. auditoria de logs confirmando zero vazamento e zero runner residual.
8. Guard comprovando headroom por sensor Windows após reclaim bounded, sem
   `drop_caches`, sem assumir bytes exatos e sem tratar os tetos WSL/VM como
   capacidade disponível.

Código local aprovado não altera esse veredito.
