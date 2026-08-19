#!/usr/bin/env bash
# Generate a self-signed CA and serving certificate for the kqos webhook, then
# install them as a Secret and stamp the CA bundle into the
# MutatingWebhookConfiguration.
#
# Real clusters should use cert-manager. This script exists so that `make
# deploy` works on a fresh kind cluster with nothing but kubectl and openssl,
# because a demo that requires installing a certificate controller first is a
# demo nobody runs.
set -euo pipefail

NAMESPACE="${NAMESPACE:-kqos-system}"
SERVICE="${SERVICE:-kqos-webhook}"
SECRET="${SECRET:-kqos-webhook-certs}"
OUT_DIR="${OUT_DIR:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/certs}"
WEBHOOK_CONFIG="${WEBHOOK_CONFIG:-kqos-pod-mutator}"

mkdir -p "${OUT_DIR}"
cd "${OUT_DIR}"

DNS="${SERVICE}.${NAMESPACE}.svc"

echo "==> generating CA"
openssl genrsa -out ca.key 2048 2>/dev/null
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 \
  -subj "/CN=kqos-webhook-ca" -out ca.crt 2>/dev/null

echo "==> generating serving certificate for ${DNS}"
openssl genrsa -out tls.key 2048 2>/dev/null

cat > csr.conf <<EOF
[req]
req_extensions = v3_req
distinguished_name = dn
prompt = no
[dn]
CN = ${DNS}
[v3_req]
basicConstraints = CA:FALSE
keyUsage = digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${SERVICE}
DNS.2 = ${SERVICE}.${NAMESPACE}
DNS.3 = ${DNS}
DNS.4 = ${DNS}.cluster.local
EOF

openssl req -new -key tls.key -out tls.csr -config csr.conf 2>/dev/null
openssl x509 -req -in tls.csr -CA ca.crt -CAkey ca.key -CAcreateserial \
  -out tls.crt -days 3650 -sha256 -extensions v3_req -extfile csr.conf 2>/dev/null

echo "==> installing secret ${NAMESPACE}/${SECRET}"
kubectl -n "${NAMESPACE}" create secret tls "${SECRET}" \
  --cert=tls.crt --key=tls.key \
  --dry-run=client -o yaml | kubectl apply -f - >/dev/null

CA_BUNDLE="$(base64 < ca.crt | tr -d '\n')"

echo "==> patching MutatingWebhookConfiguration ${WEBHOOK_CONFIG}"
if kubectl get mutatingwebhookconfiguration "${WEBHOOK_CONFIG}" >/dev/null 2>&1; then
  kubectl patch mutatingwebhookconfiguration "${WEBHOOK_CONFIG}" \
    --type='json' \
    -p "[{\"op\":\"replace\",\"path\":\"/webhooks/0/clientConfig/caBundle\",\"value\":\"${CA_BUNDLE}\"}]" >/dev/null
  echo "==> ca bundle installed"
else
  echo "==> webhook configuration not applied yet; re-run this script after applying it"
fi

echo "==> certificates written to ${OUT_DIR}"
