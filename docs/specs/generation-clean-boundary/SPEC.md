# SPEC — fronteira limpa por geração exata

Issue: `emersonbusson/civm#226`  
PRD: [`PRD.md`](PRD.md)  
Status: implementado localmente — rollout e Tier III pendentes  
Escopo: host Windows, fila de geração, contrato do gate consumidor e
documentação operacional.

## 0. Decisão e invariantes

Esta SPEC implementa os RF-1 a RF-10 do PRD sem compatibilidade dual-path:

1. A unidade de fila é a geração exata `pr-N@SHA` / `branch-ref@SHA`.
2. Antes de publicar qualquer geração, o host precisa completar a fronteira
   limpa e observar `V: >= 80 GiB`.
3. Qualquer observação incompleta ou atividade no guest é **defer**, nunca
   compactação forçada.
4. Nenhum fluxo automático deste orquestrador usa `Stop-VM -Force` nem
   compactação offline enquanto houver trabalho ativo ou desconhecido.
5. O reaper canônico continua cuidando de SHA supersedido/PR fechado; a fila não
   cancela trabalho para encurtar uma fronteira.

Passos críticos seguem
[`disciplines/KAHNEMAN-DISCIPLINES.md`](../../../disciplines/KAHNEMAN-DISCIPLINES.md):

| Passo | Disciplina | Evidência mínima | Abort / rollback |
| --- | --- | --- | --- |
| atividade GitHub | #13 existência ≠ função, #15 fail-safe | todas as páginas/status verificadas | erro/parsing ausente → defer |
| idle/drain | #5 worst-case, #15 | `idle-check=0`, enter e segundo idle=0 | busy/unknown → defer |
| compactação | #2 counterfactual, #3 número | VM Off, lock, `V:>=80` pós-Optimize | não atingir 80 → bloquear |
| publicação | #14 retries calibrados, #16 idempotência | todas as fases anteriores bem-sucedidas | manter contexto anterior |

## 1. Arquivos e mudanças

| Arquivo | Mudança | RF |
| --- | --- | --- |
| `deploy/windows/civm-pr-queue.ps1` | contexto SHA, grace 10 min; remover push-wave | RF-1, RF-3 |
| `deploy/windows/civm-pr-queue.test.ps1` | tabelas RED/GREEN para geração exata e grace | RF-1, RF-3 |
| `deploy/windows/civm-vm-orchestrator.ps1` | atividade verificada/paginada, idle canônico, drain/clean/shutdown/compact/restore e publicação pós-condição | RF-2, RF-4–RF-10 |
| `deploy/windows/civm-orchestrator-decision.ps1` | piso 80 rígido; eliminar bypass e panic destrutivo | RF-6, RF-9 |
| `deploy/windows/civm-orchestrator-decision.test.ps1` | tabela do piso 80 e sem compactação ativa | RF-6, RF-9 |
| `deploy/bin/civm-generation-boundary` | protocolo root-owned fixo: capability, prepare, resume e warn-clean | RF-4, RF-5, RF-7 |
| `deploy/sudoers.d/civm-cleanup` | NOPASSWD apenas para wrappers validados | RF-4, RF-5 |
| `cmd/civmctl/capability.go` | marcador compatível `civm-generation-boundary/v1` | RF-4 |
| `internal/hook`, `internal/doctor` | instala/prova wrapper e expõe falha crítica | RF-4, RF-10 |
| `internal/hostdisk/generation_boundary_test.go` | regressões estáticas executáveis em Go | RF-4–RF-9 |
| `runbooks/PR-QUEUE-ENABLE.md` | gate dinâmico, contexto SHA, 80 GiB, defer | RF-1, RF-6, RF-8 |
| `PAID-CI-PARITY.md`, `CI-PARITY-CHECKLIST.md`, `runbooks/MULTI-PROJECT-RUNNER.md` | contrato de fronteira limpa | RF-5, RF-6, RF-10 |
| `validation.md` | somente resultado empírico posterior ao deploy | RF-10 |

Não serão criados secrets, Scheduled Tasks ou arquivos de estado. A única nova
superfície CLI é a capability read-only, sem argumentos livres. São reutilizados
`civmctl idle-check`, `maintenance`, `cleanup`, `reap-runs`,
`V:\civm-reclaim.lock` e `V:\civm-pr-queue.json` atrás do wrapper fixo.

## 2. Fila de geração — `civm-pr-queue.ps1`

### 2.1 Contexto puro

Adicionar função pura `Get-RunGenerationContext`:

```powershell
function Get-RunGenerationContext {
    param([Parameter(Mandatory)]$Run)
    # retorna '' se faltar head_sha ou identidade PR/branch válida
    # PR:     "pr-$number@$headSha"
    # branch: "branch-$headBranch@$headSha"
}
```

