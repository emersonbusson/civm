---
slug: civm-runner-reliability
title: Confiabilidade do runner civm — camada vinculante de resolução (Passo 2.5)
milestone: —
issues: []
---

# SPECv2 — Confiabilidade do runner civm: equivalente gratuito e auto-curável do GitHub Actions

> Extensão de 30/07/2026: queue stall de runner org online/ocioso usa
> assinatura persistente, dwell, idle-check completo e exatamente 1 restart
> por incidente. Fonte focada:
> `../stalled-runner-recovery/{PRD,SPEC,IMPL}.md`.

> Versão melhorada após auditoria do PASSO 2.5 (4 perspectivas, verificada contra o código vivo em
> `/home/emdev/codespace/civm` e contra o estado do box).
> Baseline preservado: `SPEC.md` (mantido como está; este arquivo é a camada de overrides).
> Disciplinas: `disciplines/KAHNEMAN-DISCIPLINES.md` (#1 WYSIATI, #3 número-não-adjetivo, #5 Availability/worst-case).
> Validação: `go test ./... -race -count=1`, `golangci-lint run -c .golangci.yml ./...`, `gofmt -l`,
> e (host) PSScriptAnalyzer + `schtasks /query`.
> Day-0: o civm não tem produção viva; backfill = N/A. Solução primária única, sem dual-path.

## Como ler este v2

`SPEC.md` (baseline) define os ITENS, a estrutura, a matriz PRD→SPEC e a ordem de implementação. Este
`SPECv2.md` é a **camada vinculante de resolução**: cada `DT-v2-N` fecha um blocker do PASSO 2.5 e,
quando aplicável, **substitui** o trecho correspondente do baseline. PASSO 3 implementa `SPEC.md`
**com os overrides deste v2**. Onde houver conflito, **o v2 prevalece**.

A auditoria deu **no-go provisório** por blockers de **precisão de implementação e de segurança**, não de
arquitetura. A direção (escalada `safedelete` escopada ao `_work`, fail-open no `job-completed`,
1 MB BlockSize + Optimize offline como primário, watchdog sem falsa-falha + auto-recuperação,
observabilidade via `hooks.jsonl`) está correta. O que estava errado: o restart referenciava
campos/sintaxe/unidades inexistentes; o sudoers tinha mismatch de path que faria a escalada falhar-fechada
em todo box; PRD e SPEC discordavam sobre o escopo do sudoers; o contador anti restart-loop não tinha onde
morar; e vários pontos de fiação (ctx, ordem de leitura de host-metrics, `go:embed` impossível) não eram
implementáveis como escritos.

### Fatos verificados no box (WYSIATI — #1: declarar o não-visto)

Confirmado diretamente antes de fechar os blockers:

- **Este box de dev é WSL2** (`6.6.87.2-microsoft-standard-WSL2`), **sem systemd runner units**
  (`systemctl list-units 'actions.runner.*'` e `'civm-*'` retornam 0) e **sem `/run/civm`**. Logo
  **todo dado de baseline e todo abort-trigger numérico do guest têm de ser coletados no guest
  `gha-ubuntu-2404`, não aqui** (fecha o blocker MEDIUM de "evidências não verificáveis").
- **`/usr/bin/chown`, `/bin/chown`, `/usr/bin/rm` e `/bin/rm` TODOS existem** e `which chown`/`which rm`
  resolvem para `/usr/bin/*`. Isto é a raiz do blocker CRÍTICO do sudoers (DT-v2-1).
- `runner.Restart` é **função** `Restart(ctx, opts RestartOptions)` (`restart.go:52`), **não** struct literal;
  `RestartOptions{Unit, Execute, RunFn, Short}`; `validateRestartOptions` roda `civm.ValidateServiceUnit`
  (regex `…[.]service$` + bloqueio de `..`) quando `Unit != ""` (`restart.go:120-122`).
- `runner.Status` (`list.go:12-20`) tem `UnitName`, `Repo`, `Name`, estados — **não** tem `WorkingDirectory`.
  `List` glob `actions.runner.*` (`list.go:41`); `parseRunnerUnit` produz `actions.runner.OWNER-REPO.NAME.service`.
  **Não existe unidade `civm-<slot>` no espaço enumerado pelo `List`.**
- `resolveWatchdogRunnerRepo` (`watchdog.go:838-858`) **já** mapeia `Status.UnitName` (validado) →
  `systemctl show … WorkingDirectory` → `<dir>/.runner` → `repo`. `enrichWatchdogSystemdRepos`
  (`watchdog.go:827-836`) já popula `Status.Repo` com isso. `restartCandidates` (`watchdog.go:525-548`) e
  `restartWatchdogRunners` (`watchdog.go:265-289`, restart via `opts.RunFn(ctx,"sudo","systemctl","restart",unit)`)
  só usam `Status.UnitName` enumerado pelo systemd — **fonte confiável**.
- `rerunState{ Reruns map[string]RerunMarker }` (`watchdog.go:566-568`); `RerunMarker{Repo,RunID,HeadSHA,RerunAt}`
  (`watchdog.go:559-564`). **Não há** campo de contador de auto-restart por unidade/janela.
  `writeRerunState` (`watchdog.go:591-602`) faz `WriteFileFn(MarkerPath, data, 0644)` **não-atômico**
  (sem temp+rename).
- `cleanup.Run` (`cleanup.go:121`) **já** early-returns quando `LockActiveFn()` ativo (`cleanup.go:127-132`);
  `dockerPrune` (`cleanup.go:334`) chama `ensureIdle` (`cleanup.go:344`) antes de `docker system prune -af --volumes`
  (`cleanup.go:348`). O `rm -rf` do cron (`cleanup.go:270`) é por-candidato, **sem `--`**.
- `validateCleanupRoot(root) (string, error)` (`cleanup.go:290`) valida a **raiz** (rejeita `/`, raízes
  perigosas, home bare) — não valida filho.
- `collectHookChecks(opts, systemd, systemdErr)` (`doctor.go:276`) **não recebe ctx**; os checks atuais usam só
  `ReadFileFn`. `Collect` (`doctor.go:135`) tem ctx.
- `safeWorkRoot(root)` (`hook.go:450-458`) é só `HasPrefix("/home/") && Contains("/actions-runner") && HasSuffix("/_work")`
  sobre `filepath.Clean` — **sem `EvalSymlinks`/`Lstat`**.
- **Zero `//go:embed`** em `internal/` e `cmd/` (grep = 0). Module root `github.com/emersonbusson/civm`;
  `internal/hook/install.go` não pode `embed` `../../deploy/...` (embed proíbe `..`). O padrão estabelecido
  é ler ou usar `*Fn` injetados (`ScriptContent`, `ReadFileFn`).
- `civm-host-metrics.ps1` tem **três** emissões de `$metrics`: host-only-failed (119-130, `exit 0` em 134),
  sucesso (144-154, `exit 0` em 163) e re-stamp pós-falha-de-entrega (168-173, **muta** o hashtable de sucesso).
  `$vhd` está em escopo desde `Get-VHD` (104).
- `diskdoctor.Diagnose` chama `composeRootCause(report)` (`diskdoctor.go:116`, **por valor**) **antes** do único
  read de host-metrics em `hostHeadroomViolation(opts)` (`diskdoctor.go:117`), que parseia só `hostMetrics{VFreeGB}`
  (`diskdoctor.go:86-88`) e descarta o resto. `composeRootCause` (`diskdoctor.go:266-277`) não lê arquivo.

---

## Resolução dos blockers do PASSO 2.5 (decisões fechadas)

| # | Blocker (sev.) | Decisão vinculante (override do baseline) |
| --- | --- | --- |
| **DT-v2-1** | **CRÍTICO — sudoers bare-name vs absolute-path: a escalada falha-fechada em todo box** | `safedelete` invoca **o caminho absoluto exato** que o sudoers whitelista, resolvido uma vez no boot. Como `/usr/bin/chown` E `/bin/chown` existem no box, `RunFn(ctx,"sudo","-n","chown",…)` (bare) pode resolver para `/bin/chown` via `secure_path` e **não casar** a regra `/usr/bin/chown` → `sudo -n` sai 1 → killer #1 **não** é corrigido (só mascarado pelo demote do RF-2). Fix: ver §"Escalada `safedelete`" — caminho absoluto pinado + sudoers cobrindo **ambos** `/usr/bin` e `/bin` + doctor que prova a **capacidade exata**. |
| **DT-v2-2** | **CRÍTICO — watchdog auto-restart aponta unidade `civm-<slot>` inexistente e usa sintaxe inválida** | Abandonar o framing `civm-<slot>` e `runner.Restart{Unit:…}` (struct sobre função, não compila). A auto-recuperação mapeia a sentinela `repository` → `Status.Repo` (já enriquecido por `enrichWatchdogSystemdRepos`/`resolveWatchdogRunnerRepo`) → reinicia `Status.UnitName` (a unidade real `actions.runner.OWNER-REPO.NAME.service` que `restartCandidates`/`restartWatchdogRunners` já reiniciam). O Unit reiniciado **vem sempre da enumeração systemd**, nunca de string interpolada do log. Ver §"Auto-recuperação do watchdog". |
| **DT-v2-3** | **CRÍTICO — PRD path-scoped vs SPEC unrestricted: colapso da defesa-em-profundidade** | Reconciliar para **wrapper validado**, não NOPASSWD em binário cru nem wildcard de path. Day-0 envia `deploy/bin/civm-safedelete` (root-owned 0755) que **re-valida o path in-process** (mesma lógica `safeWorkRoot`/`validateCleanupRoot` + EvalSymlinks de DT-v2-7) e só então `exec`a `chown`/`rm`. O sudoers escopa **apenas** `NOPASSWD: /usr/local/bin/civm-safedelete`. Isto preserva o argumento do baseline (wildcard de path em sudoers é frágil contra `..`) **e** restaura o escopo OS-level que o PRD exige. Ver §"Escalada `safedelete`" e §"Reconciliação PRD". |
| **DT-v2-4** | **CRÍTICO — contador anti restart-loop não tem onde morar em `rerunState`** | `rerunState` é estendido com um **campo novo tipado** e versionado; o write vira **atômico** (temp+rename). Ver §"Estado de auto-restart". O baseline DT-8 dizia "já gravado por writeRerunState/loadRerunState" — **falso**, sobrescrito aqui. |
| **DT-v2-5** | **HIGH — `//go:embed ../../deploy/...` impossível + dupla fonte de verdade** | Remover a opção `go:embed` (erro de compilação através do boundary do pacote). Day-0: `civm-safedelete` (o wrapper de DT-v2-3) e o `civm-cleanup` sudoers são **enviados** para `/opt/civm/deploy/` no provisioning; `installScopedSudoers` os lê por `ReadFileFn` a partir de `DefaultDeploySourceDir` (espelha o padrão `DefaultUnitsSourceDir`/`ScriptContent`). **Uma fonte de verdade** versionada em `deploy/`. Ver §"Instalação do sudoers". |
| **DT-v2-6** | **HIGH — diskdoctor: BlockSize não está disponível quando `composeRootCause` roda** | `Diagnose` lê host-metrics **uma vez, cedo**, popula `report.VHDXBlockSizeBytes`/`report.BlockSizeReclaimBlocker` **antes** da linha 116; `composeRootCause` passa a ramificar nesses campos do `report`. `hostHeadroomViolation` é colapsado para usar o mesmo read (sem dois parses). Ver §"diskdoctor". |
| **DT-v2-7** | **HIGH/MEDIUM — `safedelete` sem hardening de symlink/`..` apesar de escalada privilegiada** | Antes de qualquer remoção privilegiada, `safedelete` resolve `filepath.EvalSymlinks` e re-afirma que o alvo continua sob um `_work`/cleanup-root **real** (`Lstat` dono `emdev`), recusando se escapar a árvore ou for symlink para fora. Aplica-se também ao wrapper `civm-safedelete`. Testes de path são **gates CRITICAL**, não unit comum. Ver §"Escalada `safedelete`". |
| **DT-v2-8** | **HIGH — `--` (terminador de opções) ausente no rm/chown privilegiado** | Todo invoke privilegiado usa `--` antes do path: `… chown -R <uid>:<gid> -- <path>` e `… rm -rf --one-file-system -- <path>`. O mesmo no `civm-safedelete` e no `RemoveAllFn` documentado. Teste assenta `--` imediatamente antes do path. Ver §"Escalada `safedelete`". |
| **DT-v2-9** | **HIGH — GuardFn de cleanup: semântica/assinatura incompatível** | O `GuardFn` é `func(path string) error` e valida **a relação pai/filho** ("`candidate` é filho direto de um cleanup-root validado por `validateCleanupRoot`"), simétrico ao hook ("filho direto de `safeWorkRoot()`"). `validateCleanupRoot` valida a **raiz**; um adaptador explícito a embrulha. `validateCleanupRoot` **não** é passado direto como `GuardFn`. Ver §"GuardFn por chamador". |
| **DT-v2-10** | **HIGH — `collectHookChecks` não recebe ctx; check do sudoers precisa de capacidade, não de leitura de arquivo 0440** | (a) `collectHookChecks` passa a receber `ctx` (e `Collect` o repassa) — mudança de assinatura **explícita**, não "aditiva". (b) `checkScopedSudoers` é um **probe de capacidade**, não leitura de arquivo (o drop-in é 0440 root:root e `doctor` roda como `emdev`): `sudo -n /usr/local/bin/civm-safedelete --check` (no-op que sai 0) — testa o que `safedelete` realmente fará. Ver §"doctor self-check". |
| **DT-v2-11** | **HIGH — host-metrics.ps1: campo só num dos três blocos `$metrics`** | `vhdx_block_size_bytes = [int64]$vhd.BlockSize` é montado **uma vez** numa função base e referenciado nas **três** emissões (host-only-failed 119-130, sucesso 144-154, re-stamp 168-173). `$vhd` já está em escopo. Ver §"host-metrics.ps1". |
| **DT-v2-12** | **MEDIUM — classificador fail-open por substring frágil** | A classe de erro decide por **erro tipado** (`errors.Is(err, fs.ErrPermission)`, `syscall.ENOTEMPTY`, `errors.Is(err, fs.ErrNotExist)`) propagado por `safedelete`/cleanup, **não** por `strings.Contains` da mensagem. Substring fica só como último recurso quando o tipo é inacessível, com conjunto documentado. Erro **não-reconhecido** no `job-completed` permanece fatal. Ver §"Classificador fail-open". |
| **DT-v2-13** | **MEDIUM — ctx-cancel mid-cleanup conflado com fail-open** | Em `ctx.Err()!=nil`, a Action recebe `escalation="aborted"` (distinta de `failed`), o run **não** é marcado limpo, e sentinela/doctor distinguem "incompleto-por-cancel" de "tentou-tudo-com-warning". Exit 0 mantido (contrato do hook), mas a incompletude é observável e o próximo cron/hook retoma. Ver §"Atomicidade e ctx". |
| **DT-v2-14** | **MEDIUM — `secure_path` do sudo não verificado** | ITEM-0 (baseline, no guest) verifica `which chown rm` sob `/usr/bin` **e** que `sudo -n /usr/local/bin/civm-safedelete --check` casa após `hook install --execute`. Como DT-v2-3 escopa um caminho único de wrapper, a ambiguidade `secure_path` some (o wrapper é absoluto e único). Ver §ITEM-0. |
| **DT-v2-15** | **MEDIUM — fail-safe quando `hooks.jsonl` ausente/truncado** | `detectBrokenRunner` e `checkBuildxCapability`: se `ReadFileFn(HooksLogPath)` falha/retorna vazio/JSONL truncado → **não** dispara restart e **não** marca CRITICAL (degrada para info/no-op). O parsing tolera linha final truncada (ignora a última se não-JSON). Ver §"Auto-recuperação" e §"doctor self-check". |
| **DT-v2-16** | **MEDIUM — baseline numérico não persistido; rollback-trigger não-falsificável** | ITEM-0 **produz um artefato commitado** `docs/specs/civm-runner-reliability/baseline-<YYYY-MM-DD>.txt` (coletado **no guest**) antes de qualquer código. O rollback-trigger numérico referencia esse artefato. Ver §ITEM-0 e §"Rollback trigger v2". |
| **DT-v2-17** | **LOW — DT-7 superestima o hazard do cron prune** | DT-7 é requalificado: o early-return de `Run` (`cleanup.go:127-132`) + `ensureIdle` (`cleanup.go:344`) são mitigações **parciais**; a mudança fecha a **janela residual** (pulls/builds de sibling que **não** seguram a docker-heavy lock + falsos-negativos do idle probe) e remove o GC de conteúdo (`-af --volumes`) que o idle guard não torna seguro. Ver §"dockerPrune isolation-aware". |
| **DT-v2-18** | **LOW — `DefaultRunDir`/`DefaultHooksLogPath` (consistência de fonte única)** | `DefaultRunDir = "/run/civm"` é **adicionado** a `civm.go` e `DefaultDockerHeavyLockPath` deriva dele via `filepath.Join`. `hook.Options.LogPath` é **renomeado** para `HooksLogPath`, todos os pacotes (hook/doctor/watchdog) referenciam `civm.DefaultHooksLogPath`. Ver §"Constantes e nomes". |
| **DT-v2-19** | **LOW — referências de linha defasadas** | Todas as referências `file.go:NN` do baseline são **re-ancoradas por nome de função** (estável) na §"Re-âncoras". O implementador segue os nomes, não os offsets. |

---

## Decisões de implementação detalhadas (overrides)

### Escalada `safedelete` — fecha DT-v2-1, DT-v2-3, DT-v2-5, DT-v2-7, DT-v2-8

Substitui DT-4/DT-9 e os ITENS-1/6 do baseline naquilo que conflita.

**Mecanismo de escalada — wrapper validado, não NOPASSWD em binário cru.**

1. Day-0 envia ao guest (provisioning, **fora** do binário Go):
   - `/opt/civm/deploy/bin/civm-safedelete` (versionado em `deploy/bin/civm-safedelete`), instalado como
     `/usr/local/bin/civm-safedelete`, **root-owned 0755**. É um script/binário pequeno que recebe
     `chown <uid>:<gid> <path>` ou `rm <path>`, **re-valida o path in-process** (abs; sem NUL; `!= /`;
     `!= $HOME`; `!= /home/<x>` bare; `EvalSymlinks` resolve sob um `_work`/cleanup-root real com dono `emdev`)
     e só então executa `/usr/bin/chown -R <uid>:<gid> -- <real>` ou `/usr/bin/rm -rf --one-file-system -- <real>`.
   - `/opt/civm/deploy/sudoers.d/civm-cleanup` (versionado em `deploy/sudoers.d/civm-cleanup`):
     `emdev ALL=(root) NOPASSWD: /usr/local/bin/civm-safedelete`. **Escopado a um único caminho de wrapper**
     — sem wildcard de path (frágil) e sem NOPASSWD em `rm`/`chown` crus (irrestrito).

2. `internal/safedelete.Remove(ctx, opts, path)`:
   - (1) validação interna fixa (abs via `filepath.IsAbs`; sem NUL; `!= "/"`, `!= $HOME`, não `/home/<x>` bare, `!= ""`);
   - (2) `opts.GuardFn(path)` (relação pai/filho — ver DT-v2-9) reprova **antes** de qualquer remoção;
   - (3) **`EvalSymlinks` + `Lstat` dono `emdev`** (DT-v2-7): se o path resolvido escapa a árvore real `_work`/cleanup-root
     ou é symlink para fora → `ErrUnsafePath`, **sem sudo**;
   - (4) `RemoveAllFn(path)` não-privilegiado → sucesso → `(false, nil)`;
   - (5) `errors.Is(err, fs.ErrPermission)` → `escalate`: `RunFn(ctx, "sudo", "-n", "/usr/local/bin/civm-safedelete", "chown", uidgid, real)`
     então `RemoveAllFn(real)`; se ainda EACCES → `RunFn(ctx, "sudo", "-n", "/usr/local/bin/civm-safedelete", "rm", real)`;
     retorna `(true, errOrNil)`;
   - (6) erro não-permissão → `(false, err)` sem sudo.

   O **único** binário que `sudo` autoriza é `civm-safedelete`. O `chown`/`rm` absolutos (`/usr/bin/*`) são
   chamados **de dentro** do wrapper (já root), removendo de vez a ambiguidade `/usr/bin` vs `/bin` (DT-v2-1/DT-v2-14):
   o wrapper hardcoda `/usr/bin/chown` e `/usr/bin/rm`.

**Por que wrapper e não NOPASSWD cru (#5 Availability/worst-case):** o pior caso é `sudo rm -rf` num path errado.
Com NOPASSWD cru em `rm`, toda a contenção de blast-radius mora na `GuardFn` Go (uma regressão = primitivo de
deleção root irrestrito). Com o wrapper, a validação de path roda **também no lado root**, in-process, antes do
`exec` — duas camadas independentes (Go caller + wrapper), restaurando o "guard duplo" que o PRD exige (§5).

**Testes (gates CRITICAL, não unit comum):** path seguro próprio do user → sem sudo; EACCES → `sudo -n civm-safedelete chown`
então `rm`, `escalated=true`, **`--` precede o path**; `chown` falha → `rm` tentado; ambos falham → erro claro;
`GuardFn` reprova → wrapper **nunca** chamado; `/`/`$HOME`/`/home/x` bare/relativo/NUL recusados; **symlink `_work`,
`_work/../../etc`, `_work` que é symlink para `/`** recusados antes de qualquer sudo; ctx cancelado → propaga.
Mais: teste do wrapper em si (shell/Go) que `civm-safedelete rm /etc` é recusado mesmo invocado como root.

### Escalar alvo root-owned + gate de integração — fecha DT-v2-20

Correção **pós-implementação** (descoberta no box em operação, não no PASSO 2.5).
O passo (3) acima — "`EvalSymlinks` + `Lstat` dono `emdev`" — **recusava** todo
alvo cujo dono não fosse o runner, inclusive `uid 0` (root). Isso contradiz o
propósito do `safedelete`: a sobra do Docker-as-root é exatamente um path
root-owned. O modelo do SPEC assumiu que só **filhos** do `_work` seriam
root-owned (a entrada top-level sempre do runner); na prática a própria entrada
`_work/<repo>` ficou root-owned. Resultado: `resolveAndAffirmOwner` retornava
`ErrUnsafePath` antes da escalada, o erro era fatal em `cleanWorkRoot`
(job-completed exit 1), **e** o checkout do próximo job batia EACCES — todo job
Docker quebrava o próximo no box.

**Decisão.** A checagem de dono aceita `uid == runner` (happy path, remove sem
sudo) **OU** `uid == 0` (root — a sobra que a escalada limpa via `chown -R` +
`rm` no wrapper). Qualquer outro uid continua recusado (o runner nunca
escala-deleta arquivo de terceiro). As guardas de blast-radius permanecem:
`GuardFn` (prefixo `_work`), re-validação do path resolvido (symlink que escapa é
reprovado) e o wrapper root-side com `realpath` + `--one-file-system`. Aceitar
root **não** alarga o escopo além do `_work`.

**Por que o SPEC não previu (raiz de processo — disciplina #13 do
`KAHNEMAN-DISCIPLINES.md`, ilusão de validade):** o teste
`TestRemoveRejectsRootOwnedResolvedTarget` afirmava a recusa de root-owned como
correta — hermético + suposição errada + verde = confiança falsa. E o gate de
deploy era só `civm-safedelete --check` (existe?), nunca "limpa um root-owned de
verdade?". Existência ≠ função.

**Gate novo (o que segura pra não repetir):**

- Unit: `TestRemoveEscalatesRootOwnedTarget` afirma que root-owned **escala**
  (propósito); `TestRemoveRejectsThirdUserOwnedTarget` mantém a recusa só pra
  terceiro usuário.
- Integração: `safedelete_integration_test.go` (`//go:build integration`) cria
  um dir root-owned **real** via sudo e prova que a detecção de dono real deixa
  passar pra escalada — **falha** no código do #59. Roda no job `runner-smoke`
  (self-hosted) do `ci.yml`; self-skip sem sudo sem-senha (no-op em fork).
- Pré-deploy: a validação funcional (escala um root-owned de scratch?) substitui
  o `--check` de existência como prova de que a ferramenta faz o trabalho, não
  só de que está instalada.

### Auto-recuperação do watchdog — fecha DT-v2-2

Substitui DT-8 e ITEM-10 do baseline naquilo que conflita.

- **Não existe `runner.Restart{...}` (struct literal) nem unidade `civm-<slot>`.** A chamada correta é a **função**
  `runner.Restart(ctx, runner.RestartOptions{Unit: unit, Execute: opts.Execute, RunFn: opts.RunFn})`,
  **ou** reusar o helper inline de `restartWatchdogRunners` (`opts.RunFn(ctx,"sudo","systemctl","restart",unit)`).
  **Decisão de caminho único (Day-0):** a auto-recuperação por sentinela usa o **mesmo** helper inline de
  `restartWatchdogRunners` (não introduz um segundo padrão de `systemctl restart` no arquivo). `runner.Restart`
  fica reservado ao caminho CLI (`cmd/civmctl/runner.go`).
- **Mapeamento da sentinela → unidade (invariante de segurança):** a entrada de `hooks.jsonl` carrega `repository`
  (`hook.go` appendLog) e o campo `escalation`. `detectBrokenRunner`:
  1. lê o tail de `hooks.jsonl` (DT-v2-15: ausente/truncado → no-op);
  2. extrai o `repository` das entradas-sentinela (`decision=error` por filesystem **OU** Action `work_root`
     com `escalation=failed`) recentes;
  3. casa `repository` contra `Status.Repo` do `[]Status` **já enriquecido** por `enrichWatchdogSystemdRepos`;
  4. o Unit a reiniciar é o `Status.UnitName` enumerado pelo systemd (a unidade real `actions.runner.*.service`).
     **A string `repository` do log nunca é composta num nome de unidade.** `civm.ValidateServiceUnit(unit)`
     é re-aplicada antes do restart.
- **`escalation="aborted"` (DT-v2-13) NÃO é sentinela de restart** (é incompletude por cancel, não runner quebrado).
- **Anti restart-loop:** ver §"Estado de auto-restart" (DT-v2-4).
- **Runner org `busy` sem `Runner.Worker`:** esse sinal pode permanecer stale
  no GitHub depois do primeiro restart. O watchdog reserva e persiste a chave
  `org-busy:<UnitName>` antes de agir e permite no máximo **1 restart/hora**
  nesse caminho. Ticks seguintes emitem `org-busy-cooldown`; não reiniciam o
  listener novamente enquanto o job órfão ainda aparece como busy.
- **Teto e DoS:** o teto por unidade/hora também limita restarts induzidos por outro repo escrevendo sentinelas
  (hooks.jsonl é 0644 e atacável por um step CI). Como o restart só atinge unidades **enumeradas pelo systemd**
  e casadas por `Status.Repo`, um log forjado que não casa nenhuma unidade → **nenhum restart** (teste explícito).

### Estado de auto-restart — fecha DT-v2-4

- `rerunState` ganha campo novo tipado:
  ```go
  type autoRestartWindow struct {
      Count       int       `json:"count"`
      WindowStart time.Time `json:"window_start"`
  }
  type rerunState struct {
      Reruns       map[string]RerunMarker     `json:"reruns"`
      AutoRestarts map[string]autoRestartWindow `json:"auto_restarts"` // key = UnitName
  }
  ```
- **Janela horária:** se `now - WindowStart > 1h`, reset (`Count=0`, `WindowStart=now`). Incrementa por restart.
  Acima de `civm.DefaultRunnerAutoRestartPerHour` (=3) na janela → WARN sem restart.
- **Write atômico:** `writeRerunState` passa a escrever temp + `os.Rename` (RenameFn injetável), **não** mais
  `WriteFileFn(0644)` direto — fecha a perda de incremento sob watchdogs concorrentes/timer overlap.
  Se o timer puder sobrepor (8 runners), o rename atômico + read-modify-write por invocação é o contrato;
  documenta-se que sem flock há uma janela de last-writer-wins aceita (o teto é defensivo, não exato ao 1).
- **Teste:** N>teto sentinelas de unidade quebrada na janela → exatamente `teto` restarts, depois WARN-only;
  reset após 1h; `busy` org stale em dois ticks → exatamente 1 restart.

### Reconciliação PRD — fecha DT-v2-3 (parte documental)

O PRD (linhas 112, 159, 230, 299) descreve o sudoers como "limitando `rm`/`chown` a `/home/*/actions-runner*/_work/*`"
(path-scoped). Este v2 **substitui** isso pela abordagem **wrapper validado** (`civm-safedelete` + sudoers escopado
ao wrapper). Justificativa: wildcard de path em sudoers é contornável via `..`; o wrapper alcança o **mesmo objetivo
do PRD** (escopo OS-level ao `_work`) de forma robusta, mantendo o "guard duplo" de PRD §5. **Ação:** atualizar PRD
linhas 112/159/230/299 e o §"Boundary" para descrever o wrapper; o SPEC.md ITEM-6/DT-9 é sobrescrito por DT-v2-3.
Sem este v2, o contrato fica partido (PRD scoped, SPEC unrestricted) — proibido.

### Instalação do sudoers — fecha DT-v2-5

- `installScopedSudoers(opts)` lê `deploy/sudoers.d/civm-cleanup` **via `ReadFileFn`** a partir de
  `civm.DefaultDeploySourceDir` (novo; default `/opt/civm/deploy`, espelha `DefaultUnitsSourceDir`).
  **Sem `go:embed`** (impossível através do boundary do pacote; grep confirmou zero embeds).
- Escreve em `/etc/sudoers.d/civm-cleanup.tmp` (0440), `RunFn(ctx,"visudo","-cf",tmp)`; só em sucesso
  `RenameFn(tmp,"/etc/sudoers.d/civm-cleanup")`. Falha de `visudo` → erro, **não** instala parcial. Idempotente.
- `ensureBuildxCapability` igual ao baseline (best-effort, não falha o install).
- **Uma fonte de verdade:** o conteúdo do sudoers e do wrapper vive só em `deploy/`; o binário Go nunca embute cópia.

### doctor self-check — fecha DT-v2-10, DT-v2-15

- `collectHookChecks` **muda de assinatura** para receber `ctx` (e `Collect` o repassa). O baseline ITEM-9 dizia
  "assinatura de Collect já tem ctx" — verdade para `Collect`, **falso** para `collectHookChecks`. Mudança explícita.
- `checkScopedSudoers(ctx, opts)` é **probe de capacidade**, não leitura de arquivo (o drop-in é 0440 root:root;
  `doctor` roda como `emdev` e `ReadFileFn` daria EACCES). Roda `RunFn(ctx,"sudo","-n","/usr/local/bin/civm-safedelete","--check")`
  (no-op que valida o caminho e sai 0). Sucesso → OK; `sudo: a password is required`/`command not allowed`/erro →
  `HookCheck{Severity: SeverityCritical, Detail: "...; run sudo civmctl hook install --execute"}`. Classificação
  exata: exit 0 → OK; qualquer outro → CRITICAL. Teste: capacidade ausente → `report.Exit=2`.
- `checkBuildxCapability(opts)`: lê o tail de `hooks.jsonl` por `ReadFileFn`+`HooksLogPath`; ausente/truncado →
  OK (no-op, DT-v2-15); flood de `docker_buildx_prune ... exit status 125` → WARN com contagem.

### dockerPrune isolation-aware — fecha DT-v2-9, DT-v2-17

- O `Execute` de `dockerPrune` (`cleanup.go:348`) **nunca** roda `docker system prune -af --volumes`. Emite o conjunto
  concorrência-seguro do hook: `docker buildx prune --force --filter until=24h` (fallback `builder prune`),
  `docker image prune -f` (dangling-only), `docker container prune -f`. **`docker volume prune -f` NÃO roda no caminho
  cron** (é o mais propenso a quebrar `docker pull`/`compose up --build` de sibling que **não** segura a docker-heavy lock;
  o `LockActiveFn` não protege esses siblings — DT-v2-17). Se uma reclamação de volume for desejada, ela só roda quando
  o próprio cron **adquire** a docker-heavy lock para a janela (serializa siblings atrás dela) — documentado como
  follow-up opcional, **não** Day-0.
- O re-check `LockActiveFn` **dentro** de `dockerPrune` é **removido** (redundante: `Run` já early-returns em
  `cleanup.go:127-132`); não se adiciona um segundo guard estrutural sempre-falso.
- `scanAndMaybeDelete` (`cleanup.go:215-278`): o `rm -rf` por-candidato (`cleanup.go:270`) vira
  `opts.SafeDeleteFn(ctx, candidate.path)` (wrappa `safedelete.Remove` com o `GuardFn` de filho — DT-v2-9),
  **não dá `break` no primeiro erro** (acumula `a.Err` = primeiro, continua). `--` garantido dentro de `safedelete`.

### GuardFn por chamador — fecha DT-v2-9

- `safedelete.Options.GuardFn func(path string) error`.
- **hook:** `GuardFn` = "`path` é filho direto de um `safeWorkRoot()` válido" (não o root em si).
- **cleanup:** `GuardFn` = adaptador `func(path string) error` que (1) deriva o root pai de `path`,
  (2) roda `validateCleanupRoot(root)` (que valida a **raiz**), (3) confirma que `path` é filho direto desse root.
  `validateCleanupRoot` (assinatura `(string,error)`) **não** é usado como `GuardFn` diretamente.

### diskdoctor — fecha DT-v2-6

- `hostMetrics` struct local ganha `VHDXBlockSizeBytes int64 \`json:"vhdx_block_size_bytes"\``.
- `Diagnose` lê host-metrics **uma vez, cedo** (antes da linha 116): popula `report.VHDXBlockSizeBytes` e
  `report.BlockSizeReclaimBlocker = report.VHDXBlockSizeBytes > civm.DefaultVHDXTargetBlockSizeBytes`.
  `hostHeadroomViolation` é colapsado para reusar esse read (sem segundo parse).
- `composeRootCause(r Report)` ganha, **antes** do fallthrough `rootCauseTrimSupported` (`diskdoctor.go:276`):
  `if r.BlockSizeReclaimBlocker && r.TrimEffective { return rootCauseBlockSizeTooLarge }`.
  Sem host-metrics/sem campo → `BlockSizeReclaimBlocker=false` → árvore atual inalterada (degrada silencioso).

### host-metrics.ps1 — fecha DT-v2-11

- Logo após `Get-VHD` (linha 104), computar `$vhdxBlockSize = [int64]$vhd.BlockSize` uma vez.
- Adicionar `vhdx_block_size_bytes = $vhdxBlockSize` às **três** emissões: host-only-failed (119-130),
  sucesso (144-154) e — como o re-stamp (168-173) **muta** o hashtable de sucesso — garantir que o de sucesso
  já tem o campo (o re-stamp herda). Idealmente extrair a montagem do `$metrics` para uma função base
  compartilhada para impedir drift. **Gated** por `Get-VHD .BlockSize` confirmado em host elevado (Kahneman #1).

### Classificador fail-open — fecha DT-v2-12

- `classifyCleanupError(a Action)` decide por **erro tipado** propagado por `safedelete`/cleanup:
  `errors.Is(err, fs.ErrPermission)`, `errors.Is(err, syscall.ENOTEMPTY)` (ou `fs.PathError`+`ENOTEMPTY`),
  `errors.Is(err, fs.ErrNotExist)`. Cobre `Name ∈ {work_root, cache, cache_trim}`. Substring (`directory not empty`)
  só como último recurso documentado quando o tipo é inacessível.
- **Erro não-reconhecido no `job-completed` permanece fatal** (não é demovido). Teste de regressão explícito:
  um erro arbitrário (ex.: mount/overlay) no job-completed → exit 1; um job **nunca** é aceito com disco
  `>= HardFailPct` (abort trigger do baseline preservado).

### Atomicidade e ctx — fecha DT-v2-13

- `cleanWorkRoot`/`trimCacheByAge`/`scanAndMaybeDelete` checam `ctx.Err()` entre entradas.
- Em `ctx.Err()!=nil`: a Action recebe `Escalation="aborted"` (distinta de `"failed"`/`"ok"`/`"none"`), o run
  **não** conta como limpo, e a sentinela do watchdog **não** trata `aborted` como runner quebrado (não reinicia).
  Exit 0 mantido (contrato do hook), mas a incompletude é observável; o próximo cron/hook retoma as entradas restantes.

### Constantes e nomes — fecha DT-v2-18

Adicionar/ajustar em `internal/civm/civm.go`:

- `DefaultRunDir = "/run/civm"` (novo); `DefaultDockerHeavyLockPath = filepath.Join(DefaultRunDir, "docker-heavy.lock")`
  (deriva — fonte única). **§Escopo do baseline e ITEM-12 ficam consistentes** (o baseline listava "run dir" como
  novo mas ITEM-12 dizia que já existia — contradição resolvida: agora existe de fato).
- `DefaultDeploySourceDir = "/opt/civm/deploy"` (novo; raiz de onde `installScopedSudoers` lê o drop-in e o wrapper).
- `DefaultSafeDeleteWrapperPath = "/usr/local/bin/civm-safedelete"` (novo; o único caminho que o sudoers escopa).
- `DefaultSudoersDropInPath = "/etc/sudoers.d/civm-cleanup"`, `DefaultHookWarnOutputTailBytes = 512`,
  `DefaultVHDXTargetBlockSizeBytes = 1 << 20`, `DefaultRunnerAutoRestartPerHour = 3` (como no baseline).
- `DefaultHooksLogPath = "/var/log/civm/hooks.jsonl"` (novo). **Renomear** `hook.Options.LogPath → HooksLogPath`
  e atualizar callers; hook/doctor/watchdog derivam todos de `civm.DefaultHooksLogPath` (um nome, uma fonte).

### Re-âncoras (nomes de função estáveis em vez de offsets) — fecha DT-v2-19

| Símbolo | Arquivo | Verificado |
| --- | --- | --- |
| `cleanWorkRoot` | `internal/hook/hook.go:211` | sim |
| `trimCacheByAge` | `internal/hook/hook.go:321` | sim |
| `cleanup(opts, ctx, purgeCaches)` | `internal/hook/hook.go:163` | sim (ctx é 2º param) |
| `runWithTimeout` | `internal/hook/hook.go:401` | sim |
| `safeWorkRoot` | `internal/hook/hook.go:450-458` (sem EvalSymlinks) | sim |
| `appendLog` | `internal/hook/hook.go:541` (0644 world-readable) | sim |
| `isIgnorableCacheDeleteRace` | `internal/hook/hook.go:534-538` | sim |
| `onlyIgnorable…`/`demoteIgnorable…` | `internal/hook/hook.go:511-532` | sim |
| `EventJobCompleted` branch | `internal/hook/hook.go:138` | sim |
| `Action` struct (+`Escalation`) | `internal/hook/hook.go:41` | sim |
| `Install`/`InstallOptions`/`InstallResult` | `internal/hook/install.go:90/31/49` | sim |
| `ScriptContent` (padrão de conteúdo runtime) | `internal/hook/install.go:230` | sim |
| `validateCleanupRoot (string,error)` | `internal/cleanup/cleanup.go:290` | sim |
| `scanAndMaybeDelete` (rm sem `--`) | `internal/cleanup/cleanup.go:215/270` | sim |
| `dockerPrune` (`system prune -af --volumes`) | `internal/cleanup/cleanup.go:334/348` | sim |
| `Run` early-return por lock | `internal/cleanup/cleanup.go:127-132` | sim |
| `Collect` (tem ctx) | `internal/doctor/doctor.go:135` | sim |
| `collectHookChecks` (SEM ctx — mudar) | `internal/doctor/doctor.go:276` | sim |
| `Restart` (função, não struct) | `internal/runner/restart.go:52` | sim |
| `RestartOptions{Unit,Execute,RunFn,Short}` | `internal/runner/restart.go:14-21` | sim |
| `ValidateServiceUnit` (Unit explícito) | `internal/runner/restart.go:120-122` / `civm.go:165` | sim |
| `Status{UnitName,Repo,Name}` (sem WorkingDirectory) | `internal/runner/list.go:12-20` | sim |
| `restartCandidates`/`restartWatchdogRunners` | `internal/runner/watchdog.go:525/265` | sim |
| `resolveWatchdogRunnerRepo`/`enrichWatchdogSystemdRepos` | `internal/runner/watchdog.go:838/827` | sim |
| `rerunState`/`RerunMarker`/`writeRerunState` (não-atômico) | `internal/runner/watchdog.go:566/559/591` | sim |
| idle skip (`maxExit(...,1)` em 201) | `internal/runner/watchdog.go:190-203` | sim |
| `composeRootCause(r Report)` (antes do host read) | `internal/diskdoctor/diskdoctor.go:116/266` | sim |
| `hostMetrics{VFreeGB}`/`hostHeadroomViolation` | `internal/diskdoctor/diskdoctor.go:86/282` | sim |
| `civm-host-metrics.ps1` 3 blocos `$metrics` | `deploy/windows/civm-host-metrics.ps1:119/144/168` | sim |

---

## ITEM-0 — Baseline (Slice 0, bloqueante) — fecha DT-v2-14, DT-v2-16

**Coletado NO GUEST `gha-ubuntu-2404` (não no box de dev WSL2, que não tem runners nem `/run/civm`).** Produz o
artefato commitado `docs/specs/civm-runner-reliability/baseline-<YYYY-MM-DD>.txt` **antes** de qualquer código:

- `grep -c '"decision":"error"' /var/log/civm/hooks.jsonl` (e quantos por filesystem/`permission denied`/`unlinkat`);
- count de `exit status 125` em `hooks.jsonl`;
- `civmctl disk-doctor --json`; `docker system df`;
- `Get-VHD .BlockSize` (host elevado — gate de ITEM-13/14);
- `which chown rm` (confirmar `/usr/bin/*`); após `hook install --execute`:
  `sudo -n /usr/local/bin/civm-safedelete --check` deve sair 0 (DT-v2-14);
- `cat /proc/sys/net/ipv4/ip_local_port_range`, `systemctl list-units 'actions.runner.*'`.

O rollback-trigger numérico (§abaixo) referencia esse artefato; sem a pré-imagem persistida, o gate "→0 em 3 dias"
é não-falsificável.

## Mapa Kahneman v2 (overrides/adições)

| Etapa / ITEM | Disciplina | Pergunta obrigatória | Evidência mínima | Abort trigger |
| --- | --- | --- | --- | --- |
| DT-v2-1 (sudoers casa o invoke?) | #1, #5 | O `sudo -n` da escalada casa a regra do sudoers em **todo** box? | `/usr/bin/chown` E `/bin/chown` existem; wrapper único elimina ambiguidade; `sudo -n civm-safedelete --check`=0 no ITEM-0 | `sudo -n civm-safedelete --check` falhar após `hook install` em qualquer host → CRITICAL, não prosseguir |
| DT-v2-3/DT-v2-7 (escalada não escapa o `_work`) | #5 | A escalada pode apagar fora do `_work` no pior caso? | guard duplo (GuardFn caller + validação interna + EvalSymlinks/Lstat) **e** wrapper re-valida no lado root | a escalada tocar **1** path fora do escopo (teste/auditoria) → CRITICAL, reverter imediato |
| DT-v2-2 (mapeamento sentinela→unidade) | #1, #5 | O restart pode atingir a unidade errada a partir de log atacável? | Unit vem da **enumeração systemd** (`Status.UnitName`), `repository` só casa `Status.Repo`; `ValidateServiceUnit` re-aplicada | log forjado disparar restart de unidade não-enumerada → bug de mapeamento, reverter gatilho |
| DT-v2-4 (anti restart-loop) | #5 | Fechar o loop de auto-cura cria restart-loop? | contador tipado `AutoRestarts` + janela 1h + write atômico; teto=3 | auto-restart > teto/hora na mesma unidade → desligar gatilho, tratar manual |
| DT-v2-12 (fail-open por tipo, não substring) | #5 | Tornar não-fatal esconde falha real (mount/overlay)? | classifica por `errors.Is`/`syscall.ENOTEMPTY`; não-reconhecido = fatal | job aceito com erro não-reconhecido demovido → reverter o demote |
| DT-v2-6 (BlockSize root-cause) | #1 | O root-cause "online shrink expected" é falso neste box? | empírico: 32 MiB + UNMAP online não honrado; BlockSize lido **antes** de composeRootCause | composeRootCause não trocar para `rootCauseBlockSizeTooLarge` com BlockSize>1MiB → fiação errada |
| (herdados) ITEM-13/14 BlockSize 1MiB; ITEM-15 `/run/civm`; ITEM-4b cron prune | #1/#3/#5 | (ver baseline §Mapa) | (ver baseline) | (ver baseline) + DT-v2-17 qualifica o hazard do prune |

**Gate Kahneman #1 (mantido):** `Get-VHD .BlockSize` declarado "a confirmar em host elevado" antes de afirmar 1 MiB;
ITEM-13/14 bloqueados até confirmar.

## Rollback trigger v2 — fecha DT-v2-16

Reverter a slice se, **referenciado ao artefato `baseline-<data>.txt`**:

- após RF-1, `job-completed` com `decision=error` por filesystem **não** cair a 0 em 3 dias de jobs root-writing
  (delta medido contra o baseline persistido); OU
- a escalada `sudo` tocar **qualquer** path fora do escopo validado 1x (CRITICAL — reverter imediato); OU
- `sudo -n /usr/local/bin/civm-safedelete --check` falhar em qualquer um dos 8 hosts após `hook install` (DT-v2-1); OU
- após RF-4, o Optimize offline num VHDX 1 MiB não recuperar ≈o esperado em 3 medições; OU
- o auto-restart do watchdog disparar > `DefaultRunnerAutoRestartPerHour` vezes/hora na mesma unidade.

## Checklist de segurança v2 (overrides)

- [ ] **Escalada de privilégio:** o **único** binário sob NOPASSWD é `/usr/local/bin/civm-safedelete`;
      `chown`/`rm` absolutos são chamados de dentro do wrapper (já root). Sem NOPASSWD em binário cru, sem wildcard de path.
- [ ] **Exec safety:** `--` precede todo path em `chown -R ... -- <path>` e `rm -rf --one-file-system -- <path>`
      (no wrapper e no contrato Go). `exec.CommandContext` sem shell. Sudoers via temp + `visudo -cf` + `os.Rename`.
- [ ] **Symlink hardening (DT-v2-7):** `EvalSymlinks`+`Lstat` dono `emdev` antes do rm privilegiado; testes de
      symlink/`..` são gates CRITICAL.
- [ ] **Input validation:** `safedelete.Remove` valida (abs, sem NUL, não-`/`/`$HOME`/bare-home, GuardFn, EvalSymlinks);
      `hooks.jsonl` truncado tratado (DT-v2-15); restart só de unidade enumerada (DT-v2-2).
- [ ] **Fail-open por tipo (DT-v2-12):** classifica por `errors.Is`/`syscall.ENOTEMPTY`, não substring; não-reconhecido = fatal.
- [ ] **Secrets/logs:** nenhum segredo em `deploy/`; `hooks.jsonl` grava paths (não conteúdo); labels limitados
      (event/decision/escalation/unit), sem slug/PII.

## Contratos e documentação viva (overrides)

| Documento | Atualização v2 | Motivo |
| --- | --- | --- |
| `docs/specs/civm-runner-reliability/PRD.md` (112/159/230/299, 117/276) | **Alterar** | sudoers wrapper-scoped (não path-wildcard); restart de `actions.runner.*` (não `civm-<slot>`) — DT-v2-1/2/3 |
| `deploy/bin/civm-safedelete` (novo, versionado) | **Criar** | wrapper root validado; única coisa sob NOPASSWD — DT-v2-3/5 |
| `deploy/sudoers.d/civm-cleanup` | **Criar** | `NOPASSWD: /usr/local/bin/civm-safedelete` — DT-v2-1/3 |
| `runbooks/RUNBOOK-CIVM-RUNNER-RELIABILITY.md` | **Criar** | + verificação `secure_path`/`which`, baseline-artifact, wrapper |
| `docs/specs/civm-runner-reliability/baseline-<data>.txt` | **Criar** (no guest) | pré-imagem do rollback-trigger — DT-v2-16 |
| `host-volume-reclamation/SPECv2.md`, `multi-project-isolation/SPECv2.md`, runbooks vizinhos | **Alterar** | como no baseline §Contratos |

---

## Veredito Go/No-Go para PASSO 3

**GO — condicionado a este SPECv2 como camada vinculante.**

A arquitetura do `SPEC.md` é aprovada. PASSO 3 implementa `SPEC.md` **com os overrides DT-v2-1..DT-v2-19 acima**.
Os 4 blockers CRÍTICOS (sudoers path-mismatch, watchdog unit/sintaxe inexistentes, PRD↔SPEC sudoers divergente,
contador anti-loop sem home) estão fechados com decisão de código exata; os HIGH/MEDIUM/LOW idem.

**Gates obrigatórios antes de declarar a slice pronta (bloqueiam o merge):**

1. **ITEM-0 baseline-artifact commitado** (no guest) antes de qualquer código de implementação — DT-v2-16.
2. **`sudo -n /usr/local/bin/civm-safedelete --check` = 0** em todos os 8 hosts após `hook install --execute` — DT-v2-1/14.
3. **Testes de path do `safedelete` (incl. symlink/`..`) verdes como gates CRITICAL** — DT-v2-7.
4. **`Get-VHD .BlockSize` confirmado em host elevado** antes de afirmar 1 MiB / habilitar ITEM-13/14 — Kahneman #1.
5. **Auto-restart só de unidade enumerada pelo systemd**, com teste de log forjado → nenhum restart — DT-v2-2.
6. **Contador `AutoRestarts` tipado + write atômico** com teste N>teto → exatamente `teto` restarts — DT-v2-4.
7. `go test ./... -race -count=1`, `golangci-lint run -c .golangci.yml ./...`, `gofmt -l` limpos;
   `go list -deps ./internal/safedelete | grep -E 'internal/(hook|cleanup)'` vazio (sem import cycle).

Onde este v2 conflitar com `SPEC.md`, **o v2 prevalece**.
