#!/usr/bin/env bash
#
# End-to-end smoke test for drydock against a real Docker daemon.
#
# It starts a private registry, runs a container from a tag, then moves the tag
# to new images and checks that drydock:
#
#   1. reports an up-to-date container,
#   2. updates to a changed image and keeps the container running,
#   3. rolls back when the new image cannot start,
#   4. restores the previous version on a manual rollback,
#   5. continues following the original tag after a rollback.
#
# Usage: ./scripts/smoke.sh
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BINARY="${BINARY:-$ROOT/drydock}"
PORT="${PORT:-5055}"
REG="127.0.0.1:${PORT}"
REG_NAME="drydock-smoke-registry"
APP_NAME="drydock-smoke-app"
WORK="$(mktemp -d)"

log()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m  ✓\033[0m %s\n' "$*"; }
die()  { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

cleanup() {
  docker rm -f "$APP_NAME" "$REG_NAME" >/dev/null 2>&1 || true
  docker rmi "$REG/smoke/app:latest" >/dev/null 2>&1 || true
  rm -rf "$WORK"
}
trap cleanup EXIT

[[ -x "$BINARY" ]] || die "binary not found at $BINARY (run 'make build' first)"
command -v docker >/dev/null || die "docker is required"
command -v python3 >/dev/null || die "python3 is required"

outcome_status() {
  python3 -c 'import json,sys; d=json.load(sys.stdin); print(" ".join(o["status"] for o in d["outcomes"]))'
}

image_of() { docker inspect -f '{{.Image}}' "$APP_NAME"; }
running()  { docker inspect -f '{{.State.Status}}' "$APP_NAME"; }

assert_status() {
  local json="$1" want="$2" got
  got="$(printf '%s' "$json" | outcome_status)"
  [[ "$got" == "$want" ]] || die "expected status '$want', got '$got'"
  ok "status = $want"
}

assert_image() {
  local want="$1" got
  got="$(image_of)"
  [[ "$got" == "$want" ]] || die "expected image $want, got $got"
  ok "running image $want"
}

log "starting private registry on $REG"
docker rm -f "$REG_NAME" >/dev/null 2>&1 || true
docker run -d --name "$REG_NAME" -p "127.0.0.1:${PORT}:5000" registry:2 >/dev/null
for _ in $(seq 1 30); do
  if curl -fsS "http://${REG}/v2/" >/dev/null 2>&1; then break; fi
  sleep 0.5
done
curl -fsS "http://${REG}/v2/" >/dev/null || die "registry did not start"
ok "registry ready"

log "preparing images"
docker pull -q busybox:1.36 >/dev/null
docker pull -q alpine:3.20 >/dev/null
mkdir -p "$WORK/scratch"
printf 'FROM scratch\n' > "$WORK/scratch/Dockerfile"
docker build -q -t "$REG/smoke/broken:latest" "$WORK/scratch" >/dev/null
ok "images ready"

push_as_app() { # <image>
  docker tag "$1" "$REG/smoke/app:latest"
  docker push -q "$REG/smoke/app:latest" >/dev/null
}

log "pushing the initial image and starting the container"
push_as_app busybox:1.36
docker rmi "$REG/smoke/app:latest" >/dev/null 2>&1 || true
docker pull -q "$REG/smoke/app:latest" >/dev/null
docker run -d --name "$APP_NAME" "$REG/smoke/app:latest" sleep 3600 >/dev/null
BUSYBOX_ID="$(image_of)"
ok "container running $(printf '%s' "$BUSYBOX_ID" | cut -c1-19)"

cat > "$WORK/config.yaml" <<EOF
interval: 1h
state_file: $WORK/state.json
concurrency: 1
rollback: true
settle: 1s
health_timeout: 20s
stop_timeout: 5s
log_level: warn
registry:
  plain_http: true
  timeout: 10s
  retries: 1
EOF

log "check: image has not moved"
JSON="$("$BINARY" check --config "$WORK/config.yaml" --only "$APP_NAME" --json 2>/dev/null)" || true
assert_status "$JSON" "up_to_date"

log "pushing a new image"
push_as_app alpine:3.20
JSON="$("$BINARY" update --config "$WORK/config.yaml" --only "$APP_NAME" --json 2>/dev/null)" || true
assert_status "$JSON" "updated"
ALPINE_ID="$(image_of)"
[[ "$ALPINE_ID" != "$BUSYBOX_ID" ]] || die "image did not change"
[[ "$(running)" == "running" ]] || die "container is not running after update"
ok "container is running $(printf '%s' "$ALPINE_ID" | cut -c1-19)"

log "pushing an image that cannot start"
docker tag "$REG/smoke/broken:latest" "$REG/smoke/app:latest"
docker push -q "$REG/smoke/app:latest" >/dev/null
JSON="$("$BINARY" update --config "$WORK/config.yaml" --only "$APP_NAME" --json 2>/dev/null)" || true
assert_status "$JSON" "rolled_back"
assert_image "$ALPINE_ID"
[[ "$(running)" == "running" ]] || die "container was not restored"
ok "container restored and running"

log "manual rollback to the previous version"
JSON="$("$BINARY" rollback "$APP_NAME" --config "$WORK/config.yaml" --json 2>/dev/null)" || true
printf '%s' "$JSON" | python3 -c 'import json,sys; d=json.load(sys.stdin); assert not d.get("error"), d.get("error")'
assert_image "$BUSYBOX_ID"
[[ "$(running)" == "running" ]] || die "container is not running after rollback"
ok "container is running"

log "the restored container must still follow its original tag"
push_as_app alpine:3.20
JSON="$("$BINARY" update --config "$WORK/config.yaml" --only "$APP_NAME" --json 2>/dev/null)" || true
assert_status "$JSON" "updated"
assert_image "$ALPINE_ID"

printf '\n\033[1;32mSMOKE TEST PASSED\033[0m\n'
