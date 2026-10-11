#!/usr/bin/env bash
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
for cmd in terraform virsh ansible-playbook curl sha256sum; do
  command -v "$cmd" >/dev/null || { echo "Falta $cmd" >&2; exit 1; }
done
[[ -r /dev/kvm ]] || { echo 'KVM no disponible.' >&2; exit 1; }
net_active=$(LC_ALL=C virsh -c qemu:///system net-info default | awk '$1 == "Active:" {print $2}')
[[ $net_active == yes ]] || { echo 'Red libvirt default no activa.' >&2; exit 1; }
[[ $(nproc) -ge 4 ]] || { echo 'Host con menos de 4 CPU.' >&2; exit 1; }
echo 'Aviso: laboratorio experimental con RAM limitada.' >&2
pool_path=$(virsh -c qemu:///system pool-dumpxml default | sed -n '/<target>/,/<\/target>/s|.*<path>\(.*\)</path>.*|\1|p' | head -n1)

[[ -n "$pool_path" && -d "$pool_path" ]] || {
  echo "No se pudo determinar la ruta del pool libvirt default." >&2
  exit 1
}

repo_free=$(df -BG --output=avail "$here" | tail -1 | tr -dc '0-9')
pool_free=$(df -BG --output=avail "$pool_path" | tail -1 | tr -dc '0-9')

echo "Espacio libre en repositorio: ${repo_free} GiB"
echo "Espacio libre en pool libvirt: ${pool_free} GiB"

[[ $repo_free -ge 10 ]] || {
  echo "Menos de 10 GiB disponibles para el repositorio." >&2
  exit 1
}

[[ $pool_free -ge 40 ]] || {
  echo "Menos de 40 GiB disponibles para las imágenes de la VM." >&2
  exit 1
}

echo "Aviso: almacenamiento reducido; vigilar espacio durante el despliegue."
"$here/fetch-image.sh"
cd "$here/terraform"
terraform init -input=false
terraform fmt -check
terraform validate
terraform plan -input=false -out=lab.tfplan
terraform show -json lab.tfplan | python3 -c '
import json,sys
plan=json.load(sys.stdin)
danger=[c["address"] for c in plan.get("resource_changes",[]) if "delete" in c["change"]["actions"]]
if danger: raise SystemExit("Plan contiene borrados: "+", ".join(danger))
'
terraform apply -input=false lab.tfplan
"$here/provision.sh"
