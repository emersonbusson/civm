# SPECv2 — fronteira limpa por geração exata (após red-team)

Issue: `emersonbusson/civm#226`  
Base: [`SPEC.md`](SPEC.md)  
Status: versão executável após Passo 2.5  

> Esta versão prevalece sobre `SPEC.md` onde houver conflito. O red-team deu
> **no-go** ao baseline por três blockers: a coleta ainda poderia ignorar run
> ativo antigo, `maintenance enter` aceita drenagem parcial e seu timeout padrão
> de 30 s é curto para uma operação que chama systemd e GitHub. Abaixo está a
> solução única que fecha os três sem criar caminhos de force.

## 1. Findings do red-team e decisões

| ID | Contraexemplo confirmado | Risco | Decisão v2 |
| --- | --- | --- | --- |
| RT-1 | `Get-PrActivity` filtra atividade por idade | run ativo/stalled pode desaparecer da fila | não aplicar filtro de idade na fila de geração; status real não-terminal sempre segura o slot |
| RT-2 | `maintenance.Enter` retorna sucesso se ao menos um runner drenou | listener de outro runner pode continuar aceitando job entre idle e shutdown | adicionar `--strict`, que só retorna 0 quando todos os runners elegíveis estão parados |
| RT-3 | `maintenance` usa timeout padrão de 30 s | timeout pode deixar drain parcial e erro pouco acionável | fronteira chama enter/exit com `--timeout=120`; failure preserva state e faz defer |
| RT-4 | `Get-OrchestratorDecision` ainda pode admitir fora da fila | `-EnforceQueue` poderia ser contornado por branch genérico | em enforce, só `Invoke-PrGenerationQueue` pode preparar/publicar/admitir geração |
| RT-5 | labels SHA não existem nos quatro gate runners reais antes da publicação | job fica inelegível e parece “espera” | publisher seleciona o cohort pela allowlist de nomes e atribui `civm-gate` + label SHA exata; ausência mantém fail-closed |

RT-5 substitui explicitamente a decisão estática anterior. O setup deixa cada
gate online com zero labels customizadas; o publisher não depende da label-base
para descobrir o cohort, publica a label dinâmica exata e o peer nunca faz
fallback ao vencer timeout.

Evidência RT-2: `internal/maintenance/maintenance.go:Enter` tolera falha de
stop/label por runner e só falha quando ambos falham em **todos**. Isso é correto
para maintenance convencional, mas insuficiente para a garantia desta fronteira.

## 2. Drain estrito — `civmctl maintenance enter --strict`

### 2.1 Interface pública

Em `cmd/civmctl/maintenance.go`:

```text
civmctl maintenance enter --execute --strict --timeout=120
```

- `--strict` só é válido para `enter`; em `exit` é erro de uso.
- Sem `--strict`, o comportamento atual permanece inalterado.
- A fronteira sempre usa `--execute --strict --timeout=120`, nunca `--force`.

### 2.2 Modelo interno

Em `internal/maintenance/maintenance.go`:

```go
type Options struct {
    // existente
    RequireStopped bool // novo: enter só retorna sucesso quando todos pararam
}
```

`Enter` mantém o snapshot persistido com cada `RunnerState`, inclusive parcial.
Quando `RequireStopped` está ativo:

1. Antes de mutar, executa o idle check normal. Busy/unknown retorna erro sem
   parar listener novo.
2. Para cada runner descoberto, tenta o drain normal. O critério estrito é
   `RunnerState.Stopped == true`; remoção de label é defesa adicional e continua
   registrada, mas não substitui parada da unit.
3. Se qualquer runner não parar, grava o snapshot parcial **antes** de retornar
   erro. Assim `exit` pode restaurar o que já parou e um retry conhece o estado.
4. Em novo `enter --strict` com snapshot existente, roda idle check e re-tenta
   somente os `RunnerState` ainda `Stopped=false`, atualizando o mesmo snapshot.
   Não trata a mera existência do arquivo como sucesso.
5. Só depois de todos `Stopped=true` atualiza `DrainedAt` e retorna 0.
6. `Exit` permanece idempotente e restaura somente efeitos registrados. Não há
   rollback automático que religue listener enquanto a fronteira ainda está
   bloqueada.

Casos:

| Estado | `enter --strict` | Efeito |
| --- | --- | --- |
| todos param | 0 | segue para clean |
| ao menos um ativo/stop falha | 1 | snapshot parcial, defer; não compacta |
| snapshot parcial + retry + todos param | 0 | segue para clean |
| snapshot parcial + guest busy | 1 | não toca units; defer |
| `exit` com snapshot parcial | 0/erro já existente | restaura só o que mudou |

### 2.3 Testes Go (TDD)

Adicionar primeiro testes em `internal/maintenance/maintenance_test.go`:

1. strict com um stop falho retorna erro e persiste estado parcial;
2. retry strict repara somente runner pendente e então retorna sucesso;
3. retry strict busy não emite novo `systemctl stop`;
4. exit de estado parcial restaura só os parados;
5. CLI rejeita `maintenance exit --strict`;
6. CLI encaminha `--strict` para `Options.RequireStopped`.

