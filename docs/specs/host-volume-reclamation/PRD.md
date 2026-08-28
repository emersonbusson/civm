---
slug: host-volume-reclamation
title: Reclamação de volume do host (VHDX) — guest-free vira host-free com segurança
milestone: —
issues: []
---

# PRD — Reclamação de volume do host (VHDX): guest-free vira host-free com segurança

> Tipo: nova capacidade de plataforma do runner (`civmctl` guest + componente Windows host + runbook). Sem schema de banco, sem endpoint de produto.
> Política Day-0: civm não tem produção viva com dados legados; backfill = N/A. Solução primária única.
> Origem: sessão de 2026-05-29 onde o host Windows `V:` chegou a 3 GB livres (98%) com o guest Linux em 44 GB livres (58%); a compactação foi manual (drain de label, `fstrim`, zero-fill perigoso, `Optimize-VHD` interativo via UAC que pendurou) e levou ~1h18m sem atingir a meta. Este PRD substitui esse procedimento por um primitivo civm correto, seguro e automatizado.

---

## 1. Resumo

O runner civm roda numa VM Hyper-V Linux (`gha-ubuntu-2404`) cujo disco é um **VHDX dinâmico** num volume Windows `V:` de ~120 GB. O VHDX cresce quando o guest escreve, mas **não encolhe** quando o guest libera blocos. Observado (sessão 2026-05-29): guest `/dev/sda2` 108 GB a 58-62% (≈44 GB livres), mas `gha-ubuntu-2404.vhdx` ≈ 116 GB (116.068.974.592 bytes) ocupando quase todo o `V:`, que ficou com **3 GB livres (98%)**.

Esse descompasso é o problema. Três causas que o civm hoje **não trata** (Confirmado no codebase):

1. **Pipeline de discard quebrado e não diagnosticado.** O civm roda `sudo fstrim -av` cegamente (`internal/hook/hook.go:200`), mas **nenhum** código verifica se o discard chega ao VHDX: não lê `/proc/mounts`, não checa `discard`, não detecta controlador SCSI vs IDE, não checa suporte a TRIM via `lsblk -D`. Na sessão, `fstrim -av` liberou só **737,8 MiB** — sinal de que os blocos liberados pelo guest **não** viram UNMAP para o VHDX. Sem isso, o VHDX só encolhe por compactação offline (`Optimize-VHD`), que só recupera blocos zerados/trimados.
2. **Zero observabilidade do host.** `capacity.Report` faz `Statfs` só no guest `/` (`internal/capacity/capacity.go:17-26,52-62`); civm **não enxerga** o volume `V:` nem o tamanho do VHDX. Ninguém viu a parede de 3 GB chegar.
3. **VHDX pode encher o host inteiro.** O VHDX dinâmico cresce até ~120 GB num `V:` de 120 GB — **sem headroom** para o scratch do `Optimize-VHD` nem para crescimento. O runbook fala de "VM 128GB SSD" e do guest, mas **não** menciona VHDX, `V:` nem o teto do host (Confirmado em docs — `runbooks/MULTI-PROJECT-RUNNER.md` §Disk pressure).

E a correção é **inteiramente manual** (Confirmado no codebase — zero `.ps1`/`Optimize-VHD`/`Get-VM`/`schtasks` no repo): o operador teve que (a) drenar runners removendo a label `civm` via `gh api` + `systemctl stop`, (b) rodar `fstrim` ineficaz, (c) tentar zero-fill **perigoso** (escrever zeros no guest **cresce** o VHDX; com 3 GB livres no host pode estourar o `V:`), (d) abrir `Optimize-VHD` elevado interativo via `Start-Process -Verb RunAs`, que **pendurou** esperando UAC e não reduziu o VHDX por falta de blocos trimados, (e) restaurar a label. Resultado: ~1h18m, host ainda em 3 GB livres.

Este PRD entrega um primitivo civm **melhor que o manual** porque ataca a **raiz** em vez do sintoma:

