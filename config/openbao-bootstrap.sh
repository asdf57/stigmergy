#!/bin/sh
set -eu

umask 077

initialized_now=false
status_json="$(bao status -format=json 2>/dev/null || true)"
if echo "$status_json" | grep -q '"initialized"[[:space:]]*:[[:space:]]*false'; then
  init_tmp=/run/openbao-init/init.json.tmp
  bao operator init -key-shares=1 -key-threshold=1 -format=json > "$init_tmp"

  awk '
    /"unseal_keys_b64"/ { in_keys = 1; next }
    in_keys && /"/ {
      value = $0
      sub(/^[[:space:]]*"/, "", value)
      sub(/"[,]?[[:space:]]*$/, "", value)
      print value
      exit
    }
  ' "$init_tmp" > /run/openbao-init/unseal-key
  awk -F '"' '/"root_token"/ { print $4; exit }' "$init_tmp" > /run/openbao-init/root-token
  rm -f "$init_tmp"

  if [ ! -s /run/openbao-init/unseal-key ] || [ ! -s /run/openbao-init/root-token ]; then
    echo "failed to extract OpenBao initialization credentials" >&2
    exit 1
  fi
  initialized_now=true
fi

if [ ! -s /run/openbao-init/unseal-key ] || [ ! -s /run/openbao-init/root-token ]; then
  echo "OpenBao is initialized but its automation credentials are missing from the openbao-init volume" >&2
  exit 1
fi

status_json="$(bao status -format=json 2>/dev/null || true)"
if echo "$status_json" | grep -q '"sealed"[[:space:]]*:[[:space:]]*true'; then
  bao operator unseal "$(cat /run/openbao-init/unseal-key)" >/dev/null
fi

export BAO_TOKEN
BAO_TOKEN="$(cat /run/openbao-init/root-token)"

if ! bao secrets list -format=json | grep -q '"kv2/"'; then
  bao secrets enable -path=kv2 kv-v2
fi

if ! bao auth list -format=json | grep -q '"approle/"'; then
  bao auth enable approle
fi

bao policy write stigmergy-api /openbao/bootstrap/openbao-api-policy.hcl

role_existed=true
if ! bao read auth/approle/role/stigmergy-api >/dev/null 2>&1; then
  role_existed=false
fi

bao write auth/approle/role/stigmergy-api \
  token_policies=stigmergy-api \
  token_ttl=1h \
  token_max_ttl=4h \
  token_num_uses=0 \
  secret_id_ttl=0 \
  secret_id_num_uses=0

if [ "$initialized_now" = true ] || [ "$role_existed" = false ] || [ ! -s /run/openbao-bootstrap/role-id ] || [ ! -s /run/openbao-bootstrap/secret-id ]; then
  role_id_tmp=/run/openbao-bootstrap/role-id.tmp
  secret_id_tmp=/run/openbao-bootstrap/secret-id.tmp

  bao read -field=role_id auth/approle/role/stigmergy-api/role-id > "$role_id_tmp"
  bao write -field=secret_id -f auth/approle/role/stigmergy-api/secret-id > "$secret_id_tmp"

  mv "$role_id_tmp" /run/openbao-bootstrap/role-id
  mv "$secret_id_tmp" /run/openbao-bootstrap/secret-id
fi

echo "OpenBao is initialized, unsealed, and its AppRole bootstrap is ready"
