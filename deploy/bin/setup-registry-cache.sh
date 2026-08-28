#!/usr/bin/env bash
# setup-registry-cache.sh — runner auto-limpante (civm-self-cleaning-runner, RF do
# registry pull-through cache). Resolve a falha "No such image de largada" na CI:
# o Docker Hub rate-limita pulls anônimos (100/6h/IP) e o mirror.gcr.io só espelha
# imagens `library/` (não cobre minio/clamav/evoapicloud). Sob carga do runner
# compartilhado os pulls falham e o `compose up --build` morre antes de começar.
#
# Solução (padrão de empresa séria): um registry:2 LOCAL como pull-through cache do
# docker.io. Cacheia QUALQUER namespace na primeira pull e serve local depois — as
# CIs seguintes não tocam o Docker Hub (zero rate limit). Sobrevive a `docker prune`
# (volume nomeado + container restart=always + imagem tagged em uso; cf.
# internal/hook/hook.go cleanup que já só poda DANGLING). Auth upstream opcional via
# DOCKERHUB_USER/DOCKERHUB_TOKEN levanta o limite anônimo do warm inicial.
#
# Idempotente: rodar de novo reconcilia o estado, nunca duplica.
#
# Uso:
#   ./setup-registry-cache.sh [--repo /caminho/acme] [--warm]
#   DOCKERHUB_USER=... DOCKERHUB_TOKEN=... ./setup-registry-cache.sh --warm
set -euo pipefail

CACHE_NAME="registry-cache"
CACHE_VOLUME="registry-cache-data"
CACHE_ADDR="127.0.0.1:5000"
CACHE_IMAGE="registry:2"
DAEMON_JSON="/etc/docker/daemon.json"
REPO=""
DO_WARM=0

while [ $# -gt 0 ]; do
  case "$1" in
    --repo) REPO="$2"; shift 2 ;;
    --warm) DO_WARM=1; shift ;;
    *) echo "arg desconhecido: $1" >&2; exit 2 ;;
  esac
done

log() { echo "[setup-registry-cache] $*"; }

# --- 1. registry:2 pull-through cache (idempotente) -------------------------------
ensure_cache() {
  docker volume inspect "$CACHE_VOLUME" >/dev/null 2>&1 || docker volume create "$CACHE_VOLUME" >/dev/null
  if docker inspect "$CACHE_NAME" >/dev/null 2>&1; then
    log "container $CACHE_NAME já existe; garantindo que está up"
    docker start "$CACHE_NAME" >/dev/null 2>&1 || true
    return
  fi
  log "subindo $CACHE_NAME ($CACHE_IMAGE) como pull-through cache de docker.io em $CACHE_ADDR"
  # Auth upstream é opcional: sem PAT o cache ainda funciona (pull anônimo na 1a vez,
  # local depois). Com PAT, o warm inicial não toma rate limit.
  local auth_env=()
  if [ -n "${DOCKERHUB_USER:-}" ] && [ -n "${DOCKERHUB_TOKEN:-}" ]; then
    log "auth upstream Docker Hub habilitada (usuário ${DOCKERHUB_USER})"
    auth_env=(-e "REGISTRY_PROXY_USERNAME=${DOCKERHUB_USER}" -e "REGISTRY_PROXY_PASSWORD=${DOCKERHUB_TOKEN}")
  else
    log "sem DOCKERHUB_USER/TOKEN: pull-through anônimo (cacheia mesmo assim; warm inicial sujeito ao limite anônimo)"
  fi
  # Escuta só em localhost (não expõe o cache à rede). restart=always sobrevive a reboot.
  docker run -d --restart always --name "$CACHE_NAME" \
    -p "${CACHE_ADDR}:5000" \
    -v "${CACHE_VOLUME}:/var/lib/registry" \
    -e "REGISTRY_PROXY_REMOTEURL=https://registry-1.docker.io" \
    -e "REGISTRY_STORAGE_DELETE_ENABLED=true" \
    "${auth_env[@]}" \
    "$CACHE_IMAGE" >/dev/null
}

