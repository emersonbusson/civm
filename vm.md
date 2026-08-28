# VM — gha-ubuntu-2404 (civm CI runner)

> Inventário da máquina virtual que hospeda os self-hosted runners do civm.
> Mantenha atualizado a cada mudança de toolchain/SO ou limpeza grande.
>
> **Disco revalidado em 2026-07-30; memória em 2026-08-05** — coleta read-only
> no guest e métricas do host. Toolchains abaixo mantêm a última atualização
> completa de 2026-06-17 até novo inventário.

## Host & hardware

- **Hypervisor:** Hyper-V no host Windows (EMEDEV). Acesso elevado via Windows
  `sudo` (UAC off) — Get-VM/Optimize-VHD/Start/Stop funcionam.
- **VHDX:** dinâmico em `V:\Hyper-V\gha-ubuntu-2404\Virtual Hard Disks\gha-ubuntu-2404.vhdx`.
  O volume **V: (119 GB) é o teto real de disco** — o VHDX cresce nele.
- **vCPU:** 12 · **RAM:** 12 GiB fixos (Hyper-V materializa VMRS enquanto ligada;
  scale-to-zero o libera quando ociosa) · **Disco do guest:** 40 GiB.

## Sistema

| | |
| --- | --- |
| OS | Ubuntu 24.04.4 LTS |
| Kernel | 6.8.0-124-generic |
| apt lists atualizado | 2026-06-17 04:46 |
| dpkg última mudança | 2026-06-17 04:49 |

## Toolchains globais

| Ferramenta | Versão |
| --- | --- |
| Go | go1.26.5 (última; em /usr/local/go) |
| Node (default via nvm) | v24.15.0 |
| npm | 11.13.0 (global: 11.12.1) |
| yarn | 1.22.22 (Classic / v1) |
| Python | 3.12.3 |
| Docker | 29.1.3 |
| git | presente |

## nvm + Node (multi-versão)

- **nvm:** instalado (`~/.nvm`).
- **Versões de Node instaladas:** v4.9.1 · v6.17.1 · v8.17.0 · v10.24.1 ·
  v12.22.12 · v14.21.3 · v16.20.2 · v18.20.8 · v20.20.2 · v22.22.2 · v24.14.1 ·
  v24.15.0 · v24.16.0.
- **Default:** v24.15.0 (mais recente instalada: **v24.16.0**).
- Jobs com `actions/setup-node` baixam/usam a versão pedida sob
  `~/actions-runner-*/_work/_tool/node`.

## npm globais

- `corepack@0.34.6`
- `npm@11.12.1`

## Runners (multi-repo — NÃO é dedicada ao acme)

A VM hospeda runners self-hosted para **7 repos**, todos compartilhando a mesma
box (CPU/RAM/disco/daemon Docker):

`acme` · `acme-org` · `service-a` · `service-b` ·
`service-c` · `service-d` · `peer`

Cada um com seu `~/actions-runner-{repo}/` e cache yarn escopado
(`~/.cache/yarn-{repo}-*`), então uma limpeza pode ser escopada por repo.

## Disco — modelo de limpeza

- **Estado atual:** `/` tem `37,70 GiB` totais e `14,03 GiB` disponíveis; Docker
  deixou `174` volumes inativos e `10,44 GB` recuperáveis.
- **Alvo legado inválido:** `MinFreeGB=58` é maior que o filesystem inteiro e
  não pode ser atingido. A recalibração depende primeiro da limpeza scoped da
  issue #181 e das medições da issue #182.
- **VHDX:** dinâmico, máximo lógico atual `40 GiB`. O owner `civm-host`
  executa `Optimize-VHD -Mode Full` somente com VM Off, cadeia base-only e
  lock de reclaim. Em 30/07/2026, o boundary natural deixou `V:` com
  `80,50 GiB` livres.
- **Hygiene contínua (deployada):**
  - `civmctl-buildcache-prune.timer` (3 min) → build cache (`builder prune -af`)
    + imagens de service de runs finalizadas (`civm-run-{runid}-*`),
    BuildKit/container-safe, sem deferir ao heavy-lock.
  - `civmctl-disk-watchdog` + `civmctl-cleanup` → hygiene geral.
  - `civm-vhdx-autoreclaim` (host) → **DESABILITADA desde 2026-06-17**
    (superseded pelo orchestrator scale-to-zero, que virou o único dono do
    stop+compact). Compactava o VHDX quando V: baixo + guest idle; hoje quem
    faz isso é o orchestrator (próximo item).
- **Scale-to-zero (orchestrator):** `deploy/windows/civm-vm-orchestrator.ps1` —
  liga a VM sob demanda (job na fila) e, na fronteira de cada PR (idle ≥ N min),
  faz full clean + Stop-VM + Optimize-VHD, devolvendo **RAM e disco ao Windows**
  entre rajadas.

> Correção de 30/07/2026: `Compacted` ainda não prova full clean. O fluxo Off
> pulou a limpeza guest e os volumes named sobreviveram. Issues:
> `civm-host#17`, `#181` e `#182`.