- Usa o primeiro `pull_requests[].number` quando houver; caso contrário usa
  `head_branch` não vazio.
- Não normaliza o branch; compara SHA sem case sensitivity apenas onde necessário.
- `''` é erro de observação na camada I/O, nunca contexto válido.

### 2.2 Estado

`Ensure-PrQueueState` preserva `contexts`, `currentPr` e
`currentIdleSinceUtc`. Campos históricos de push-wave podem permanecer no JSON
existente, mas não são lidos/escritos nem têm comportamento compatível.

`Resolve-PrSlot` mantém sua assinatura, com `DoneGraceMinutes = 10` por default.
Ele continua puro e só retorna `grant`, `hold`, `boundary_advance` ou `idle`.
O caller só torna `grant`/`boundary_advance` visível após a preparação de geração.

### 2.3 Remoções obrigatórias

Remover `Resolve-PushWaveCompact`, `lastCompactHeadSha`,
`lastCompactContext` e todo teste/uso correspondente. O SHA é agora parte do ID,
logo não há mutação “dentro” do contexto e não pode haver `skip_clean` ou
`push_wave_force_compact`.

### 2.4 Testes RED antes do código

Em `civm-pr-queue.test.ps1`, acrescentar antes da implementação os casos:

1. `pr-1@aaa` e `pr-1@bbb` recebem contextos distintos.
2. SHA novo de mesmo PR permanece atrás do SHA atual.
3. grace de 9m59s mantém; exatamente 10m avança.
4. reaparecimento de jobs da geração atual zera grace.
5. não existe função/ação de push-wave no módulo carregado.

## 3. Observação de atividade — `civm-vm-orchestrator.ps1`

### 3.1 Contrato de retorno

Substituir a hashtable ambígua de `Get-PrActivity` por:

```powershell
[pscustomobject]@{
    verified = $true
    counts   = @{ 'pr-42@abc' = 3 }
    errors   = @()
}
```

`verified=false` é obrigatório quando ocorrer qualquer uma destas condições:

- `$Repos` vazio em modo `-EnforceQueue`;
- token ausente ou ilegível;
- HTTP/timeout em qualquer repo/status/página;
- JSON/run sem status esperado, timestamp parseável, SHA ou identidade válida;
- paginação inconsistente.

O caller registra `pr_activity_unverified`, mantém `pq.currentPr`, não publica
e não chama manutenção destrutiva. Isso implementa RF-2/#15.

### 3.2 Paginação

Para cada repo e cada status `queued`/`in_progress`, buscar páginas de 100 até
receber menos de 100 runs. Cada página deve ter `workflow_runs`; falha de rede
ou parse torna a coleta inteira não verificável. Depois de validar `run.status`,
não aplicar filtro de idade na fila: um status não terminal antigo protege o
slot até o reaper canônico comprovar que o PR fechou ou o SHA foi supersedido.

Ao adicionar contexto novo, `contexts` preserva os existentes ainda ativos e
anexa novos IDs em ordem de primeira observação. Nunca ordenar IDs para decidir
FIFO de novo; a ordem persistida é a ordem de chegada.

## 4. Predicado canônico de idle e drain

### 4.1 `Get-GuestHasActiveJob`

Substituir o `pgrep` por um comando remoto que preserve a conectividade SSH e
retorne o código de `civmctl idle-check` como stdout:

```sh
civmctl idle-check >/dev/null 2>&1; status=$?; printf '%s\n' "$status"; exit 0
```

Mapeamento:

| SSH / stdout | Resultado |
| --- | --- |
| SSH 0 e `0` | idle (`$false`) |
| SSH 0 e `1`/`2`/outro | busy (`$true`) |
| SSH não comprovado | busy (`$true`) |
| VM Off | não há processo guest; o preparo a inicia antes de exigir idle |

O log diferencia `guest_idle_busy`, `guest_idle_unknown` e
`guest_active_probe_failed`. Essa função é o único guard usado por parar,
limpar ou avançar geração.

### 4.2 Capability e protocolo privilegiado

O host aceita somente o marcador exato retornado por:

```text
sudo -n /usr/local/bin/civm-generation-boundary --check
```

O único `NOPASSWD` novo aponta para esse wrapper root-owned. Não há permissão
genérica para `civmctl`, `systemctl`, Docker, `fstrim` ou shell. `prepare`
executa maintenance strict, idle-check, cleanup, wipe completo de estado
regenerável, Docker prune, verificação de zero containers, fstrim e poweroff.
`resume` executa maintenance exit e idle-check. Falha mantém o gate fechado.

## 5. Preparação de geração

### 5.1 Interface interna

Adicionar:

```powershell
function Invoke-PrepareGeneration {
    param([Parameter(Mandatory)][string]$ContextId)
    # retorna @{ succeeded = [bool]; reason = <string>; vFreeGB = <int> }
}
```

