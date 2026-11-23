# Glance Go con Ceph RBD: pruebas automatizadas aprobadas

Fecha local 2026-11-18. Revisión `5408405` (rama `feat/glance-integration-suite`),
árbol limpio (`git_dirty=0`). Comando:

```sh
make glance-test-rbd GLANCE_RBD_TEST_OUTPUT=<directorio nuevo>
```

Entorno: host de desarrollo (7 GiB, sin VM), Docker. Ceph 18.2.7 Reef (1 MON y 1 OSD
memstore, `auth none`, réplica 1) y MySQL 8.0.46 desechable. Los dos comparten
una red sólo de loopback y tienen memoria limitada (`memory.txt`). Go 1.27.3,
go-ceph v0.38.0 (`versions.txt`).

## Resultado (`summary.json`)

| Paso | Resultado |
|---|---|
| `go vet` con los tags `rbd` e `integration` | pass |
| Store RBD contra Ceph real: 6 pruebas (`store-rbd.log`) | pass |
| B25 de punta a punta con el store de disco, como referencia (`e2e-filesystem.log`) | pass |
| B25 de punta a punta con el store RBD (`e2e-rbd.log`) | pass |
| Imágenes RBD que quedan en el pool al terminar | 0 |

La prueba B25 recorre el servicio real:
- autenticación (401 sin token y con token inválido);
- POST de imagen;
- PUT de bytes a través del coordinador y el store;
- GET de los bytes;
- recibo físico en MySQL;
- PATCH;
- **reapertura del coordinador y del store** (vuelve a tomar el lock RADOS y relee los bytes);
- DELETE con tombstone;
- comprobación de que no quedan objetos.

Las pruebas del store cubren:
- longitud desconocida, con comparación byte a byte;
- **64 MiB + 1 byte**: atraviesa objetos de 4 MiB y el crecimiento por tramos;
- la snapshot `snap` protegida;
- los límites de tamaño y los recibos alterados;
- el lock exclusivo, el scope y la reapertura;
- el **rechazo del borrado mientras existe un clon copy-on-write**, que es el caso de un disco de Nova.

Tiempos de la prueba de 64 MiB + 1 byte. Son descriptivos: un OSD en RAM y el
cliente en el mismo host. Stage 299 ms, Publish (verificación y snapshot)
464 ms, **Open (verificación previa al primer byte) 213 ms** y lectura 300 ms.

## Límites

- No es un despliegue: `auth none`, memstore y réplica 1. Prueba el contrato y la integración del código, no la durabilidad ni el rendimiento de Ceph.
- Autenticación por doble de prueba en la B25. Keystone real se probó por separado en la suite B26.
- Nova todavía no usa RBD: faltan `direct_url` y `images_type=rbd` (fases F3 y F4 en `docs/glance/ceph-rbd-projection.md`).

Ver docs/glance/ceph-rbd.md.
