---
slug: generation-clean-boundary
title: Fronteira limpa de geração — fila exata por SHA, 80 GiB e zero interrupção de checks
milestone: —
issues: [226]
---

# PRD — Fronteira limpa de geração do runner compartilhado

> Tipo: mudança estrutural no host Windows que controla a fila, a VM e o VHDX.
> SSDV3 é obrigatório por `rules/ssdv3.md`. Este PRD cobre a solução única para
> `emersonbusson/civm#226`; a adoção do consumidor Acme ocorre no PR dele, após este
> contrato estar implantado.

## 1. Resumo

- **Confirmado no codebase:** a fila atual concede `pr-N`, mas os gates do
  consumidor já aguardam `pr-N@SHA`. Portanto o publicador pode escrever uma
  identidade que nenhum gate reconhece.
- **Confirmado no codebase:** o caminho `push_wave_force_compact` reapava,
  esperava 90 s e depois permitia `Stop-VM -Force` mesmo com worker ativo.
- **Confirmado por decisão do operador:** cada geração só pode iniciar depois
  de limpeza integral e compactação que deixe `V:` com **pelo menos 80 GiB
  livres**. Falha de prova, de limpeza ou de capacidade bloqueia a fila; nunca
  interrompe checks.

O resultado é uma fronteira central por geração exata (`pr-N@head_sha` ou
`branch-ref@sha`): a geração seguinte só recebe o contexto publicado depois de
o Civm provar o término da anterior, drenar o runner, limpar Docker/cache,
compactar e medir `V: >= 80`. Isso reproduz o começo limpo do CI pago sem pôr
operação de disco em cada workflow consumidor.

## 2. Contexto técnico

- **Confirmado no codebase:** `deploy/windows/civm-pr-queue.ps1` mantém FIFO e
  `Resolve-PrSlot`; `deploy/windows/civm-vm-orchestrator.ps1` consulta a API,
  publica `C:\ProgramData\civm\gate\current-context` e executa a manutenção Hyper-V.
- **Confirmado no codebase:** `Get-PrActivity` agrupa hoje por `pr-N`/`branch-N`,
  descarta erro de token/API com `continue`, e busca no máximo uma página de
  cada status. Isso não prova que a ausência observada significa término.
- **Confirmado no codebase:** `Resolve-PushWaveCompact` modela mudança de SHA
  dentro de um contexto por PR e deixa o caller transformar worker residual em
  compactação forçada. Os campos `lastCompactHeadSha` e
  `lastCompactContext` existem apenas para esse desenho.
- **Confirmado no codebase:** `Get-GuestHasActiveJob` consulta só
  `Runner.Worker`. O contrato mais completo já existe em `civmctl idle-check`:
  ele classifica `Runner.Worker`, `Runner.PluginHost`, trabalho em `_work` e
  builds Docker/Compose, com `0=idle`, `1=busy`, `2=unknown`.
- **Confirmado no codebase:** `civmctl maintenance enter|exit` já drena e
  restaura runners de forma idempotente, usando estado e lock do guest. É o
  primitivo reutilizável que fecha a janela entre uma prova de idle e o shutdown.
- **Confirmado no codebase:** o wrapper root-owned
  `/usr/local/bin/civm-generation-boundary` concentra a limpeza privilegiada
  com verbos fixos. No boundary ele remove todo conteúdo regenerável de `_work`
  (inclusive `_tool` e `_actions`), `_diag`, caches de linguagem/pacotes,
  journal, imagens, containers, volumes e builder cache, seguido de `fstrim`.
- **Confirmado no codebase:** o piso vivo de admissão é 55 GiB e há bypass após
  duas tentativas. Isso conflita com o requisito atual de 80 GiB rígidos.
- **Confirmado na documentação oficial:** `Stop-VM -Force` pode forçar o
  desligamento após prazo e perder dados não salvos; shutdown solicitado no
  guest é o caminho apropriado após o drain. A referência é a documentação
  Microsoft do cmdlet `Stop-VM`.
- **Confirmado na documentação oficial:** um job `runs-on` só é elegível em
  runner que tenha **todos** os labels pedidos. Os runners de gate atuais têm
  `civm-gate`, não labels dinâmicos por SHA; exigir um label SHA sem provisioná-lo
  deixa o job em fila. A referência é a documentação GitHub de self-hosted
  runners e labels.
