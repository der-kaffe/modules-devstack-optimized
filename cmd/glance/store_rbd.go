//go:build linux && rbd

package main

import (
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

const rbdBuilt = true

func rbdConfig(b config.Binary) store.RBDConfig {
	return store.RBDConfig{ConfFile: b.RBDConf, User: b.RBDUser, Pool: b.RBDPool}
}

func openRBD(b config.Binary, scope string) (store.Backend, error) {
	return store.NewRBD(rbdConfig(b), scope)
}

func initRBD(b config.Binary, scope string) error { return store.InitializeRBD(rbdConfig(b), scope) }

// rbdImageCount lists the pool's RBD images (diagnostics and cleanup checks).
func rbdImageCount(b config.Binary) (int, error) { return store.CountRBDImages(rbdConfig(b)) }
