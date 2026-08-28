#!/usr/bin/env bash
# civm CI artifact hygiene — roda a cada tick do civmctl-buildcache-prune.timer
# (3min). Reclama os dois artefatos efemeros que mais incham o disco da box, de
# forma SEGURA sob carga (sem deferir ao docker-heavy-lock, ao contrario do
# disk-watchdog que faz early-return quando um build pesado segura o lock):
#
#   1. Build cache nao-usado: `docker builder prune --force --all`. BuildKit
#      exclui o cache em uso por um build ativo (so dropa o de builds
#      finalizados) e nunca toca imagem tagged -> sem a corrida "No such image".
#
#   2. Imagens de service de runs JA FINALIZADAS (`civm-run-{runid}-*`). Cada
#      run builda ~15 imagens taggeadas com o seu run id; o teardown per-job
#      deveria derruba-las, mas run cancelada nao roda teardown -> elas vazam
#      (somavam ~15G de runs antigas, 2026-06-17). Aqui e seguro porque:
#        - so removemos um runid cujo container NAO esta rodando (docker ps), e
#        - o `docker rmi` recusa qualquer imagem ainda usada por um container.
#      As imagens vendor (acme/postgres, redis, minio, clamav...) nao casam o
#      glob civm-run-* -> nunca tocadas (sem a corrida vendor-date do image -a).
set -u

docker builder prune --force --all >/dev/null 2>&1 || true

# runids cujo container esta vivo agora — o run ATIVO, que nao podemos tocar.
active="$(docker ps --format '{{.Image}}' 2>/dev/null | grep -oE 'civm-run-[0-9]+' | sort -u)"

# Guarda de idade: `docker ps` NAO ve a run numa janela entre-jobs (imagem ja
# buildada, container do passo anterior parado, o `up` do proximo passo ainda
# nao subiu) -> sem isto o prune evictava a imagem de uma run ATIVA e o `up`
# seguinte morria com "No such image" (2026-06-17, tenant-isolation-smoke). So
# removemos imagens com mais de 3h: nenhuma run dura tanto, entao 3h sem
# container = run garantidamente encerrada. Fail-safe: data que nao parseia
# vira epoch futuro -> tratada como recente e NAO removida.
cutoff="$(date -d '3 hours ago' +%s 2>/dev/null || echo 0)"

docker images 'civm-run-*' --format '{{.ID}} {{.Repository}}' 2>/dev/null \
  | sort -u | while read -r id repo; do
      rid="$(printf '%s' "$repo" | grep -oE 'civm-run-[0-9]+')"
      [ -z "$rid" ] && continue
      # pula o run ativo; o rmi tambem recusa sozinho qualquer imagem in-use.
      printf '%s\n' "$active" | grep -qxF "$rid" && continue
      # pula imagem recente (<3h): a run pode estar numa janela entre-jobs.
      created="$(docker inspect --format '{{.Created}}' "$id" 2>/dev/null)"
      created_epoch="$(date -d "$created" +%s 2>/dev/null || echo 9999999999)"
      [ "$created_epoch" -gt "$cutoff" ] && continue
      docker rmi "$id" >/dev/null 2>&1 || true
    done

exit 0
