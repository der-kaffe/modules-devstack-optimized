#!/usr/bin/env bash
# Automated Ceph RBD tests for Glance Go, on the developer host (no VM, no sudo).
#
# One command: builds infra/ceph-micro, starts a 1-MON/1-OSD Ceph and a
# disposable MySQL (sharing one network namespace, loopback only, memory-capped),
# then runs:
#   1. go vet with the rbd/integration tags;
#   2. the RBD store tests against the real cluster;
#   3. the B25 end-to-end test (HTTP + coordinator + MySQL + store) with the
#      filesystem store, as reference;
#   4. the same end-to-end test with the RBD store, and checks the pool is empty.
# Containers are always removed at the end, also on failure.
#
# Usage: infra/ceph-micro/run-tests.sh <NEW output directory>
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
root=$(cd "$here/../.." && pwd)
out=${1:?'usage: run-tests.sh <NEW output directory>'}
[[ ! -e $out ]] || { echo "Output must be new: $out" >&2; exit 1; }
for cmd in docker go; do command -v "$cmd" >/dev/null || { echo "Falta $cmd" >&2; exit 1; }; done
toolchain=${GLANCE_TOOLCHAIN:-go1.27.3}
goroot=$(go env GOROOT)
modcache=$(go env GOMODCACHE)
id=$(head -c 4 /dev/urandom | od -An -tx1 | tr -d ' \n')
ceph=tt-ceph-$id
db=tt-glance-mysql-$id
mkdir -p "$out"
out=$(cd "$out" && pwd)

