# PRD — recuperação de runner ocioso com fila pendente

> Issue: `emersonbusson/civm#180`. SSDV3 passo 1.

## Problema

Após reboot em 30/07/2026, o listener registrou conflito de sessão e depois
`Listening for Jobs`, porém não consumiu a fila. A API indicava runner
`online`, `busy=false`, não existia `Runner.Worker` local e os jobs ficaram
`16` jobs ficaram pendentes por aproximadamente `20 min`. Um restart
supervisionado do runner
afetado fez o primeiro job ser aceito em aproximadamente `4 s`.

O estado parece saudável para verificações isoladas, mas o efeito — consumir
fila elegível — não ocorre. Hoje não há cura automática limitada para essa
assinatura.

## Resultado esperado

- Detectar somente fila real/fresca destinada ao runner.
- Exigir API `online` e `busy=false`, ausência local de Worker e listener
  presente durante uma janela nomeada.
- Exigir o idle-check completo: Worker, PluginHost, `_work` e Docker.
- Reiniciar apenas o serviço afetado, exatamente uma vez por incidente.
- Falhar fechado se fila, API, processo ou ownership forem inconclusivos.
- Limitar tentativas e expor cooldown/assinatura no journal e `doctor`.
- Não reiniciar runner saudável, runner ocupado ou box sem fila.

## Fora de escopo

- Ligar, desligar ou compactar a VM.
- Recuperar a sessão antes de existir fila elegível.
- Cancelar runs; isso pertence ao reaper.
- Alterar o protocolo de publicação do `civm-host`.

## Rollback trigger

Reverter se 1 Worker ativo for interrompido, se 1 runner sem fila for
reiniciado ou se o mesmo incidente produzir mais de 1 restart.
