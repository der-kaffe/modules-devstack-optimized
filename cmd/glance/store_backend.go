package main

import (
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

// openStore and initStore select the operator-configured byte backend. The
// filesystem store is always available; RBD only in binaries built with -tags rbd.
func openStore(b config.Binary, scope string) (store.Backend, error) {
	if b.Backend == "rbd" {
		return openRBD(b, scope)
	}
	return store.NewFilesystem(b.Root, scope)
}

func initStore(b config.Binary, scope string) error {
	if b.Backend == "rbd" {
		return initRBD(b, scope)
	}
	return store.Initialize(b.Root, scope)
}
