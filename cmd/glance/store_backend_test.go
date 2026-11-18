//go:build !(linux && rbd)

package main

import (
	"errors"
	"testing"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/config"
)

// The default (static, cgo-free) binary must refuse rbd explicitly, never fall
// back to another store.
func TestDefaultBinaryRefusesRBD(t *testing.T) {
	b := config.Binary{Backend: "rbd", RBDConf: "/etc/ceph/ceph.conf", RBDUser: "glance", RBDPool: "images"}
	if _, err := openStore(b, "00000000-0000-5000-8000-000000000001"); !errors.Is(err, errNoRBD) {
		t.Fatal(err)
	}
	if err := initStore(b, "00000000-0000-5000-8000-000000000001"); !errors.Is(err, errNoRBD) || rbdBuilt {
		t.Fatal(err)
	}
}
