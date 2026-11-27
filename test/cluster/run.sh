#!/bin/bash
# Build warren, put it into the cluster of setup.sh, and do the cluster checks.
# Use: run.sh [isolation] [storage] [lifecycle]   (default: all three)
# Set WARREN_IMAGE to use a warren image that Docker already has.
# Necessary programs: docker, python3. The lifecycle check also needs the
# Python package "websockets".
set -uo pipefail
cd "$(dirname "$0")"

NODE=warren-dev-control-plane
K="docker exec -i $NODE kubectl"
PYTHON="${PYTHON:-python3}"
TERMINAL=ghcr.io/open-webui/open-terminal:0.18.1
checks=("$@")
[ ${#checks[@]} -eq 0 ] && checks=(isolation storage lifecycle)

# Copy one image from Docker into the node. The command "kind load" cannot
# copy an image that has more than one platform.
load() {
  local tar; tar="$(mktemp)"
  docker save --platform linux/amd64 "$1" -o "$tar" 2>/dev/null || docker save "$1" -o "$tar"
  docker cp "$tar" "$NODE:/image.tar"
  docker exec "$NODE" ctr -n k8s.io images import --platform linux/amd64 /image.tar >/dev/null 2>&1 || exit 1
  docker exec "$NODE" rm /image.tar
  rm -f "$tar"
}

# Remove all sandboxes, apply the manifests from standard input, and start warren again.
deploy() {
  local manifests; manifests="$(cat)"
  $K -n warren-sandboxes delete pods,pvc --all --wait=true --timeout=120s >/dev/null 2>&1 </dev/null
  printf '%s\n' "$manifests" | $K apply -f - >/dev/null || exit 1
  $K -n warren-system rollout restart deploy/warren >/dev/null </dev/null
  $K -n warren-system rollout status deploy/warren --timeout=240s >/dev/null </dev/null || exit 1
  sleep 5
}

if [ -n "${WARREN_IMAGE:-}" ]; then
  docker tag "$WARREN_IMAGE" warren:dev || exit 1
else
  docker build -q -t warren:dev ../.. >/dev/null || exit 1
fi
load warren:dev
for tag in openshift slim alpine; do
  docker image inspect "$TERMINAL-$tag" >/dev/null 2>&1 || docker pull -q "$TERMINAL-$tag" >/dev/null || exit 1
  load "$TERMINAL-$tag"
done

result=0
for check in "${checks[@]}"; do
  case "$check" in
    # The lifecycle check tests the sandbox limit of a user with a limit of 2.
    lifecycle) sed 's/--max-sandboxes-per-user=4/--max-sandboxes-per-user=2/' manifests.yaml | deploy ;;
    isolation|storage) deploy < manifests.yaml ;;
    *) echo "unknown check: $check"; exit 2 ;;
  esac
  "$PYTHON" "$check.py" > "$check.out" 2>&1
  code=$?
  echo "$check: exit $code, $(grep -c '^PASS' "$check.out") passed"
  grep -E '^(FAIL|FAILED|Traceback)' "$check.out" | cut -c1-400
  [ $code -ne 0 ] && result=1
done
exit $result
