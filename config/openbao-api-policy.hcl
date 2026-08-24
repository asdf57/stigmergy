path "kv2/data/secrets/*" {
  capabilities = ["create", "read", "update"]
}

path "kv2/metadata/secrets/*" {
  capabilities = ["delete"]
}