Fluxo exato (RF-4 a RF-7):

1. Se a VM está Off, `Start-VM`, aguarda VM/SSH. O contexto ainda não é
   publicado; gates no host continuam bloqueando jobs reais.
2. `Get-GuestHasActiveJob` deve provar idle; caso contrário retorna defer.
3. O probe `--check` deve devolver exatamente
   `civm-generation-boundary/v1`.
4. O wrapper `prepare` deve retornar sucesso. Ele próprio executa drain strict,
   idle duplo, limpeza completa e solicita poweroff gracioso.
5. Esperar `Get-VM.State == Off` até o prazo existente. Não chamar
   `Stop-VM -Force` em falha.
6. `Invoke-CompactStoppedVm` adquire o lock canônico, valida slack, monta
   read-only, executa `Optimize-VHD -Mode Full`, desmonta e mede `V:`.
7. Se `V:<80` ou a medida é inválida, logar
   `generation_capacity_blocked` e retornar falha sem publicar.
8. `Start-VM`, aguardar SSH, comprovar a capability novamente, chamar o wrapper
   `resume` e provar listener pronto. Só então retornar sucesso.

Todos os `return` após `prepare` preservam o estado drenado. Não tentar “curar”
com force ou remover o arquivo de maintenance fora de `maintenance exit`.

### 5.2 Clean e compactação

O PowerShell não monta payload root arbitrário. `Invoke-GuestBoundaryPrepare`
faz apenas o probe v1 e invoca
`sudo -n /usr/local/bin/civm-generation-boundary prepare`. O wrapper versionado
é a fonte única da limpeza; qualquer container remanescente torna a fase falha.

Extrair de `Invoke-StopAndCompact` a parte exclusivamente offline para
`Invoke-CompactStoppedVm`. Ela exige VM Off, mantém o lock, `Test-OptimizeSlack`,
mount retry e `Dismount-VHD` no `finally`. Seu retorno sempre contém
`succeeded`, `vFreeGB` e `reason`. `vFreeGB >= 80` é condição de sucesso, não
apenas evento de warning.

O antigo `Invoke-StopAndCompact` pode ser removido quando todos os callers forem
direcionados para esse fluxo seguro. Nenhuma função automática pode conter
`Stop-VM -Force`.

### 5.3 Publicação e retry

Adicionar `Publish-CurrentContext` que escreve somente depois de retorno bem
sucedido de `Invoke-PrepareGeneration`. O conteúdo é uma geração exata, ASCII,
sem newline. Erro de escrita mantém o contexto anterior e é logado como
`pr_publish_error`.

Em `Invoke-PrQueuePushWave` (nome pode ser renomeado para
`Invoke-PrGenerationQueue`):

1. Se atividade não é verificável, retorna sem alterar estado/publicação.
2. `grant`: prepara o target; em êxito publica e persiste `currentPr`.
3. `boundary_advance`: mantém o contexto anterior até preparar o target; em
   falha converte efetivamente em `hold`, preservando `currentIdleSinceUtc`.
4. boundary sem próximo contexto ainda limpa/compacta após término, publica
   vazio somente se a manutenção concluir; pode manter a VM Off e drenada.
5. `hold`: apenas mantém/república o contexto já concedido; nunca chama clean.

O nome público da função pode permanecer temporariamente para não alterar a
Scheduled Task, mas comentários, logs e testes devem usar “generation”.

## 6. Decisão geral de disco

Em `civm-orchestrator-decision.ps1`:

- `AdmitFloorGB` default passa a 80.
- Remover `AdmitReclaimAttempts`, `Update-AdmitAttempts` e
  `Resolve-AdmitTransition`; não existe bypass de capacidade.
- VM Off com trabalho e `V:<80`/medida ausente retorna ação de bloqueio, não
  `start`. Em modo `-EnforceQueue`, a preparação de geração é a única que pode
  iniciar VM para admissão.
- Sob trabalho ativo, `V:<PanicFloorGB` devolve somente limpeza online segura
  (`warn_clean`); `panic_compact` é removido.
- `stop_and_compact` só pode ser usado se idle canônico for provado; ele deve
  delegar à mesma preparação/drain ou sair em defer. Não pode reintroduzir um
  caminho com force.

Os testes da decision table cobrem bordas 79/80, medida 0, VM Off, fila quente e
trabalho ativo. Não há caso que aceite abaixo de 80.

## 7. Contrato do gate consumidor

No Acme (PR separado, dependente deste), cada `wait-for-slot` deve usar o
runner Windows, a label-base e uma label de geração pré-publicada:

```yaml
runs-on:
  - self-hosted
  - Windows
  - civm-gate
  - civm-generation-${{ github.repository }}#pr-${{ github.event.pull_request.number }}-${{ github.event.pull_request.head.sha }}
timeout-minutes: 720
```

