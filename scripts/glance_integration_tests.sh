#!/usr/bin/env bash
# Glance Go MySQL integration suites against the lab VM's MySQL, the one the
# original Glance uses. For each suite it creates a disposable pair on that
# server:
#   glance_go_shared_test_<suite>   NEW EMPTY bookkeeping database
#   glance_official_test_<suite>    copy of the original's catalog DDL (taken
#                                   live from the glance database, no rows)
#                                   at the original's alembic revision
# and runs the suite from this host through an SSH tunnel (the DSN stays on
# loopback, as Glance Go requires for plaintext MySQL). It uses the original's
# MySQL account, read from /etc/glance/glance-api.conf on the VM; the password
# is kept only in a private temporary file removed at exit. All test databases
# are dropped at exit, also on failure. The original's glance database is only
# read (DDL and revision).
#
# Usage: scripts/glance_integration_tests.sh [NEW output directory]
# Env:   LAB_HOST (stack@192.168.122.10), LAB_KEY (~/.ssh/id_ed25519),
#        LAB_KNOWN_HOSTS (infra/lab-vm/.cache/known_hosts),
#        GLANCE_INTEGRATION_RACE=1 to add -race.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
host=${LAB_HOST:-stack@192.168.122.10}
key=${LAB_KEY:-$HOME/.ssh/id_ed25519}
known=${LAB_KNOWN_HOSTS:-$root/infra/lab-vm/.cache/known_hosts}
out=${1:-$(mktemp -d "${TMPDIR:-/tmp}/glance-integration-XXXXXX")}
[[ -d $out && -z $(ls -A "$out") ]] || [[ ! -e $out ]] || { echo "Output must be new or empty: $out" >&2; exit 1; }
mkdir -p "$out"
out=$(cd "$out" && pwd)
private=$(mktemp -d)
chmod 700 "$private"
ssh_cmd=(ssh -o BatchMode=yes -o IdentitiesOnly=yes -o StrictHostKeyChecking=yes
         -o "UserKnownHostsFile=$known" -o ConnectTimeout=10 -i "$key")
# A free loopback port for the tunnel.
port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()')
suites=(history_persist partial transfers catalog members deletion fences contention e2e imports stores)
tunnel=

# Remote prelude: the original's MySQL account, read on the VM from
# glance-api.conf; the password only reaches mysql through MYSQL_PWD.
# shellcheck disable=SC2016  # expanded on the VM, not here
account='set -euo pipefail
url=$(sudo python3 -c "import configparser; c=configparser.RawConfigParser(strict=False); c.read(\"/etc/glance/glance-api.conf\"); print(c.get(\"database\",\"connection\"))")
user=$(python3 -c "import sys,urllib.parse as u; print(u.unquote(u.urlparse(sys.argv[1]).username))" "$url")
MYSQL_PWD=$(python3 -c "import sys,urllib.parse as u; print(u.unquote(u.urlparse(sys.argv[1]).password))" "$url")
export MYSQL_PWD
'

# Runs SQL from stdin on the VM with the original's account.
remote_sql() {
  # shellcheck disable=SC2016  # expanded on the VM
  "${ssh_cmd[@]}" "$host" "$account"'exec mysql --batch --skip-column-names -h127.0.0.1 -u"$user"'
}

# shellcheck disable=SC2317  # invoked by the EXIT trap
cleanup() {
  local status=$?
  if [[ -n ${created:-} ]]; then
    for s in "${suites[@]}"; do
      echo "DROP DATABASE IF EXISTS glance_go_shared_test_$s; DROP DATABASE IF EXISTS glance_official_test_$s;"
    done | remote_sql > /dev/null 2>&1 || echo "WARNING: could not drop the test databases" >&2
  fi
  if [[ -n $tunnel ]]; then kill "$tunnel" 2> /dev/null || true; fi
  rm -rf "$private"
  exit "$status"
}
trap cleanup EXIT

# Credentials and server identity (stay in $private, 0600).
"${ssh_cmd[@]}" "$host" 'sudo python3 -c "import configparser; c=configparser.RawConfigParser(strict=False); c.read(\"/etc/glance/glance-api.conf\"); print(c.get(\"database\",\"connection\"))"' > "$private/url"
read -r user password < <(python3 - "$private/url" <<'PY'
import sys, urllib.parse as u
p = u.urlparse(open(sys.argv[1]).read().strip())
print(u.unquote(p.username), u.unquote(p.password))
PY
)
revision=$(echo 'SELECT version_num FROM glance.alembic_version;' | remote_sql)
server_uuid=$(echo 'SELECT @@GLOBAL.server_uuid;' | remote_sql)
echo "SHOW TABLES FROM glance;" | remote_sql > "$out/official-tables.txt"
# Live DDL of the original's catalog, without data or AUTO_INCREMENT counters.
# shellcheck disable=SC2016  # expanded on the VM
"${ssh_cmd[@]}" "$host" "$account"'mysqldump --no-data --skip-comments --skip-add-drop-table --no-tablespaces -h127.0.0.1 -u"$user" glance' \
  | sed -E 's/ AUTO_INCREMENT=[0-9]+//' > "$out/official-ddl.sql"
grep -qF "CREATE TABLE \`images\`" "$out/official-ddl.sql" || { echo "Could not read the original's DDL" >&2; exit 1; }