- **Diagnostica** por que o `fstrim` não recupera espaço (RF-1).
- **Conserta o pipeline de discard** (VHDX em SCSI + discard no guest) para o VHDX **encolher automaticamente** com o `fstrim` que já roda — eliminando a maior parte das compactações offline (RF-2).
- **Dá observabilidade do host** (volume `V:` + tamanho do VHDX + gap guest×host) para alarmar **antes** dos pisos de 30 GB/10 GB (RF-3).
- **Automatiza a compactação offline segura e não-interativa** (Scheduled Task como SYSTEM, sem UAC; drain→shutdown gracioso→`Optimize-VHD`→start→restore; **proíbe zero-fill sob baixo headroom**) — substituindo a dança manual (RF-4).
- **Dá um primitivo de drain** (`civmctl maintenance enter|exit`) idempotente, no lugar da remoção manual de label (RF-5).
- **Right-sizing estrutural** (teto do VHDX abaixo do `V:`, ou expandir/mover) para o host nunca poder ser preenchido (RF-6).

Valor: o runner para de morrer por disco do host de forma silenciosa; a manutenção vira um comando seguro e auditável em vez de uma operação manual de 1h+ com risco de estourar o volume.

---

## 2. Contexto técnico

### Topologia (Confirmado em observação operacional + docs)

```
Windows host (EMEDEV)  ── Hyper-V ──> VM Linux "gha-ubuntu-2404" (guest)
  V: (120 GB NTFS)                      /dev/sda2 (108 GB ext4)  ← runners + civmctl
   └─ gha-ubuntu-2404.vhdx (dinâmico,   └─ civmctl-*.timer (cleanup/watchdog/...)
      ≈116 GB, cresce, não encolhe)
  WSL (operador) ── powershell.exe + ssh gha-ubuntu-2404 ──> guest
```

- civm é um binário Go que roda **no guest** (Ubuntu 24.04). Sem componente no host Windows hoje (Confirmado no codebase).
- O operador/agente roda em WSL no host e alcança o Windows via `powershell.exe` e o guest via `ssh gha-ubuntu-2404`.

### Estado atual confirmado no codebase

- **`fstrim`** roda em `internal/hook/hook.go:200` (`sudo fstrim -av`, timeout 120s) no `job-started` (modo pressão, `>=` 60%) e no `job-completed` (rotina). `cleanup.Run()` (`internal/cleanup/cleanup.go:96-119`) **não** roda fstrim — só prune/tmp/work/apt.
- **Sequência de cleanup do hook** (`hook.go:158-202`): clean `_work`, trim/purge caches (go-build 5GB, npm 3GB, yarn 3GB, pnpm 5GB; protege mtime < 24h), docker prune (`system prune -af --volumes` em pressão; `buildx/image/container/volume prune` em rotina), `apt-get clean`, `journalctl --vacuum-time=1d`, `fstrim -av`.
- **diskwatchdog** (`internal/diskwatchdog/diskwatchdog.go:76-138`) monitora só o guest `/`; threshold 60%; dispara `cleanup.Run()` agressivo. **Não toca o host.**
- **diskaudit** (`internal/diskaudit/diskaudit.go:118-154`) reporta roots do guest (`_work`, `_tool`, `_actions`, `~/.cache`, `~/go/pkg`, `~/codespace`, `/var/log`, `/var/cache`, docker reclaimable). **Não conhece VHDX nem discard.**
- **capacity.Report** (`internal/capacity/capacity.go:17-26`): `DiskPath, DiskUsedPct, DiskFreeGB, DiskTotalGB, RunnerServices, RunnerWorkers, AcceptingJobs, Reason` — `Statfs` no guest `/`. **Não vê o `V:`.**
- **idle.Check** (`internal/idle/idle.go:116-169`): busy se `Runner.Worker`/`docker build|compose`/`buildx`/`buildctl`/`/_work/`; ignora `civmctl cleanup|disk-watchdog|idle-check`. Reutilizável para gating de manutenção.
- **Nenhum primitivo de drain/maintenance** em civmctl (`cmd/civmctl/runner.go:18-42` só tem add/list/remove/restart/upgrade/watchdog). Drain hoje = `gh api DELETE/POST .../labels` + `systemctl stop actions.runner.*` manual.
- **Zero automação de host** (`.ps1`/`Optimize-VHD`/`Get-VM`/`Start-VM`/`schtasks`/`Hyper-V`): grep retorna nada (Confirmado no codebase). `internal/bootstrap` provisiona só o guest; **não** configura `discard` no fstab nem assume VHDX.
- **5 timers systemd no guest** (`deploy/systemd/`): cleanup (diário 04:00), disk-watchdog (horário), runner-watchdog (~2min), reverse-watchdog (4h), metrics (1min). **Nenhuma** task no host.
- **Constantes** (`internal/civm/civm.go:37-40`): PreCleanup 60%, HardFail 90%. **Runbook** (`MULTI-PROJECT-RUNNER.md`): "manter sempre >30GB livres" (§Disk pressure), rollback "90% >3× em 30 dias → escalar" e "disk free <10 GB → cleanup". Tudo **guest**; nada sobre VHDX/host.

