---
slug: civm-runner-reliability
title: Confiabilidade do runner civm — equivalente gratuito e auto-curável do GitHub Actions para todos os repos
milestone: —
issues: []
---

# SPEC — Confiabilidade do runner civm: equivalente gratuito e auto-curável do GitHub Actions

> Gerado a partir de `docs/specs/civm-runner-reliability/PRD.md` (PASSO 2 SSDV3).
> Disciplinas: `disciplines/KAHNEMAN-DISCIPLINES.md`. Validação: `go test ./... -race -count=1`,
> `golangci-lint run -c .golangci.yml ./...`, `gofmt -l`, e (host) PSScriptAnalyzer + `schtasks /query`.
> Day-0: o civm não tem produção viva; backfill = N/A. Solução primária única, sem dual-path.
> Boundary: este SPEC concede ao civm um sudoers escopado + tmpfiles.d no guest e amplia o
> componente host de `host-volume-reclamation`. Reconcilia `host-volume-reclamation/SPECv2.md`
> (online-shrink demovido a "verificar-mas-não-confiar"; 1 MB BlockSize + Optimize offline = primário).

## Escopo fechado desta implementação

**Entra agora:**

- `internal/safedelete` (novo): remoção privilegiada de path validado, fonte única do padrão `sudo chown -R`/`rm -rf` escopado ao `_work` — RF-1/RF-3.
- `internal/hook/hook.go`: `cleanWorkRoot` escala root-owned via `safedelete`, acumula erros, recebe `context`; `EventJobCompleted` fail-open para hygiene; classificador genérico de erro de filesystem; `runWithTimeout` carrega tail do output — RF-1/RF-2/RF-5/RF-8.
- `internal/hook/install.go`: instala sudoers drop-in idempotente; capability self-check; buildx capability-check + builder/fallback persistente — RF-1/RF-5.
- `internal/cleanup/cleanup.go`: `scanAndMaybeDelete` via `safedelete` (não-aborta-no-primeiro-erro); `dockerPrune` isolation-aware (nunca `system prune -af --volumes`; defere docker-heavy lock) — RF-3/RF-7.
- `internal/doctor/doctor.go`: host check de sudoers escopado (CRITICAL se ausente) + flag buildx-125 — RF-1/RF-5.
- `internal/runner/watchdog.go`: skip host-busy/host-idle-unknown → exit 0; auto-restart da unidade `civm-*` por sentinela de runner quebrado — RF-6.
- `internal/civm/civm.go`: constantes novas (sudoers path, runner UID fallback, buildx fallback filter, decision-counter path, run dir) — todos os RF.
- `cmd/civmctl/{hook,doctor,cleanup}.go`: superfície de flags/exit estendida — RF-1/RF-3/RF-5/RF-7.
- `deploy/sudoers.d/civm-cleanup`, `deploy/tmpfiles.d/civm.conf` (novos, versionados) — RF-1/RF-8.
- `deploy/windows/civm-host-metrics.ps1`: emite `vhdx_block_size_bytes` (`Get-VHD .BlockSize`) — RF-4.
- `deploy/windows/civm-vhdx-optimize.ps1`: asserta `BlockSize == 1MB` antes do Optimize; documenta o `Convert-VHD -BlockSizeBytes 1MB` one-time headroom-guarded — RF-4.
- `internal/hostdisk`/`internal/diskdoctor`: campo + flag `vhdx_block_size_bytes > 1MB` como bloqueador real — RF-4.
- `runbooks/RUNBOOK-CIVM-RUNNER-RELIABILITY.md` (novo) + reconciliação de `host-volume-reclamation/SPECv2.md` e `MULTI-PROJECT-RUNNER.md` — RF-9.

**Fica fora agora:** exigir mudança nos passos CI dos 8 repos (não controlável); reescrever civmctl em Windows/PS; trocar hypervisor; expandir fisicamente o `V:` como solução única; online SCSI+discard como mecanismo **primário** (demovido); multi-project isolation em si (`docs/specs/multi-project-isolation/`, aqui só materializamos `/run/civm`); mudança em produto de peer.

**Dependências assumidas prontas:**

- `safeWorkRoot()` (`internal/hook/hook.go:450-458`) valida que o root é `/home/.../actions-runner.../_work` — reusado como primeiro guard antes de qualquer `sudo`.
- `dockerlock.IsActive`/`ReclaimStale`/`Holder` (`internal/dockerlock/dockerlock.go:376/437/391`) e o lock em `civm.DefaultDockerHeavyLockPath` (`/run/civm/docker-heavy.lock`) — reusados por cleanup isolation-aware e materializados por tmpfiles.d.
- `idle.Check`/`idle.Result` (`internal/idle/idle.go`, `func Check(ctx, opts) Result`) — já reusado pelo watchdog.
- `runner.Restart`/`restartWatchdogRunners` (`internal/runner/restart.go:52`, `internal/runner/watchdog.go:265-292`) já têm `sudo systemctl restart <unit>` + verify — reusados pela auto-recuperação.
- `commandActionWarn`/`runWithTimeout` (`internal/hook/hook.go:394/401`) já são best-effort com timeout por comando — estendidos, não reescritos.
- `hook.Install`/`InstallOptions` (`internal/hook/install.go:90`) já é o ponto idempotente de provisioning por-runner — estendido com sudoers + buildx capability.
- Convenção `deploy/systemd/` (guest) espelhada em `deploy/windows/` (host); `deploy/sudoers.d`/`deploy/tmpfiles.d` seguem o mesmo padrão versionado.
- `civm-vhdx-optimize.ps1` já faz SCSI re-attach + Optimize offline com try/catch/finally e watchdog (`host-volume-reclamation/SPECv2.md` ITEM-11/ITEM-15) — aqui só adiciona a asserção de BlockSize e a doc do Convert one-time.

## Matriz de rastreabilidade PRD → SPEC

| PRD | Implementação no SPEC |
| --- | --- |
| RF-1 remoção `_work` robusta a root-owned | ITEM-1 (`internal/safedelete`), ITEM-2 (`cleanWorkRoot` escala+acumula+context), ITEM-6 (sudoers drop-in), ITEM-7 (`hook install` instala sudoers + self-check), ITEM-9 (`doctor` capability check), ITEM-12 (constantes) |
| RF-2 `job-completed` fail-open | ITEM-3 (classificador genérico + demote no `EventJobCompleted`) |
| RF-3 cron `cleanup.Run` robusto | ITEM-1 (`safedelete`), ITEM-4 (`scanAndMaybeDelete` via safedelete, não-aborta) |
| RF-4 disco self-healing (1 MB BlockSize + Optimize offline + tasks + obs) | ITEM-13 (host-metrics `vhdx_block_size_bytes`), ITEM-14 (optimize asserta BlockSize + Convert one-time doc), ITEM-5 (`hostdisk`/`diskdoctor` flag BlockSize>1MB), ITEM-16 (registrar 3 tasks — ação humana), ITEM-17 (reconciliar SPECv2) |
| RF-5 buildx 125 diagnosticado/silenciado | ITEM-7 (buildx capability-check no `hook install`), ITEM-8 (`runWithTimeout` tail + fallback `docker builder prune`), ITEM-9 (`doctor` flag buildx-125) |
| RF-6 watchdog sem falsa-falha + auto-recuperação | ITEM-10 (`Watchdog` host-busy → exit 0; auto-restart por sentinela) |
| RF-7 cleanup cron isolation-aware | ITEM-4b (`dockerPrune` concorrência-seguro; nunca `--volumes`; defere lock) |
| RF-8 bootstrap durável + timeouts + contador | ITEM-15 (`tmpfiles.d`), ITEM-2 (context em `cleanWorkRoot`/`trimCacheByAge`), ITEM-11 (contador de decisões do hook) |
| RF-9 contrato/docs | ITEM-17 (runbook + reconciliar SPECv2/MULTI-PROJECT-RUNNER) |

## Decisões técnicas

