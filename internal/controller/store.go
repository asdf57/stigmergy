package controller

import (
	"github.com/asdf57/prov-controller-test/go/internal/store/etcd"
)

//	type Store interface {
//		Watch(context.Context, string, int64) (<-chan Event, error)
//		List(context.Context, string) (resource.List, error)
//	}

type ControllerStore struct {
	etcdClient *etcd.Store
}

func NewControllerStore(etcdClient *etcd.Store) *ControllerStore {
	return &ControllerStore{etcdClient: etcdClient}
}

func (c *ControllerStore) WatchOrSomething() {
}