### Confirmado na documentação oficial (Hyper-V/VHDX)

- VHDX **dinâmico** não encolhe sozinho ao liberar blocos no guest; encolhe via TRIM/UNMAP online (com o disco num controlador **SCSI** — o **IDE do Hyper-V não repassa UNMAP**) ou via `Optimize-VHD -Mode Full` com a VM desligada.
- `Optimize-VHD` exige privilégio de Hyper-V Admin/elevação; só recupera blocos zerados/descartados dentro do VHDX.
- `fstrim` (ou montagem com `discard`) só recupera espaço no VHDX se o caminho guest→controlador→VHDX repassa UNMAP (SCSI + VHDX dinâmico/thin).
- Escrever zeros (zero-fill) **cresce** um VHDX dinâmico até o tamanho do espaço livre do guest — perigoso quando o host tem pouco espaço.

### O que está sendo proposto (Inferência / proposta)

Diagnóstico do pipeline de discard (RF-1); correção para auto-shrink (RF-2); observabilidade do host via componente Windows + consumo no civm (RF-3); Scheduled Task de compactação offline segura e não-interativa (RF-4); `civmctl maintenance` (RF-5); right-sizing/headroom do VHDX (RF-6); runbook + contrato (RF-7).

### Tenant scope

N/A — sem dados de tenant. É infraestrutura de runner.

---

## 3. Opção recomendada

### Solução escolhida (raiz-primeiro, em camadas, Day-0 único)

1. **Diagnóstico (`civmctl disk-doctor`, guest) — RF-1.** Antes de qualquer ação, descobrir **por que** o `fstrim` não recupera espaço: controlador (SCSI vs IDE), `discard` em `/proc/mounts`, suporte a TRIM (`lsblk -D` com DISC-GRAN/DISC-MAX > 0), e teste de efetividade (free→fstrim→medir). Saída JSON com o root cause.
2. **Auto-shrink via discard correto — RF-2.** Se o diagnóstico mostrar IDE ou discard desligado, a correção é **re-anexar o VHDX a um controlador SCSI** (one-time, host) + garantir discard no guest (o `fstrim` periódico que já roda passa a ser efetivo). O VHDX **encolhe online**, sem `Optimize-VHD` offline na maioria dos casos.
3. **Observabilidade do host — RF-3.** Componente Windows (`deploy/windows/civm-host-metrics.ps1` + Scheduled Task) que emite `V:` free/size + VHDX FileSize/MinimumSize/Max + gap guest×host para um arquivo/endpoint que o civm consome e alarma **antes** de 30 GB/10 GB.
4. **Compactação offline segura e não-interativa — RF-4.** Scheduled Task `civm-vhdx-optimize` como **SYSTEM** (sem UAC interativo), acionável on-demand (`schtasks /run`) ou por threshold: drain → shutdown gracioso do guest → `Optimize-VHD -Mode Full` (timeout + tratamento de erro) → Start-VM → restore. Idempotente, abort-safe (**nunca** deixa a VM desligada), **proíbe zero-fill quando o host livre < headroom**.
5. **Drain primitivo (`civmctl maintenance enter|exit`, guest) — RF-5.** Idempotente: `enter` para `actions.runner.*` e/ou remove a label `civm` (gravando as labels anteriores), `exit` restaura. Substitui a dança manual de `gh api`.
6. **Right-sizing estrutural — RF-6.** Capar o **tamanho máximo do VHDX abaixo da capacidade do `V:`** (ex.: max 100 GB num volume de 120 GB) deixando scratch para `Optimize-VHD`; ou expandir `V:`/mover o VHDX. Invariante: "host `V:` free ≥ headroom".
7. **Contrato/docs — RF-7.** Novo `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md` + atualização de `MULTI-PROJECT-RUNNER.md` §Disk + `capacity` docs.

### Motivo da escolha

