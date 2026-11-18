# Store Ceph RBD de Glance Go

Estado (2026-11-18): **implementado y probado contra un Ceph real (Reef 18.2,
1 MON y 1 OSD en contenedor), incluida la prueba de punta a punta** con HTTP,
coordinador de cargas, MySQL y RBD. Todavía no se integra con Nova
(`images_type=rbd`): esas son las fases F3 y F4 de la
[proyección](ceph-rbd-projection.md).

## Qué hace

Glance Go puede guardar los bytes de las imágenes en Ceph RBD en lugar del
directorio local. El backend se elige al arrancar y la API no cambia:

| Operación del contrato `store.Store` | RBD |
|---|---|
| Stage | Crea la imagen RBD `<image>-<attempt>.part` (objetos de 4 MiB), escribe en streaming y calcula MD5/SHA-512 en el mismo paso. Si no hay Content-Length, crece en tramos de 64 MiB y se recorta al final. Si todo cuadra, la renombra a `.ready`. |
| Publish | Relee `.ready` y compara con el recibo; crea la snapshot `snap` y la protege; renombra a `.blob`. `rbd_rename` nunca pisa un destino existente: `ErrExists`. |
| Open / OpenStream | `OpenStream` (descargas) abre `.blob@snap` en sólo lectura sin hashear, como glance_store. `Open` (recuperación y borrado) además mide los hashes. |
| Delete | Borra los nombres de la clave. Si la snapshot tiene clones (discos de Nova), falla con `ErrInUse` y conserva el contenido. |
| Exclusión | Lock exclusivo RADOS sobre el objeto marcador `glance-go-store.scope`, que además guarda el UUID de scope (equivale al flock y al marcador del disco). |

La snapshot protegida es lo que Nova necesita para clonar copy-on-write una imagen
cuando su backend también es RBD.

## Compilar y configurar

El backend usa [go-ceph](https://github.com/ceph/go-ceph) v0.38.0 (cgo, librados y
librbd). Esa versión mantiene `go 1.24.0` en `go.mod` y soporta Reef sin tags extra.
Sólo se compila con el tag `rbd`. El binario por defecto (`make glance-build`)
sigue siendo estático y sin Ceph; si se configura `rbd` en él, lo rechaza con
un error explícito.

```sh
make glance-build-rbd            # bin/glance-rbd; requiere librados-dev y librbd-dev
```

| Variable | Valor |
|---|---|
| `GLANCE_GO_STORE_BACKEND` | `filesystem` (por defecto) o `rbd` |
| `GLANCE_GO_RBD_CONF` | Ruta absoluta del `ceph.conf` |
| `GLANCE_GO_RBD_USER` | Usuario cephx (por ejemplo `glance`) |
| `GLANCE_GO_RBD_POOL` | Pool RBD existente y vacío |
| `GLANCE_GO_STORE_SCOPE` | UUID de `glance store-scope`, igual que con disco |

Con `rbd`, `GLANCE_GO_STORE_ROOT` debe quedar vacío, para que no haya dos stores
configurados a la vez. `glance store-init` escribe el marcador en un pool sin
marcador previo y nunca adopta uno ajeno.

## Pruebas automatizadas

Un solo comando, en el host de desarrollo, con Docker, sin VM y sin sudo:

```sh
make glance-test-rbd             # o: infra/ceph-micro/run-tests.sh <directorio nuevo>
```

[`infra/ceph-micro/run-tests.sh`](../../infra/ceph-micro/run-tests.sh) hace lo siguiente:
1. Construye la imagen y levanta Ceph (1 MON, 1 OSD memstore, sin mgr) y un MySQL 8.0.46 desechable. Los dos comparten red y escuchan sólo en loopback, con memoria limitada.
2. Crea los pools, los esquemas `glance_go_test_*` y cuentas separadas de migración y runtime con permisos mínimos.
3. Ejecuta, guardando un log por paso:
   - `go vet` con los tags `rbd` e `integration`;
   - las 6 pruebas del store RBD (`internal/glance/store/rbd_linux_test.go`);
   - la prueba B25 de punta a punta con disco, como referencia;
   - la misma prueba con RBD, que además exige el pool vacío al final.
4. Escribe `summary.json` y **elimina siempre los contenedores**.

Las pruebas del store cubren:
- un ciclo completo con longitud desconocida y comparación byte a byte;
- 64 MiB + 1 byte cruzando objetos y el tramo de crecimiento;
- la snapshot protegida;
- los límites de tamaño y los recibos alterados;
- el lock exclusivo, el scope y la reapertura;
- el borrado rechazado mientras existe un clon copy-on-write.

## Hallazgo

El dato de la prueba de 64 MiB es descriptivo: un OSD en RAM, en el host. `Open`
(el camino verificado, que hoy usan sólo la recuperación y el borrado) tarda
unos 0,2 s en verificar los hashes antes del primer byte, y después
la lectura tarda unos 0,3 s. Es el mismo patrón que explica el TTFB de las
descargas grandes en la campaña v3. Con un Ceph real en red, ese costo sería
una lectura completa extra por la red. Cambiarlo (verificar mientras se envía, o
confiar en el recibo) modifica el contrato y el objeto medido, y queda propuesto
en la [proyección](ceph-rbd-projection.md).

## Límites

- El Ceph de prueba usa `auth none`, memstore y réplica 1: sirve para verificar el contrato, no la durabilidad ni el rendimiento. Un despliegue real usa cephx y OSDs con disco.
- Falta exponer `direct_url` y configurar Nova con `images_type=rbd` (F3 y F4). Hoy Nova sigue descargando la imagen por HTTP.
- No se midió una campaña con RBD (F5).