Não é aceitável implementar `--strict` apenas como checagem de label: labels
podem falhar na API e não provam que um listener local parou.

## 3. Coleta de atividade v2

`Get-PrActivity` não tem mais `MaxAgeHours`. Para cada repo/status:

1. busca `per_page=100&page=N` até página com menos de 100;
2. valida que a resposta contém `workflow_runs` e que cada item listado tem o
   status pedido, `head_sha` e identidade de contexto;
3. soma apenas `queued`/`in_progress` cujo `run.status` ainda seja igual ao
   pedido; entrada que a API retornou já completed é ignorada como ghost;
4. qualquer erro, página malformada ou identificação ausente faz
   `{verified=false}`.

Um run realmente antigo com status não-terminal permanece visível. Se ele é
supersedido/PR fechado, o reaper canônico o cancela; se é o head atual, ele é
evidência que exige diagnóstico, não permissão para compactar.

## 4. Autoridade de admissão v2

Quando `-EnforceQueue` está ativo:

1. `Invoke-PrGenerationQueue` roda antes da decisão genérica.
2. Ela é a única função autorizada a chamar `Invoke-PrepareGeneration`,
   `Start-VM` para admissão, `maintenance exit` e `Publish-CurrentContext`.
3. Se a fila está não verificável ou a preparação falha, o switch genérico não
   pode executar `start`, `reclaim_before_admit`, `mark_busy` que reative runner
   ou admita trabalho. Ele só pode registrar observabilidade e fazer limpeza
   online não destrutiva se o guest estiver ativo.
4. `-Observe` nunca altera estado, labels, runner ou VM.

Isso elimina a rota paralela pela qual um job poderia começar antes do piso de
80 GiB. Peer sem gate não pode reivindicar a garantia; o rollout habilita enforce
somente após o teste estático de adoção do gate.

## 5. Sequência v2 de fronteira

Para `grant` e `boundary_advance`, conservar o contexto antigo/ausente e executar:

```text
atividade verificável
  -> iniciar VM apenas se Off (sem publicar target)
  -> idle-check = 0
  -> capability civm-generation-boundary/v1
  -> wrapper prepare (maintenance strict + idle + full clean + poweroff)
  -> esperar Off
  -> Optimize-VHD; V: >= 80
  -> Start-VM; esperar SSH
  -> capability civm-generation-boundary/v1
  -> wrapper resume (maintenance exit + idle-check)
  -> listener pronto
  -> publicar target exato
```

Cada seta pode devolver `defer`; nenhuma devolve force. Após um defer depois do
drain, a VM/runners permanecem no estado mais seguro que foi provado. Um retry
é idempotente por locks/snapshot e não publica cedo.

## 6. Mudanças adicionais de arquivo

Além da tabela da SPEC base:

| Arquivo | Alteração v2 | RF |
| --- | --- | --- |
| `cmd/civmctl/maintenance.go` | flag `--strict`, validação de subcomando e ajuda | RF-4 |
| `cmd/civmctl/maintenance_test.go` ou `main_test.go` | contrato da flag | RF-4 |
| `internal/maintenance/maintenance.go` | `RequireStopped`, persist/retry estrito | RF-4 |
| `internal/maintenance/maintenance_test.go` | casos de partial/retry/exit | RF-4 |
| `internal/hostdisk/generation_boundary_test.go` | exige `--strict --timeout=120` no host | RF-4 |
| `deploy/bin/civm-generation-boundary` | wrapper root-owned de argumentos fixos | RF-4–RF-7 |
| `internal/hook`, `internal/doctor` | instalação e prova funcional da capability v1 | RF-4, RF-10 |

## 7. Aceitação v2

Além dos CA da SPEC base:

- **CA-v2-1:** nenhum run não-terminal é descartado pela idade na fila de
  geração.
- **CA-v2-2:** drain strict nunca retorna sucesso com `Stopped=false` em algum
  runner elegível.
- **CA-v2-3:** estado parcial sobrevive a retry e é restaurável por exit.
- **CA-v2-4:** o wrapper root-owned contém explicitamente
  `maintenance enter --execute --strict --timeout=120`; o host só chama os
  verbos fixos e não contém `Stop-VM -Force` em linha de código.
- **CA-v2-5:** em `-EnforceQueue`, busca estática/teste prova que o caminho
  genérico não chama `Start-VM` para admissão antes da fila conceder.

## 8. Rollback trigger revisado

Além dos triggers da SPEC base, fazer rollback do artefato se:

- `enter --strict` retornar sucesso com qualquer unit ativa;
- retry strict perder o snapshot parcial;
- um run não-terminal sumir da coleta sem cancelamento do reaper;
- uma rota `-EnforceQueue` iniciar/reativar runner antes de `V:>=80`.

Rollback desliga enforce e restaura o artefato anterior somente após verificar
que nenhum job está ativo. Não restaura força de desligamento.
