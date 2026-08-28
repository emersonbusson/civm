# IMPL — fronteira limpa por geração exata

Issue: `emersonbusson/civm#226`  
PRD: [`PRD.md`](PRD.md)  
SPEC: [`SPECv2.md`](SPECv2.md)  
Estado: implementação local concluída; rollout/Tier III pendentes.

## Implementação

- A fila PowerShell usa contexto imutável `contexto@head_sha`, pagina toda a
  atividade não terminal e preserva FIFO pela primeira criação observada.
- Não existe avanço forçado por tempo, `push_wave_force_compact`, `skip_clean`
  ou `Stop-VM -Force` no fluxo automático.
- `maintenance enter --strict` só conclui depois de retirar labels, reprovar
  idle e parar todos os listeners; estado parcial é persistido para retry/exit.
- `civmctl capability generation-clean-boundary` expõe somente o marcador
  compatível `civm-generation-boundary/v1`.
- `/usr/local/bin/civm-generation-boundary` é instalado root-owned e aceita
  apenas `--check`, `prepare`, `resume` e `warn-clean`.
- `prepare` remove todo estado regenerável de runner, caches de linguagem,
  Docker, volumes e journal; falha se restar container; executa `fstrim` e
  solicita poweroff gracioso.
- `resume` restaura maintenance e prova idle; `warn-clean` limita a limpeza
  online ao builder cache e `fstrim`.
- O sudoers concede NOPASSWD somente aos wrappers fixos. `hook install` prova
  funcionalmente o marcador, e `doctor` reporta incompatibilidade como crítica.
- Todo comando SSH do rollback PowerShell recebe deadline remoto e `flock`; o
  fallback do reaper usa `pipefail` para não converter falha em sucesso.
- A admissão exige `V: >=80 GiB` depois da compactação e não tem bypass por
  contador.

## Rastreabilidade

| Requisito | Implementação principal |
| --- | --- |
| RF-1–RF-3 | `deploy/windows/civm-pr-queue.ps1`, coleta/persistência da fila |
| RF-4 | maintenance strict, capability v1, wrapper e probes do host |
| RF-5 | `deploy/bin/civm-generation-boundary` |
| RF-6 | decisão/orquestrador com piso rígido de 80 GiB |
| RF-7 | `systemctl poweroff --no-block` + poll de Off, sem force |
| RF-8 | publicação somente após resultado completo |
| RF-9 | `warn-clean` fixo; nenhuma compactação offline com worker |
| RF-10 | logs, doctor crítico e runbook de rollout |

## Validação local

Executado em 2026-08-05 no worktree isolado, sem acessar a VM:

- `bash -n deploy/bin/civm-generation-boundary`: exit 0;
- `go build ./...`: exit 0;
- `go test ./... -count=1`: todos os pacotes verdes;
- `go test -race -count=1 ./...`: todos os pacotes verdes;
- `go test -count=1 -cover ./internal/...`: todos verdes; manutenção
  91,8%, hook 90,9%, hostdisk 89,6% e doctor 85,4%;
- `go vet ./...`: exit 0;
- `govulncheck ./...`: nenhuma vulnerabilidade encontrada;
- `gitleaks dir . --redact --no-banner`: nenhum segredo encontrado;
- testes estruturais Go do PowerShell: verdes;
- `git diff --check`: exit 0.

`pwsh` não está instalado neste WSL. Os testes PowerShell/AST nativos não foram
executados localmente e permanecem gate obrigatório do CI Windows do PR.

## Validação operacional pendente

O rollout segue
[`runbooks/GENERATION-CLEAN-BOUNDARY-ROLLOUT.md`](../../../runbooks/GENERATION-CLEAN-BOUNDARY-ROLLOUT.md).
Nenhuma mutação de produção ou Tier III foi executada por esta implementação
local. O PR Acme só pode ser atualizado depois da fase guest e da fase host.

## Rollback trigger

Reverter se um worker for interrompido, se contexto for publicado abaixo de
80 GiB, se o marcador v1 não for comprovado ou se três fronteiras ociosas
falharem sem outcome classificável.
