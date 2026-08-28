# Runbook — rollout da fronteira limpa por geração

## Objetivo

Implantar o contrato que impede uma geração nova de CI de começar antes de:

1. concluir todos os checks da geração atual;
2. drenar todos os listeners sem `--force`;
3. remover workspace, actions, tool cache, caches de linguagem e Docker;
4. desligar a guest de modo gracioso;
5. compactar o VHDX offline;
6. comprovar `V:` com pelo menos **80 GiB livres**;
7. restaurar os listeners e provar o broker antes da publicação.

O rollout de artefatos é **guest primeiro, host depois**, mas a admissão só é
aberta no fim do cutover coordenado entre `civm`, `civm-host` e o peer. O
marcador compatível é `civm-generation-boundary/v1`.

## O que este runbook não autoriza

- Não clonar repositório manualmente na guest.
- Não rodar `prepare` como teste: esse verbo limpa estado e desliga a VM.
- Não ativar dois owners de power-state/compactação.
- Não mudar a RAM dinâmica de 7–12 GiB.
- Não restaurar labels customizadas dos gates antes de o peer exigir a label
  dinâmica de geração.
- Não fazer cutover, rollback ou Tier III sem janela supervisionada.

## Pré-condições

- PR do `civm` mergeado e release com todos os checks verdes.
- Artefatos daquela release já staged em `/opt/civm` pelo canal de deploy
  existente; isso não é autorização para criar um checkout manual na guest.
- `civmctl idle-check` retorna 0 e não há `Runner.Worker`, `_work` ativo,
  Docker build/Compose/buildctl ativo ou boundary anterior em curso.
- `CIVM_REAPER_REPOS` está configurado e o timer do reaper está ativo.
- Existe cópia recuperável do binário/configuração implantados antes da janela.

## Fase A — guest compatível

Na guest, somente pela superfície operacional já provisionada:

```bash
civmctl idle-check
sudo civmctl self-upgrade --execute
civmctl capability generation-clean-boundary
sudo civmctl hook install --execute --no-restart \
  --deploy-source=/opt/civm/deploy
sudo -n /usr/local/bin/civm-generation-boundary --check
civmctl doctor --repos=auto --json
systemctl is-enabled civmctl-run-reaper.timer
systemctl is-active civmctl-run-reaper.timer
journalctl -u civmctl-run-reaper.service -n 20 --no-pager
civmctl idle-check
```

Condições obrigatórias antes de seguir:

- os dois probes imprimem exatamente `civm-generation-boundary/v1`;
- `doctor` não contém crítico `GENERATION_BOUNDARY_CAPABILITY`;
- reaper timer está `enabled` e `active`, com execução recente classificável;
- a instalação não reiniciou listeners (`--no-restart`);
- o host continua com o gate da próxima geração fechado.

Se qualquer item falhar, parar aqui. Não deployar o controlador novo.

## Fase B — gates e controlador do host

Somente em PowerShell elevado e com janela supervisionada:

1. esperar toda run atual ficar terminal; confirmar `Runner.Worker=0`, gates
   `busy=false`, lock de reclaim livre e fila sem geração em transição;
2. desabilitar o publisher/owner antigo e provar que seu processo terminou;
3. validar build/test do `civm-host` que usa os comandos fixos `--check`,
   `prepare` e `resume`;
4. provisionar e configurar os quatro gates, um por vez. Cada gate deve
   terminar online, sob `NETWORK SERVICE`, com **zero labels customizadas** e
   zero Worker; o publisher continua desabilitado;
5. auditar globalmente que todos e somente os nomes da allowlist existem, são
   Windows, têm IDs únicos e estão online/idle, sem labels residuais;
6. deployar o owner C# ainda `Disabled`, configurado com
   `CIVM_GENERATION_GATE_RUNNERS` exato e
   `C:\ProgramData\civm\gate\current-context`;
7. atualizar o peer para exigir `civm-gate` **e** a label dinâmica da geração.
   Os jobs permanecem inelegíveis, com `runner_id=0`, porque os gates ainda têm
   zero labels customizadas;
8. habilitar somente o owner C#. Ele descobre o cohort pelos nomes da allowlist,
   mesmo sem labels customizadas, e restaura atomicamente a label-base e a label
   dinâmica exata;
9. confirmar owner único, heartbeat, `LastTaskResult=0` e contexto ainda vazio
   antes do canário.

O PowerShell deste repositório permanece `Disabled` como rollback legado. Ele
usa o mesmo wrapper, o mesmo piso de 80 GiB e nunca chama `Stop-VM -Force`, mas
não publica labels dinâmicas. Só pode ser ativado depois de desabilitar o owner
C#, remover todas as labels customizadas dos gates e reverter o peer, de forma
coordenada, para o contrato estático anterior.

## Canário e Tier III

O canário é um PR real, pois é assim que o CI pago e a box fazem checkout:

1. geração A executa todos os checks;
2. geração B fica inelegível até o publisher atribuir ao cohort a label
   dinâmica exata de B;
3. o job gate de B inicia, mas espera o contexto ainda vazio;
4. logs mostram `prepare` → VM `Off` → compactação → `V: >=80` → boot →
   `resume` → broker-ready;
5. somente então o contexto exato de B é publicado;
6. após B, repetir com novo push no mesmo PR para provar isolamento por SHA;
7. executar black-box, múltiplos seeds e carga no PR consumidor;
8. observar duas gerações por pelo menos 15 minutos, sem rerun manual, e provar
   zero listener SYSTEM, zero label residual e zero run/registro órfão.

Registrar em `validation.md` somente os valores medidos. Teste local não é
evidência de Tier III.

## Runs órfãs

O host não cancela workflow run. A cura canônica continua sendo
`civmctl reap-runs` a cada 5 minutos:

- `pr-not-open`: PR fechado;
- `superseded-sha`: SHA antigo de PR ainda aberto.

Um status não terminal antigo permanece protegendo a fila até o reaper provar
uma dessas duas condições. Falha do reaper aparece no journal/health e nunca é
mascarada pelo fallback do host.

## Rollback trigger

Fazer rollback supervisionado se ocorrer qualquer um destes sinais:

- 1 worker for interrompido pelo host;
- 1 geração for publicada com `V:` menor que 80 GiB ou medida desconhecida;
- capability diferente de `civm-generation-boundary/v1`;
- owner zero/dual ou `processBlockedReason` preenchido;
- 3 fronteiras ociosas consecutivas falharem sem causa classificável.

O rollback primeiro desabilita o publisher e remove todas as labels
customizadas do cohort. Ele restaura o artefato anterior e mantém a admissão
fechada até reprovar idle, reaper e owner único. O peer só volta ao contrato
estático na mesma janela que ativa o rollback legado. Nunca restaura
desligamento forçado.
