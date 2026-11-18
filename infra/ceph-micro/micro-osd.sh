#!/usr/bin/env bash
# Starts 1 MON + 1 OSD (memstore) and the pool used by the Glance Go RBD tests.
# Writes the client config to $CEPH_DIR/ceph.conf. Test-only settings.
set -euo pipefail
dir=${CEPH_DIR:-/tmp/ceph}
pool=${GLANCE_RBD_POOL:-glance-go}
mkdir -p "$dir"/{mon,osd,run,log}
fsid=$(cat /proc/sys/kernel/random/uuid)
cat > "$dir/ceph.conf" <<CONF
[global]
fsid = $fsid
mon host = v1:127.0.0.1:6789
ms bind msgr2 = false
auth cluster required = none
auth service required = none
auth client required = none
osd pool default size = 1
osd pool default min size = 1
osd crush chooseleaf type = 0
mon allow pool size one = true
mon allow pool delete = true
mon warn on pool no redundancy = false
run dir = $dir/run
log file = $dir/log/\$name.log
osd objectstore = memstore
memstore device bytes = 805306368
osd memory target = 536870912
osd data = $dir/osd
mon data = $dir/mon
[mon.a]
mon addr = v1:127.0.0.1:6789
CONF
export CEPH_CONF="$dir/ceph.conf"
ceph-mon --mkfs -i a --conf "$CEPH_CONF"
ceph-mon -i a --conf "$CEPH_CONF"
osd=$(ceph osd create)
ceph-osd -i "$osd" --mkfs --conf "$CEPH_CONF"
ceph-osd -i "$osd" --conf "$CEPH_CONF"
ceph osd pool create "$pool" 8
ceph osd pool application enable "$pool" rbd
rbd pool init "$pool"
# No mgr on purpose (RAM): "ceph pg stat" would block. Probe real I/O instead.
for _ in $(seq 1 60); do
  if timeout 5 rados -p "$pool" put tt-probe "$CEPH_CONF" 2>/dev/null && timeout 5 rados -p "$pool" rm tt-probe; then
    echo "ceph listo: pool $pool, conf $CEPH_CONF"
    exit 0
  fi
  sleep 1
done
echo 'Ceph no aceptó escrituras en el pool.' >&2
exit 1