- **Conserta a raiz, não o sintoma.** Codex compactou uma vez, manualmente; este PRD faz o VHDX **parar de inchar** (discard correto) e torna a rara compactação offline **segura e automática**. A maior parte do problema some quando o `fstrim` que já roda passa a ser efetivo.
- **Fecha o ponto cego.** Observabilidade do host transforma "descobrir a 3 GB" em "alarmar a 30 GB" (Kahneman #5 worst-case; #3 número).
- **Elimina os modos de falha do manual.** SYSTEM scheduled task → sem UAC pendurado; ordem segura sem zero-fill sob baixo headroom → sem risco de estourar o `V:`; drain idempotente → sem label dance.
- **Reuso máximo.** `idle.Check` (gating), `fstrim`/cleanup (já existem), padrão `deploy/systemd/` (espelhado em `deploy/windows/`), dispatch por `switch`, `Report` estendível.
- **Fail-safe.** Compactação só com guest idle + drenado; abort se host livre < headroom; nunca deixa a VM off; nunca zero-fill perigoso.

### Alternativas descartadas

| Alternativa | Por que descartada |
| --- | --- |
| **Repetir o procedimento manual do Codex (compactar à mão quando estourar)** | Trata sintoma; interativo (UAC pendura); zero-fill perigoso; sem observabilidade → reincide. É o que estamos substituindo. |
| **Só compactação offline agendada, sem consertar discard** | Mantém o VHDX inchando entre janelas; exige downtime recorrente; não recupera nada se os blocos não foram trimados. Só como fallback. |
| **Zero-fill + Optimize-VHD como caminho padrão** | Zero-fill cresce o VHDX; sob 3 GB livres no host pode estourar o `V:`. Só aceitável com headroom amplo e sem discard — nunca como padrão. |
| **Montar `/` com `discard` em vez de fstrim periódico** | `discard` síncrono pode ter custo de I/O por delete; o `fstrim` periódico que civm já roda basta **se** o pipeline (SCSI+thin) estiver correto. Mantemos fstrim; discard contínuo fica como opção. |
| **VHDX de tamanho fixo (não dinâmico)** | Aloca 100% do `V:` de imediato; remove a elasticidade e não resolve "guest-free vira host-free". |
| **Mover o civmctl para o host / reescrever em Windows** | Quebra a arquitetura guest-Linux do civm; o host precisa só de um script PS + task. |
| **Ignorar e só expandir o `V:`/disco** | Capex puro; adia o problema (o VHDX volta a encher); sem observabilidade nem automação. Vale como mitigação estrutural (RF-6), não como solução. |

### Trade-offs aceitos

- **civm passa a ter um componente no host Windows** (`deploy/windows/` + Scheduled Task). Aceito: única forma de automatizar `Optimize-VHD`/observar `V:`; isolado em `deploy/` como o `deploy/systemd/` do guest, com contrato claro.
- **Re-anexar o VHDX a SCSI (RF-2) é uma janela one-time** (requer VM off). Aceito: paga-se uma vez e elimina compactações recorrentes.
- **A compactação offline ainda exige drain + shutdown** quando usada. Aceito como fallback; o caminho primário (online discard) não precisa de downtime.
- **Privilégio:** a Scheduled Task roda como SYSTEM com direito de Hyper-V. Aceito: mínimo para `Optimize-VHD` sem UAC; documentado em segurança.

---

## 4. Requisitos funcionais

### RF-1 — `civmctl disk-doctor` (guest): diagnóstico do pipeline de discard

Novo subcomando read-only que reporta por que o `fstrim` recupera (ou não) espaço para o VHDX.

- **Critério de aceite:** `civmctl disk-doctor --json` reporta: device de `/`, tipo de controlador (SCSI/IDE/virtio), `discard` em `/proc/mounts` (sim/não), `lsblk -D` DISC-GRAN/DISC-MAX (>0 = TRIM suportado), resultado de teste de efetividade (alocar→liberar→`fstrim`→delta) e campo `root_cause` legível. Exit 0 sempre (diagnóstico); `trim_effective` booleano. Teste unit com `/proc/mounts`/`lsblk` mockados.
- **Tenant isolation:** N/A.

### RF-2 — Auto-shrink via discard correto (online, preferido)

Garantir o pipeline guest→SCSI→VHDX para o `fstrim` existente encolher o VHDX online; corrigir controlador IDE→SCSI (host, one-time) quando o `disk-doctor` apontar.

- **Critério de aceite:** após a correção, um ciclo "liberar N GB no guest → `fstrim` → medir VHDX no host" reduz o `FileSize` do VHDX em ≈N GB (±tolerância), **sem** `Optimize-VHD` offline. Evidência: `disk-doctor` `trim_effective=true` + medição host antes/depois.
- **Tenant isolation:** N/A.

### RF-3 — Observabilidade do host (volume `V:` + VHDX)

Componente Windows que emite métricas do host; civm consome e alarma antes dos pisos.

- **Critério de aceite:** existe `civm-host-metrics` (PS + task) que escreve JSON com `v_free_gb`, `v_size_gb`, `vhdx_file_size_gb`, `vhdx_min_size_gb`, `vhdx_max_size_gb`, `guest_free_gb`, `gap_gb`, `timestamp`; civm expõe isso (extensão de `capacity --json` ou novo `civmctl host-disk`) e marca `host_accepting=false`/alarme quando `v_free_gb < 30` (warn) / `< 10` (crit). Teste de parse/serialização.
- **Tenant isolation:** N/A.

### RF-4 — Compactação offline segura e não-interativa

Scheduled Task `civm-vhdx-optimize` como SYSTEM: drain→shutdown gracioso→`Optimize-VHD -Mode Full`→Start-VM→restore; idempotente; abort-safe; sem zero-fill sob baixo headroom.

- **Critério de aceite:** `schtasks /run /tn civm-vhdx-optimize` executa o ciclo **sem prompt de UAC** e grava log estruturado; se `v_free_gb < headroom`, **aborta** com motivo (não tenta zero-fill); se `Optimize-VHD` falhar/timeout, **religa a VM** e sai com erro (nunca deixa off); idempotente (re-run seguro). Validação manual documentada + log esperado.
- **Tenant isolation:** N/A.

### RF-5 — `civmctl maintenance enter|exit` (guest): drain idempotente

Substitui a remoção manual de label/stop por um par idempotente que drena e restaura este runner.

- **Critério de aceite:** `civmctl maintenance enter --execute` para `actions.runner.*` e/ou remove a label `civm` gravando o estado anterior em `/var/lib/civm/maintenance.json`; `exit --execute` restaura exatamente; re-run é no-op; dry-run mostra o plano. Teste com systemctl/gh mockados.
- **Tenant isolation:** N/A.

### RF-6 — Right-sizing / headroom estrutural do VHDX

Capar o max do VHDX abaixo do `V:` (scratch para `Optimize-VHD`), ou expandir/mover; invariante de headroom documentado e checado.

- **Critério de aceite:** `disk-doctor`/`host-disk` reporta `vhdx_max_size_gb` vs `v_size_gb` e sinaliza violação do invariante "`v_size_gb - vhdx_max_size_gb ≥ headroom`"; runbook documenta como aplicar (`Resize-VHD`/expandir `V:`/mover). Evidência: campo de violação no JSON.
- **Tenant isolation:** N/A.

### RF-7 — Contrato e documentação

Novo `RUNBOOK-HOST-VHDX-MAINTENANCE.md` + updates em `MULTI-PROJECT-RUNNER.md` §Disk + `capacity`/`deploy` docs.

- **Critério de aceite:** runbook descreve diagnóstico, pipeline SCSI/discard, a Scheduled Task (instalação/uso/segurança), `civmctl maintenance`, headroom, e a ordem segura (sem zero-fill sob baixo headroom). `npm run docs:check` verde.
- **Tenant isolation:** N/A.

---

## 5. Requisitos não-funcionais

### Performance

- Alvo: **host `V:` nunca abaixo de 30 GB livres** em operação normal; recuperação **sem downtime** no caminho online (RF-2). Métrica: gap guest×host → ≈0 após RF-2; `v_free_gb` ≥ 30 sustentado.
- `disk-doctor`/`host-metrics` são leves (segundos). `Optimize-VHD` leva minutos a dezenas de minutos com a VM off — por isso é fallback, não rotina.

### Segurança

- A Scheduled Task roda como **SYSTEM** com direito de Hyper-V — privilégio mínimo para `Optimize-VHD` sem UAC interativo; documentado. **Nunca** `pull_request_target`/código de PR no host.
- Sem segredo no `deploy/windows/`; logs sem PII. O componente host não expõe rede; é local + agendado.
- `maintenance` usa o `GITHUB_TOKEN`/`gh` do operador já presente; não introduz novo segredo.

### Observabilidade

- `host-metrics` JSON (V: free/size, VHDX file/min/max, gap, timestamp) + alarme nos pisos 30/10 GB. `civmctl` expõe via `capacity`/`host-disk`. Sem PII.
- Logs estruturados da Scheduled Task em arquivo no host (`V:\civm-hyperv-maintenance.log`, convenção observada) + linha de auditoria.

### Escalabilidade

- Primitivo por-VM/host; escala por host. O gap guest×host é o sinal de saúde por VM.

### LGPD

- N/A. Sem dado pessoal.

### Resiliência (worst-case — Kahneman #5)

- **Host a 3 GB livres** (observado): NUNCA zero-fill; abort da compactação; alarme crítico; caminho online (discard) recupera sem crescer o VHDX.
- **`Optimize-VHD` pendura/falha:** timeout + religar a VM; nunca deixar off; abort-safe.
- **VM ocupada:** drain + `idle-check` antes de qualquer shutdown; não interromper build em andamento.
- **VHDX em IDE (sem UNMAP):** diagnosticado por `disk-doctor`; correção SCSI one-time.

---

## 6. Fluxos

### Happy path A — auto-shrink online (após RF-2, sem downtime)

1. Job termina → hook `job-completed` roda cleanup + `fstrim -av` (já existe).
2. Com VHDX em SCSI + discard efetivo (RF-2), o Hyper-V recebe UNMAP e **encolhe o VHDX online**.
3. `civm-host-metrics` (RF-3) registra `v_free_gb` subindo; gap guest×host ≈ 0. Sem intervenção.

### Happy path B — compactação offline segura (fallback, quando online insuficiente)

1. `host-metrics` cruza o piso (ex.: `v_free_gb < 30`) → alarme.
2. Operador/automação aciona `schtasks /run /tn civm-vhdx-optimize` (RF-4).
3. Task: checa headroom (se `v_free_gb < headroom_min` → **abort**, sem zero-fill); chama `civmctl maintenance enter` via SSH (RF-5) → guest drena e fica idle; `sudo shutdown -h now`; espera VM Off; `Optimize-VHD -Mode Full` (timeout); `Start-VM`; espera Running; SSH `civmctl maintenance exit`; loga antes/depois.
4. `host-metrics` confirma `v_free_gb` recuperado.

### Fluxos de erro

| Condição | Resultado | Log | Consistência |
| --- | --- | --- | --- |
| `v_free_gb < headroom` na compactação | **Abort** sem zero-fill; alarme crítico; orienta expandir/mover (RF-6) | `error` headroom | Host intacto; nada destrutivo |
| `Optimize-VHD` timeout/erro | Religa a VM; sai com erro | `error` + religou | VM nunca fica Off |
| VM ocupada no `maintenance enter` | Aguarda idle/drena; não desliga build | `warning`/`queued` | Build não interrompido |
| `disk-doctor` aponta IDE/no-discard | Reporta `root_cause`; compactação offline segue como paliativo até re-anexar SCSI | `warning` root cause | — |
| zero-fill solicitado manualmente sob baixo headroom | **Recusado** pelo contrato | `error` proibido | Host intacto |

---

## 7. Modelo de dados

**N/A — sem banco.** Estado novo (arquivos):
- `/var/lib/civm/maintenance.json` (guest): labels/serviços drenados para restore idempotente (RF-5).
- Host: arquivo JSON de métricas (ex.: `V:\civm-host-metrics.json`) e log da task (`V:\civm-hyperv-maintenance.log`, convenção já observada).

Backfill = **N/A — Day-0**.

---

## 8. API / Interfaces

Sem endpoint HTTP/OpenAPI/evento. Interfaces: CLI + componente host + arquivos.

### CLI civmctl (guest)

| Interface | Mudança |
| --- | --- |
| `civmctl disk-doctor [--json]` | **novo** (RF-1): diagnóstico discard/SCSI/TRIM |
| `civmctl maintenance enter\|exit [--execute] [--json]` | **novo** (RF-5): drain idempotente |
| `civmctl host-disk [--json]` ou extensão de `civmctl capacity` | **novo/estendido** (RF-3): consome métricas do host |

### Componente host (Windows, `deploy/windows/`)

| Artefato | Função |
| --- | --- |
| `civm-host-metrics.ps1` + Scheduled Task | emite JSON de `V:`/VHDX (RF-3) |
| `civm-vhdx-optimize.ps1` + Scheduled Task (SYSTEM) | compactação offline segura, não-interativa (RF-4) |

### Paths / contratos

- `/var/lib/civm/maintenance.json` (guest, RF-5); `V:\civm-host-metrics.json` + `V:\civm-hyperv-maintenance.log` (host).
- Invariante headroom: `v_size_gb - vhdx_max_size_gb ≥ headroom` (RF-6).

### Impacto em OpenAPI/SDK/eventos

**N/A.**

---

## 9. Dependências e riscos

### Pré-requisitos

- Acesso elevado one-time no host para: instalar as Scheduled Tasks (SYSTEM) e re-anexar o VHDX a SCSI (RF-2) — janela de manutenção com a VM off.
- `disk-doctor` (RF-1) **antes** de tudo: medir o root cause (Kahneman #3 — não assumir SCSI/IDE).

### Riscos técnicos (com mitigação)

| Risco | Mitigação |
| --- | --- |
| Re-anexar a SCSI requer VM off / pode mudar device name (`/dev/sda`→`/dev/sdX`) | Janela planejada; validar boot + `fstab` por UUID (não device); `disk-doctor` confirma pós-troca |
| Scheduled Task SYSTEM amplia superfície no host | Privilégio mínimo, script versionado em `deploy/windows/`, sem rede, log auditável |
| `Optimize-VHD` longo segura a VM off além do esperado | Timeout + religar; rodar em janela; alarme se exceder budget |
| Zero-fill sob baixo headroom estoura o `V:` | **Proibido** por contrato (RF-4); abort com headroom check |
| Observabilidade do host depende de WSL/host vivo | Task agendada no Windows independe do WSL; civm degrada para guest-only se métricas ausentes |
| Boundary: civm ganhar artefato no host Windows | Isolado em `deploy/windows/` com contrato; documentado; reversível (remover task) |

### Impacto em componentes existentes

`internal/capacity` (host-disk), `cmd/civmctl` (2-3 subcomandos novos), `internal/idle` (reuso no maintenance), `deploy/` (novo `windows/`), runbooks. Guest cleanup/hook inalterados (o `fstrim` passa a ser efetivo via RF-2).

### Breaking changes

Nenhum. Aditivo; sem RF-2 aplicado, tudo segue como hoje (degradado).

### Estratégia de rollout

Slice 0 (diagnóstico/baseline, sem mudança) → Slice 1 (`disk-doctor` + `maintenance` guest) → Slice 2 (host-metrics + observabilidade) → Slice 3 (RF-2 SCSI/discard one-time + verificação) → Slice 4 (`civm-vhdx-optimize` task segura) → Slice 5 (headroom RF-6 + runbook RF-7).

### Estratégia de rollback

- **App:** `civmctl self-upgrade` anterior; subcomandos novos viram no-op. 
- **Host:** remover as Scheduled Tasks (`schtasks /delete`); reverter SCSI→IDE se a troca causar boot issue (janela). 
- **Dados:** N/A. 
- **Proibido:** zero-fill sob baixo headroom; deixar a VM Off. 
- **Rollback trigger numérico (fechar no SPEC):** reverter a slice se, após RF-2, o gap guest×host não cair (VHDX não encolhe em ≈N GB após liberar N GB) em 3 medições; OU a task de compactação deixar a VM Off uma vez; OU `v_free_gb` cruzar 10 GB sem alarme prévio.

### Hipóteses que exigirão disciplina explícita no SPEC (`disciplines/KAHNEMAN-DISCIPLINES.md`)

- **#3 (número, não adjetivo):** "fstrim ineficaz", "VHDX não encolhe" viram medição (`disk-doctor` delta; host FileSize antes/depois).
- **#5 (availability/worst-case):** host a 3 GB, Optimize pendurado, zero-fill perigoso, IDE sem UNMAP — todos com mitigação.
- **#2 (counterfactual):** rollback trigger numérico no SPEC.

---

## 10. Estratégia de implementação

| Slice | Conteúdo | Depende de | Validável cedo |
| --- | --- | --- | --- |
| **Slice 0 — Diagnóstico/baseline** | Rodar `disk-doctor` manual (lsblk -D, /proc/mounts, controlador) + medir VHDX FileSize × guest free. Sem código. | — | Output colado no SPEC; prova o root cause |
| **Slice 1 — guest CLI** | `civmctl disk-doctor` + `civmctl maintenance enter\|exit` + testes. | Slice 0 | Local (mock lsblk/systemctl/gh) |
| **Slice 2 — host observabilidade** | `deploy/windows/civm-host-metrics.ps1` + task; `civmctl host-disk`/extensão `capacity`. | Slice 1 | Task no host escreve JSON; civm lê |
| **Slice 3 — RF-2 SCSI/discard** | Re-anexar VHDX a SCSI (janela one-time) + confirmar `trim_effective` + auto-shrink. | Slice 1-2 | Medição host antes/depois |
| **Slice 4 — compactação segura** | `deploy/windows/civm-vhdx-optimize.ps1` + task SYSTEM (drain/shutdown/optimize/start/restore, headroom guard). | Slice 1-3 | `schtasks /run` sem UAC; abort sob baixo headroom |
| **Slice 5 — headroom + docs** | RF-6 (cap/expand) + `RUNBOOK-HOST-VHDX-MAINTENANCE.md` + `MULTI-PROJECT-RUNNER` §Disk. | Slices 1-4 | `npm run docs:check` |

---

## 11. Documentos a atualizar

- `docs/specs/host-volume-reclamation/{PRD.md (este), SPEC.md, IMPL.md}`
- `runbooks/RUNBOOK-HOST-VHDX-MAINTENANCE.md` (novo)
- `runbooks/MULTI-PROJECT-RUNNER.md` §Disk pressure / §Rollback trigger (host VHDX + observabilidade)
- `deploy/windows/` (novo: scripts + Scheduled Task XML/registro)
- `deploy/systemd/README.md` (referência cruzada guest↔host)
- `cmd/civmctl/main.go` `printHelp` (novos subcomandos)
- `docs/INDEX.md` (regenerar `npm run docs:index`)
- `AGENTS.md`/`CODEX.md` (boundary: civm passa a ter componente host)

## 12. Fora de escopo

| Item | Motivo |
| --- | --- |
| Migrar civmctl para Windows / reescrever em PS | Mantém arquitetura guest-Linux; host só precisa de scripts + task |
| Trocar Hyper-V por outro hypervisor | Fora do problema |
| Expandir fisicamente o `V:`/disco como solução única | Capex; mitigação estrutural (RF-6), não a solução |
| Mudança em produto de peer (acme/peer) | Puramente plataforma de runner |
| Multi-project isolation (lock/porta/project-name) | Coberto por `docs/specs/multi-project-isolation/` |

## 13. Critérios de aceitação

- `civmctl disk-doctor --json` reporta root cause do discard (controlador/discard/TRIM/efetividade) — RF-1.
- Após RF-2, liberar N GB no guest + `fstrim` reduz o VHDX em ≈N GB **sem** `Optimize-VHD` — RF-2.
- `host-metrics` expõe `v_free_gb`/VHDX/gap e alarma a 30/10 GB — RF-3.
- `civm-vhdx-optimize` roda sem UAC, aborta sob baixo headroom, nunca deixa a VM Off — RF-4.
- `civmctl maintenance enter/exit` drena/restaura idempotente — RF-5.
- Invariante de headroom reportado e documentado — RF-6.
- Runbook publicado; `npm run docs:check` verde — RF-7.

## 14. Validação

- **Go (civm):** `go test ./... -race -count=1` — `disk-doctor` (mock lsblk/proc), `maintenance` (mock systemctl/gh, idempotência), `host-disk` parse, capacity estendido.
- **Host (manual/scriptado):** `disk-doctor` no guest real; `host-metrics` task escreve JSON; `civm-vhdx-optimize` em janela (headroom-guard testado abortando sob baixo espaço); medição VHDX antes/depois.
- **Lint/format:** `golangci-lint run -c .golangci.yml`, `gofmt`; PSScriptAnalyzer no PS (se disponível).
- **Docs:** `npm run docs:index` + `npm run docs:check`.
- **Gates cognitivos:** cada etapa crítica aponta `disciplines/KAHNEMAN-DISCIPLINES.md` com pergunta/evidência/abort trigger.
- **Prova end-to-end:** ciclo "liberar→fstrim→VHDX encolhe online" + um `civm-vhdx-optimize` seguro, com `v_free_gb` recuperado e VM Running.