e formar:

```powershell
$ctx = if ('${{ github.event.pull_request.number }}'.Trim()) {
  '${{ github.repository }}#pr-${{ github.event.pull_request.number }}@${{ github.event.pull_request.head.sha }}'
} else {
  '${{ github.repository }}#branch@${{ github.sha }}'
}
$deadline = (Get-Date).AddMinutes(710)
```

O publisher adiciona a label exata somente ao cohort allowlisted e remove labels
de geração anteriores antes do job. O workflow nunca faz fallback para uma
label estática ao vencer timeout; ausência da label mantém a admissão fechada.

## 8. Testes de implementação

### 8.1 PowerShell

- `civm-pr-queue.test.ps1`: §2.4 + teste de state sem campos push-wave.
- `civm-orchestrator-decision.test.ps1`: 80 rígido, sem bypass/panic force.
- novo teste de orquestração por inspeção/fixtures, se possível no host: não
  publicar depois de falha de API, busy, clean, shutdown, compactação ou 79 GiB.

### 8.2 Go estático

`internal/hostdisk/generation_boundary_test.go` deve falhar se o script de
orquestração contiver linha de código (comentários ignorados):

- `Stop-VM -Force`;
- `push_wave_force_compact`;
- `skip_clean`;
- bypass de `AdmitReclaimAttempts`.

E deve exigir os tokens de segurança:

- `civmctl idle-check`;
- `sudo -n /usr/local/bin/civm-generation-boundary --check`;
- `sudo -n /usr/local/bin/civm-generation-boundary prepare`;
- `sudo -n /usr/local/bin/civm-generation-boundary resume`;
- `flock -n` e deadline remoto com TERM/KILL;
- piso `80` na decisão/orquestrador.

### 8.3 Validação empírica pós-deploy

1. Deployar primeiro o binário/assets guest, instalar o wrapper com
   `hook install --no-restart` em janela ociosa e provar capability/doctor.
2. Só depois deployar o host e confirmar owner único, sem checkout manual na
   guest.
3. Abrir/atualizar o único PR Acme; observar label dinâmica e contexto exato.
4. Verificar log com `generation_boundary_*`, `V:>=80` e nenhuma ocorrência
   `Stop-VM -Force`/cancelamento iniciado pelo host.
5. Rodar a matriz black-box limpa e com carga/seeds no próprio PR. Registrar
   valores reais, duração e conclusões em `validation.md`.

## 9. Red-team obrigatório (Passo 2.5)

Antes do código, confrontar esta SPEC com estes contraexemplos:

1. API retorna uma página e falha na segunda — pode avançar? Resposta exigida:
   não; `verified=false`.
2. `Runner.Worker` saiu, mas Docker build ainda está ativo — pode limpar? Não;
   `idle-check` o classifica busy.
3. Job chega no intervalo idle→shutdown — pode ser perdido? Não; maintenance
   enter remove listeners/label antes da segunda prova.
4. `shutdown` falha — pode usar `Stop-VM -Force`? Não; manter drenado e defer.
5. Optimize termina com 79 GiB — pode liberar? Não; manter bloqueado.
6. SHA novo chega enquanto SHA anterior ainda roda — pode compactar? Não; é
   contexto separado e espera término/reaper canônico.
7. Gate pede label que nenhum runner tem — pode iniciar? Não; o publisher
   seleciona o cohort pela allowlist de nomes e só então atribui, em conjunto,
   `civm-gate` e a label dinâmica exata da geração.

Qualquer resposta diferente exige `SPECv2.md` antes da implementação.

## 10. Rollout e rollback

1. Validar código e testes em PR Civm.
2. Fazer merge/release somente com checks verdes.
3. Guest primeiro: civmctl compatível, assets root-owned, sudoers validado,
   capability v1, reaper timer e doctor verdes.
4. Host depois: gates online com zero labels customizadas e owner ainda
   desabilitado; validar o cohort completo pela allowlist exata.
5. Deployar o owner desabilitado e atualizar o PR único do Acme para exigir a
   label dinâmica, mantendo os jobs inelegíveis.
6. Habilitar o owner único; ele restaura a label-base e a label dinâmica da
   geração somente no cohort allowlisted.
7. Só mergear Acme após o black-box exato do head verde, duas fronteiras e a
   auditoria de zero registro, label ou run órfão.

Rollback é obrigatório se ocorrer um dos sinais:

- um job sofre interrupção atribuída ao host;
- publicação acontece com `v_free_gb <80`;
- um gate fica sem publicação por erro lógico reproduzível;
- a preparação deixa a VM Off sem log claro e sem retry seguro.

O rollback desabilita `-EnforceQueue` na task e restaura a última versão
implantada do orquestrador. Nunca restaura o caminho force.
