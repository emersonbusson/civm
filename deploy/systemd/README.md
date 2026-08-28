# systemd units para civmctl

5 timers systemd disponíveis:
- `civmctl-cleanup.timer` — diário 04:00 UTC, full cleanup (Docker, /tmp,
  _work, apt). Idempotente e fail-closed quando há job/build ativo.
- `civmctl-disk-watchdog.timer` — hourly, dispara cleanup agressivo se
  disk > DefaultPreCleanupPct (60% no momento). Reativo a picos de uso
  entre execuções diárias e usa o mesmo guard de ociosidade do cleanup.
- `civmctl-runner-watchdog.timer` — a cada ~2min depois do boot, repara
  hooks e reinicia runners offline/failed se a VM estiver idle. Não faz
  rerun automático por padrão.
- `civmctl-reverse-watchdog.timer` — 4-em-4h, alerta se o disk-watchdog
  parou de disparar.
- `civmctl-metrics.timer` — a cada minuto, grava métricas Prometheus no
  textfile collector do node_exporter.

Além dos timers, há um **service oneshot** que não é dirigido por timer:

- `civmctl-registry-cache.service` — sobe o registry pull-through cache
  (`:5000` mirror do docker.io) via `setup-registry-cache.sh`. Roda uma vez
  por boot (`Type=oneshot` + `RemainAfterExit=yes`); o container do registry
  usa `restart=always`, então a unit só reconcilia o estado no boot. Como é
  service e não timer, o `civmctl bootstrap` **não** o habilita
  automaticamente — exige `systemctl enable` manual (ver abaixo). Sem o cache
  ligado, os pulls anônimos do Docker Hub batem no rate limit (100/6h/IP) e o
  `compose up --build` da CI morre com "No such image de largada".

Instalação manual após `civmctl bootstrap` ter colocado o binário em
`/usr/local/bin/civmctl`.

## Instalação

### Todos os timers operacionais

```bash
sudo cp civmctl-*.service /etc/systemd/system/
sudo cp civmctl-*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now civmctl-cleanup.timer civmctl-disk-watchdog.timer civmctl-runner-watchdog.timer civmctl-reverse-watchdog.timer civmctl-metrics.timer
```

### Registry pull-through cache (service oneshot)