cleanup() {
  docker rm -f "$ceph" "$db" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# The container runs offline (GOPROXY=off): fetch modules and the toolchain first.
(cd "$root" && GOTOOLCHAIN=$toolchain go mod download && GOTOOLCHAIN=$toolchain go version) > "$out/host-go.log"
docker build -q -t tt-ceph-micro:reef "$here" > "$out/image-id.txt"

docker run -d --name "$ceph" --memory "${CEPH_MEMORY:-1536m}" \
  -v "$root:/src:ro" -v "$goroot:/usr/local/go:ro" -v "$modcache:/root/go/pkg/mod:ro" \
  -e PATH=/usr/local/go/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
  -e GOTOOLCHAIN="$toolchain" -e GOFLAGS=-mod=mod -e GOCACHE=/tmp/gocache -e GOPROXY=off \
  -e GOTELEMETRY=off -e GOMAXPROCS=2 -e CEPH_CONF=/tmp/ceph/ceph.conf \
  tt-ceph-micro:reef sleep infinity > /dev/null
docker exec "$ceph" micro-osd.sh > "$out/ceph-start.log" 2>&1
docker exec "$ceph" bash -c 'ceph osd pool create glance-go-e2e 8 && ceph osd pool application enable glance-go-e2e rbd && rbd pool init glance-go-e2e' >> "$out/ceph-start.log" 2>&1

# MySQL shares the Ceph container's network namespace: Glance Go allows
# plaintext MySQL only on loopback. The root password lives only in this run.
rootpw=$(head -c 24 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')
docker run -d --name "$db" --network "container:$ceph" --memory "${MYSQL_MEMORY:-768m}" \
  -e MYSQL_ROOT_PASSWORD="$rootpw" mysql:8.0.46 \
  --innodb-buffer-pool-size=64M --performance-schema=OFF --skip-name-resolve --bind-address=127.0.0.1 > /dev/null
ready=0
for _ in $(seq 1 90); do
  # The image's temporary init server has no TCP: wait for the real one.
  if docker exec "$db" mysql -uroot -p"$rootpw" -h127.0.0.1 -e 'SELECT 1' > /dev/null 2>&1; then ready=1; break; fi
  sleep 2
done
((ready)) || { echo 'MySQL no respondió.' >&2; exit 1; }

for name in fs rbd; do
  schema=glance_go_test_${name}_e2e
  escaped=${schema//_/\\_}
  mig=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')
  run=$(head -c 18 /dev/urandom | base64 | tr -dc 'A-Za-z0-9')
  docker exec -i "$db" mysql -uroot -p"$rootpw" -h127.0.0.1 2> /dev/null <<SQL
CREATE DATABASE $schema;
CREATE USER 'gg_${name}_mig'@'127.0.0.1' IDENTIFIED BY '$mig';
CREATE USER 'gg_${name}_run'@'127.0.0.1' IDENTIFIED BY '$run';
GRANT SELECT, INSERT, UPDATE, DELETE, CREATE, ALTER, INDEX, REFERENCES ON \`$escaped\`.* TO 'gg_${name}_mig'@'127.0.0.1';
GRANT SELECT, INSERT, UPDATE, DELETE ON \`$escaped\`.* TO 'gg_${name}_run'@'127.0.0.1';
SQL
  docker exec "$ceph" bash -c "umask 077; mkdir -p /tmp/dsn; printf '%s' 'gg_${name}_mig:$mig@tcp(127.0.0.1:3306)/$schema' > /tmp/dsn/$name-mig.dsn; printf '%s' 'gg_${name}_run:$run@tcp(127.0.0.1:3306)/$schema' > /tmp/dsn/$name-run.dsn"
done
docker exec "$ceph" bash -c 'umask 077; mkdir /tmp/fs-root'

{
  echo "git_head=$(git -C "$root" rev-parse HEAD)"
  echo "git_dirty=$(git -C "$root" status --porcelain | wc -l)"
  echo "ceph=$(docker exec "$ceph" ceph --version)"
  echo "mysql=$(docker exec "$db" mysqld --version)"
  echo "go=$(docker exec -w /src "$ceph" go version)"
} > "$out/versions.txt"

declare -A result
step() {
  local name=$1
  shift
  if docker exec -w /src "$@" > "$out/$name.log" 2>&1; then result[$name]=pass; else result[$name]=fail; fi
  echo "$name: ${result[$name]}"
}
step vet "$ceph" go vet -tags 'rbd integration' ./cmd/glance/ ./internal/glance/store/
step store-rbd -e GLANCE_GO_RBD_TEST_CONF=/tmp/ceph/ceph.conf "$ceph" \
  go test -p 1 -count=1 -tags rbd -run RBD -v ./internal/glance/store/
step e2e-filesystem -e GLANCE_GO_B25_MYSQL_TEST=1 -e GLANCE_GO_TEST_DSN_FILE=/tmp/dsn/fs-mig.dsn \
  -e GLANCE_GO_TEST_RUNTIME_DSN_FILE=/tmp/dsn/fs-run.dsn -e GLANCE_GO_B25_TEST_ROOT=/tmp/fs-root "$ceph" \
  go test -p 1 -count=1 -tags integration -run '^TestB25MySQLFilesystemHTTP$' -v ./cmd/glance/
step e2e-rbd -e GLANCE_GO_B25_MYSQL_TEST=1 -e GLANCE_GO_TEST_DSN_FILE=/tmp/dsn/rbd-mig.dsn \
  -e GLANCE_GO_TEST_RUNTIME_DSN_FILE=/tmp/dsn/rbd-run.dsn -e GLANCE_GO_B25_STORE=rbd \
  -e GLANCE_GO_RBD_TEST_CONF=/tmp/ceph/ceph.conf -e GLANCE_GO_RBD_TEST_POOL=glance-go-e2e "$ceph" \
  go test -p 1 -count=1 -tags 'integration rbd' -run '^TestB25MySQLFilesystemHTTP$' -v ./cmd/glance/
images=$(docker exec "$ceph" rbd -p glance-go-e2e ls | wc -l)
docker stats --no-stream --format '{{.Name}} {{.MemUsage}}' "$ceph" "$db" > "$out/memory.txt"

passed=true
for name in vet store-rbd e2e-filesystem e2e-rbd; do [[ ${result[$name]} == pass ]] || passed=false; done
[[ $images == 0 ]] || passed=false
printf '{"passed": %s, "vet": "%s", "store_rbd": "%s", "e2e_filesystem": "%s", "e2e_rbd": "%s", "rbd_images_left": %s}\n' \
  "$passed" "${result[vet]}" "${result[store-rbd]}" "${result[e2e-filesystem]}" "${result[e2e-rbd]}" "$images" > "$out/summary.json"
cat "$out/summary.json"
[[ $passed == true ]]
