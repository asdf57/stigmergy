package etcd

import "strings"

type keyspace struct {
	prefix string
}

func newKeyspace(prefix string) keyspace {
	return keyspace{prefix: "/" + strings.Trim(strings.TrimSpace(prefix), "/")}
}

func (k keyspace) resource(kind, name string) string {
	return k.resourcePrefix(kind) + name
}

func (k keyspace) resourcePrefix(kind string) string {
	return k.prefix + "/resources/" + kind + "/"
}

func (k keyspace) uid(uid string) string {
	return k.prefix + "/uids/" + uid
}