created=1
for s in "${suites[@]}"; do
  {
    echo "CREATE DATABASE glance_go_shared_test_$s; CREATE DATABASE glance_official_test_$s; USE glance_official_test_$s;"
    cat "$out/official-ddl.sql"
    echo "INSERT INTO alembic_version (version_num) VALUES ('$revision');"
  } | remote_sql > /dev/null
  umask 077
  printf '%s:%s@tcp(127.0.0.1:%s)/glance_go_shared_test_%s' "$user" "$password" "$port" "$s" > "$private/$s.dsn"
done

"${ssh_cmd[@]}" -N -o ExitOnForwardFailure=yes -L "127.0.0.1:$port:127.0.0.1:3306" "$host" &
tunnel=$!
ready=0
for _ in $(seq 1 30); do
  kill -0 "$tunnel" 2> /dev/null || break
  if (exec 3<> "/dev/tcp/127.0.0.1/$port") 2> /dev/null; then ready=1; break; fi
  sleep 0.5
done
((ready)) || { echo "SSH tunnel to the VM's MySQL failed" >&2; exit 1; }

race=()
[[ ${GLANCE_INTEGRATION_RACE:-0} == 1 ]] && race=(-race)
{
  echo "git_head=$(git -C "$root" rev-parse HEAD)"
  echo "git_dirty=$(git -C "$root" status --porcelain | wc -l)"
  echo "official_revision=$revision"
  echo "go=$(go version)"
  echo "race=${race[*]:-off}"
} > "$out/versions.txt"

declare -A result
run() {
  local name=$1 pkg=$2 test=$3
  shift 3
  if (cd "$root" && env "$@" go test "${race[@]}" -p 1 -count=1 -tags integration "$pkg" -run "^$test\$" -v) > "$out/$name.log" 2>&1; then
    result[$name]=pass
  else
    result[$name]=fail
  fi
  echo "$name: ${result[$name]}"
}
sql=./internal/glance/repository/mysql
run persistence "$sql" TestMySQLPersistenceAndTransactions GLANCE_GO_MYSQL_TEST=1 "GLANCE_GO_TEST_DSN_FILE=$private/history_persist.dsn"
run history "$sql" TestMySQLHistoryReadOnlyVerification GLANCE_GO_MYSQL_TEST=1 \
  "GLANCE_GO_TEST_HISTORY_DSN_FILE=$private/history_persist.dsn" "GLANCE_GO_TEST_HISTORY_SERVER_UUID=$server_uuid"
run partial-ddl "$sql" TestMySQLPartialDDLRemainsDirtyAcrossConnections GLANCE_GO_MYSQL_TEST=1 "GLANCE_GO_TEST_PARTIAL_DSN_FILE=$private/partial.dsn"
run transfers "$sql" TestMySQLTransferOperations GLANCE_GO_MYSQL_TEST=1 "GLANCE_GO_TEST_DSN_FILE=$private/transfers.dsn"
run catalog "$sql" TestMySQLCatalogOperations GLANCE_GO_MYSQL_TEST=1 "GLANCE_GO_TEST_DSN_FILE=$private/catalog.dsn"
run members "$sql" TestMySQLMembersTagsActionsUsage GLANCE_GO_MYSQL_TEST=1 "GLANCE_GO_TEST_DSN_FILE=$private/members.dsn"
run deletion "$sql" TestMySQLCoordinatedDeletion GLANCE_GO_MYSQL_TEST=1 "GLANCE_GO_TEST_DSN_FILE=$private/deletion.dsn"
run fences "$sql" TestB25MySQLConcurrentFences GLANCE_GO_B25_MYSQL_TEST=1 "GLANCE_GO_TEST_DSN_FILE=$private/fences.dsn"
run contention "$sql" TestB25MySQLConcurrentDistinctImages GLANCE_GO_B25_MYSQL_TEST=1 "GLANCE_GO_TEST_DSN_FILE=$private/contention.dsn"
mkdir -m 700 "$private/e2e-root"
run e2e ./cmd/glance TestB25MySQLFilesystemHTTP GLANCE_GO_B25_MYSQL_TEST=1 \
  "GLANCE_GO_TEST_DSN_FILE=$private/e2e.dsn" "GLANCE_GO_B25_TEST_ROOT=$private/e2e-root"

mkdir -m 700 "$private/imports-root"
run imports ./cmd/glance TestImportMySQLFilesystemHTTP GLANCE_GO_B25_MYSQL_TEST=1 \
  "GLANCE_GO_TEST_DSN_FILE=$private/imports.dsn" "GLANCE_GO_B25_TEST_ROOT=$private/imports-root"

mkdir -m 700 "$private/stores-root"
run stores ./cmd/glance TestMultiStoreMySQLFilesystemHTTP GLANCE_GO_B25_MYSQL_TEST=1 \
  "GLANCE_GO_TEST_DSN_FILE=$private/stores.dsn" "GLANCE_GO_B25_TEST_ROOT=$private/stores-root"

# Logs must not carry the account's password.
if grep -rqF -- "$password" "$out"; then echo "Password found in logs; removing them" >&2; rm -f "$out"/*.log; exit 1; fi
failed=0
for name in "${!result[@]}"; do [[ ${result[$name]} == pass ]] || failed=1; done
echo "logs: $out"
exit $failed