- **Inferência / proposta:** a fila usará contexto exato por SHA e manterá os
  gates em labels estáticos `self-hosted,civm-gate`; a exclusão vem do arquivo
  de contexto, não de labels efêmeros.

## 3. Opção recomendada

- **Inferência / proposta:** substituir o push-wave por uma única fila de
  gerações exatas. Cada run ativo produz `pr-N@head_sha` ou
  `branch-ref@head_sha`; um push novo é naturalmente uma nova geração FIFO,
  sem reap/espera/force especiais.
- **Inferência / proposta:** antes de qualquer `grant` ou `boundary_advance`,
  executar uma fronteira obrigatória: prova de API completa, `idle-check`,
  capability `civm-generation-boundary/v1`, wrapper `prepare`, shutdown
  gracioso, `Optimize-VHD`, medição `V: >= 80`, boot, wrapper `resume` e só
  então publicação do contexto seguinte.
- **Inferência / proposta:** qualquer estado busy, unknown, erro de API,
  limpeza, shutdown, compactação, boot, restore ou capacidade mantém o contexto
  anterior e registra causa. Não existe timeout que promova `Stop-VM -Force`.

Alternativas descartadas:

- **Confirmado no codebase, rejeitada:** preservar `pr-N` e detectar SHA em
  push-wave. Mantém duas identidades e o caminho que já causou o force após 90 s.
- **Confirmado no codebase, rejeitada:** pular limpeza quando `V:` parece alto.
  Espaço alto não prova Docker/cache limpo nem equivale a uma VM efêmera.
- **Confirmado por decisão do operador, rejeitada:** piso 55 GiB e bypass após
  duas tentativas. Viola o piso rígido de 80 GiB.
- **Confirmado na documentação oficial, rejeitada:** label dinâmico por SHA sem
  um runner que possua esse label. O roteamento cumulativo o deixa inelegível.
- **Inferência / proposta, rejeitada:** compatibilidade dual-write entre
  `pr-N` e `pr-N@SHA`. Não há formato persistente que justifique um shim; a
  geração correta é a única fonte de verdade após o deploy coordenado.

Trade-off aceito: a próxima geração espera a manutenção e pode ficar bloqueada
por capacidade insuficiente. Isso é preferível a iniciar suja ou destruir um
check em andamento.

## 4. Requisitos funcionais

- **RF-1 — identidade exata.** Cada workflow run ativo vira um contexto
  `pr-N@head_sha` ou `branch-ref@head_sha`.
  - Critério de aceite: a string publicada é idêntica à que o gate calcula.
  - Isolamento: SHA novo nunca substitui a geração atual; entra depois dela.
- **RF-2 — atividade verificável.** A coleta pagina todos os runs ativos de
  cada repo/status; erro de token, HTTP, parse ou contexto sem SHA devolve
  `verified=false`.
  - Critério de aceite: `verified=false` não altera `currentPr` nem publica.
  - Isolamento: ausência parcial jamais é interpretada como fila vazia.
- **RF-3 — término conservador.** Uma geração só termina após atividade
  verificável vazia e grace de 10 minutos; atividade reaparecida zera o grace.
  - Critério de aceite: nenhum avanço antes do grace; novo run do mesmo SHA
    mantém o slot.
  - Isolamento: o grace cobre propagação entre workflows sem compactar no meio.
- **RF-4 — idle canônico e drain.** A fronteira usa `civmctl idle-check`; busy,
  unknown ou SSH falho é busy. Antes de shutdown, o wrapper `prepare` precisa
  comprovar a capability v1, maintenance strict e os idle-checks internos.
  - Critério de aceite: não há `Stop-VM -Force` nem `shutdown` quando a prova
    não é idle.
  - Isolamento: o drain impede novo job entre a última prova e o desligamento.
- **RF-5 — limpeza obrigatória.** Todo `grant` inicial e todo avanço entre
  gerações executa a limpeza completa existente, incluindo Docker volumes e
  builder cache, seguida de `fstrim`.
  - Critério de aceite: não há ramo `skip_clean`.
  - Isolamento: a limpeza só roda após RF-4; nunca toca build ativo.
- **RF-6 — capacidade rígida.** Só publica uma geração se `V:` medido após
  `Optimize-VHD` for `>=80` GiB. Medida ausente ou menor bloqueia.
  - Critério de aceite: não há contador/bypass que admita abaixo de 80.
  - Isolamento: o runner continua drenado enquanto a capacidade é insuficiente.
