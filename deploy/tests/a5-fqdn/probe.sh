#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
# Run the A5 probes from the test pod. Prints one line per target.
export KUBECONFIG=${KUBECONFIG:?set KUBECONFIG to an admin kubeconfig}
p() { # label, curl args...
  local label=$1; shift
  local out
  out=$(kubectl -n a5-egress-test exec probe -- curl -sS -o /dev/null -m 8 -w '%{http_code}' "$@" 2>&1)
  printf '%-34s %s\n' "$label" "$(tr -d '\n' <<<"$out" | cut -c1-90)"
}
p "allowed  api.anthropic.com:443" https://api.anthropic.com/
p "blocked  example.com:443" https://example.com/
p "blocked  github.com:443" https://github.com/
p "blocked  1.1.1.1:443 (raw IP)" -k https://1.1.1.1/
p "blocked  api.anthropic.com:80" http://api.anthropic.com/
printf '%-34s %s\n' "dns      example.com lookup" "$(kubectl -n a5-egress-test exec probe -- nslookup example.com 2>&1 | grep -A1 'Name:' | tail -1 | tr -s ' ')"