# --- 2. daemon.json: registry-mirrors -> cache local ------------------------------
# O pull-through local cobre TODOS os namespaces docker.io; substitui o gcr.io
# (que só cobre library/). Preserva as demais chaves do daemon.json.
ensure_daemon_mirror() {
  local mirror="http://${CACHE_ADDR}"
  local tmp; tmp="$(mktemp)"
  if [ -s "$DAEMON_JSON" ] && command -v jq >/dev/null 2>&1; then
    jq --arg m "$mirror" '.["registry-mirrors"]=[$m]' "$DAEMON_JSON" >"$tmp"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$DAEMON_JSON" "$mirror" >"$tmp" <<'PY'
import json,sys,os
path,mirror=sys.argv[1],sys.argv[2]
d={}
if os.path.exists(path) and os.path.getsize(path):
    with open(path) as f: d=json.load(f)
d["registry-mirrors"]=[mirror]
print(json.dumps(d,indent=2))
PY
  else
    printf '{\n  "registry-mirrors": ["%s"]\n}\n' "$mirror" >"$tmp"
  fi
  if [ -f "$DAEMON_JSON" ] && cmp -s "$tmp" "$DAEMON_JSON"; then
    log "daemon.json já aponta para o cache; sem mudança"
    rm -f "$tmp"; return 1
  fi
  sudo install -m 0644 "$tmp" "$DAEMON_JSON"; rm -f "$tmp"
  log "daemon.json atualizado: registry-mirrors -> $mirror"
  return 0
}

# --- 3. warm set: pull das tags exatas do compose + bases dos Dockerfiles ---------
warm_images() {
  local imgs=()
  if [ -n "$REPO" ] && [ -f "$REPO/infra/docker-compose.yml" ]; then
    log "derivando warm set de $REPO (compose image: + Dockerfile FROM)"
    mapfile -t comp < <(grep -hoE '^\s*image:\s*\S+' "$REPO"/infra/docker-compose*.yml 2>/dev/null | sed -E 's/^\s*image:\s*//' | grep -vE '^acme/' | sort -u)
    mapfile -t froms < <(grep -rhoE '^FROM\s+\S+' "$REPO"/infra/Dockerfile.* "$REPO"/services/*/Dockerfile "$REPO"/web/Dockerfile 2>/dev/null | awk '{print $2}' | grep -vE '^(scratch|\$\{)' | grep -E '[:/]' | sort -u)
    imgs=("${comp[@]}" "${froms[@]}")
  else
    log "sem --repo: warm set core hardcoded (pinned do compose atual)"
    imgs=(
      redis:8.6.1-alpine3.23
      minio/minio:RELEASE.2025-09-07T16-13-09Z
      clamav/clamav:1.5
      alpine:3.23
      postgres:18.3-alpine3.23
    )
  fi
  # dedup
  mapfile -t imgs < <(printf '%s\n' "${imgs[@]}" | sort -u | grep -E '\S')
  log "aquecendo ${#imgs[@]} imagens via cache..."
  local ok=0 fail=0
  for img in "${imgs[@]}"; do
    if docker pull "$img" >/dev/null 2>&1; then
      log "  ✓ $img"; ok=$((ok+1))
    else
      log "  ✗ $img (rate limit? adicione DOCKERHUB_TOKEN ou reaqueça depois)"; fail=$((fail+1))
    fi
  done
  log "warm: $ok ok, $fail falhas"
  [ "$fail" -eq 0 ]
}

ensure_cache
need_restart=0
ensure_daemon_mirror && need_restart=1 || true
if [ "$need_restart" -eq 1 ]; then
  log "reiniciando docker para aplicar o mirror..."
  sudo systemctl restart docker
  sleep 5
  # o cache tem restart=always; garante que voltou
  docker start "$CACHE_NAME" >/dev/null 2>&1 || true
  sleep 2
fi
docker inspect -f 'cache: {{.State.Status}} (restart={{.HostConfig.RestartPolicy.Name}})' "$CACHE_NAME" 2>/dev/null || log "AVISO: cache não está running"
if [ "$DO_WARM" -eq 1 ]; then
  warm_images || log "warm parcial — ver acima"
fi
log "done. Mirrors ativos:"; docker info 2>/dev/null | grep -A2 'Registry Mirrors' || true