- **RF-7 — desligamento não destrutivo.** O wrapper pede
  `systemctl poweroff --no-block` ao guest e o host aguarda Off; falha no prazo
  não usa `Stop-VM -Force`.
  - Critério de aceite: a falha preserva a VM e o contexto anterior.
  - Isolamento: Hyper-V só recebe `Optimize-VHD` com VM comprovadamente Off.
- **RF-8 — publicação pós-condição.** O arquivo de contexto só muda depois de
  RF-2 a RF-7; erro mantém o valor anterior e o estado da fila para retry.
  - Critério de aceite: teste de falha não concede próximo contexto.
  - Isolamento: gates estáticos esperam o arquivo, sem label SHA efêmero.
- **RF-9 — pressão sem matar check.** O antigo `panic_compact` deixa de usar
  compactação offline com trabalho. Sob pressão durante atividade, o máximo é
  limpeza online segura + alerta; a próxima geração segue bloqueada até 80 GiB.
  - Critério de aceite: nenhuma saída de decisão chama compactação offline com
    `Running>0`.
  - Isolamento: proteção de disco não se torna causa de cancelamento de CI.
- **RF-10 — observabilidade.** Cada defer ou sucesso registra contexto, fase,
  motivo, `v_free_gb` e resultado de idle sem segredos.
  - Critério de aceite: log distingue API não verificável, guest busy,
    limpeza falha, capacidade baixa e publicação concluída.

## 5. Requisitos não-funcionais

- **RNF-1 — segurança operacional (Confirmado por decisão do operador):** zero
  interrupções intencionais de check; nenhum caminho automático chama
  `Stop-VM -Force` para reclamar espaço.
- **RNF-2 — privilégio (Confirmado no codebase):** a Scheduled Task do control
  plane continua SYSTEM para Hyper-V. O polling de PR confiável roda no host em
  runner dedicado `NETWORK SERVICE/Limited`, sem checkout ou secrets fornecidos
  pelo workflow, com binários read-only e contexto somente leitura.
- **RNF-3 — disponibilidade (Inferência / proposta):** a espera é ilimitada
  no control plane, com logs por tick; o timeout do job-gate do consumidor será
  ampliado ao máximo suportado pelo GitHub, mas falhará fechado e visivelmente.
- **RNF-4 — performance (Inferência / proposta):** o custo adicional é uma
  limpeza/compactação por geração, deliberado para paridade; nunca otimizar por
  `skip_clean` sem evidência nova e aprovada.
- **RNF-5 — consistência (Confirmado no codebase):** reutilizar o lock canônico
  `V:\civm-reclaim.lock` e o lock de maintenance; não criar um segundo dono de
  power-state/VHDX.
- **RNF-6 — resiliência (Inferência / proposta):** erro de API/SSH/Hyper-V é
  fail-closed para admissão e fail-open para o processo já rodando: mantém a VM
  e nunca mata work por incerteza.

## 6. Fluxos

### Happy path — primeira geração

1. **Host / GitHub API (Confirmado no codebase):** coleta runs ativos, pagina e
   forma `pr-N@SHA`.
2. **Host / fila (Inferência / proposta):** `Resolve-PrSlot` escolhe o primeiro
   contexto; não publica ainda.
3. **Host→guest (Inferência / proposta):** se a VM está Off, inicia apenas para
   a manutenção; gates no host ainda seguram jobs reais.
4. **Guest (Inferência / proposta):** capability v1 → wrapper `prepare`
   (maintenance strict, idle, limpeza integral, idle, poweroff).
5. **Guest/host (Inferência / proposta):** Off confirmado, `Optimize-VHD`,
   `V: >=80`, boot e wrapper `resume` (maintenance exit + idle).
6. **Host (Inferência / proposta):** grava o contexto exato; gate concede e a
   geração usa um guest limpo.

### Happy path — push no mesmo PR

1. **Host (Inferência / proposta):** o SHA novo aparece como contexto novo,
   não como mutação do antigo.
2. **Guest (Confirmado no codebase):** o reaper canônico pode cancelar SHA
   supersedido pelo timer; a fila não força o worker a terminar.
