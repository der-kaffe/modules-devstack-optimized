//go:build !(linux && rbd)

package main

import (
	"errors"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
	"github.com/PaoloCastanon/modules-devstack-optimized/internal/glance/store"
)

const rbdBuilt = false

var errNoRBD = errors.New("GLANCE_GO_STORE_BACKEND=rbd requires a binary built with -tags rbd")

func openRBD(config.Binary, string) (store.Backend, error) { return nil, errNoRBD }

func initRBD(config.Binary, string) error { return errNoRBD }

func rbdImageCount(config.Binary) (int, error) { return 0, errNoRBD }
