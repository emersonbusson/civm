---
slug: civm-runner-reliability
title: Confiabilidade do runner civm — equivalente gratuito e auto-curável do GitHub Actions para todos os repos
milestone: —
issues: []
---

# PRD — Confiabilidade do runner civm: equivalente gratuito e auto-curável do GitHub Actions para todos os repos

> Extensão de 30/07/2026: o estado `active/running + online,busy=false` com
> fila elegível não era coberto pelo RF-6. O contrato complementar está em
> `../stalled-runner-recovery/{PRD,SPEC,IMPL}.md` (issue #180).

> Tipo: capacidade de plataforma do runner (`civmctl` guest + componente Windows host + sudoers/tmpfiles deploy + runbook). Sem schema de banco, endpoint de produto ou evento de domínio.
> Política Day-0: o civm não tem produção viva com dados legados obrigatórios; backfill = N/A. Solução primária única, sem dual-path "disciplina-OU-primitivo".
> Origem: o box civm (host Windows Hyper-V `EMEDEV` + guest `gha-ubuntu-2404`) é o **único** runner self-hosted gratuito que substitui os GitHub-hosted runners para **todos** os repos do usuário (`acme/app`, `acme/civm`, `other/{peer,service-b,service-c,service-d,service-a}` — 8 serviços: `civm-app`, `civm-acme-org`, `civm-service-a`, `civm-service-b`, `civm-self`, `civm-service-c`, `civm-service-d`, `civm-peer`). Ele **precisa** se comportar como "GitHub-hosted Actions" porque o usuário não pode pagar runners hospedados. O diagnóstico de 2026-05-29 (verificado contra código-fonte E box vivo) encontrou modos de falha que tiram runners do ar em silêncio e os deixam quebrados para **todos** os jobs subsequentes. Este PRD os substitui por primitivos civm corretos, seguros e auto-curáveis.

---

## 1. Resumo

O civm precisa ser um **GitHub Actions gratuito que não falha em silêncio**. O diagnóstico encontrou três famílias de falha que quebram essa promessa, todas verificadas contra o código E o box vivo (logs em `/var/log/civm/hooks.jsonl`, `systemctl`, `powershell.exe`):

1. **Lixo root-owned no `_work` derruba o hook `job-completed` (EACCES) e quebra o runner inteiro — o killer de confiabilidade #1, repo-agnóstico.** Confirmado vivo: `hooks.jsonl` tem **34 eventos fatais**, todos em `acme/app`, com a mensagem predita: `work_root: unlinkat /home/emdev/actions-runner-acme/_work/acme/app/docs/events-catalog.json: permission denied`. Um passo CI containerizado roda como root e escreve `docs/events-catalog.json` / `config-reference.json` no `_work` montado; o runner (usuário `emdev`) então **não consegue deletar** → `os.RemoveAll` retorna EACCES → hook `job-completed` retorna exit 1 → o passo "Complete runner" do GitHub falha → o runner para de processar jobs (janela observada de ~11 min com retries). Não há caminho de escalada de privilégio no código (`grep` por `chown`/`Chown`/`Geteuid` no cleanup retorna nada; os únicos `sudo` no hook são `apt`/`journalctl`/`fstrim`, todos best-effort). Day-0, qualquer repo com passo containerizado que escreve como root está a um job de brickar seu runner.

2. **O disco do host (VHDX) enche sem alarme nem auto-cura — o pipeline de reclamação automático não existe na prática.** Confirmado vivo: o guest `/var/lib/civm/host-metrics.json` está **ausente** (`civmctl host-disk` → `level=crit stale=true v_free=0GB`); as **três** Scheduled Tasks do host (`civm-host-metrics`, `civm-vhdx-optimize`, `civm-vhdx-optimize-watchdog`) **não estão registradas** (`Get-ScheduledTask` = NOT REGISTERED). Os scripts existem em `deploy/windows/` mas nunca rodaram `Register-ScheduledTask`. Além disso, o mecanismo descrito como **primário** na SPEC `host-volume-reclamation` (online SCSI+discard → `fstrim` encolhe o VHDX) é **empiricamente falso neste box**: BlockSize de 32 MB + Hyper-V **não honra UNMAP/discard online**, então `fstrim`+`Optimize-VHD` recuperaram ~nada (≈4 GB mesmo com zero-fill). A única remediação que funcionou foi **`Convert-VHD -BlockSizeBytes 1MB`** (V: 3 GB → 51 GB livres) + **`Optimize-VHD` OFFLINE** num VHDX de bloco 1 MB — e esse mecanismo está **ausente de todos os scripts** (`grep` por `BlockSizeBytes`/`1MB`/`1048576` retorna zero). Combinado com o gap buildx-125 (build-cache nunca reclamado), o disco do guest **não tem nenhuma reclamação automática funcionando**.

3. **Robustez multi-repo degradada: falhas não-fatais viram fatais, ruído mascara sinal, falta auto-recuperação.** O hook `job-completed` é **all-or-nothing**: qualquer erro de step de filesystem (não só EACCES) falha o runner, e o único demote de raça ignorável só cobre `cache`/`cache_trim` com "directory not empty" e só está cabeado no branch `job-started`. `docker buildx prune` emite `exit status 125` em **essencialmente todo job** (6211 ocorrências em `hooks.jsonl`, ~99,5% das linhas de warning) — build-cache nunca reclamado e o sinal real (EACCES/OOM) afogado. A unidade `civmctl-runner-watchdog` fica cronicamente em `failed` porque um skip benigno (host busy) retorna exit 1, poluindo qualquer alerta keyed em unidades falhas. O caminho `civmctl cleanup` (cron) ainda roda o `docker system prune -af --volumes` perigoso que o hook evita, com mortes por OOM/timeout observadas. E não há gatilho que ligue "runner quebrou" a um restart automático da unidade `civm-*` afetada, mesmo o watchdog já tendo `systemctl restart` pronto.

Este PRD entrega primitivos que tornam o box um **GitHub Actions equivalente, confiável e auto-curável** para os 8 repos, atacando a raiz de cada família:

- **Remoção de `_work` robusta a arquivos root-owned** (escalada `sudo` idempotente, escopada estritamente ao subtree `_work` validado) + sudoers versionado + self-check de capacidade — RF-1.
- **Hook `job-completed` resiliente** (best-effort para erros de filesystem após a escalada falhar; fatal só sob pressão real de disco) com classificador de erro genérico — RF-2.
- **Mesma robustez no caminho cron `cleanup.Run`** (escalada + não-aborta-no-primeiro-erro), extraída para um único pacote `safedelete` compartilhado — RF-3.
- **Disco self-healing real:** registrar as 3 Scheduled Tasks; reconciliar a SPEC para tornar **1 MB BlockSize + Optimize OFFLINE** o mecanismo primário documentado; expor BlockSize/freshness na observabilidade do host — RF-4.
- **Buildx 125 diagnosticado e silenciado** (capability-check único; fallback `docker builder prune`; reclamação de cache realmente acontece) — RF-5.
- **Watchdog deixa de falsa-falhar** (skip host-busy = exit 0) + **auto-recuperação do runner** (restart da unidade `civm-*` após Complete-runner/EACCES-fallback) — RF-6.
- **`cleanup` cron isolation-aware** (mesmo conjunto de prune concorrência-seguro do hook; nunca `system prune --volumes`; defere sob docker-heavy lock) — RF-7.
- **Bootstrap durável** (`/run/civm` via tmpfiles.d; sudoers; lock heartbeat materializado) + timeouts nas ops de filesystem + contador de decisões do hook para observabilidade — RF-8.
- **Runbook + contrato Day-0** consolidando tudo — RF-9.

Valor: o runner para de morrer em silêncio por arquivo root-owned ou disco cheio; uma falha de job vira auto-cura ou alarme visível, não fila travada por horas; e o usuário tem um GitHub Actions gratuito que se comporta como hospedado para os 8 repos.

---

## 2. Contexto técnico

### Topologia (Confirmado em observação operacional + docs)

```
Windows host (EMEDEV)  ── Hyper-V ──> VM Linux "gha-ubuntu-2404" (guest)
  V: (119,2 GB NTFS SSD)                /dev/sda2 ext4  ← 8 runners + civmctl
   └─ gha-ubuntu-2404.vhdx (dinâmico,   ├─ actions-runner-acme/_work/...
      pós-Convert 1MB BlockSize;        ├─ actions-runner-acme-org/_work/...
      45,4 GB livres pós-remediação)    ├─ ...(8 slots: acme, acme-org,
  3 Scheduled Tasks (NÃO registradas):  │     service-a, service-b,
   ├─ civm-host-metrics  (/mo 10)       │     self, service-c, service-d,
   ├─ civm-vhdx-optimize (onstart)      │     peer)
   └─ ...-watchdog       (/mo 5)        └─ civmctl-*.timer (cleanup/watchdog/metrics)
  WSL (operador) ── powershell.exe + ssh gha-ubuntu-2404 ──> guest
```

- civm é um binário Go que roda **no guest** (Ubuntu 24.04). O componente Windows (`deploy/windows/`) é introduzido pela SPEC `host-volume-reclamation` e estendido aqui.
- O usuário `emdev` no guest tem `(ALL) NOPASSWD: ALL` (probado no box), mas o cleanup **não usa** sudo para o `_work`. Defesa-em-profundidade exige um sudoers escopado (não depender de NOPASSWD ALL).
- O mesmo hook (`ACTIONS_RUNNER_HOOK_JOB_STARTED`/`_COMPLETED`) é compartilhado pelos 8 serviços. Um job com arquivo root-owned tira **um** runner do ar; a fila daquele repo trava até intervenção manual.

### Estado atual confirmado no código E no box vivo

**Família 1 — `_work` root-owned (CRÍTICO):**

- `internal/hook/hook.go` `cleanWorkRoot()` deleta cada entrada de `_work` com `opts.RemoveAllFn` (= `os.RemoveAll`, não-privilegiado, roda como `emdev`). Em arquivo root-owned → EACCES, seta `a.Error` e **retorna na primeira entrada que falha** (deixa o resto sem deletar → vaza disco).
- `EventJobCompleted` (`hook.go:146`): `firstActionError()` promove **qualquer** Action com `Error` não-vazio a `errorResult` → `ExitCode 1` → "Complete runner" falha.
- `isIgnorableCacheDeleteRace` (`hook.go:534`): só demote para `a.Name ∈ {cache, cache_trim}` **E** erro contém "directory not empty". `work_root` nunca é demotável; "permission denied" nunca é demotável. O demote (`onlyIgnorableCacheDeleteRaces`) só está cabeado no branch `job-started`, **não** no `job-completed`.
- Nenhum `Geteuid`/`chown`/`sudo rm` no caminho de remoção (`grep` confirma; só mocks em testes).
- **Vivo:** 34 eventos fatais hoje em `hooks.jsonl`, todos `acme/app`, exatamente `docs/events-catalog.json` root-owned. 68 referências `unlinkat` a esse arquivo; run_id 26671037619 falhou 3+ vezes seguidas no "Complete runner". O box mostra 0 arquivos root-owned agora só porque rebootou ~1h atrás mid-job; o modo recorre a cada passo containerizado root.

**Família 2 — disco do host (HIGH):**

- `internal/cleanup/cleanup.go:270` `scanAndMaybeDelete` deleta via `RunFn(ctx, 'rm', '-rf', path)` **sem sudo** e **BREAK no primeiro erro** (abandona o resto). `dockerPrune` (`cleanup.go:348`) roda `docker system prune -af --volumes` **sem sudo**.
- `internal/hostdisk/hostdisk.go:140`: `host-metrics.json` ausente → `level=crit stale=true` (fail-safe correto). **Vivo:** o arquivo está ausente; `civmctl host-disk` retorna crit.
- As 3 Scheduled Tasks do host (`deploy/windows/register-civm-host-metrics.ps1`, `register-civm-vhdx-optimize.ps1`) **não foram registradas** (`Get-ScheduledTask` = NOT REGISTERED). Os workers `civm-host-metrics.ps1` e `civm-vhdx-optimize.ps1` existem mas nunca rodaram.
- `civm-vhdx-optimize.ps1` faz `Convert-VhdxToScsi` (IDE→SCSI) + `Optimize-VHD -Mode Full`. **Não há** `Convert-VHD -BlockSizeBytes 1MB` em lugar nenhum (`grep` por `BlockSizeBytes|1MB|1048576` em `deploy/` e nas SPECs = **zero**).
- `host-volume-reclamation/SPECv2.md` DT-v2-10 declara a árvore de decisão parando em "TRIM supported, online shrink expected" — empiricamente **falso** neste box (UNMAP online não honrado no VHDX de bloco 32 MB).
- `diskdoctor.composeRootCause` (`internal/diskdoctor/diskdoctor.go:266`) roda só no guest, vê `discard`+`DISC-MAX>0` → `TrimEffective=true` → reportaria falso "online shrink expected". Não tem conceito de VHDX BlockSize.

**Família 3 — robustez multi-repo (HIGH/MEDIUM):**

- `docker_buildx_prune` em `hook.go:197` via `commandActionWarn` (não-fatal, correto) emite `exit status 125` em ~99,5% das linhas de warning (**6211 ocorrências** em `hooks.jsonl`) — buildx subsystem/builder indisponível, cache nunca reclamado, sinal real afogado. `runWithTimeout` (`hook.go:408`) descarta o `CombinedOutput`, então o warning só carrega "exit status 125".
- `civmctl-runner-watchdog.service` = `failed (status=1/FAILURE)` no box. `watchdog.go:202`: skip por host-busy (no-op saudável e comum num runner multi-repo) faz `report.Exit = maxExit(report.Exit, 1)` → systemd vê falha.
- `internal/runner/watchdog.go:274` e `restart.go:93` têm `sudo systemctl restart <unit>` prontos, mas **nada** liga "runner quebrou / Complete-runner falhou repetidamente" a um restart automático da unidade `civm-*` afetada.
- `cleanup.go:128` defere a docker-heavy lock (`LockActiveFn`), mas o **hook** `job-completed` roda `docker_*_prune` incondicionalmente, mesmo com sibling job mid `compose up --build`/`docker pull` em outro dos 8 runners.
- `cleanWorkRoot` (`os.RemoveAll`) e `trimCacheByAge` (`WalkDir`) **não têm timeout** (só os comandos externos têm `DefaultRoutineCleanupCmdTimeoutSecs=120s`). Nada observa `hooks.jsonl`; um runner pode entrar em estado quebrado e ficar lá até alguém notar.
- `/run/civm` **ausente** no box (perdido no reboot; `/run` é tmpfs); `/var/lib/civm/port-blocks.json` ausente. A lock docker-heavy de `multi-project-isolation` não tem onde materializar; o isolamento depende só de `COMPOSE_PROJECT_NAME` (funcionando — containers `acme-26661232043-service-a-1` etc.). 13 volumes docker órfãos (608 MB, 100% reclaimable) persistem.

### Confirmado na documentação oficial (Hyper-V/VHDX)

- VHDX dinâmico **não** encolhe ao liberar blocos no guest; encolhe via UNMAP/discard online (quando o host honra) ou via `Optimize-VHD -Mode Full` offline.
- O **BlockSize** do VHDX (default 32 MB) determina a granularidade da compactação: blocos grandes raramente ficam 100% zerados, então `Optimize-VHD` recupera quase nada. `Convert-VHD -BlockSizeBytes 1MB` reescreve o VHDX com granularidade fina, tornando o `Optimize-VHD` offline efetivo.
- `Optimize-VHD` exige a VM Off + privilégio Hyper-V; só recupera blocos zerados/descartados.
- Zero-fill **cresce** um VHDX dinâmico; perigoso sob baixo headroom do host.

### O que está sendo proposto (Inferência / proposta)

Robustez root-owned no hook E no cron (RF-1/RF-2/RF-3, via pacote `safedelete`); disco self-healing real com 1 MB BlockSize + Optimize offline + registro das tasks + observabilidade de BlockSize/freshness (RF-4); buildx-125 diagnosticado/silenciado com fallback de reclamação (RF-5); watchdog sem falsa-falha + auto-recuperação do runner (RF-6); cleanup cron isolation-aware (RF-7); bootstrap durável + timeouts + contador (RF-8); runbook/contrato (RF-9).

### Tenant scope

N/A — sem dados de tenant. É infraestrutura de runner compartilhada pelos 8 repos.

---

## 3. Opção recomendada

### Solução escolhida (raiz-primeiro, em camadas, Day-0 único)

1. **Remoção de `_work` robusta a root-owned (RF-1).** Em `cleanWorkRoot`, quando `opts.RemoveAllFn` retorna erro de permissão (`errors.Is(err, fs.ErrPermission)`/EACCES/EPERM), escalar idempotentemente via uma função injetada `opts.PrivilegedRemoveFn` (default: `sudo -n chown -R <runner-uid> <path>` então `RemoveAll`, fallback `sudo -n rm -rf --one-file-system <path>`), **escopada estritamente** ao subtree `_work` validado por `safeWorkRoot()` (rejeita `/`, `$HOME`, qualquer não-`_work` antes de invocar sudo). Parar de retornar no primeiro erro: acumular e continuar. Sudoers versionado em `deploy/` limitando `rm`/`chown` a `/home/*/actions-runner*/_work/*`. Self-check de capacidade (`sudo -n true` + `sudo -n -l`) em `civmctl doctor` e no `hook install` → CRITICAL se ausente.
2. **Hook `job-completed` resiliente (RF-2).** O contrato do hook de cleanup é "sempre retorna exit 0 para o runner continuar". Erros de step de filesystem no `job-completed` (após a escalada `sudo` também falhar, ou raças ENOTEMPTY/ENOENT) viram **warnings** (best-effort); fatal só sob pressão real de disco (`HardFailPct` no `job-started`). Generalizar `isIgnorableCacheDeleteRace` num classificador cobrindo `work_root` + permission/ENOTEMPTY/ENOENT, aplicado também no branch `job-completed`. WARN estruturado em `hooks.jsonl` com o path para observabilidade.
3. **Mesma robustez no cron `cleanup.Run` (RF-3).** Espelhar a escalada e o "não-aborta-no-primeiro-erro" em `scanAndMaybeDelete`. Extrair a lógica "remoção privilegiada de path validado" para um único pacote `internal/safedelete` importado por hook E cleanup (uma fonte de verdade para o padrão sudo + guard de path + uma superfície de teste).
4. **Disco self-healing real (RF-4).** (a) Registrar as 3 Scheduled Tasks no host (`register-civm-host-metrics.ps1`, `register-civm-vhdx-optimize.ps1` como SYSTEM/HIGHEST, watchdog /mo 5) — ação humana de instalação elevada Day-0. (b) Reconciliar `host-volume-reclamation/SPECv2.md` + esta SPEC: registrar "UNMAP online NÃO honrado neste box; fstrim insuficiente"; tornar **primário** o **1 MB BlockSize (`Convert-VHD`, one-time, headroom-guarded) + `Optimize-VHD` OFFLINE recorrente**; demover online-shrink a "verificar-mas-não-confiar". (c) Adicionar lógica de BlockSize ao `civm-vhdx-optimize.ps1` (ou ao menos asserção de `BlockSize==1MB`); expor `vhdx_block_size_bytes` no `host-metrics.json` (`Get-VHD .BlockSize`); `hostdisk`/`diskdoctor` sinalizam `BlockSize > 1MB` como o bloqueador real de reclamação.
5. **Buildx 125 diagnosticado/silenciado (RF-5).** No `hook install`/capability-check: `docker buildx version`/`docker buildx ls`. Se buildx ausente, ou criar um builder persistente (`docker buildx create --use --name civm`, idempotente) ou cair para `docker builder prune --force --filter until=24h` (GC do cache legado, funciona sem buildx). Detectar exit 125 especificamente e trocar para o fallback; rebaixar o 125 recorrente a um único check de startup em vez de warning por-hook. `diskdoctor` flagueia buildx-125 para a degradação silenciosa ficar visível.
6. **Watchdog sem falsa-falha + auto-recuperação (RF-6).** Skip por host-busy/host-idle-unknown → exit 0 (warning logado, nada a reparar); non-zero reservado para falha real (hook-install/runner-restart falhou → 2; runner ainda offline pós-reparo → 1). Auto-recuperação: se o hook teve que invocar o fallback EACCES OU o runner reporta Complete-runner failure (sentinela em `hooks.jsonl`/`_diag`), o watchdog `sudo systemctl restart civm-<slot>` para limpar o estado quebrado + emite métrica/auditoria.
7. **Cleanup cron isolation-aware (RF-7).** `cleanup.dockerPrune` usa o mesmo conjunto concorrência-seguro do hook (`buildx/builder prune --filter until=24h`, `image prune -f` dangling-only, `container prune -f`, `volume prune -f` só sem docker-heavy lock). **Nunca** `system prune -af --volumes` no box multi-repo. Teste de regressão: cleanup nunca emite `system prune --volumes` e defere com lock fresco.
8. **Bootstrap durável + timeouts + observabilidade (RF-8).** tmpfiles.d `d /run/civm 0755 emdev emdev -` (durável após reboot) para o heartbeat da lock docker-heavy; bound `cleanWorkRoot`/`trimCacheByAge` ao context (op de filesystem stalled é time-limited); contador estruturado de decisões do hook (event+decision) para alarme quando `job-completed != ok` recorre + opção de auto-`civmctl cleanup --execute` (agora com fallback) como self-heal.
9. **Runbook/contrato (RF-9).** Novo `runbooks/RUNBOOK-CIVM-RUNNER-RELIABILITY.md` + update das SPECs `host-volume-reclamation`/`multi-project-isolation` + `deploy/` docs.

### Motivo da escolha

- **Conserta a raiz, não o sintoma.** O killer #1 (root-owned `_work`) é resolvido por escalada idempotente escopada, não por "pedir para os repos não rodarem containers como root" (não controlável: 8 repos heterogêneos). A SPEC de disco é reconciliada com a **evidência empírica** (1 MB BlockSize), não com a teoria que o box já refutou.
- **Fail-open para hygiene, fail-closed para perigo.** Cleanup é higiene best-effort e **nunca** deve falhar o runner por motivo não-disco; `HardFailPct` continua o único gate de rejeição de job. (Kahneman #5 worst-case; #2 counterfactual numérico.)
- **Fecha o ponto cego.** Observabilidade do host (incl. BlockSize/freshness) + contador de decisões do hook transformam "descobrir quando a fila trava" em "alarmar/auto-curar". (Kahneman #3 número; #1 declarar o não-visto.)
- **Reuso máximo.** `safeWorkRoot()` (já valida o path), `idle.Check`/`dockerlock.IsActive`, `runner watchdog`/`restart` (já têm `systemctl restart`), `commandActionWarn` (já best-effort), `deploy/systemd` espelhado em `deploy/windows`. Um único `safedelete` em vez de duplicar o padrão sudo em hook E cleanup.
- **Repo-agnóstico.** Cada fix se aplica aos 8 runners pelo mesmo hook/cleanup/watchdog compartilhado — corrige uma vez, beneficia todos.

### Alternativas descartadas

| Alternativa | Por que descartada |
| --- | --- |
| **Exigir que os repos não rodem passos containerizados como root** | Não controlável: 8 repos heterogêneos, muitos com docker-compose que roda como root. É disciplina por convenção — exatamente o anti-padrão que `multi-project-isolation` já rejeitou. O fix tem que ser por construção no runner. |
| **`chmod -R` / `setfacl` preventivo no `_work` antes do job** | Não impede o passo root de re-escrever como root durante o job; o EACCES recorre no `job-completed`. A escalada na remoção é o ponto correto de intervenção. |
| **Depender do `NOPASSWD: ALL` que `emdev` já tem** | Frágil: provisioning manual por-host (8 serviços); se faltar num host, o fallback falha com "a password is required", re-criando o EACCES fatal. Sudoers escopado + self-check CRITICAL é defesa-em-profundidade. |
| **Manter o mecanismo online SCSI+discard como primário (SPEC atual)** | Empiricamente refutado no box: UNMAP online não honrado no VHDX de 32 MB; `fstrim`+Optimize recuperaram ~nada. Manter como "verificar-mas-não-confiar"; o primário é 1 MB BlockSize + Optimize offline. |
| **Zero-fill + Optimize como caminho de reclamação** | Zero-fill cresce o VHDX; sob baixo headroom estoura o `V:` (já chegou a 3 GB). Proibido por contrato; só headroom amplo e nunca como padrão. |
| **VHDX de tamanho fixo** | Aloca 100% do `V:` de imediato; sem elasticidade; não resolve a granularidade de compactação. |
| **Tornar `job-completed` simplesmente sempre exit 0 (ignorar tudo)** | Esconderia pressão real de disco; o runner pode aceitar jobs num disco cheio. Mantemos fatal sob `HardFailPct`; best-effort só para hygiene não-disco. |
| **Reescrever o cleanup do zero / mover para outra linguagem** | Over-engineering; o código existente é correto exceto pelos gaps pontuais. Patches cirúrgicos + um pacote `safedelete` extraído. |

### Trade-offs aceitos

- **civm passa a invocar `sudo` para deletar `_work`.** Aceito: escopado a path validado + sudoers versionado + injeção testável (`PrivilegedRemoveFn` permite testes sem sudo real). É a única forma de limpar lixo root-owned que os repos produzem.
- **`job-completed` deixa de falhar por erro de filesystem.** Aceito: cleanup é hygiene; `HardFailPct` no `job-started` continua protegendo contra disco cheio; WARN estruturado preserva observabilidade.
- **As tasks do host exigem registro elevado one-time.** Aceito: é a SPEC `host-volume-reclamation` já decidida (DT-v2-6); aqui só fechamos a lacuna "nunca foram registradas".
- **1 MB BlockSize via `Convert-VHD` é uma janela one-time com VM Off.** Aceito: paga-se uma vez e torna o Optimize offline efetivo para sempre; o box já provou o ganho (3 GB → 51 GB livres).
- **Auto-restart da unidade `civm-*` pelo watchdog.** Aceito: o watchdog já tem `systemctl restart`; o gatilho só fecha o loop de auto-cura. Métrica/auditoria evitam restart-loop silencioso.

---

## 4. Requisitos funcionais

### RF-1 — Remoção de `_work` robusta a arquivos root-owned (CRÍTICO — killer #1)

`cleanWorkRoot` (hook) deixa de falhar em arquivos root-owned via escalada de privilégio idempotente escopada ao `_work` validado.

- **Critério de aceite:** quando `opts.RemoveAllFn` retorna erro de permissão, `cleanWorkRoot` invoca `opts.PrivilegedRemoveFn(path)` (default: `sudo -n chown -R <uid>:<gid> <path>` então `RemoveAll`; fallback `sudo -n rm -rf --one-file-system <path>`) **somente** após `safeWorkRoot()` validar que o path é filho direto de `/home/.../actions-runner.../_work`; rejeita `/`, `$HOME`, não-`_work`. `cleanWorkRoot` acumula erros e continua para as demais entradas (não retorna na primeira). Sudoers drop-in versionado em `deploy/` (`rm`/`chown` limitados a `/home/*/actions-runner*/_work/*`) instalado idempotentemente por `hook install --execute`. `civmctl doctor` + `hook install` rodam `sudo -n true` + `sudo -n -l` filtrado e reportam CRITICAL se a capacidade NOPASSWD escopada faltar. Teste unit: `RemoveAllFn` retorna EACCES → assert que `PrivilegedRemoveFn` roda e a entrada some; `PrivilegedRemoveFn` falha → erro claro (não silencioso). Teste: sudoers self-check ausente → doctor CRITICAL.
- **Tenant isolation:** N/A.

### RF-2 — Hook `job-completed` resiliente (fail-open para hygiene, fail-closed só para disco)

Erros de step de filesystem no `job-completed` não falham mais o runner.

- **Critério de aceite:** no `EventJobCompleted`, erros de `work_root`/`cache`/`cache_trim` que sejam (a) permission-denied após o fallback de RF-1 também falhar, ou (b) ENOTEMPTY/ENOENT/raça, são demovidos a `a.Warning` (exit 0); fatal só para erros de config inseguro (unsafe work root, statfs failure) e, no `job-started`, disco `>= HardFailPct` (exit 75). `isIgnorableCacheDeleteRace` é generalizado num classificador (`work_root` + permission/ENOTEMPTY/ENOENT) aplicado também no branch `job-completed`. WARN estruturado em `hooks.jsonl` com o path. Teste `job-completed` espelhando o demote de `job-started`: EACCES pós-fallback + ENOTEMPTY → exit 0 com warnings; statfs failure → fatal. Teste: `job-started` rejeita (exit 75) **só** quando disco `>= HardFailPct`, nunca por erro de permissão de cleanup.
- **Tenant isolation:** N/A.

### RF-3 — Cron `cleanup.Run` igualmente robusto, via pacote `safedelete` compartilhado

`scanAndMaybeDelete` (cron) escala em root-owned e não aborta no primeiro erro; o padrão é único.

- **Critério de aceite:** `internal/safedelete` (novo) expõe "remoção privilegiada de path validado" (guard de path + escalada sudo) com testes próprios; hook (`cleanWorkRoot`) e cleanup (`scanAndMaybeDelete`) importam-no — uma única fonte de verdade. `scanAndMaybeDelete` registra erro e **continua** (não `break`), com sumário agregado não-fatal. `validateCleanupRoot` continua rejeitando `/`, `/home`, `/root`, home bare. Teste: candidato root-owned → escalada roda, demais candidatos processados, sumário não-fatal.
- **Tenant isolation:** N/A.

### RF-4 — Disco self-healing real (1 MB BlockSize + Optimize offline + tasks registradas + observabilidade)

O pipeline de reclamação do host passa a existir e a funcionar; a SPEC é reconciliada com a evidência.

- **Critério de aceite:** (a) as 3 Scheduled Tasks (`civm-host-metrics` /mo 10, `civm-vhdx-optimize` onstart, `civm-vhdx-optimize-watchdog` /mo 5) ficam **registradas** no host (documentado como passo de instalação Day-0); pós-registro, `/var/lib/civm/host-metrics.json` aparece no guest em ≤10 min e `civmctl host-disk` vira `level=ok`. (b) `civm-vhdx-optimize.ps1` asserta `BlockSize == 1MB` (`Get-VHD .BlockSize`) e documenta/scripta o `Convert-VHD -BlockSizeBytes 1MB` one-time headroom-guarded; `host-metrics.json` inclui `vhdx_block_size_bytes`; `hostdisk`/`diskdoctor` flagueiam `BlockSize > 1MB` como bloqueador real. (c) `host-volume-reclamation/SPECv2.md` é atualizada: "UNMAP online NÃO honrado neste box"; primário = 1 MB + Optimize offline; online-shrink demovido a "verificar-mas-não-confiar". Evidência: um ciclo offline-Optimize observado com low-water free-GB registrado (fecha a base de `DefaultHostVolumeHeadroomGB`, DT-v2-19); `diskdoctor` no box deixa de reportar falso "online shrink expected".
- **Tenant isolation:** N/A.

### RF-5 — Buildx 125 diagnosticado e silenciado, com reclamação de cache efetiva

O `exit status 125` recorrente deixa de poluir e o build-cache passa a ser reclamado.

- **Critério de aceite:** `hook install`/capability-check roda `docker buildx version`/`ls`; se buildx ausente/sem builder → ou cria builder persistente idempotente, ou usa `docker builder prune --force --filter until=24h` como fallback que realmente reclama. Exit 125 específico → troca para o fallback. O `125` recorrente vira um único check de startup, não warning por-hook (as 6211 linhas/`hooks.jsonl` param). `runWithTimeout` passa a incluir um tail truncado do output no `a.Warning` para diagnóstico. `diskdoctor` flagueia buildx-prune retornando 125. Evidência: `docker system df` antes/depois mostra reclamação líquida; `hooks.jsonl` sem o flood 125.
- **Tenant isolation:** N/A.

### RF-6 — Watchdog sem falsa-falha + auto-recuperação do runner

A unidade do watchdog deixa de ficar cronicamente `failed`; runner quebrado se auto-recupera.

- **Critério de aceite:** skip por host-busy/host-idle-unknown (no-op saudável) → `report.Exit = 0` (warning logado, sem `maxExit(...,1)`); non-zero só para falha real (hook-install/runner-restart falhou → 2; runner offline pós-reparo → 1). `systemctl status civmctl-runner-watchdog` deixa de aparecer em `systemctl --failed` num skip host-busy. Auto-recuperação: se o hook invocou o fallback EACCES OU detecta Complete-runner failure (sentinela em `hooks.jsonl` ou `_diag`), o watchdog `sudo systemctl restart civm-<slot>` da unidade afetada + emite métrica/auditoria. Teste: host-busy skip → Exit 0; sentinela de runner quebrado → restart da unidade correta.
- **Tenant isolation:** N/A.

### RF-7 — Cleanup cron isolation-aware (nunca destrói estado de sibling job)

`cleanup.dockerPrune` deixa de rodar o prune perigoso e respeita a docker-heavy lock.

- **Critério de aceite:** `cleanup.dockerPrune` usa o conjunto concorrência-seguro (`buildx/builder prune --filter until=24h`, `image prune -f` dangling-only, `container prune -f`, `volume prune -f` só quando `dockerlock.IsActive` reporta sem holder); **nunca** `docker system prune -af --volumes`. Teste de regressão: cleanup nunca emite `system prune --volumes` e defere ("deferred-by-docker-heavy-lock") com lock fresco. Os 13 volumes órfãos observados são reapados por `volume prune` sem o `--volumes` perigoso.
- **Tenant isolation:** N/A.

### RF-8 — Bootstrap durável + timeouts de filesystem + contador de decisões do hook

Os primitivos param de depender de estado tmpfs perdido em reboot; ops de filesystem são time-limited; decisões são observáveis.

- **Critério de aceite:** tmpfiles.d `d /run/civm 0755 emdev emdev -` (versionado em `deploy/`) recria `/run/civm` no boot para o heartbeat da lock docker-heavy; pós-reboot um job docker-heavy materializa `.hb` em `/run/civm` e cleanup loga "deferred-by-docker-heavy-lock". `cleanWorkRoot`/`trimCacheByAge` recebem o context (op stalled time-limited, não só os comandos externos). Contador estruturado de decisões do hook (event+decision, via `cmd/civmctl` metrics path) + alarme quando `job-completed != ok` recorre; opção de auto-`civmctl cleanup --execute` (com fallback de RF-1) como self-heal. Teste: WalkDir/RemoveAll respeitam cancelamento do context; contador incrementa por decisão.
- **Tenant isolation:** N/A.

### RF-9 — Contrato e documentação Day-0

Runbook novo + reconciliação das SPECs vizinhas + `deploy/` docs.

- **Critério de aceite:** `runbooks/RUNBOOK-CIVM-RUNNER-RELIABILITY.md` (novo) cobre: a escalada root-owned + sudoers + self-check; fail-open do `job-completed`; registro das 3 tasks como passo Day-0; 1 MB BlockSize + Optimize offline como mecanismo primário (com a constatação "UNMAP online não honrado"); buildx-125; auto-recuperação do watchdog; tmpfiles.d. `host-volume-reclamation/SPECv2.md` e `multi-project-isolation/SPECv2.md` atualizadas onde colidem. `npm run docs:index` + `npm run docs:check` verdes.
- **Tenant isolation:** N/A.

---

## 5. Requisitos não-funcionais

### Performance

- Alvo primário: **zero jobs falhos por lixo root-owned no `_work`** (hoje: 34 eventos fatais/dia em acme). Métrica: `job-completed` com `decision=error` por motivo de filesystem → 0 sustentado.
- Alvo de disco: **host `V:` ≥ 30 GB livres** em operação normal, com reclamação efetiva (Optimize offline num VHDX 1 MB recupera ≈o esperado; baseline a registrar em RF-4/DT-v2-19). Buildx-prune passa a reclamar cache (RF-5) — `docker system df` antes/depois.
- `safedelete`/escalada são segundos; o `sudo chown -R`/`rm -rf` é escopado ao job dir, não ao `_work` inteiro.

### Segurança

- A escalada `sudo` é **estritamente escopada** ao subtree `_work` validado por `safeWorkRoot()` (guard duplo: regex de path + rejeição de `/`/`$HOME`/não-`_work`) e a um sudoers drop-in versionado (`rm`/`chown` só sob `/home/*/actions-runner*/_work/*`) — não depende de NOPASSWD ALL.
- self-check de capacidade trata NOPASSWD escopado ausente como **CRITICAL** (não warning): a load-bearing fix do killer #1 não pode falhar em silêncio.
- A Scheduled Task de Optimize roda como SYSTEM com direito Hyper-V (privilégio mínimo, sem rede, sem segredo) — herdado de `host-volume-reclamation`. Nunca `pull_request_target`/código de PR no host.
- Logs sem PII; `hooks.jsonl` registra paths de `_work` (não conteúdo). Métricas com labels limitados (event/decision), sem slug/tenant.

### Observabilidade

- Contador de decisões do hook (event+decision) + alarme em `job-completed != ok` recorrente. `host-metrics.json` com `vhdx_block_size_bytes` + `delivery_status` + freshness. `diskdoctor` flagueia BlockSize > 1 MB e buildx-125. Auditoria nas auto-recuperações do watchdog.

### Escalabilidade

- O hook/cleanup/watchdog é compartilhado pelos 8 serviços de runner — cada fix se aplica a todos por construção. A lock docker-heavy (RF-8) é o sinal de fairness entre repos concorrentes.

### LGPD

- N/A. Sem dado pessoal.

### Resiliência (worst-case — Kahneman #5)

- **Passo CI containerizado escreve root-owned `_work`** (observado, 34x/dia): escalada idempotente limpa; se o sudo também falhar, `job-completed` demove a warning (runner sobrevive) e o watchdog pode reiniciar a unidade.
- **Host `V:` a 3 GB livres** (observado): NUNCA zero-fill; Optimize aborta; alarme crítico; 1 MB BlockSize + Optimize offline numa janela recupera de verdade.
- **buildx ausente**: fallback `docker builder prune` reclama; sem flood de 125.
- **Runner em estado quebrado**: auto-restart da unidade `civm-*` + métrica, em vez de fila travada por horas.
- **`/run/civm` perdido em reboot**: tmpfiles.d recria; a lock docker-heavy volta a coordenar.
- **NOPASSWD escopado ausente num host**: doctor CRITICAL antes do próximo arquivo root-owned, não EACCES fatal surpresa.

---

## 6. Fluxos

### Happy path A — job termina com `_work` root-owned (o caso que hoje quebra o runner)

1. Job containerizado (root) escreveu `docs/events-catalog.json` no `_work` montado.
2. Hook `job-completed` → `cleanWorkRoot` → `os.RemoveAll` retorna EACCES.
3. RF-1: `safeWorkRoot()` valida o path → `PrivilegedRemoveFn` faz `sudo -n chown -R emdev <path>` então `RemoveAll` (ou `sudo -n rm -rf`). Entrada removida; demais entradas continuam.
4. Hook retorna **exit 0**; "Complete runner" sucede; o runner continua aceitando jobs.

### Happy path B — disco do host reclamado (após RF-4)

1. `civm-host-metrics` (registrada) escreve `host-metrics.json` com `v_free_gb`, `vhdx_block_size_bytes`, freshness; civm vê `level=ok`.
2. `v_free_gb` cruza 30 GB → alarme; `civm-vhdx-optimize` (SYSTEM, headroom-guarded) drena → shutdown → `Optimize-VHD -Mode Full` (efetivo no VHDX 1 MB) → Start-VM → restore.
3. `host-metrics` confirma `v_free_gb` recuperado; watchdog garante VM nunca Off.

### Happy path C — runner quebrado se auto-recupera

1. Por qualquer motivo o "Complete runner" falha (sentinela em `hooks.jsonl`/`_diag`).
2. `civmctl-runner-watchdog` (sem falsa-falha após RF-6) detecta a sentinela → `sudo systemctl restart civm-<slot>` → unidade limpa, fila destrava + auditoria.

### Fluxos de erro

| Condição | Resultado | Log | Consistência |
| --- | --- | --- | --- |
| `_work` root-owned + `sudo` falha (NOPASSWD ausente) | `job-completed` demove a warning (exit 0); doctor já reportou CRITICAL | WARN path + CRITICAL capability | Runner sobrevive; lixo fica até doctor corrigido |
| `os.RemoveAll` EACCES + path NÃO em `_work` | **Recusado** pelo guard; nunca invoca sudo | `error` unsafe path | Nada destrutivo fora de `_work` |
| `job-completed` erro de filesystem não-disco | Demovido a warning; exit 0 | WARN estruturado | Runner continua |
| disco `>= HardFailPct` no `job-started` | Job **rejeitado** (exit 75) | `error` disk pressure | Único caso fatal legítimo |
| buildx prune exit 125 | Fallback `docker builder prune`; warning único de startup | `warn` 125 (1x) | Cache reclamado |
| `Optimize-VHD` timeout/erro | Religa a VM; sai com erro (DT-v2-1) | `error` + religou | VM nunca Off |
| `v_free_gb < headroom` na compactação | **Abort** sem zero-fill; alarme crítico | `error` headroom | Host intacto |
| watchdog skip host-busy | Exit 0, warning | `warning` host-busy | Unidade não-falha |
| `/run/civm` ausente pós-reboot | tmpfiles.d recria no boot | — | Lock volta a coordenar |

---

## 7. Modelo de dados

**N/A — sem banco.** Estado (arquivos/host):

- `/home/*/actions-runner*/_work/**` (guest): subtree validado, único alvo da escalada sudo.
- `deploy/.../sudoers.d/civm-cleanup` (guest): NOPASSWD escopado a `rm`/`chown` sob `_work`.
- `deploy/.../tmpfiles.d/civm.conf` (guest): `d /run/civm 0755 emdev emdev -`.
- `/run/civm/docker-heavy.lock(.hb)` (guest, tmpfs): heartbeat da lock docker-heavy.
- `/var/lib/civm/host-metrics.json` (guest, entregue por SSH do host): + `vhdx_block_size_bytes`, `delivery_status`, freshness.
- `/var/log/civm/hooks.jsonl` (guest): decisões do hook + sentinela de runner quebrado.
- Host: `civm-host-metrics.json`, `civm-vhdx-optimize.lock`, log da task; 3 Scheduled Tasks.

Backfill = **N/A — Day-0**.

---

## 8. API / Interfaces

Sem endpoint HTTP/OpenAPI/evento. Interfaces = CLI civmctl + hooks + componente host + arquivos de deploy.

### CLI civmctl (guest)

| Interface | Mudança |
| --- | --- |
| `civmctl hook install [--execute]` | **estendido** (RF-1/RF-5): instala sudoers escopado idempotente; capability self-check; buildx capability-check/builder |
| `civmctl doctor` | **estendido** (RF-1): self-check NOPASSWD escopado → CRITICAL se ausente; flag buildx-125 |
| `civmctl cleanup [--execute]` | **alterado** (RF-3/RF-7): `safedelete` + escalada; prune isolation-aware; nunca `system prune --volumes` |
| `civmctl runner watchdog` | **alterado** (RF-6): host-busy skip → exit 0; auto-restart da unidade em runner quebrado |
| `civmctl host-disk [--json]` / `disk-doctor` | **estendido** (RF-4): expõe/flagueia `vhdx_block_size_bytes` > 1 MB |

### Hooks (guest)

| Interface | Mudança |
| --- | --- |
| `ACTIONS_RUNNER_HOOK_JOB_COMPLETED` (`cleanWorkRoot`) | **alterado** (RF-1/RF-2/RF-8): escalada root-owned; acumula erros; fail-open hygiene; context timeout |

### Componente host (Windows, `deploy/windows/`)

| Artefato | Função |
| --- | --- |
| `civm-host-metrics.ps1` + task registrada | emite `host-metrics.json` (+ `vhdx_block_size_bytes`) (RF-4) |
| `civm-vhdx-optimize.ps1` + task SYSTEM | Optimize offline; asserta/Converte BlockSize 1 MB (RF-4) |
| `register-*.ps1` | **executados** Day-0 (RF-4) |

### Pacotes Go (guest)

| Pacote | Mudança |
| --- | --- |
| `internal/safedelete` | **novo** (RF-3): remoção privilegiada de path validado, compartilhado hook+cleanup |

### Impacto em OpenAPI/SDK/eventos

**N/A.**

---

## 9. Dependências e riscos

### Pré-requisitos

- Acesso elevado one-time no host para registrar as 3 Scheduled Tasks e (uma vez) `Convert-VHD -BlockSizeBytes 1MB` com a VM Off — janela de manutenção (o probe rodou non-elevated; `Get-VM`/`Get-VHD` retornaram vazio).
- sudoers escopado instalado no guest (`hook install`) — sem ele, o fix do killer #1 cai no fallback warning (não-fatal) mas doctor reporta CRITICAL.
- Medir o baseline com `disk-doctor`/`docker system df` ANTES de mudar (Kahneman #3 — não assumir).

### Riscos técnicos (com mitigação)

| Risco | Mitigação |
| --- | --- |
| Escalada `sudo` apagar fora do `_work` | Guard duplo (`safeWorkRoot()` + rejeição `/`/`$HOME`/não-`_work`) antes de qualquer sudo; sudoers limita a `/home/*/actions-runner*/_work/*`; teste de path inseguro |
| NOPASSWD escopado faltando num host | doctor CRITICAL + `hook install` instala idempotente; fallback warning mantém runner vivo |
| `fail-open` esconder disco cheio | `HardFailPct` no `job-started` continua fatal; só hygiene não-disco é demovida |
| `Convert-VHD 1MB` requer VM Off / mudar device name | Janela planejada; `fstab` por UUID; valida boot + `disk-doctor` (herda DT-v2-12) |
| Auto-restart do watchdog virar restart-loop | Métrica/auditoria por restart; só dispara com sentinela específica, não em skip host-busy |
| `safedelete` extraído introduzir regressão | Uma superfície de teste única; testes de hook E cleanup apontam para ele |
| Tasks SYSTEM ampliam superfície no host | Privilégio mínimo Hyper-V, versionado em `deploy/windows`, sem rede/segredo (herda RF-NF Segurança) |
| Boundary: civm ganha sudoers/tmpfiles/host | Isolado em `deploy/` com contrato; reversível (remover drop-in/task); documentado |

### Impacto em componentes existentes

`internal/hook` (escalada + fail-open + timeout), `internal/cleanup` (safedelete + isolation-aware), `internal/safedelete` (novo), `internal/runner` (watchdog exit + auto-restart), `internal/hostdisk`/`internal/diskdoctor` (BlockSize/buildx flag), `cmd/civmctl` (doctor/hook install/host-disk), `deploy/` (sudoers, tmpfiles.d, windows tasks registradas), runbooks/SPECs vizinhas.

### Breaking changes

Nenhum. Aditivo/cirúrgico; sem o sudoers, o comportamento degrada para warning (não pior que hoje, exceto que hoje é fatal). Reverter via `civmctl self-upgrade` anterior.

### Estratégia de rollout

Slice 0 (baseline: `hooks.jsonl` count, `docker system df`, `disk-doctor`, `Get-VHD`) → Slice 1 (RF-1/RF-2/RF-3 `safedelete` + escalada + fail-open + sudoers — **o killer #1 primeiro**) → Slice 2 (RF-6 watchdog exit + auto-restart) → Slice 3 (RF-5 buildx + RF-7 cleanup isolation-aware) → Slice 4 (RF-8 tmpfiles/timeouts/contador) → Slice 5 (RF-4 tasks registradas + 1 MB BlockSize + observabilidade host) → Slice 6 (RF-9 runbook + reconciliar SPECs).

### Estratégia de rollback

- **App:** `civmctl self-upgrade` anterior; novos comportamentos viram no-op; `safedelete` revertido com o binário.
- **Guest:** remover sudoers/tmpfiles drop-in (escalada cai no fallback warning).
- **Host:** `schtasks /delete` das 3 tasks; reverter SCSI/BlockSize só se boot quebrar (janela).
- **Proibido:** zero-fill sob baixo headroom; deixar a VM Off; sudo fora do `_work`.
- **Rollback trigger numérico (fechar no SPEC):** reverter a slice se, após RF-1, `job-completed` com `decision=error` por filesystem **não** cair a 0 em 3 dias de operação com jobs root-writing; OU a escalada sudo tocar qualquer path fora de `_work` 1x (CRITICAL — reverter imediato); OU, após RF-4, o Optimize offline num VHDX 1 MB não recuperar ≈o esperado em 3 medições; OU o auto-restart do watchdog disparar > N vezes/hora na mesma unidade (restart-loop).

### Hipóteses que exigirão disciplina explícita no SPEC (`disciplines/KAHNEMAN-DISCIPLINES.md`)

- **#1 (WYSIATI — declarar o não-visto):** `Get-VHD .BlockSize` atual não pôde ser re-verificado (probe non-elevated); o SPEC deve declarar "BlockSize pós-Convert presumido 1 MB, a confirmar em host elevado" antes de afirmar.
- **#3 (número, não adjetivo):** "runner quebra", "disco enche", "buildx falha" viram medição (34 eventos fatais/`hooks.jsonl`; 6211 linhas 125; `v_free_gb`; `docker system df` antes/depois).
- **#5 (availability/worst-case):** root-owned 34x/dia, host a 3 GB, buildx ausente, runner quebrado, `/run/civm` perdido — todos com mitigação.
- **#2 (counterfactual):** rollback trigger numérico acima, fechado no SPEC.

---

## 10. Estratégia de implementação

| Slice | Conteúdo | Depende de | Validável cedo |
| --- | --- | --- | --- |
| **Slice 0 — Baseline** | Coletar: `grep -c decision\":\"error` em `hooks.jsonl`; count de `exit status 125`; `disk-doctor --json`; `docker system df`; `Get-VHD` (elevado). Sem código. | — | Output colado no SPEC; prova os números |
| **Slice 1 — killer #1** | `internal/safedelete` + escalada em `cleanWorkRoot`+`scanAndMaybeDelete`; fail-open `job-completed`; sudoers + self-check; testes (RF-1/RF-2/RF-3) | Slice 0 | Local (mock `RemoveAllFn`/`PrivilegedRemoveFn`/sudo); injeção testável |
| **Slice 2 — watchdog** | host-busy skip → exit 0; auto-restart por sentinela (RF-6) | Slice 1 | Local (mock systemctl/idle); `systemctl --failed` limpo |
| **Slice 3 — buildx + cleanup** | buildx capability-check/fallback (RF-5); cleanup isolation-aware sem `system prune --volumes` (RF-7) | Slice 1 | `hooks.jsonl` sem flood 125; teste regressão de prune |
| **Slice 4 — bootstrap/obs** | tmpfiles.d `/run/civm`; context timeout em filesystem; contador de decisões (RF-8) | Slices 1-3 | Pós-reboot `.hb` aparece; cancelamento de context testado |
| **Slice 5 — disco host** | registrar 3 tasks; `Convert-VHD 1MB` + Optimize offline; `vhdx_block_size_bytes` + flags (RF-4) | Slices 1-4 | `host-metrics.json` em ≤10 min; `host-disk=ok`; medição antes/depois |
| **Slice 6 — docs/SPECs** | `RUNBOOK-CIVM-RUNNER-RELIABILITY.md`; reconciliar `host-volume-reclamation`/`multi-project-isolation` SPECv2 (RF-9) | Slices 1-5 | `npm run docs:check` |

---

## 11. Documentos a atualizar

- `docs/specs/civm-runner-reliability/{PRD.md (este), SPEC.md, SPECv2.md, IMPL.md}`
- `runbooks/RUNBOOK-CIVM-RUNNER-RELIABILITY.md` (novo)
- `docs/specs/host-volume-reclamation/SPECv2.md` (reconciliar: UNMAP online não honrado; primário = 1 MB BlockSize + Optimize offline; DT-v2-10/DT-v2-19)
- `docs/specs/multi-project-isolation/SPECv2.md` (referência cruzada da lock docker-heavy materializada por tmpfiles.d)
- `runbooks/MULTI-PROJECT-RUNNER.md` §Disk pressure / §Rollback trigger (root-owned + buildx + auto-recuperação)
- `deploy/` (novo: `sudoers.d/civm-cleanup`, `tmpfiles.d/civm.conf`; `windows/` tasks registradas)
- `deploy/systemd/README.md` (referência cruzada guest↔host + watchdog auto-restart)
- `cmd/civmctl/main.go` `printHelp` (doctor/hook install estendidos)
- `docs/INDEX.md` (regenerar `npm run docs:index`)
- `AGENTS.md`/`CODEX.md` (boundary: sudoers escopado + tasks de host)

## 12. Fora de escopo

| Item | Motivo |
| --- | --- |
| Exigir mudança nos passos CI dos 8 repos (não rodar como root) | Não controlável; o fix é por construção no runner |
| Migrar civmctl para Windows / reescrever em PS | Mantém arquitetura guest-Linux; host só precisa de scripts + tasks |
| Trocar Hyper-V por outro hypervisor | Fora do problema |
| Expandir fisicamente o `V:`/disco como solução única | Capex; mitigação estrutural, não a solução (BlockSize resolve a granularidade) |
| Online SCSI+discard como mecanismo primário de reclamação | Empiricamente refutado neste box; demovido a "verificar-mas-não-confiar" (RF-4) |
| Multi-project isolation (lock/porta/project-name) em si | Coberto por `docs/specs/multi-project-isolation/`; aqui só materializamos `/run/civm` (RF-8) |
| Mudança em produto de peer (acme/peer/etc.) | Puramente plataforma de runner |

## 13. Critérios de aceitação

- `cleanWorkRoot` escala em root-owned (sudo escopado a `_work`), acumula erros, e `job-completed` retorna exit 0; doctor CRITICAL sem sudoers — RF-1.
- `job-completed` demove erros de filesystem não-disco a warning; fatal só sob `HardFailPct` — RF-2.
- `cleanup.Run`/`scanAndMaybeDelete` escala + não-aborta-no-primeiro-erro via `safedelete` compartilhado — RF-3.
- 3 Scheduled Tasks registradas (`host-metrics.json` em ≤10 min, `host-disk=ok`); 1 MB BlockSize + Optimize offline primário; `vhdx_block_size_bytes` exposto e flag de BlockSize > 1 MB — RF-4.
- buildx 125 vira check de startup + fallback que reclama cache; sem flood em `hooks.jsonl` — RF-5.
- watchdog host-busy → exit 0 (unidade não-falha); auto-restart da unidade `civm-*` em runner quebrado — RF-6.
- `cleanup` nunca emite `system prune --volumes`; defere sob docker-heavy lock — RF-7.
- `/run/civm` recriado por tmpfiles.d pós-reboot; ops de filesystem com context timeout; contador de decisões do hook — RF-8.
- `RUNBOOK-CIVM-RUNNER-RELIABILITY.md` publicado; SPECs vizinhas reconciliadas; `npm run docs:check` verde — RF-9.

## 14. Validação

- **Go (civm):** `go test ./... -race -count=1` — `safedelete` (mock sudo, guard de path), `cleanWorkRoot` (EACCES→escalada, acumula erros, context cancel), hook `job-completed` (demote de filesystem; fatal só HardFailPct), `cleanup` (não-aborta; nunca `system prune --volumes`; defere lock), watchdog (host-busy exit 0; auto-restart por sentinela), hostdisk/diskdoctor (BlockSize > 1 MB flag; buildx-125).
- **Host (manual/scriptado):** registrar as 3 tasks (elevado); confirmar `host-metrics.json` em ≤10 min e `civmctl host-disk=ok`; `Convert-VHD 1MB` one-time + Optimize offline numa janela com low-water free-GB registrado; `Get-VHD .BlockSize == 1MB`.
- **Vivo (box):** após Slice 1, monitorar `hooks.jsonl` por 3 dias de jobs root-writing → `job-completed decision=error` por filesystem = 0; `systemctl --failed` sem `civmctl-runner-watchdog`; `hooks.jsonl` sem flood 125; `docker system df` antes/depois prova reclamação.
- **Lint/format:** `golangci-lint run -c .golangci.yml`, `gofmt`; PSScriptAnalyzer no PS (se disponível).
- **Docs:** `npm run docs:index` + `npm run docs:check`.
- **Gates cognitivos:** cada etapa crítica aponta `disciplines/KAHNEMAN-DISCIPLINES.md` com pergunta/evidência/abort trigger (mapa abaixo).
- **Prova end-to-end:** um job com arquivo root-owned no `_work` que hoje quebraria o runner termina com "Complete runner" verde e o runner segue aceitando jobs; um ciclo Optimize offline recupera `v_free_gb`.

---

## 15. Mapa de disciplinas Kahneman (pergunta / evidência / abort-trigger por decisão crítica)

> Cada decisão não-trivial deste PRD carrega: a **pergunta** (o que de fato decidimos), a **evidência** (número/observação que sustenta — Kahneman #3, não adjetivo) e o **abort-trigger** (counterfactual numérico — Kahneman #2). Decisão sem abort-trigger específico = Sistema 1 disfarçado e não deve ser implementada.

| Decisão crítica | Disciplina | Pergunta | Evidência (número/observação) | Abort-trigger (counterfactual numérico) |
| --- | --- | --- | --- | --- |
| **Escalada `sudo` no `_work` é o fix do killer #1 (RF-1)** | #1, #3 | Escalar privilégio na remoção em vez de pedir disciplina aos repos resolve a raiz? | 34 eventos fatais/dia em `hooks.jsonl`, todos `acme/app`, `unlinkat ... events-catalog.json: permission denied`; nenhum `chown`/`Geteuid` no código | Se `job-completed decision=error` por filesystem **não** cair a 0 em 3 dias de jobs root-writing após RF-1 → reverter e reabrir diagnóstico |
| **`sudo` escopado estritamente a `_work` validado (RF-1)** | #5 | A escalada pode apagar fora do `_work` no pior caso? | `safeWorkRoot()` já valida path; sudoers limita a `/home/*/actions-runner*/_work/*` | Se a escalada tocar **1** path fora de `_work` (teste ou auditoria) → CRITICAL, reverter imediato |
| **`job-completed` fail-open para hygiene (RF-2)** | #5 | Tornar não-fatal esconde disco cheio? | `HardFailPct=90%` no `job-started` continua o gate de rejeição; só erro não-disco é demovido | Se um job for aceito com disco `>= HardFailPct` após RF-2 → bug, reverter o demote |
| **NOPASSWD escopado é load-bearing → doctor CRITICAL (RF-1)** | #1, #5 | E se o sudoers faltar num dos 8 hosts? | probe: `emdev` tem NOPASSWD ALL hoje, mas provisioning é manual por-host | Se um host sem sudoers escopado não for flagueado CRITICAL pelo doctor → o self-check está furado |
| **1 MB BlockSize + Optimize offline é o mecanismo primário de disco (RF-4)** | #1, #3 | Online SCSI+discard realmente reclama neste box? | Empírico: 32 MB BlockSize + UNMAP online não honrado; `fstrim`+Optimize ≈4 GB; `Convert-VHD 1MB` → 3 GB→51 GB livres | Se Optimize offline num VHDX 1 MB não recuperar ≈o esperado em 3 medições → reverter e investigar (DT-v2-20 herdado) |
| **`Get-VHD .BlockSize` atual presumido 1 MB (RF-4)** | #1 | O BlockSize pós-Convert é mesmo 1 MB? | NÃO re-verificado: probe PowerShell non-elevated retornou `Get-VM`/`Get-VHD` vazio | SPEC deve declarar "a confirmar em host elevado" antes de afirmar; bloquear RF-4(b) até confirmar |
| **3 Scheduled Tasks registradas é ação humana (RF-4)** | #3 | O pipeline de disco está rodando? | `Get-ScheduledTask` = NOT REGISTERED (3x); `host-metrics.json` ausente; `host-disk=crit stale` | Se pós-registro `host-metrics.json` não aparecer no guest em ≤10 min → task mal registrada, reabrir |
| **buildx-125 vira check de startup + fallback (RF-5)** | #3 | O cache está sendo reclamado de verdade? | 6211 ocorrências `exit status 125` em `hooks.jsonl` (~99,5% dos warnings); cache nunca reclamado | Se `docker system df` antes/depois não mostrar reclamação líquida após o fallback → o fallback não funciona, reverter |
| **Auto-restart da unidade pelo watchdog (RF-6)** | #5 | Fechar o loop de auto-cura cria restart-loop? | `watchdog.go:274`/`restart.go:93` já têm `systemctl restart`; falta só o gatilho | Se o auto-restart disparar > N vezes/hora na mesma unidade → desligar o gatilho (restart-loop), tratar manual |
| **`cleanup` nunca `system prune -af --volumes` (RF-7)** | #5 | O prune do cron corrompe estado de sibling job? | `cleanup.go:348` roda o prune perigoso; "docker_prune: signal: killed" 5x, "exit status 1" 6x; 13 volumes órfãos | Se um job sibling falhar com "lease does not exist"/"No such image" após o cleanup → o isolation-aware não está cobrindo, reabrir |
| **`/run/civm` via tmpfiles.d (RF-8)** | #5 | O estado da lock sobrevive a reboot? | `/run/civm` ausente no box (perdido no reboot; `/run` é tmpfs) | Se pós-reboot a lock docker-heavy não materializar `.hb` → tmpfiles.d não aplicado, reabrir |