3. **Host (Inferência / proposta):** após RF-3, executa a mesma fronteira e
   só então publica o SHA novo.

### Fluxos de erro

| Condição | Resultado | Log mínimo | Consistência |
| --- | --- | --- | --- |
| API/token/parse falha | mantém contexto atual; não compacta/publica | `pr_activity_unverified`, repo/status | nenhum check é interrompido |
| guest busy/unknown | mantém contexto atual; retry no próximo tick | `generation_boundary_deferred`, idle | não drena nem desliga |
| maintenance/clean falha | mantém contexto atual; runner fica/restaura conforme a fase | `generation_boundary_failed`, phase | não publica geração suja |
| shutdown não chega a Off | aborta compactação sem force | `generation_shutdown_not_off` | VM fica ativa e drenada, requer retry |
| optimize/floor <80 | não publica; alerta crítico | `generation_capacity_blocked`, v_free_gb | próxima geração não inicia |
| boot/maintenance exit falha | não publica; retry conservador | `generation_restore_failed` | nenhum job recebe slot novo |

## 7. Modelo de dados

**Confirmado no codebase:** não há banco. O estado relevante é arquivo host.

`V:\civm-pr-queue.json` passa a usar apenas:

```json
{
  "contexts": [
    { "id": "pr-1625@0123abcd", "firstSeenUtc": "2026-08-04T00:00:00Z" }
  ],
  "currentPr": "pr-1624@deadbeef",
  "currentIdleSinceUtc": "2026-08-04T00:10:00Z"
}
```

- **Inferência / proposta:** `lastCompactHeadSha` e `lastCompactContext` deixam
  de participar da decisão; nenhum dual-write é criado.
- **Inferência / proposta:** `C:\ProgramData\civm\gate\current-context` contém uma única
  geração exata ou fica vazio. O valor anterior é mantido até o êxito total da
  fronteira, para que um leitor nunca receba concessão antecipada.
- **Confirmado no codebase:** locks existentes permanecem o mecanismo de
  exclusão; não há backfill de dados de produção.

## 8. API / Interfaces

- **Inferência / proposta:** `Get-PrActivity` retorna
  `{ verified: bool, counts: hashtable, errors: array }`; não retorna mais uma
  hashtable ambígua quando a observação falha.
- **Inferência / proposta:** `Get-RunGenerationContext` forma o ID exato e
  rejeita run sem `head_sha`, PR ou branch válida.
- **Inferência / proposta:** `Get-GuestHasActiveJob` executa
  `civmctl idle-check` via SSH e traduz todo código não-zero em busy.
- **Inferência / proposta:** `Invoke-PrepareGeneration` retorna
  `{ succeeded, reason, vFreeGB }`; o caller só chama o publicador com
  `succeeded=true` e `vFreeGB>=80`.
- **Confirmado no codebase:** adiciona somente a capability read-only
  `civmctl capability generation-clean-boundary`, que imprime o marcador v1;
  a mutação continua fechada no wrapper root-owned de argumentos fixos.
- **Inferência / proposta, consumidor:** gates usam labels estáticos
  `[self-hosted, civm-gate]`, calculam o mesmo contexto exato e esperam até o
  limite documentado; não exigem label por SHA.

## 9. Dependências e riscos

- **Confirmado no codebase:** a fronteira depende de SSH guest, API GitHub,
  Hyper-V, locks de reclaim e `civmctl` já implantado no guest.
- **Confirmado no codebase:** o reaper é o dono canônico de queued/in-progress
  órfãos; esta mudança não duplica cancelamento no workflow.
- **Inferência / proposta:** se `V:` não alcançar 80 após limpeza e compactação,
  a fila ficará bloqueada até intervenção/capacidade real. Isso é um sinal de
  capacidade, não um motivo para bypass.
- **Inferência / proposta:** se o gate consumidor não estiver adotado em um
  peer, o host não consegue impedir que um runner recém-bootado aceite job
  direto. A implantação exige gate antes de habilitar `-EnforceQueue` naquele
  peer; Acme é o canário obrigatório.
- **Confirmado na documentação oficial:** label adicional sem runner compatível
  mantém job queued; por isso labels SHA não entram no rollout.

## 10. Estratégia de implementação

1. **Confirmado por regra SSDV3:** criar este PRD, o SPEC e o IMPL antes de
   modificar `deploy/windows`.
