#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Local watch-officer test harness (Podman). See README.md.
#
#   ./run.sh up      build images, create networks/volumes/secret, start everything
#   ./run.sh down    stop and remove containers (keeps volumes, images, secret)
#   ./run.sh purge   down + remove volumes, networks and the key secret
#
# Network layout mirrors the Phase 1 CiliumNetworkPolicies:
#   fam-int (--internal, no internet, no host): hermes, model-gate, tool servers
#   fam-ext (normal):  model-gate (-> api.anthropic.com), tool servers (-> kubeproxy, Traefik)
#   kubeproxy is on fam-ext only, so hermes can never reach the Kubernetes API.
set -euo pipefail
export PATH=/opt/podman/bin:$PATH

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
# Site values come from the environment or from local/tests/hermes-local.env
# (gitignored): READONLY_KUBECONFIG, TRAEFIK (ingress host:port), CERT_HOSTS.
site_env="$repo/local/tests/hermes-local.env"
if [ -f "$site_env" ]; then . "$site_env"; fi
key_file=${KEY_FILE:-$HOME/.config/familiar/watch-officer.key}
kubeconfig=${READONLY_KUBECONFIG:?set READONLY_KUBECONFIG (a read-only kubeconfig)}

HERMES_IMAGE=docker.io/nousresearch/hermes-agent@sha256:3e9fe4c6ef36490f43aa0d0d80f9df64e9c92a576eee3042f1a97c8813c05355 # v0.23.3
KUBECTL_IMAGE=registry.k8s.io/kubectl@sha256:1c652f3ee7a49fea921d49ba8102b5fd3d909f6561e2fd02acf0bbd3e8547153 # v1.37.2
GATE_IMAGE=localhost/familiar/model-gate:test
TOOLS_IMAGE=localhost/familiar/tools:test

PROM_URL=http://kubeproxy:8001/api/v1/namespaces/monitoring/services/kps-prometheus:9090/proxy
TRAEFIK=${TRAEFIK:?set TRAEFIK to the ingress controller address, host:port}
CERT_HOSTS=${CERT_HOSTS:?set CERT_HOSTS to the comma-separated hosts cert-probe checks}

containers=(hermes memory-auditor cluster-read metrics-query ceph-health cert-probe model-gate kubeproxy)

