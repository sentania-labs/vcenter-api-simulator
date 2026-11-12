#!/usr/bin/env bash
# Install the chart into a kind cluster and prove what an operator would see:
# the pod comes up with the default values, PowerCLI connects and lists the
# hosts and VMs, the certificate survives a pod restart (the identity a
# management pack pins), and a tls.existingSecret upgrade serves a CA-issued
# certificate that verifies against that CA. Then uninstall.
#
#   tools/smoke/helm-kind.sh <image:tag> [kind-cluster-name]
#
# Needs kind, kubectl, helm, openssl, pwsh with VMware.PowerCLI (the smoke
# script installs it with -Install), and a Docker daemon. Creates the cluster
# when it does not exist and leaves it for the caller to delete.
set -euo pipefail

image="${1:?image:tag to load into kind}"
cluster="${2:-vcsim-smoke}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
chart="$here/../../charts/vcenter-api-simulator"
ns=vcsim-smoke
release=vcsim
work="$(mktemp -d)"
pf_pid=""

cleanup() {
  [ -n "$pf_pid" ] && kill "$pf_pid" 2>/dev/null || true
  rm -rf "$work"
}
trap cleanup EXIT

fail() { echo "helm-kind: FAIL: $*" >&2; exit 1; }
k() { kubectl --context "kind-$cluster" -n "$ns" "$@"; }
h() { helm --kube-context "kind-$cluster" -n "$ns" "$@"; }

fingerprint() {
  openssl s_client -connect 127.0.0.1:18443 -servername vcsim.example.test </dev/null 2>/dev/null \
    | openssl x509 -noout -fingerprint -sha256 | cut -d= -f2
}

forward() {
  [ -n "$pf_pid" ] && kill "$pf_pid" 2>/dev/null || true
  k port-forward "svc/$release-vcenter-api-simulator" 18443:443 >"$work/pf.log" 2>&1 &
  pf_pid=$!
  for _ in $(seq 1 30); do
    curl -sk --max-time 2 https://127.0.0.1:18443/sdk/vimServiceVersions.xml | grep -q '<version>' && return 0
    sleep 1
  done
  cat "$work/pf.log" >&2
  fail "port-forward to the service did not come up"
}

smoke() {
  pwsh -NoLogo -File "$here/smoke.ps1" -Server 127.0.0.1 -Port 18443 -Install "$@"
}

wait_ready() {
  k rollout status "deploy/$release-vcenter-api-simulator" --timeout=180s || {
    k describe pod -l app.kubernetes.io/instance="$release" >&2
    k logs -l app.kubernetes.io/instance="$release" --tail=50 >&2 || true
    fail "deployment did not become ready"
  }
}

if ! kind get clusters 2>/dev/null | grep -qx "$cluster"; then
  kind create cluster --name "$cluster" --wait 120s
fi
kind load docker-image --name "$cluster" "$image"
kubectl --context "kind-$cluster" create namespace "$ns" --dry-run=client -o yaml | kubectl --context "kind-$cluster" apply -f -

# The simulator has no default password, so the first install supplies a
# placeholder account through the chart, as a deployment would.
echo "helm-kind: 1/4 install with a generated certificate on a PVC and a placeholder login"
h upgrade --install "$release" "$chart" --wait --timeout 180s \
  --set image.repository="${image%%:*}" --set image.tag="${image##*:}" \
  --set image.pullPolicy=Never --set hostname=vcsim.example.test \
  --set credentials.enabled=true --set credentials.username=first-operator --set credentials.password=first-placeholder
wait_ready
k get pod,svc,pvc -l app.kubernetes.io/instance="$release"
forward
smoke -User first-operator -Password first-placeholder
fp1="$(fingerprint)"
echo "helm-kind:   certificate $fp1"

echo "helm-kind: 2/4 restart: the pod is replaced and the certificate must not change"
k delete pod -l app.kubernetes.io/instance="$release" --wait=true
wait_ready
forward
fp2="$(fingerprint)"
echo "helm-kind:   certificate $fp2"
[ "$fp1" = "$fp2" ] || fail "certificate changed across a restart: $fp1 -> $fp2"
smoke -User first-operator -Password first-placeholder >/dev/null && echo "helm-kind:   smoke PASS after restart"

echo "helm-kind: 3/4 upgrade: restricted login and a CA-issued certificate from tls.existingSecret"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 30 -subj '/CN=vcsim smoke CA' \
  -addext 'basicConstraints=critical,CA:TRUE' -addext 'keyUsage=critical,keyCertSign,cRLSign' \
  -keyout "$work/ca.key" -out "$work/ca.pem" 2>/dev/null
openssl req -new -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj '/CN=vcsim.example.test' \
  -addext 'subjectAltName=DNS:vcsim.example.test,DNS:localhost,IP:127.0.0.1' \
  -keyout "$work/tls.key" -out "$work/tls.csr" 2>/dev/null
openssl x509 -req -in "$work/tls.csr" -CA "$work/ca.pem" -CAkey "$work/ca.key" -CAcreateserial -days 30 \
  -copy_extensions copy -out "$work/tls.crt" 2>/dev/null
k create secret tls vcsim-smoke-tls --cert="$work/tls.crt" --key="$work/tls.key" --dry-run=client -o yaml | k apply -f -
k create secret generic vcsim-smoke-login --from-literal=username=operator --from-literal=password=placeholder-only --dry-run=client -o yaml | k apply -f -
h upgrade "$release" "$chart" --wait --timeout 180s --reuse-values \
  --set tls.existingSecret=vcsim-smoke-tls \
  --set credentials.enabled=true --set credentials.existingSecret=vcsim-smoke-login
wait_ready
forward
verify="$(openssl s_client -connect 127.0.0.1:18443 -servername vcsim.example.test -CAfile "$work/ca.pem" </dev/null 2>/dev/null | grep 'Verify return code')"
echo "helm-kind:   $verify"
echo "$verify" | grep -q 'Verify return code: 0' || fail "served certificate does not verify against the CA"
if smoke -User operator -Password wrong >/dev/null 2>&1; then fail "a wrong password logged in"; fi
echo "helm-kind:   wrong password refused"
smoke -User operator -Password placeholder-only

echo "helm-kind: 4/4 uninstall (the PVC is kept on purpose)"
h uninstall "$release" --wait
k delete pvc -l app.kubernetes.io/instance="$release" --ignore-not-found
k delete secret vcsim-smoke-tls vcsim-smoke-login --ignore-not-found
echo "helm-kind: PASS"