2. **Inferência / proposta:** escrever primeiro testes PowerShell para contexto
   exato, grace de 10 min, erro não verificável, defer busy, piso 80 e ausência
   de ação force/skip.
3. **Inferência / proposta:** remover `Resolve-PushWaveCompact` e
   `Get-ContextHeadSha`; atualizar a fila para geração exata e atividade
   verificada/paginada.
4. **Inferência / proposta:** implementar preparação de geração com drain,
   limpeza, shutdown gracioso, compactação e restore; devolver objeto de
   resultado e só publicar após êxito.
5. **Inferência / proposta:** remover bypass de tentativas e `panic_compact`
   destrutivo da decisão; elevar o piso host para 80.
6. **Inferência / proposta:** atualizar runbooks/paridade e o gate Acme
   estático; executar testes locais, CI do Civm, deploy controlado e PR Acme
   black-box antes de qualquer merge.

## 11. Documentos a atualizar

- **Confirmado no codebase:** `PAID-CI-PARITY.md`,
  `CI-PARITY-CHECKLIST.md`, `runbooks/MULTI-PROJECT-RUNNER.md` e
  `runbooks/PR-QUEUE-ENABLE.md` descrevem a política atual e precisam refletir
  80 GiB, geração por SHA, defer e shutdown seguro.
- **Confirmado por regra do repo:** `validation.md` recebe somente resultado
  empírico de deploy/PR, nunca hipótese.
- **Inferência / proposta:** os documentos autoritativos README/AGENTS/CODEX e
  `rules/*.md` não mudam, pois o propósito/invariantes permanecem; a mudança é
  implementação e runbook da política já declarada. Se essa avaliação mudar,
  serão atualizados no mesmo commit conforme a sync rule.

## 12. Fora de escopo

- **Confirmado por decisão do operador:** configurar WSL com RAM fixa de 16 GiB.
  A admissão Docker-heavy continua usando controle central, não mudança de RAM.
- **Confirmado no codebase:** redesenhar o reaper/timer ou introduzir auto-merge.
- **Inferência / proposta:** migrar todos os peers para namespace de contexto
  com nome de repositório. O contrato atual `pr-N@SHA` é preservado para o
  rollout coordenado; colisão cross-repo é tratada em slice futura se observada.
- **Inferência / proposta:** reduzir o custo de `Optimize-VHD` via cache
  persistente. Isso contraria a limpeza requerida e não entra nesta correção.

## 13. Critérios de aceitação

- **CA-1:** testes puros provam que `pr-1@aaa` e `pr-1@bbb` são gerações
  distintas e FIFO; o grace de 10 min não avança cedo.
- **CA-2:** qualquer erro de coleta mantém o contexto e não publica próximo.
- **CA-3:** busca estática/teste prova ausência de `push_wave_force_compact`,
  `skip_clean` e `Stop-VM -Force` no fluxo automático.
- **CA-4:** teste da decisão prova `V:<80` nunca admite por contador/bypass e
  trabalho ativo nunca retorna compactação offline.
- **CA-5:** caminho de fronteira só chama publicador depois de idle, drain,
  clean, shutdown, optimize, `V:>=80` e restore bem-sucedidos.
- **CA-6:** o workflow Acme usa runner `civm-gate` estático e o gate calcula
  a mesma string que o Civm publica.
- **CA-7:** um PR real posterior ao deploy mostra no log uma fronteira completa
  `>=80` e todos os checks da mesma geração concluídos sem cancelamento causado
  pelo host.

## 14. Validação

- **Confirmado por regra do repo:** `go test -race -count=1 ./...`, `go vet
  ./...`, `git diff --check` e os testes PowerShell do deploy devem passar.
- **Inferência / proposta:** executar `civm-pr-queue.test.ps1` e
  `civm-orchestrator-decision.test.ps1` no host Windows/Pwsh que receberá o
  artefato; Linux local sem `pwsh` não é evidência de execução PowerShell.
- **Inferência / proposta:** teste de integração controlado mede antes/depois
  `V:`, confirma `civmctl idle-check`, manutenção enter/exit, contexto publicado
  e log JSON da fronteira.
- **Confirmado por decisão do operador:** somente depois do deploy, o PR único
  do Acme dispara a validação black-box limpa com múltiplos seeds/carga; o
  resultado real será anexado a `validation.md` e à descrição da PR.
