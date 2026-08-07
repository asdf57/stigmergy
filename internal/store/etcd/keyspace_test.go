package etcd

import "testing"

func TestKeyspaceNormalizesPrefix(t *testing.T) {
	t.Parallel()

	keys := newKeyspace("homelab/v1/")
	if got, want := keys.resource("Server", "lab-node"), "/homelab/v1/resources/Server/lab-node"; got != want {
		t.Fatalf("resource key = %q, want %q", got, want)
	}
}
