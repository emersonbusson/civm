# Runbook — ligar a fila FIFO por-PR da box (canario primeiro)

> **SUPERSEDED em 2026-08-05:** produção usa o owner C# e contexto exato por
> SHA. Este documento permanece como referência do rollback PowerShell. O
> rollout vigente é
> [`GENERATION-CLEAN-BOUNDARY-ROLLOUT.md`](GENERATION-CLEAN-BOUNDARY-ROLLOUT.md).
> Não use gate que libera ao vencer timeout.

Cada geração executa todos os checks; a box limpa todo estado regenerável,
compacta até comprovar `V: >=80 GiB` e só então admite a próxima geração.

## O que ja esta no codigo (commitado, DESLIGADO)
- `civm-pr-queue.ps1` — cerebro puro `Resolve-PrSlot` (grant/hold/grace/boundary_advance).
- `civm-vm-orchestrator.ps1` — observe (VIVO, loga `would_*`) + enforce atras de `-EnforceQueue`
  (default OFF): publica `currentPr` em
  `C:\ProgramData\civm\gate\current-context` + limpa+compacta no boundary.
- `serialize.go` ignora apenas gates systemd do guest com sufixo `-gate`. O
  pool Windows `civm-<owner>-gate-<index>` não aparece nessa enumeração e é
  controlado pela allowlist exata do publisher C#.
- `civm-gate-runner-provision.ps1` — provisiona o runner Windows do HOST.

## O job-gate (adicionar nos workflows box-heavy — go.yml primeiro, no canario)
```yaml
  wait-for-slot:
    if: ${{ vars.CI_BACKEND != 'paid' }}      # no-op no pago
    runs-on: ${{ fromJSON(format('["self-hosted","Windows","civm-gate","civm-generation-{0}#{1}-{2}"]', github.repository, github.event_name == 'pull_request' && format('pr-{0}', github.event.pull_request.number) || 'branch', github.event.pull_request.head.sha || github.sha)) }}
    timeout-minutes: 720
    steps:
      - name: Wait for PR slot (FIFO box queue)
        shell: pwsh
        run: |
          $ctx = if ('${{ github.event.pull_request.number }}'.Trim()) { '${{ github.repository }}#pr-${{ github.event.pull_request.number }}@${{ github.event.pull_request.head.sha }}' } else { '${{ github.repository }}#branch@${{ github.sha }}' }
          $path = 'C:\ProgramData\civm\gate\current-context'; $deadline = (Get-Date).AddMinutes(710)
          while ($true) {
            $cur = ''; try { $cur = (Get-Content -LiteralPath $path -Raw -ErrorAction Stop).Trim() } catch {}
            if ($cur -eq $ctx) { Write-Host "slot: $ctx"; break }
            if ((Get-Date) -gt $deadline) { throw "timeout aguardando slot $ctx; admissao continua fechada" }
            Write-Host "fila: atual='$cur' eu='$ctx'"; Start-Sleep -Seconds 10
          }
```
Os jobs reais ganham `needs: [wait-for-slot, ...]`. Remover o `concurrency`
per-workflow (o gate substitui — acaba o cancel 1-pending). O contexto inclui
o SHA imutável; um push novo nunca herda a fronteira do push anterior.

## Passos do canario
1. **Provisionar o gate runner** (host, elevado):
   desabilitar primeiro a task `civm-host-orchestrator`. No PowerShell elevado,
   ler um token administrativo sem eco com `$adminToken = Read-Host
   'GitHub token' -AsSecureString`; ele fica apenas em memória. Depois executar
   `& C:\civm-deploy\civm-gate-runner-provision.ps1 -GitHubToken
   $adminToken -Url https://github.com/myorg -Index 1`. O provisionador cria o
   registration token via API, remove `civm-gate`,
   exige 3 segundos estáveis sem `busy`/Worker, protege staging/rollback e
   registra de novo somente com a label-base. Conferir: `gh api
   'orgs/myorg/actions/runners?per_page=100' --jq '.runners[] | select(.name ==
   "civm-gate-1") | {name,busy,labels}'`.
   **Persistencia (sobreviver reboot/crash):** NAO use o service do Windows
   (`config.cmd --runasservice` da `Win32 1068` nesta box mesmo sem dependencias
   declaradas — beco sem saida). Use o WATCHDOG via scheduled task:
   `& C:\civm-deploy\civm-gate-task-setup.ps1 -Index 1 -GitHubToken
   $adminToken` registra uma task
   com trigger `AtStartup` + tick de 2min
   `IgnoreNew`, como `NETWORK SERVICE/Limited`; a raiz e os binários recebem
   somente `ReadAndExecute`, `_work`/`_diag` recebem `Modify` e
   `C:\ProgramData\civm\gate\current-context` recebe somente `Read`, sempre
   sem ACEs herdadas. O provisionador usa runner 2.336.0 com SHA-256 pinado,
   diretório limpo side-by-side e `--disableupdate`; atualizar em menos de 30
   dias de cada release continua responsabilidade operacional. O setup para a
   instância anterior, prova acesso negado e confirma o owner do listener novo.
   A action executa `Runner.Listener.exe run` diretamente porque `run.cmd`
   tenta escrever `run-helper.cmd` na raiz imutável. O setup remove todas as
   labels customizadas e deixa o listener online, mas inelegível.
   Repita o `-Index` por runner do pool.
2. Implantar o publisher ainda `Disabled`, atualizar o peer para exigir a label
   de geração e somente então habilitar o owner C#; ele restaura `civm-gate` e
   a label exata juntas.
3. **PR throwaway A** com o gate so no `go.yml`. Abrir **PR throwaway B** logo depois.
   Medir no `civm-orchestrator.log`: `pr_boundary_compact done=pr-A next=pr-B`, e que o
   `go.yml` do B fica em `wait-for-slot` ate A terminar + a box compactar
   (`V: >=80 GiB`) -> aí B roda.
4. **Validar (Kahneman #13):** so os checks do `currentPr` rodam por vez; `disk_boundary_compact`
   1x por contexto; B comeca com `V: >=80 GiB`; zero cancelamento; PR sozinho sem espera extra.
5. **Rollar** o gate pros outros 6 workflows. Fechar os PRs throwaway.

## Rollback (se algo quebrar)
- Tirar o `-EnforceQueue` da task (volta pro observe-only; o codigo enforce fica dormente).
- O gate runner pode ficar (idle, label civm-gate, nao pega jobs `[self-hosted,civm]`).
- Reverter o `needs: wait-for-slot` nos workflows -> os jobs voltam a rodar sem o gate.

## Rollback trigger
Se um PR ficar preso em `wait-for-slot` >700min, o job-gate falha e a admissão
continua fechada. Investigar capability, reaper, owner e publicação; nunca
converter timeout em autorização para rodar sem a fronteira.