`civmctl-registry-cache.service` é copiado pelo `civmctl bootstrap` junto com as
demais `civmctl-*.service`, mas como é service (não timer) o bootstrap NÃO o
habilita. Habilite manualmente uma vez:

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now civmctl-registry-cache.service
```

`--now` dispara o `setup-registry-cache.sh` imediatamente (idempotente) e o
`enable` garante o reconcile a cada boot. Verificar:

```bash
systemctl status civmctl-registry-cache.service
docker ps --filter name=registry-cache         # registry:2 up, restart=always
curl -s http://127.0.0.1:5000/v2/_catalog       # cache respondendo
journalctl -u civmctl-registry-cache.service -b  # log do boot atual
```

Para warm inicial autenticado (levanta o limite anônimo do pull de largada),
rode o script à mão uma vez com as credenciais antes do enable, ou crie um
drop-in com as env vars `DOCKERHUB_USER`/`DOCKERHUB_TOKEN`. O daemon.json
apontando `:5000` como `registry-mirror` é configurado pelo próprio script.

`civmctl-runner-watchdog.service` roda como root porque pode reparar hooks
e reiniciar services. Para chamadas `gh api`, ele carrega opcionalmente
`/etc/civm/runner-watchdog.env`. Na VM canônica, apontar para a auth do
operador sem copiar token:

```bash
sudo install -d -m 0755 /etc/civm
printf 'GH_CONFIG_DIR=/home/emdev/.config/gh\n' | sudo tee /etc/civm/runner-watchdog.env >/dev/null
sudo chmod 0600 /etc/civm/runner-watchdog.env
```

Se usar `GH_TOKEN` nesse arquivo em vez de `GH_CONFIG_DIR`, tratar como
secret operacional: root-only, fora do repo e rotacionado após incidente.

O service também lê `/etc/civm/run-reaper.env`. A variável
`CIVM_REAPER_REPOS` define a fleet usada para comprovar jobs queued com labels
`self-hosted,civm`. Sem essa fleet, a cura de sessão online/ociosa fica
desabilitada; o watchdog não adivinha repos.

O disk-watchdog checa disk %; se acima do threshold (default 60% via
`civm.DefaultPreCleanupPct`), roda `civmctl disk-watchdog --execute`,
que delega para `civmctl cleanup --execute` com thresholds agressivos
(TmpThreshold=1h, WorkThreshold=24h em vez de 1d/3d default).

(`civmctl bootstrap --execute --runner-watchdog=true
--reverse-watchdog=true --metrics-timer=true` faz isso automaticamente quando os arquivos estão
em `/etc/systemd/system/`. O step `install_systemd_timers` só roda
`enable --now` se os unit files já existem.)

## Verificar

```bash
systemctl list-timers civmctl-cleanup.timer
systemctl list-timers civmctl-disk-watchdog.timer
systemctl list-timers civmctl-runner-watchdog.timer
systemctl list-timers civmctl-reverse-watchdog.timer
systemctl list-timers civmctl-metrics.timer
systemctl status civmctl-cleanup.timer
journalctl -u civmctl-runner-watchdog.service --since "2 hours ago"
journalctl -u civmctl-cleanup.service --since "7 days ago"
civmctl health
civmctl doctor --repos=auto --json
civmctl capacity --json
civmctl disk-audit --json
```

## Desabilitar

```bash
sudo systemctl disable --now civmctl-cleanup.timer civmctl-disk-watchdog.timer civmctl-runner-watchdog.timer civmctl-reverse-watchdog.timer civmctl-metrics.timer
sudo systemctl disable --now civmctl-registry-cache.service
sudo rm /etc/systemd/system/civmctl-*.service /etc/systemd/system/civmctl-*.timer
sudo systemctl daemon-reload
```

Desabilitar o `civmctl-registry-cache.service` para a unit, mas NÃO derruba o
container `registry-cache` (ele tem `restart=always`). Para remover o cache de
vez: `docker rm -f registry-cache` e tire o `:5000` do `registry-mirror` em
`/etc/docker/daemon.json`.

## Personalizar horário

Editar `civmctl-cleanup.timer`:

```ini
[Timer]
OnCalendar=*-*-* 04:00:00 UTC   # diario 04:00 UTC
# OnCalendar=Mon..Fri 03:00 UTC  # so dias uteis
# OnCalendar=hourly              # a cada hora (excessivo, nao recomendado)
```

`RandomizedDelaySec=300` espalha em 5 minutos para evitar pico
simultâneo se múltiplas VMs convergirem.

## Opt-in para rerun no runner-watchdog

O timer padrão não reroda CI remoto. Depois de validar pelo journal, um
operador pode criar drop-in explícito:

```ini
# /etc/systemd/system/civmctl-runner-watchdog.service.d/rerun.conf
[Service]
ExecStart=
ExecStart=/usr/bin/flock -n /run/civmctl-runner-watchdog.lock /usr/local/bin/civmctl runner watchdog --execute --repos=auto --rerun-network-failures --max-run-age=6h --json
```

Aplicar:

```bash
sudo systemctl daemon-reload
sudo systemctl restart civmctl-runner-watchdog.timer
```

## Como o cleanup é seguro

- `IOSchedulingClass=idle` → I/O do cleanup só roda quando o sistema está
  ocioso; jobs de CI ativos têm prioridade.
- `Nice=15` → CPU baixa prioridade.
- `flock /run/civmctl-cleanup.lock` impede cleanup diário e disk-watchdog
  de rodarem ao mesmo tempo.
- `flock /run/civmctl-runner-watchdog.lock` impede dois watchdogs de runner
  simultâneos. O comando só muta com GitHub acessível e host idle. O timer
  padrão não passa `--rerun-network-failures`; rerun remoto exige execução
  manual ou drop-in opt-in com `--max-run-age=6h`.
- `civmctl cleanup --execute` aborta antes de mutar se detectar
  `Runner.Worker`, processo dentro de `_work`, Docker build/compose/buildctl
  ativo, ou se o detector não conseguir provar host ocioso.
- O guard roda no início e novamente antes de cada mutação (`rm -rf`,
  Docker prune, apt clean/autoremove).
- Arquivos com mtime <2h continuam pulados como segunda camada anti-job.
- `TimeoutStartSec=30min` → se cleanup ficar travado, systemd mata.

## Rollback se quebrar disco

Ver `docs/specs/civmctl/PRD.md` §"Rollback trigger".

## civmctl admit (gate de memória para jobs heavy)

`civmctl admit` envelopa um passo de job memory-heavy num slot de admissão: no
máximo **2 heavy** simultâneos por host (slots de flock em `/run/civm/`), jobs
**light** fluem sem slot. O payload roda como o usuário do runner sob um cgroup
`MemoryMax` (`systemd-run --pipe --wait`, nunca `--scope`/root). Spec:
`docs/specs/runner-memory-admission/SPECv4.md`.

Uso no passo do job (o comando após `--` roda sob a admissão):

```bash
civmctl admit --weight heavy --exec -- make test
civmctl admit --weight light --exec -- ./scripts/lint.sh
civmctl admit --weight heavy --exclusive docker --wait-minutes 30 --exec -- make up-local
```

- `--weight heavy|light|auto` — `auto` lê `CIVM_JOB_WEIGHT` (heavy/light), default heavy.
- `--exclusive docker` — também serializa no sub-slot docker (count=1), em vez do
  `civmctl lock --scope docker-heavy` legado (deprecated para jobs envelopados).
- Para payload Docker-heavy, declare `--weight heavy` explicitamente: `auto` pode
  resolver `CIVM_JOB_WEIGHT=light` e não reservar o cgroup heavy que o contrato
  R4 exige.
- `--wait-minutes N` — orçamento de espera; esgotado, sai com **exit 78** (sem slot
  no prazo) e o job-timeout do runner decide. **exit 79** = falha interna (ex:
  cgroup `memory` ausente → recusa heavy fail-closed; `/run/civm` não provisionável).
- Inerte por design (forward-only): nada chama `admit` até um workflow optar por ele.

`/run/civm` é provisionado on-demand (`sudo install -d`, runner-owned); um
`tmpfiles.d` é opcional. Pré-checagem: `civmctl doctor` reporta `ADMIT_CGROUP`,
`ADMIT_RUN_AS_USER` e `ADMIT_RAM_INVARIANT`; `civmctl capacity --json` expõe
`admit.heavy_live` / `admit.max_heavy`.

## GitHub Actions job hooks

Systemd timers handle periodic and pressure-based cleanup. Job hooks handle
the CI boundary itself.

Install or repair hook wiring with:

```bash
sudo civmctl hook install --execute
```

The command creates two managed shell scripts —
`/opt/civm/hooks/job-started.sh` and `/opt/civm/hooks/job-completed.sh` —
then upserts the corresponding `ACTIONS_RUNNER_HOOK_JOB_STARTED` and
`ACTIONS_RUNNER_HOOK_JOB_COMPLETED` entries in every
`/home/*/actions-runner*/.env`. The runner executes those `.sh` files via
bash; each script delegates to the same Go policy as
`civmctl hook job-started|completed --execute`.

All policy lives in Go under `internal/hook` and can be tested with
`go test ./internal/hook`.