| # | Decisão | Justificativa |
| --- | --- | --- |
| DT-1 | A escalada root-owned vive num único pacote `internal/safedelete`, importado por `hook` E `cleanup`. **Nem `hook` nem `cleanup` codificam o padrão sudo inline.** | Uma fonte de verdade para guard de path + escalada + superfície de teste. PRD: "uma única fonte de verdade" (§3 item 3). Evita import cycle: `safedelete → civm + stdlib` apenas. |
| DT-2 | `safedelete.Remove` escala **só** após `errors.Is(err, fs.ErrPermission)` (EACCES/EPERM) no `RemoveAllFn` não-privilegiado; nunca tenta sudo de cara. | Caminho comum (arquivo do próprio `emdev`) não invoca sudo. Sudo é o fallback estreito para o lixo root-owned. (Kahneman #5: pior caso tratado, caminho feliz barato.) |
| DT-3 | Validação de path em `safedelete` é **dupla**: (a) `GuardFn` injetada do chamador (`hook` passa `safeWorkRoot`+filho-direto; `cleanup` passa `validateCleanupRoot`+filho), (b) rejeição interna fixa de `/`, `$HOME`, `/home/<x>` bare, path relativo, byte NUL. A escalada **nunca** roda se qualquer guard reprovar. | PRD exige guard duplo antes de qualquer sudo (§5 Segurança). `sudo rm -rf` num path errado é o pior caso destrutivo; abort trigger CRITICAL (Mapa). |
| DT-4 | Default `PrivilegedRemoveFn`: `sudo -n chown -R <uid>:<gid> <path>` então `RemoveAllFn`; se ainda EACCES, `sudo -n rm -rf --one-file-system <path>`. `<uid>` resolvido por `os.Getuid()`/`os.Getgid()` no boot (o runner `emdev`). | `chown` então delete não-privilegiado é menos arriscado que `rm -rf` privilegiado e mantém o delete na lógica testada de `RemoveAllFn`. `--one-file-system` evita atravessar mounts. `-n` (non-interactive) garante fail-loud se o sudoers faltar (nunca prompt pendurado). |
| DT-5 | `job-completed` é **fail-open para hygiene**: erros de `work_root`/`cache`/`cache_trim` de classe `permission`/`ENOTEMPTY`/`ENOENT` (e após o fallback `safedelete` também falhar) viram `Warning` (exit 0). Fatal **só** para config inseguro (`unsafe work root`, `unsafe cache path`) e `statfs` failure. `HardFailPct` no `job-started` continua o único gate de rejeição de job (exit 75). | O contrato do hook é "retornar exit 0 para o runner continuar". Cleanup é higiene; só disco cheio real (`HardFailPct`) deve rejeitar job. (Kahneman #5: fail-open hygiene, fail-closed perigo.) |
| DT-6 | A classe de erro é decidida por `classifyCleanupError(a Action) cleanupErrorClass`, que generaliza o atual `isIgnorableCacheDeleteRace` para cobrir `Name ∈ {work_root, cache, cache_trim}` e mensagens `permission denied`/`directory not empty`/`no such file or directory`. Aplicado no branch `job-started` (demote de raça, como hoje) **e** no `job-completed` (novo). | O demote atual só cobre `cache`/`cache_trim` + "directory not empty" e só no `job-started` (`hook.go:511-539`). O killer #1 é `work_root` + "permission denied" no `job-completed`, hoje não-demotável. Generalizar fecha o gap. |
| DT-7 | `cleanup.dockerPrune` passa a emitir o **mesmo** conjunto concorrência-seguro do hook (`buildx prune --filter until=24h` com fallback `builder prune`, `image prune -f` dangling-only, `container prune -f`, `volume prune -f`) e **nunca** `docker system prune -af --volumes`. `volume prune` só roda quando `dockerlock.IsActive` reporta sem holder fresco. | O hook já evita o prune perigoso e documenta por quê (`hook.go:180-196`). O cron `cleanup.go:348` ainda roda o perigoso `system prune -af --volumes` — corrompe `docker pull`/`compose up --build` de sibling job. Igualar os dois caminhos é a correção. |
| DT-8 | A auto-recuperação do watchdog dispara **só** por uma sentinela específica em `hooks.jsonl` (decisão `error` por filesystem **OU** decisão `cleanup-applied` com Action `work_root` carregando `escalated=true`/`escalation_failed`), nunca por skip host-busy. Restart via `runner.Restart{Unit: <civm-slot>}` (reusa `restart.go`). Contador de restarts por unidade com teto `civm.DefaultRunnerAutoRestartPerHour`; acima dele, vira WARN sem restart (anti restart-loop). | PRD exige fechar o loop de auto-cura sem criar restart-loop (§3 item 6, abort trigger). `restart.go` já tem `sudo systemctl restart` + verify; falta o gatilho + teto. |
| DT-9 | A escalada `sudo` exige o sudoers drop-in `deploy/sudoers.d/civm-cleanup` (`emdev ALL=(root) NOPASSWD: /usr/bin/chown, /usr/bin/rm`) **escopado por wrapper validado**, não NOPASSWD ALL. `hook install --execute` instala o drop-in idempotente em `/etc/sudoers.d/civm-cleanup` (0440, `visudo -cf` antes de mover). Capacidade ausente → `doctor` CRITICAL + `hook install` reinstala. | Defesa-em-profundidade: não depender do `NOPASSWD: ALL` que `emdev` já tem (provisioning manual por-host, frágil). `-n` falha-loud, o drop-in garante o NOPASSWD escopado. |
| DT-10 | Buildx 125: `hook install` roda `docker buildx ls`; se buildx ausente/sem builder, cria builder persistente idempotente (`docker buildx create --name civm --use`) **ou** marca o slot para o fallback `docker builder prune --force --filter until=24h`. O hook `cleanup` detecta `exit 125` no `docker_buildx_prune` e troca para o fallback **no mesmo job** (uma vez), persistindo o output truncado no `Warning`. `doctor` flagueia buildx-125 recorrente. | O 125 recorrente (6211 ocorrências) é buildx indisponível; o cache nunca é reclamado e o sinal real fica afogado. Capability-check único + fallback que de fato reclama fecha o gap; `runWithTimeout` hoje descarta o `CombinedOutput` (`hook.go:408`). |
| DT-11 | 1 MB BlockSize: `Convert-VHD -BlockSizeBytes 1MB` é uma operação **one-time, host, em janela, com VM Off e headroom guard**, documentada no runbook — **não** scriptada para automática (reescreve o VHDX inteiro; perigosa sob baixo headroom). `civm-vhdx-optimize.ps1` apenas **asserta** `(Get-VHD).BlockSize -eq 1MB` antes do Optimize e loga `block_size_warn` se ≠ (Optimize segue, mas o ganho será baixo). `host-metrics.ps1` expõe `vhdx_block_size_bytes`; `hostdisk`/`diskdoctor` flagueiam `> 1MB`. | Empírico: 32 MB BlockSize + UNMAP online não honrado → `fstrim`+Optimize ≈4 GB; `Convert-VHD 1MB` → 3 GB→51 GB livres. O Convert é caro/one-time; observabilidade + asserção tornam o BlockSize errado **visível** sem automatizar a operação perigosa. (Kahneman #1: o BlockSize atual pós-Convert é presumido 1 MB, **a confirmar em host elevado** — ITEM-13/14 ficam gated até `Get-VHD .BlockSize` confirmar.) |
| DT-12 | `/run/civm` (tmpfs, perdido em reboot) é recriado por `deploy/tmpfiles.d/civm.conf` (`d /run/civm 0755 emdev emdev -`). É o diretório do heartbeat da docker-heavy lock (`civm.DefaultDockerHeavyLockPath`). | `dockerlock.Acquire` já faz `MkdirAllFn(dir)` (`dockerlock.go:203`), mas tmpfiles.d garante o dir + dono corretos no boot, antes de qualquer job, para os 8 runners. |
| DT-13 | Contador de decisões do hook: `appendLog` (`hook.go:541`) já grava `event`+`decision`+`actions` em `hooks.jsonl`. Adiciona-se um campo `escalation` por Action `work_root` (`none|ok|failed`) e a sentinela de auto-cura. Sem novo subsistema de métricas: `hooks.jsonl` continua a fonte; o watchdog lê a cauda. | Reuso máximo: a observabilidade pedida (RF-8) é um campo a mais no log já estruturado, não um daemon novo. Labels limitados (event/decision/escalation), sem slug/PII. |

## Fronteira de atomicidade e política de rollback

- **Atômico nesta entrega:** cada remoção individual de entrada do `_work` (`RemoveAllFn`/`PrivilegedRemoveFn` por path); cada `safedelete.Remove(path)`; cada `os.WriteFile` de sudoers drop-in (temp + `visudo -cf` + `os.Rename`); cada linha appendada em `hooks.jsonl`; cada `host-metrics.json` (`os.WriteFile` substitui); cada `civm-vhdx-optimize` é uma operação Hyper-V única (herdado de SPECv2).
- **Fora da atomicidade:** o varrer-e-deletar do `_work`/`scanAndMaybeDelete` (multi-path, agora **acumula** erros e continua — não transacional, parcial aceito); a sequência drain→shutdown→Optimize→start (herdada); o `Convert-VHD 1MB` one-time (reescreve o VHDX, janela supervisionada). Estados parciais aceitos: algumas entradas do `_work` removidas, outras com escalada falha (viram warning + lixo até `doctor` corrigir o sudoers); host-metrics ausente/stale (civm degrada `level=crit`, fail-safe).
- **Política de rollback:**
  - **App:** `civmctl self-upgrade` para o binário anterior; `safedelete`/escalada/fail-open viram no-op; sem sudoers o comportamento cai no fallback warning (não pior que hoje, exceto que hoje é fatal).
  - **Config (guest):** remover `/etc/sudoers.d/civm-cleanup` e `/etc/tmpfiles.d/civm.conf`; a escalada degrada para warning, `/run/civm` deixa de ser recriado no boot (`dockerlock.MkdirAllFn` ainda cria sob demanda).
  - **Host:** `schtasks /delete` das 3 tasks; reverter SCSI/BlockSize só se boot quebrar (janela); o `Convert-VHD 1MB` não é revertido (one-way benéfico).
  - **Dados:** N/A — sem banco.
  - **Proibido:** zero-fill sob baixo headroom; deixar a VM Off ao fim de qualquer caminho; `sudo` em path fora de `_work`/`validateCleanupRoot`.
  - **`forward-only`?** Não — tudo reversível por remoção de drop-in/task/binário.

## Mapa Kahneman por etapa crítica

| Etapa / ITEM | Disciplina | Link | Pergunta obrigatória | Evidência mínima | Abort trigger |
| --- | --- | --- | --- | --- | --- |
| ITEM-1/ITEM-2 (escalada root-owned é o fix do killer #1) | #1, #3 | `disciplines/KAHNEMAN-DISCIPLINES.md` #1/#3 | Escalar privilégio na remoção (vs. pedir disciplina aos 8 repos) resolve a raiz? | 34 eventos `decision=error` filesystem/dia em `hooks.jsonl`, todos `acme/app`, `unlinkat ... events-catalog.json: permission denied`; nenhum `chown`/`Geteuid` no caminho atual | `job-completed decision=error` por filesystem **não** cair a 0 em 3 dias de jobs root-writing após RF-1 → reverter, reabrir diagnóstico |
| ITEM-1/ITEM-3 (`sudo` escopado estritamente a `_work`/cleanup-root validado) | #5 | idem #5 | A escalada pode apagar fora do `_work` no pior caso? | guard duplo (`GuardFn` do chamador + rejeição interna de `/`/`$HOME`/`/home/<x>` bare/NUL); sudoers limita binários; teste de path inseguro | a escalada tocar **1** path fora do escopo (teste/auditoria) → CRITICAL, reverter imediato |
| ITEM-3 (`job-completed` fail-open hygiene) | #5 | idem #5 | Tornar não-fatal esconde disco cheio? | `HardFailPct=90` no `job-started` continua rejeitando; só erro não-disco é demovido | um job aceito com disco `>= HardFailPct` após RF-2 → bug, reverter o demote |
| ITEM-9 (NOPASSWD escopado load-bearing → doctor CRITICAL) | #1, #5 | idem #1/#5 | E se o sudoers faltar num dos 8 hosts? | `emdev` tem NOPASSWD ALL hoje, mas provisioning é manual por-host; `-n` falha-loud | host sem sudoers escopado **não** flagueado CRITICAL pelo doctor → self-check furado |
| ITEM-13/ITEM-14 (1 MB BlockSize + Optimize offline = primário) | #1, #3 | idem #1/#3 | Online SCSI+discard realmente reclama neste box? | empírico: 32 MB + UNMAP online não honrado; `fstrim`+Optimize ≈4 GB; `Convert-VHD 1MB` → 3 GB→51 GB | Optimize offline num VHDX 1 MB não recuperar ≈o esperado em 3 medições → reverter, investigar (herda DT-v2-20) |
| ITEM-13/ITEM-14 (`Get-VHD .BlockSize` atual presumido 1 MB) | #1 | idem #1 | O BlockSize pós-Convert é mesmo 1 MB? | NÃO re-verificado: probe PowerShell non-elevated retornou `Get-VHD` vazio | declarar "a confirmar em host elevado"; **bloquear ITEM-13/14 até confirmar** antes de afirmar |
| ITEM-10 (auto-restart da unidade pelo watchdog) | #5 | idem #5 | Fechar o loop de auto-cura cria restart-loop? | `restart.go:93`/`watchdog.go:274` já têm `systemctl restart`; falta gatilho + teto | auto-restart disparar > `DefaultRunnerAutoRestartPerHour` na mesma unidade → desligar gatilho, tratar manual |
| ITEM-4b (`cleanup` nunca `system prune -af --volumes`) | #5 | idem #5 | O prune do cron corrompe estado de sibling job? | `cleanup.go:348` roda o perigoso; "docker_prune: signal: killed" 5x; 13 volumes órfãos | job sibling falhar com "lease does not exist"/"No such image" após o cleanup → isolation-aware não cobre, reabrir |
| ITEM-15 (`/run/civm` via tmpfiles.d) | #5 | idem #5 | O estado da lock sobrevive a reboot? | `/run/civm` ausente no box (perdido no reboot; `/run` é tmpfs) | pós-reboot a lock docker-heavy não materializar `.hb` → tmpfiles.d não aplicado, reabrir |

**Rollback trigger numérico (fecha o PRD §9):** reverter a slice se, após RF-1, `job-completed` com `decision=error` por filesystem **não** cair a 0 em 3 dias de operação com jobs root-writing; OU a escalada sudo tocar qualquer path fora do escopo validado 1x (CRITICAL — reverter imediato); OU, após RF-4, o Optimize offline num VHDX 1 MB não recuperar ≈o esperado em 3 medições; OU o auto-restart do watchdog disparar > `DefaultRunnerAutoRestartPerHour` vezes/hora na mesma unidade.

## Checklist de segurança (pré-implementação)

- [ ] **Tenant isolation:** N/A (infra de runner; sem dado de tenant).
- [ ] **SQL injection:** N/A (sem banco).
- [ ] **Escalada de privilégio:** `safedelete` só roda `sudo -n chown/rm` após guard duplo aprovar o path; sudoers drop-in limita os binários; `-n` (non-interactive) nunca pendura prompt; injeção testável (`PrivilegedRemoveFn`/`GuardFn`) sem sudo real nos testes.
- [ ] **Exec safety:** `exec.CommandContext` sem shell em todo caminho novo; sudoers drop-in escrito em temp + `visudo -cf` + `os.Rename` (nunca `sudoers` parcial); paths validados antes de `sudo`.
- [ ] **Input validation:** `safedelete.Remove` valida path (abs, sem NUL, não-`/`/`$HOME`/bare-home, passa `GuardFn`); host-metrics JSON validado (`hostdisk.Check`); watchdog sentinela lida com escape de `hooks.jsonl` truncado.
- [ ] **Secrets:** nenhum segredo em `deploy/sudoers.d`/`deploy/tmpfiles.d`/`deploy/windows`; `hooks.jsonl` grava paths de `_work` (não conteúdo); logs sem PII/token; métricas com labels limitados (event/decision/escalation), sem slug/tenant.
- [ ] **Error messages:** `job-completed` WARN estruturado com path; erro fatal só para config inseguro/statfs/`HardFailPct`; nunca expõe conteúdo de arquivo.
- [ ] **Zero-fill / VM Off:** proibido por contrato (herdado de `host-volume-reclamation/SPECv2.md` DT-v2-1/3); `civm-vhdx-optimize.ps1` já garante `finally` Start-VM + watchdog.

## Migrações SQL

**N/A — sem banco.** Estado novo (arquivos/host):

- `/etc/sudoers.d/civm-cleanup` (guest): NOPASSWD escopado a `chown`/`rm` (versionado em `deploy/sudoers.d/civm-cleanup`).
- `/etc/tmpfiles.d/civm.conf` (guest): `d /run/civm 0755 emdev emdev -` (versionado em `deploy/tmpfiles.d/civm.conf`).
- `/run/civm/docker-heavy.lock(.hb)` (guest, tmpfs): heartbeat da docker-heavy lock (já existente; agora durável no boot).
- `/var/log/civm/hooks.jsonl` (guest): decisões do hook + campo `escalation` + sentinela de runner quebrado.
- `/var/lib/civm/host-metrics.json` (guest, entregue por SSH): + `vhdx_block_size_bytes`.

Backfill = **N/A — Day-0**.

## Arquivos a CRIAR

### `internal/safedelete/safedelete.go` (+ `safedelete_test.go`) — ITEM-1

- **Propósito:** remoção de path com escalada de privilégio escopada a um path validado. Fonte única do padrão `sudo chown -R`/`rm -rf`, importada por `hook` E `cleanup`.
- **Requisitos:** RF-1, RF-3. DT-1/2/3/4/9.
- **Structs/Funções (contratos, não pseudocódigo):**
  - `type Options struct { RunnerUID int; RunnerGID int; GuardFn func(path string) error; RemoveAllFn func(path string) error; RunFn func(ctx context.Context, name string, args ...string) ([]byte, error) }` — todos injetáveis; defaults: `RunnerUID=os.Getuid()`, `RunnerGID=os.Getgid()`, `RemoveAllFn=os.RemoveAll`, `RunFn=exec.CommandContext`.
  - `func DefaultOptions() Options`.
  - `func Remove(ctx context.Context, opts Options, path string) (escalated bool, err error)` — fluxo: (1) validação interna fixa do path (abs via `filepath.IsAbs`; sem NUL; `!= "/"`, `!= $HOME`, não `/home/<x>` bare, `!= ""`); (2) `opts.GuardFn(path)` (chamador define o escopo: `_work` ou cleanup-root) — erro reprova **antes** de qualquer remoção; (3) `RemoveAllFn(path)` — sucesso → `(false, nil)`; (4) se `errors.Is(err, fs.ErrPermission)` → `escalate(ctx, opts, path)`: `RunFn(ctx, "sudo", "-n", "chown", "-R", fmt.Sprintf("%d:%d", uid, gid), path)` então `RemoveAllFn(path)`; se ainda EACCES → `RunFn(ctx, "sudo", "-n", "rm", "-rf", "--one-file-system", path)`; retorna `(true, errOrNil)`; (5) erro não-permissão → `(false, err)` sem sudo.
  - `var ErrUnsafePath = errors.New("safedelete: refused unsafe path")` — usado pela validação interna.
- **Padrão de referência:** `internal/dockerlock` (todos os side effects injetados, importa só `civm`+stdlib); `internal/hook/hook.go:275-287` (`removePath` guard); `internal/cleanup/cleanup.go:290-311` (`validateCleanupRoot`).
- **Testes:** path seguro próprio do user → sem sudo, removido; EACCES → `RunFn` recebe `chown` então `rm -rf`, `escalated=true`; `chown` falha → `rm -rf` tentado; ambos falham → erro claro (não silencioso); `GuardFn` reprova → `ErrUnsafePath`/erro do guard, **`RunFn` nunca chamado**; path `/`/`$HOME`/`/home/x` bare/relativo/NUL → recusado sem sudo; context cancelado → `RunFn` recebe ctx cancelado.
- **Disciplina Kahneman:** #1/#3/#5 — ver Mapa.

### `deploy/sudoers.d/civm-cleanup` — ITEM-6

- **Propósito:** NOPASSWD escopado para a escalada de `safedelete`, em vez de depender de NOPASSWD ALL.
- **Requisitos:** RF-1. DT-9.
- **Conteúdo (contrato):** comentário de cabeçalho + `emdev ALL=(root) NOPASSWD: /usr/bin/chown, /usr/bin/rm` (caminhos absolutos dos binários; o escopo de path é garantido por `safedelete`+`GuardFn`, não por wildcard de sudoers — wildcard de path em sudoers é frágil contra `..`). Modo de instalação 0440, dono `root:root`.
- **Padrão de referência:** `deploy/systemd/*.service` (versionado, sem segredo).
- **Testes:** validado por `visudo -cf` no `hook install` (ITEM-7) antes do `os.Rename`; teste unit do `hook install` mocka `RunFn("visudo","-cf",...)` e `WriteFileFn`/`RenameFn`.

### `deploy/tmpfiles.d/civm.conf` — ITEM-15

- **Propósito:** recriar `/run/civm` (tmpfs) no boot, dono `emdev`, para o heartbeat da docker-heavy lock.
- **Requisitos:** RF-8. DT-12.
- **Conteúdo (contrato):** comentário + `d /run/civm 0755 emdev emdev -`.
- **Padrão de referência:** convenção `deploy/systemd/`.
- **Testes:** validação manual no box (pós-reboot `/run/civm` existe dono `emdev`; um job docker-heavy materializa `.hb`); o `dockerlock` já tem teste de `MkdirAllFn`.

### `runbooks/RUNBOOK-CIVM-RUNNER-RELIABILITY.md` — ITEM-17 (docs)

- **Propósito:** procedimento canônico de confiabilidade do runner.
- **Requisitos:** RF-9.
- **Conteúdo:** escalada root-owned + sudoers + `doctor` self-check; fail-open do `job-completed` (único caso fatal = `HardFailPct`); registro das 3 Scheduled Tasks como passo Day-0 (cross-link a `host-volume-reclamation/SPECv2.md` ITEM-15); 1 MB BlockSize + Optimize offline como mecanismo **primário** com a constatação "UNMAP online não honrado neste box" + o procedimento `Convert-VHD -BlockSizeBytes 1MB` one-time headroom-guarded (VM Off, janela); buildx-125 capability-check/fallback; auto-recuperação do watchdog + teto anti restart-loop; tmpfiles.d `/run/civm`; tabela de troubleshooting por sintoma.

## Arquivos a MODIFICAR

### `internal/hook/hook.go` — ITEM-2/ITEM-3/ITEM-8/ITEM-11

- **O que muda:**
  - **Maintenance é soberano:** antes de rede ou enumeração systemd, o
    watchdog consulta `/var/lib/civm/maintenance.json`. Snapshot presente
    retorna `runner-restart-skipped/maintenance-active` com exit 0 e zero
    mutações; consulta ambígua retorna `maintenance-state-unknown`, exit 1 e
    zero mutações. O timer nunca pode ressuscitar listener drenado.
  - `Options` ganha `Ctx`-awareness: `cleanWorkRoot` e `trimCacheByAge` passam a receber/usar `ctx` (RF-8/DT-2) — a assinatura interna passa `ctx context.Context` (vinda de `cleanup(opts, ctx, ...)`, que já recebe ctx em `hook.go:163`). Escalada e `WalkDir` checam `ctx.Err()` entre entradas.
  - `cleanWorkRoot` (`hook.go:211-250`): no laço de entradas, troca `opts.RemoveAllFn(path)` por `safedelete.Remove(ctx, sdOpts, path)` onde `sdOpts.GuardFn` valida "filho direto de um `safeWorkRoot()`"; **acumula** erros (`a.Error` recebe o primeiro, mas o laço **continua**) em vez de `return` na primeira falha; grava `escalated` por entrada (campo novo no `Action`, abaixo).
  - `Action` (`hook.go:41-49`): adiciona `Escalation string json:"escalation,omitempty"` (`none|ok|failed`) para o contador de decisões (RF-8/DT-13).
  - `EventJobCompleted` (`hook.go:138-151`): após `cleanup(...)`, em vez de `firstActionError → errorResult` incondicional, aplica `demoteCleanupHygieneErrors(res.Actions)` (novo) e só vai a `errorResult` se restar erro **fatal** (config inseguro/statfs). RF-2/DT-5/DT-6.
  - `isIgnorableCacheDeleteRace` (`hook.go:534-539`) é substituído por `classifyCleanupError(a Action) cleanupErrorClass` (DT-6): cobre `Name ∈ {work_root, cache, cache_trim}` e mensagens `permission denied`/`directory not empty`/`no such file or directory`; `onlyIgnorableCacheDeleteRaces`/`demoteIgnorableCacheDeleteRaces` (`hook.go:511-532`) passam a usar o classificador genérico, reusados pelo `job-started` (sem regressão).
  - `runWithTimeout` (`hook.go:401-417`): captura o `CombinedOutput` (`RunFn` já o retorna) e anexa um tail truncado (≤ `civm.DefaultHookWarnOutputTailBytes`) ao `a.Warning` para diagnóstico (RF-5).
  - `cleanup` (`hook.go:163-209`): o `docker_buildx_prune` (`hook.go:197`) passa por um helper `dockerBuildxPruneOrFallback` que, ao ver `exit 125`, troca para `docker builder prune --force --filter until=24h` no mesmo job (RF-5/DT-10).
- **Requisitos:** RF-1, RF-2, RF-5, RF-8.
- **Impacto:** `job-completed` deixa de retornar exit 1 por erro de filesystem; `job-started` mantém o gate `HardFailPct`. Aditivo no `Action` (`escalation` omitempty).
- **Testes:** `cleanWorkRoot` com `RemoveAllFn` EACCES → `safedelete` escala (mock `RunFn`), entrada some, `escalation="ok"`, demais entradas processadas (não retorna na primeira); `safedelete` falha → `escalation="failed"`, erro acumulado; `job-completed` com erro `work_root` permission pós-fallback + `cache_trim` ENOTEMPTY → exit 0, ambos `Warning`; `job-completed` com `unsafe work root`/statfs failure → fatal (exit 1); `job-started` rejeita (exit 75) **só** com disco `>= HardFailPct`, nunca por permission de cleanup; `runWithTimeout` anexa tail no Warning; buildx exit 125 → fallback `docker builder prune` chamado; context cancelado → laço de `cleanWorkRoot`/`trimCacheByAge` para.
- **Disciplina Kahneman:** #1/#3/#5 — ver Mapa.

### `internal/hook/install.go` — ITEM-7

- **O que muda:**
  - `InstallOptions` ganha `RenameFn func(old, new string) error` (default `os.Rename`) e `RunnerUID`/`RunnerGID` (passados a `safedelete` capability check). Mantém os `*Fn` existentes para testabilidade.
  - `Install` (`install.go:90-156`), no bloco `opts.Execute`: chama `installScopedSudoers(opts)` (novo) — escreve `deploy/sudoers.d/civm-cleanup` (conteúdo embutido via `//go:embed` ou constante) em `/etc/sudoers.d/civm-cleanup.tmp` (0440), roda `RunFn(ctx,"visudo","-cf",tmp)`; só em sucesso `RenameFn(tmp, "/etc/sudoers.d/civm-cleanup")`; falha de `visudo` → erro (não instala parcial). Idempotente (sobrescreve).
  - `Install` chama `ensureBuildxCapability(ctx, opts)` (novo): `RunFn(ctx,"docker","buildx","ls")`; se erro/sem builder → tenta `docker buildx create --name civm --use` (idempotente); se ainda falhar, marca para o fallback `docker builder prune` (estado no `InstallResult`). Best-effort: falha de buildx capability **não** falha o install (degrada para fallback). RF-5/DT-10.
  - `InstallResult` ganha `SudoersInstalled bool` e `BuildxBuilder string`/`BuildxFallback bool` para observabilidade do `doctor`.
- **Requisitos:** RF-1, RF-5. DT-9/DT-10.
- **Impacto:** `hook install --execute` passa a instalar sudoers escopado + capability buildx; sem ele a escalada cai no fallback warning (não fatal). Aditivo nos structs.
- **Testes:** `installScopedSudoers` escreve tmp → `visudo -cf` ok → `RenameFn` chamado com path final; `visudo` falha → erro, `RenameFn` **não** chamado; idempotência (re-run sobrescreve); `ensureBuildxCapability` sem builder → `buildx create` chamado; buildx ausente → `BuildxFallback=true`, install **não** falha.

### `internal/cleanup/cleanup.go` — ITEM-4/ITEM-4b

- **O que muda:**
  - `Options` ganha `SafeDeleteFn func(ctx context.Context, path string) (bool, error)` (default wrappa `safedelete.Remove` com `GuardFn` = "filho de cleanup root validado").
  - `scanAndMaybeDelete` (`cleanup.go:215-278`): o laço de delete (`cleanup.go:269-275`) troca `opts.RunFn(ctx,"rm","-rf",candidate.path)` por `opts.SafeDeleteFn(ctx, candidate.path)` e **não dá `break` no primeiro erro** — acumula (`a.Err` recebe o primeiro; o laço continua) e segue para os demais candidatos; sumário não-fatal. `validateCleanupRoot` (`cleanup.go:290-311`) continua o guard de root e vira o `GuardFn` base do `safedelete` aqui.
  - `dockerPrune` (`cleanup.go:334-356`): o caminho `Execute` (`cleanup.go:348`) troca `docker system prune -af --volumes` por o conjunto concorrência-seguro (`docker buildx prune --force --filter until=24h` com fallback `builder prune`; `docker image prune -f`; `docker container prune -f`; `docker volume prune -f` **só** quando `opts.LockActiveFn` reporta sem holder fresco). `Action` agrega `BytesFreed` por sub-prune. RF-7/DT-7. O early-return por docker-heavy lock já existe em `Run` (`cleanup.go:127-132`) — preservado.
- **Requisitos:** RF-3, RF-7. DT-1/DT-7.
- **Impacto:** o cron deixa de corromper estado de sibling job; root-owned em `_work`/tmp passa a ser limpo pela mesma escalada do hook. `Action.Err` agora é "primeiro erro de N", não "abortei no primeiro".
- **Testes:** candidato root-owned → `SafeDeleteFn` escala (mock), demais candidatos processados, sumário não-fatal; `dockerPrune` Execute **nunca** chama `docker system prune ... --volumes` (regressão); `volume prune` só sem holder fresco (`LockActiveFn=true` → pulado, Action anota "deferred-volume-prune-by-lock"); early-return por docker-heavy lock preservado.

### `internal/doctor/doctor.go` — ITEM-9

- **O que muda:**
  - `collectHookChecks` (`doctor.go:276-288`) ganha dois checks novos via helpers:
    - `checkScopedSudoers(opts)`: roda `opts.RunFn(ctx,"sudo","-n","-l")` (ou lê `/etc/sudoers.d/civm-cleanup` por `ReadFileFn`) e confirma que `chown`/`rm` NOPASSWD escopado existe; ausente → `HookCheck{Severity: SeverityCritical, Detail: "...; run sudo civmctl hook install --execute"}`. **CRITICAL**, não warning (DT-9). A função recebe `ctx` (assinatura de `Collect` já tem `ctx`).
    - `checkBuildxCapability(opts)`: detecta buildx-125 recorrente (lê o tail de `hooks.jsonl` por `ReadFileFn` contando `docker_buildx_prune ... exit status 125` recente) → `SeverityWarning` com a contagem; sem flood → OK.
  - `Options` ganha `RunFn`/`ReadFileFn` já presentes (`doctor.go:118-121`); só adiciona um `HooksLogPath` (default `/var/log/civm/hooks.jsonl`) para o check buildx.
- **Requisitos:** RF-1, RF-5. DT-9/DT-10.
- **Impacto:** `civmctl doctor` (exit por severidade, `doctor.go:38-47`) vira CRITICAL (exit 2) quando o sudoers escopado falta — antes do próximo arquivo root-owned brickar o runner. Aditivo nos checks.
- **Testes:** sudoers ausente → `checkScopedSudoers` CRITICAL, `report.Exit=2`; sudoers presente → OK; `hooks.jsonl` com flood de 125 → buildx check WARN; sem flood → OK.

### `internal/runner/watchdog.go` — ITEM-10

- **O que muda:**
  - **Host-busy/host-idle-unknown skip → exit 0 (RF-6):** o bloco `idleResult.Status != idle.StatusIdle` (`watchdog.go:190-203`) hoje faz `report.Exit = maxExit(report.Exit, 1); return`. Passa a `report.Exit = maxExit(report.Exit, 0)` (no-op saudável; warning logado via `report.add(... Severity:"warning" ...)`, já presente) e **return exit 0**. `report.Exit = maxExit(report.Exit, 1)` em `watchdog.go:201` é removido neste caminho. Non-zero fica reservado para falha real (hook-install/runner-restart falhou → 2 em `watchdog.go:207/212`; runner offline pós-reparo → 1 em `watchdog.go:218-221`).
  - **Auto-recuperação por sentinela (RF-6/DT-8):** novo passo **antes** do idle skip, `detectBrokenRunner(ctx, opts)`: lê o tail de `hooks.jsonl` (`opts.ReadFileFn` + `opts.HooksLogPath` novo) procurando a sentinela recente (decisão `error` por filesystem **OU** Action `work_root` com `escalation=failed`) e mapeia para a unidade `civm-<slot>` (via `systemd []Status` já coletado + slot do `WorkingDirectory`); para cada unidade afetada, se sob o teto `civm.DefaultRunnerAutoRestartPerHour` (estado no `MarkerPath`/`DefaultWatchdogMarkerPath`, já gravado por `writeRerunState`/`loadRerunState`), chama `runner.Restart{Unit: unit, Execute: opts.Execute}` (reusa `restart.go`), emite `WatchdogEvent{Event:"runner-auto-restarted", Severity:"warning", Unit:unit, Reason:"broken-runner-sentinel"}` + incrementa o contador; acima do teto → WARN sem restart (anti restart-loop, DT-8).
  - `WatchdogOptions` ganha `HooksLogPath string` (default `/var/log/civm/hooks.jsonl`) e `AutoRestartPerHour int` (default `civm.DefaultRunnerAutoRestartPerHour`), injetáveis.
- **Requisitos:** RF-6. DT-8.
- **Impacto:** `systemctl status civmctl-runner-watchdog` deixa de aparecer em `systemctl --failed` num skip host-busy; runner quebrado auto-recupera dentro do limite. Aditivo nos options.
- **Testes:** maintenance presente/ambígua → zero rede/systemd/hooks/restart;
  host-busy skip → `report.Exit == 0` (sem `maxExit(...,1)`);
  host-idle-unknown skip → `Exit == 0`; sentinela de runner quebrado
  (`work_root escalation=failed` recente) → `runner.Restart` da unidade correta
  (mock `RunFn`), evento `runner-auto-restarted`; teto atingido → WARN sem
  restart; sentinela ausente → nenhum restart; falha real (hook-install) →
  ainda exit 2.
- **Disciplina Kahneman:** #5 — ver Mapa.

### `internal/hostdisk/hostdisk.go` — ITEM-5 (parte host obs)

- **O que muda:** `Metrics` (`hostdisk.go:40-62`) ganha `VHDXBlockSizeBytes int64 json:"vhdx_block_size_bytes"`. `Report` ganha `BlockSizeReclaimBlocker bool json:"block_size_reclaim_blocker"`. `Check` (`hostdisk.go:134-174`) seta `BlockSizeReclaimBlocker = m.VHDXBlockSizeBytes > civm.DefaultVHDXTargetBlockSizeBytes` (1 MiB); quando true e não-stale, anexa ao `Reason` "vhdx_block_size > 1MiB: offline Optimize will reclaim little; Convert-VHD -BlockSizeBytes 1MB required". Não muda o `Level` (continua por `VFreeGB`/stale); é flag de bloqueador real, não floor.
- **Requisitos:** RF-4. DT-11.
- **Impacto:** aditivo; `host-metrics.json` antigos sem o campo → `VHDXBlockSizeBytes=0` → `BlockSizeReclaimBlocker=false` (degrada silencioso, não falso-positivo). `RenderText` (`hostdisk.go:213-225`) imprime a flag.
- **Testes:** `vhdx_block_size_bytes` 33554432 (32 MiB) → `BlockSizeReclaimBlocker=true` + reason; 1048576 (1 MiB) → false; ausente (0) → false; o flag não altera `Level` por si só.

### `internal/diskdoctor/diskdoctor.go` — ITEM-5 (parte diagnóstico)

- **O que muda:** `Report` (`diskdoctor.go:43-56`) ganha `VHDXBlockSizeBytes int64 json:"vhdx_block_size_bytes,omitempty"` e `BlockSizeReclaimBlocker bool json:"block_size_reclaim_blocker,omitempty"`, lidos do `host-metrics.json` em `HostMetricsPath` (já lido por `hostHeadroomViolation`, `diskdoctor.go:282-295` — estende o `hostMetrics` struct local `diskdoctor.go:86-88` com `VHDXBlockSizeBytes`). `composeRootCause` (`diskdoctor.go:266-277`) ganha um passo: quando `BlockSizeReclaimBlocker` (BlockSize > 1 MiB) **e** `TrimEffective`, o root cause deixa de ser o falso `rootCauseTrimSupported` ("online shrink expected") e vira a nova constante `rootCauseBlockSizeTooLarge = "VHDX BlockSize > 1MiB: online UNMAP not honored on this box; Convert-VHD -BlockSizeBytes 1MB + offline Optimize required"`. Corrige o falso "online shrink expected" no box (PRD §2 família 2).
- **Requisitos:** RF-4. DT-11. Reconcilia `host-volume-reclamation/SPECv2.md` DT-v2-10 (cuja árvore parava em "TRIM supported, online shrink expected").
- **Impacto:** aditivo; sem `host-metrics.json`/sem o campo, `composeRootCause` mantém a árvore atual (degrada para o comportamento anterior).
- **Testes:** BlockSize 32 MiB + discard + DISC-MAX>0 → `rootCauseBlockSizeTooLarge` (não `rootCauseTrimSupported`); BlockSize 1 MiB + tudo ok → `rootCauseTrimSupported`; sem host-metrics → árvore atual inalterada.

### `internal/civm/civm.go` — ITEM-12

- **O que muda:** adicionar ao bloco `const (...)` (`civm.go:15-99`), após o bloco de host-volume (`civm.go:63-74`).
- **Requisitos:** RF-1, RF-4, RF-5, RF-6, RF-8.
- **Depois (acrescentar — valores, sem código de lógica):**
  - `DefaultSudoersDropInPath = "/etc/sudoers.d/civm-cleanup"` — destino do drop-in escopado.
  - `DefaultHookWarnOutputTailBytes = 512` — tail de output anexado ao Warning de `runWithTimeout` (RF-5).
  - `DefaultVHDXTargetBlockSizeBytes = 1 << 20` — 1 MiB; alvo do `Convert-VHD`; acima disso `BlockSizeReclaimBlocker` (RF-4).
  - `DefaultRunnerAutoRestartPerHour = 3` — teto de auto-restart por unidade/hora no watchdog (anti restart-loop, RF-6/DT-8).
  - `DefaultHooksLogPath = "/var/log/civm/hooks.jsonl"` — fonte do contador de decisões + sentinela de runner quebrado (RF-8). (Hoje hardcoded em `hook.go:94`; promovido a constante.)
- **Impacto:** aditivo; nenhum caller quebra. `DefaultRunDir = "/run/civm"` já é coberto por `DefaultDockerHeavyLockPath` (`civm.go:90`); o tmpfiles.d usa o literal no `deploy/`.

### `cmd/civmctl/hook.go` — ITEM-7 (CLI)

- **O que muda:** `runHookInstall` (`cmd/civmctl/hook.go:57-93`) propaga os novos campos do `InstallResult` (`SudoersInstalled`, `BuildxBuilder`/`BuildxFallback`) ao render texto/JSON; sem novas flags (o sudoers + buildx capability são parte de `--execute`, não opt-in). `printHelp` em `main.go` ganha uma linha em `hook` indicando "instala sudoers escopado + buildx capability".
- **Requisitos:** RF-1, RF-5.
- **Impacto:** aditivo no render; comportamento sob `--execute` ampliado.
- **Testes:** `main_test.go` — `hook install --execute --json` mostra `sudoers_installed`/`buildx_*` (mock `RunFn`).

### `cmd/civmctl/doctor.go` — ITEM-9 (CLI)

- **O que muda:** `runDoctor` propaga `HooksLogPath` ao `doctor.Options` (default `civm.DefaultHooksLogPath`); sem nova flag obrigatória. O exit por severidade (`doctor.Severity.ExitCode`) já cobre CRITICAL→2.
- **Requisitos:** RF-1, RF-5.
- **Impacto:** aditivo.
- **Testes:** dispatch + exit code 2 quando sudoers ausente (mock).

### `cmd/civmctl/cleanup.go` — ITEM-4 (CLI)

- **O que muda:** `runCleanup` injeta `SafeDeleteFn`/`LockActiveFn` defaults (já vêm de `cleanup.DefaultOptions`); sem nova flag. Render anota quando `volume prune` foi deferido por lock.
- **Requisitos:** RF-3, RF-7.
- **Impacto:** aditivo.
- **Testes:** `main_test.go` — dispatch; `cleanup --dry-run` não chama prune perigoso.

### `deploy/windows/civm-host-metrics.ps1` — ITEM-13

- **O que muda:** o hashtable `$metrics` (linhas ~144-153) ganha `vhdx_block_size_bytes = [int64]$vhd.BlockSize` (de `Get-VHD -Path` já chamado em `civm-host-metrics.ps1:104`). Entregue ao guest no mesmo JSON. **Gated por confirmação:** `Get-VHD .BlockSize` deve ser confirmado em host elevado antes de afirmar 1 MiB (Kahneman #1, Mapa).
- **Requisitos:** RF-4. DT-11.
- **Impacto:** aditivo no JSON; `hostdisk`/`diskdoctor` consomem o campo.
- **Testes:** validação manual (rodar a task; checar `vhdx_block_size_bytes` no host e no guest); PSScriptAnalyzer se disponível.

### `deploy/windows/civm-vhdx-optimize.ps1` — ITEM-14

- **O que muda:** antes do `Optimize-VHD -Mode Full` (após `Convert-VhdxToScsi`, `civm-vhdx-optimize.ps1:330-339`), adicionar uma asserção: `$vhd.BlockSize` (de `Get-VHD`, já obtido em `:332`); se `-ne 1MB` → `Write-CivmLog -Event 'block_size_warn' -Level 'WARN' -Data @{ block_size_bytes = $vhd.BlockSize; expected = 1MB }` (o Optimize segue, mas o log avisa que o ganho será baixo e que o `Convert-VHD -BlockSizeBytes 1MB` one-time é necessário). `.NOTES`/`.DESCRIPTION` documenta o `Convert-VHD` one-time como passo de runbook (VM Off, headroom guard, janela) — **não** scriptado aqui, **não** automatizado (DT-11).
- **Requisitos:** RF-4. DT-11.
- **Impacto:** aditivo (uma asserção + log + doc); o caminho atual (SCSI re-attach + Optimize + finally Start-VM) é preservado.
- **Testes:** validação manual em janela (BlockSize ≠ 1 MiB → `block_size_warn` logado; Optimize ainda roda + VM volta Running).

## Arquivos a DELETAR (se houver)

| Arquivo | Motivo |
| --- | --- |
| — | Nenhum. Tudo aditivo/cirúrgico. O padrão `os.RemoveAll`-direto em `cleanWorkRoot`/`scanAndMaybeDelete` é **substituído** por `safedelete.Remove` no mesmo arquivo (não há arquivo a remover). `isIgnorableCacheDeleteRace` é generalizado em `classifyCleanupError` (substituição in-file, não deleção). |

## Observabilidade

**`hooks.jsonl` (guest):** linhas já existentes (`event`, `decision`, `exit_code`, `disk_used_pct`, `repository`, `run_id`, `actions`) + por Action `work_root`: `escalation` (`none|ok|failed`). Sentinela de runner quebrado = `decision=error` por filesystem OU `work_root.escalation=failed`. Sem PII; paths de `_work` (não conteúdo).

**`civmctl doctor`:** checks novos `SCOPED_SUDOERS` (CRITICAL se ausente) e `BUILDX_CAPABILITY` (WARN no flood de 125). Exit 2 (CRITICAL) destrava antes do próximo brick.

**`civmctl runner watchdog`:** eventos `runner-restart-skipped`/`rerun-skipped` host-busy → warning + exit 0; novo `runner-auto-restarted` (warning) por unidade recuperada; contador por unidade no `MarkerPath`.

**`host-metrics.json`/`civmctl host-disk`/`disk-doctor`:** `vhdx_block_size_bytes`; `block_size_reclaim_blocker` (hostdisk) / `rootCauseBlockSizeTooLarge` (diskdoctor) quando BlockSize > 1 MiB.

Sem segredo, sem label de alta cardinalidade (event/decision/escalation/unit), sem slug/tenant.

## Contratos e documentação viva

| Documento | Atualização | Motivo |
| --- | --- | --- |
| `runbooks/RUNBOOK-CIVM-RUNNER-RELIABILITY.md` | Criar | escalada root-owned, fail-open, tasks Day-0, 1 MB BlockSize, buildx-125, auto-recuperação, tmpfiles.d |
| `docs/specs/host-volume-reclamation/SPECv2.md` | Alterar | UNMAP online NÃO honrado neste box; primário = 1 MB BlockSize + Optimize offline; demover DT-v2-10 step (5) a "verificar-mas-não-confiar"; fechar DT-v2-19 com low-water observado |
| `docs/specs/multi-project-isolation/SPECv2.md` | Alterar | referência cruzada: `/run/civm` materializado por `deploy/tmpfiles.d/civm.conf` (lock docker-heavy durável no boot) |
| `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md` | Alterar | §reclaim primário = `Convert-VHD 1MB` + Optimize offline (não SCSI/discard online); cross-link ao novo runbook |
| `runbooks/MULTI-PROJECT-RUNNER.md` | Alterar | §Disk pressure / §Rollback trigger: root-owned + buildx + auto-recuperação do runner |
| `deploy/sudoers.d/civm-cleanup`, `deploy/tmpfiles.d/civm.conf` | Criar | NOPASSWD escopado + `/run/civm` durável |
| `deploy/systemd/README.md` | Alterar | cross-ref guest (sudoers/tmpfiles) ↔ host (tasks) + watchdog auto-restart |
| `cmd/civmctl/main.go` `printHelp` | Alterar | `hook`/`doctor` estendidos (sudoers + buildx capability) |
| `AGENTS.md`/`CODEX.md` | Alterar | boundary: civm com sudoers escopado + tmpfiles.d + tasks de host |
| `disciplines/KAHNEMAN-DISCIPLINES.md` | N/A | sem nova disciplina |
| `docs/openapi/*`/SDK/eventos | N/A | sem contrato de produto |

## Ordem de implementação

1. **ITEM-0 — Baseline (Slice 0):** colar no IMPL: `grep -c '"decision":"error"' /var/log/civm/hooks.jsonl`, count de `exit status 125`, `civmctl disk-doctor --json`, `docker system df`, `Get-VHD` (elevado, BlockSize). Sem código.
2. **ITEM-12 — Constantes** (`internal/civm/civm.go`).
3. **ITEM-1 — `internal/safedelete`** + testes (guard duplo, escalada injetável).
4. **ITEM-2/ITEM-3 — `internal/hook/hook.go`** (`cleanWorkRoot` via safedelete + acumula + context; `EventJobCompleted` fail-open; `classifyCleanupError`) + testes.
5. **ITEM-4/ITEM-4b — `internal/cleanup/cleanup.go`** (safedelete + dockerPrune isolation-aware) + testes.
6. **ITEM-6 — `deploy/sudoers.d/civm-cleanup`** + **ITEM-7 — `internal/hook/install.go`** (instala sudoers via `visudo -cf`+rename; buildx capability) + **ITEM-8** (`runWithTimeout` tail + buildx fallback) + testes.
7. **ITEM-9 — `internal/doctor/doctor.go`** (`checkScopedSudoers` CRITICAL; `checkBuildxCapability`) + `cmd/civmctl/{hook,doctor}.go` + testes + `main.go` help.
8. **ITEM-10 — `internal/runner/watchdog.go`** (host-busy → exit 0; auto-restart por sentinela + teto) + testes.
9. **ITEM-15 — `deploy/tmpfiles.d/civm.conf`** (`/run/civm` durável).
10. **ITEM-11 (contador)** já entregue por ITEM-2 (`escalation` em `hooks.jsonl`); validar a sentinela end-to-end.
11. **ITEM-5 — `internal/hostdisk` + `internal/diskdoctor`** (`vhdx_block_size_bytes` + flag/root-cause) + testes.
12. **ITEM-13/ITEM-14 — `deploy/windows/*.ps1`** (host-metrics emite BlockSize; optimize asserta) — **gated** por `Get-VHD .BlockSize` confirmado em host elevado.
13. **ITEM-16 — Registrar as 3 Scheduled Tasks** (ação humana elevada Day-0): `register-civm-host-metrics.ps1`, `register-civm-vhdx-optimize.ps1`; pós-registro `host-metrics.json` no guest em ≤10 min, `civmctl host-disk=ok`.
14. **ITEM-17 — Runbook novo + reconciliar SPECv2/MULTI-PROJECT-RUNNER/HOST-VHDX**.
15. **Prova end-to-end:** um job com arquivo root-owned no `_work` que hoje quebraria o runner termina com "Complete runner" verde e o runner segue aceitando jobs; um ciclo Optimize offline (VHDX 1 MiB) recupera `v_free_gb` com low-water registrado.

## Plano de testes

**Go (civm) — unitários (`go test ./... -race -count=1`):**

- `safedelete`: path seguro sem sudo; EACCES → `chown` então `rm -rf` (mock `RunFn`), `escalated=true`; ambos falham → erro claro; `GuardFn` reprova → `RunFn` nunca chamado; `/`/`$HOME`/`/home/x` bare/relativo/NUL recusados; context cancelado.
- `hook`: `cleanWorkRoot` EACCES → escala, acumula erros, processa demais entradas, `escalation` setado; `job-completed` permission/ENOTEMPTY pós-fallback → exit 0 (Warning); `unsafe work root`/statfs → fatal; `job-started` rejeita só em `>= HardFailPct`; `classifyCleanupError` cobre `work_root`+permission; `runWithTimeout` tail no Warning; buildx 125 → fallback `builder prune`; context cancela laços.
- `install`: `installScopedSudoers` `visudo -cf` ok → rename; visudo falha → não-rename; idempotência; `ensureBuildxCapability` cria builder / marca fallback sem falhar install.
- `cleanup`: root-owned → `SafeDeleteFn` escala + não-aborta; `dockerPrune` nunca `system prune --volumes`; `volume prune` só sem holder; early-return por docker-heavy lock preservado.
- `doctor`: sudoers ausente → CRITICAL exit 2; presente → OK; flood 125 → buildx WARN.
- `watchdog`: host-busy/idle-unknown skip → exit 0; sentinela broken-runner → `runner.Restart` da unidade correta; teto → WARN sem restart; falha real → exit 2.
- `hostdisk`: `vhdx_block_size_bytes` 32 MiB → blocker+reason; 1 MiB → não; ausente → não; Level inalterado.
- `diskdoctor`: 32 MiB + discard + DISC-MAX>0 → `rootCauseBlockSizeTooLarge`; 1 MiB → `rootCauseTrimSupported`; sem host-metrics → árvore atual.
- `cmd/civmctl`: dispatch + exit codes (`main_test.go`).

**Go — integração (guest, `-race`):**

- `safedelete` num `_work` temp com arquivo do próprio user (sem sudo real); guard rejeitando `/tmp` fora de `_work`.
- `cleanup` dry-run no guest real não emite prune perigoso.

**Host (manual/scriptado, em janela):**

- `Get-VHD .BlockSize` em host elevado (confirma 1 MiB — gate de ITEM-13/14).
- `civm-host-metrics` task emite `vhdx_block_size_bytes` no host e no guest; `civmctl host-disk` lê.
- `civm-vhdx-optimize`: `block_size_warn` se ≠ 1 MiB; Optimize roda; VM volta Running; low-water de scratch registrado (fecha DT-v2-19).
- `Convert-VHD -BlockSizeBytes 1MB` one-time (VM Off, headroom guard) — medir VHDX FileSize/`v_free` antes/depois (3 medições).

**Manuais (evidência das etapas críticas):**

- `hooks.jsonl` 3 dias pós-RF-1: `decision=error` por filesystem = 0.
- `docker system df` antes/depois do fallback buildx (reclamação líquida).
- `systemctl --failed` sem `civmctl-runner-watchdog` num skip host-busy.

## Checklist de validação

**Go (civm)**

- [ ] `gofmt -w ./...`
- [ ] `golangci-lint run -c .golangci.yml ./...` (0 issues)
- [ ] `go test ./... -race -count=1`
- [ ] `go build -o /tmp/civmctl ./cmd/civmctl`
- [ ] Import-cycle: `safedelete` importa só `civm`+stdlib (`go list -deps ./internal/safedelete | grep -E 'internal/(hook|cleanup)'` vazio)

**Guest (deploy)**

- [ ] `visudo -cf deploy/sudoers.d/civm-cleanup` válido
- [ ] `sudo civmctl hook install --execute` instala o drop-in + buildx capability; `civmctl doctor` OK
- [ ] Pós-reboot `/run/civm` recriado (tmpfiles.d) dono `emdev`; job docker-heavy materializa `.hb`

**Host (PowerShell)**

- [ ] PSScriptAnalyzer nos `.ps1` (se disponível)
- [ ] `Get-VHD .BlockSize` confirmado (gate ITEM-13/14)
- [ ] 3 Scheduled Tasks registradas (`schtasks /query`); `host-metrics.json` no guest em ≤10 min; `civmctl host-disk=ok`

**Gates cognitivos**

- [ ] Cada etapa crítica aponta `disciplines/KAHNEMAN-DISCIPLINES.md` (Mapa preenchido)
- [ ] Pergunta obrigatória, evidência mínima e abort trigger por etapa crítica
- [ ] Rollback trigger numérico definido (filesystem-error→0 em 3 dias / sudo fora do escopo / Optimize 1 MiB não recupera / restart-loop > teto)
- [ ] `Get-VHD .BlockSize` declarado "a confirmar em host elevado" antes de afirmar 1 MiB (Kahneman #1)
- [ ] Zero-fill proibido sob baixo headroom + VM nunca Off (herdado de SPECv2) preservados