up() {
  [[ -r $key_file ]] || { echo "missing API key file: $key_file" >&2; exit 1; }
  [[ -r $kubeconfig ]] || { echo "missing read-only kubeconfig: $kubeconfig" >&2; exit 1; }

  podman build -q -t "$GATE_IMAGE" "$repo/model-gate" >/dev/null
  podman build -q -t "$TOOLS_IMAGE" "$repo/tools" >/dev/null

  podman network exists fam-int || podman network create --internal fam-int >/dev/null
  podman network exists fam-ext || podman network create fam-ext >/dev/null
  for v in fam-gate-state fam-hermes-data fam-audit; do podman volume exists $v || podman volume create $v >/dev/null; done
  podman secret exists fam-watch-officer-key || podman secret create fam-watch-officer-key "$key_file" >/dev/null
  podman secret exists fam-readonly-kubeconfig || podman secret create fam-readonly-kubeconfig "$kubeconfig" >/dev/null

  podman run -d --name kubeproxy --network fam-ext \
    --secret fam-readonly-kubeconfig,target=/kubeconfig,mode=0400 \
    --read-only --cap-drop ALL --security-opt no-new-privileges \
    "$KUBECTL_IMAGE" --kubeconfig /kubeconfig proxy --address 0.0.0.0 --port 8001 --accept-hosts '^kubeproxy(:8001)?$' >/dev/null

  podman run -d --name model-gate --network fam-int:alias=model-gate.familiar-system.svc.cluster.local --network fam-ext \
    --secret fam-watch-officer-key,target=/etc/model-gate/keys/watch-officer,mode=0400,uid=65532 \
    -v "$here/model-gate.json:/etc/model-gate/config.json:ro" \
    -v fam-gate-state:/var/lib/model-gate \
    --read-only --cap-drop ALL --security-opt no-new-privileges \
    "$GATE_IMAGE" -config /etc/model-gate/config.json >/dev/null

  # Tool servers answer on fam-int under their cluster DNS names, so
  # deploy/familiar/hermes/config.yaml works unchanged.
  tool() { local n=$1; shift
    podman run -d --name $n --network fam-int:alias=$n.familiar-tools.svc.cluster.local --network fam-ext \
      --read-only --cap-drop ALL --security-opt no-new-privileges --entrypoint /$n "$@" "$TOOLS_IMAGE" >/dev/null; }
  tool cluster-read -e KUBE_DEV_PROXY_URL=http://kubeproxy:8001
  tool metrics-query -e PROMETHEUS_URL=$PROM_URL
  tool ceph-health -e PROMETHEUS_URL=$PROM_URL
  tool cert-probe -e TARGET=$TRAEFIK -e HOSTS="$CERT_HOSTS"

  # Matrix (round 2): only when the local Synapse and the bot token exist.
  local matrix_env=()
  if podman container exists synapse && podman secret exists fam-matrix-token; then
    matrix_env=(--secret fam-matrix-token,type=env,target=MATRIX_ACCESS_TOKEN
      -e MATRIX_HOMESERVER=http://synapse:8008 -e MATRIX_USER_ID=@familiar:familiar.test
      -e MATRIX_DEVICE_ID=HERMES_BOT -e MATRIX_ALLOWED_USERS=@jase:familiar.test
      -e MATRIX_E2EE_MODE=required
      -e MATRIX_RECOVERY_KEY_OUTPUT_FILE=/opt/data/platforms/matrix/recovery-key.txt
      ${MATRIX_HOME_ROOM:+-e MATRIX_HOME_ROOM=$MATRIX_HOME_ROOM})
  fi

  # Hermes: internal network only, run like deploy/familiar/hermes/deployment.yaml:
  # uid 10000, read-only root filesystem, the same config, SOUL.md and
  # scripts, read-only persistence points. The data volume is pre-owned by
  # 10000 (fsGroup in the cluster); config is copied in at every start.
  local hdir="$repo/deploy/familiar/hermes"
  podman run --rm --user 0 -v fam-hermes-data:/d -v "$hdir/config.yaml:/config.yaml:ro" \
    --entrypoint sh "$HERMES_IMAGE" -c 'cp /config.yaml /d/config.yaml && chown 10000:10000 /d /d/config.yaml && chmod 700 /d'
  podman run -d --name hermes --network fam-int --user 10000:10000 --read-only \
    -v fam-hermes-data:/opt/data --tmpfs /run:rw,mode=1777 --tmpfs /tmp:rw,mode=1777 \
    -v "${SOUL_FILE:-$hdir/SOUL.md}:/opt/data/SOUL.md:ro" \
    -v "$hdir/scripts:/opt/data/scripts:ro" \
    -v "$here/hermes/hooks:/opt/data/hooks:ro" \
    -v "$here/hermes/plugins:/opt/data/plugins:ro" \
    -v "$here/hermes/skills:/opt/data/skills:ro" \
    "${matrix_env[@]}" \
    --cap-drop ALL --security-opt no-new-privileges \
    "$HERMES_IMAGE" gateway run >/dev/null
  # memory-auditor: Hermes' volume read-only, its own volume, no network.
  podman run --rm --user 0 -v fam-audit:/a --entrypoint sh "$HERMES_IMAGE" -c 'chown 10000:10000 /a'
  podman run -d --name memory-auditor --network none --user 10000:10000 --read-only --tmpfs /tmp:rw,mode=1777 \
    -v fam-hermes-data:/hermes:ro -v fam-audit:/audit \
    -v "$hdir/config.yaml:/baseline/config.yaml:ro" -v "$hdir/memory-auditor.py:/app/memory-auditor.py:ro" \
    -e INTERVAL=${AUDIT_INTERVAL:-60} -e HOME=/tmp --cap-drop ALL --security-opt no-new-privileges \
    --entrypoint /opt/hermes/.venv/bin/python3 "$HERMES_IMAGE" /app/memory-auditor.py >/dev/null
  podman ps --filter name='^(hermes|memory-auditor|cluster-read|metrics-query|ceph-health|cert-probe|model-gate|kubeproxy)$' --format '{{.Names}}\t{{.Status}}'
}

down() { podman rm -f "${containers[@]}" >/dev/null 2>&1 || true; }

purge() {
  down
  podman volume rm -f fam-gate-state fam-hermes-data fam-audit >/dev/null 2>&1 || true
  podman network rm -f fam-int fam-ext >/dev/null 2>&1 || true
  podman secret rm fam-watch-officer-key fam-readonly-kubeconfig >/dev/null 2>&1 || true
}

case ${1:-} in
  up) up ;;
  down) down ;;
  purge) purge ;;
  *) sed -n '2,11p' "$0"; exit 1 ;;
esac
