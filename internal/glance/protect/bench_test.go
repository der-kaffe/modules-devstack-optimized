package protect

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/PaoloCastanon/modules-devstack-optimized/internal/policy"
)

// readable over a typical image: 12 properties against the sample rules
// file of the original (property-protections-roles.conf style).
func BenchmarkAllowedReadImage(b *testing.B) {
	conf := "[^x_owner_.*]\ncreate = admin,member\nread = admin,member\nupdate = admin,member\ndelete = admin,member\n" +
		"[x_none_read]\ncreate = admin\nread = !\nupdate = !\ndelete = admin\n" +
		"[x_foo_matcher]\ncreate = admin\nread = admin\nupdate = admin\ndelete = admin\n" +
		"[^x_.*]\ncreate = admin\nread = admin,member\nupdate = admin\ndelete = admin\n" +
		"[.*]\ncreate = admin,member\nread = admin,member,reader\nupdate = admin,member\ndelete = admin,member\n"
	p := filepath.Join(b.TempDir(), "p.conf")
	if err := os.WriteFile(p, []byte(conf), 0o600); err != nil {
		b.Fatal(err)
	}
	r, err := Load(p, "roles", nil)
	if err != nil {
		b.Fatal(err)
	}
	names := []string{"hw_disk_bus", "hw_scsi_model", "os_distro", "os_version", "x_owner_a", "x_none_read", "architecture", "hypervisor_type", "img_config_drive", "os_glance_import_task", "owner_specified.openstack.md5", "x_other"}
	roles := []string{"reader", "member"}
	b.ReportAllocs()
	for b.Loop() {
		for _, n := range names {
			r.Allowed(n, "read", roles, policy.Credentials{})
		}
	}
}
