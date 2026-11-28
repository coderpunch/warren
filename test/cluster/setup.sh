#!/bin/bash
# Make a kind cluster that enforces NetworkPolicy, for the cluster checks.
# Necessary programs: docker, kind, curl.
set -euo pipefail
cd "$(dirname "$0")"

NAME=warren-dev
NODE="$NAME-control-plane"
CALICO_VERSION="${CALICO_VERSION:-v3.33.1}"
# Set CALICO_REGISTRY=docker.io when the network cannot connect to quay.io.
CALICO_REGISTRY="${CALICO_REGISTRY:-quay.io}"
K="docker exec -i $NODE kubectl"

kind create cluster --name "$NAME" --config kind.yaml

# Calico, with the pod network of kind.yaml and with no encapsulation.
curl -fsSL "https://raw.githubusercontent.com/projectcalico/calico/$CALICO_VERSION/manifests/calico.yaml" \
  | sed -e "s|quay.io/calico/|$CALICO_REGISTRY/calico/|" \
        -e '/- name: CALICO_IPV4POOL_IPIP/{n;s|"Always"|"Never"|}' \
        -e 's|# - name: CALICO_IPV4POOL_CIDR|- name: CALICO_IPV4POOL_CIDR|' \
        -e 's|#   value: "192.168.0.0/16"|  value: "10.244.0.0/16"|' \
  | $K apply -f - >/dev/null

echo "Waiting for the node and the system pods."
$K wait --for=condition=Ready node --all --timeout=300s
$K -n kube-system wait --for=condition=Ready pod --all --timeout=300s
