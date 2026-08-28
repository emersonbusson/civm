# SPEC — recuperação de runner ocioso com fila pendente

> Issue: `emersonbusson/civm#180`. SSDV3 passos 2 e 2.5.

## Assinatura do incidente

Uma sessão é candidata à cura somente quando, durante toda a janela:

- existe ao menos 1 job fresco da fleet `CIVM_REAPER_REPOS`, e o endpoint de
  jobs do run confirma labels `self-hosted` e `civm`;
- a API do GitHub informa o runner `online` e `busy=false`;
- o listener local está ativo;
- `idle.Check` não encontra `Runner.Worker`, `Runner.PluginHost`, atividade em
  `_work` nem Docker ativo;
- o serviço não reiniciou dentro do cooldown;
- a leitura de qualquer sinal não retornou erro ou estado desconhecido.

Um timestamp isolado não basta. A primeira observação persiste a assinatura e
não muta. O tick posterior só atua se a mesma assinatura excedeu a janela.

A listagem de runs e jobs percorre todas as páginas da API. Para limitar o
orçamento de requests, os runs frescos são ordenados do mais antigo para o mais
novo e a busca de jobs para no primeiro run elegível de cada repo. A assinatura
global usa o mais antigo entre esses candidatos; runs mais novos não precisam
ser consultados depois que o candidato do repo foi provado.

## Ação e limites

1. revalidar todos os sinais imediatamente antes de atuar;
2. adquirir o lock operacional do runner;
3. reiniciar somente o serviço resolvido;
4. persistir assinatura, tentativa e timestamp por temp+rename atômico antes
   da mutação;
5. observar efeito em ticks posteriores.

O limite é exatamente `1` restart por incidente. Sem recuperação, o watchdog
registra `unresolved`, alerta e não tenta novamente, mesmo após 1 hora. Só
rearma depois de observar condição saudável: Worker/API busy, avanço
comprovado da fila ou fila vazia por conclusão/cancelamento confirmado.

## Observabilidade

Cada decisão registra campos estruturados: serviço, repo, idade da assinatura,
queued elegível, estado API, listener, Worker, tentativa, cooldown e motivo.
Nenhum token, URL autenticada ou ambiente do processo é logado.

O journal distingue `armed`, `observed`, `restarted`, `recovered` e
`unresolved`. `capacity` bloqueia para marker ativo ou inválido; `health` e,
por composição, `doctor`, verificam o resultado do service. O `doctor` não
reinterpreta o marker em uma segunda máquina de estados.

## Testes TDD

- sem fila: não reinicia;
- runner API offline: não reinicia;
- API busy: não reinicia;
- Worker local: não reinicia;
- listener ausente: recuperação normal de serviço, não esta assinatura;
- sinal desconhecido: falha fechado;
- abaixo da janela: persiste suspeita e não reinicia;
- acima da janela: reinicia 1 vez;
- retry: assinatura persiste e respeita cooldown;
- exaustão: 1 tentativa impede qualquer repetição do mesmo incidente;
- idempotência 2x: dois ticks iguais dentro do cooldown não reiniciam de novo;
- efeito: Worker aparece ou fila some e limpa a suspeita;
- concorrência: lock ocupado não reinicia.

## Passo 2.5 — auditoria adversarial

- **Fila não destinada ao runner:** contagem global causaria restart falso.
  Mitigação: fleet `CIVM_REAPER_REPOS` + endpoint de jobs + labels elegíveis.
- **Exaustão da API:** consultar jobs de 100 runs em todo tick consumiria até
  `3.000` requests/h por repo com intervalo de `2 min`. Mitigação: paginação da
  lista, ordenação oldest-first e parada no primeiro run elegível de cada repo.
- **API stale:** `online/busy=false` pode estar atrasado. Mitigação: janela,
  sinais locais e revalidação imediatamente antes da ação.
- **Job em preparação sem Worker:** restart poderia disputar o broker.
  Mitigação: janela maior que o cold-start medido e cooldown.
- **Loop de cura:** restart pode registrar `Listening` sem recuperar consumo.
  Mitigação: exatamente `1` restart por incidente, assinatura persistida e
  estado `unresolved`.
- **Reboot limpa memória:** estado precisa ser persistido atomically; parse
  inválido falha fechado.
- **Responsabilidade:** watchdog só gerencia processo do runner; nunca chama
  APIs de power-state ou VHDX.

**Veredito:** GO para implementação TDD usando a fleet já configurada e o
endpoint de jobs, após calibrar o dwell com ao menos `5` boots/reconexões reais.
Se os dados não estiverem disponíveis sem ampliar permissões, NO-GO para
restart automático.

## Rollback trigger

Reverter se qualquer caso negativo reiniciar serviço, se o mesmo incidente
permitir uma segunda tentativa ou se estado inválido degradar para ação.
