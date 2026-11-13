# B26 — suite automática de integración Glance Go

Estado (2026-11-12): **dos corridas reales consecutivas aprobadas (10/10 casos,
limpieza completa)** en la VM local. Nova llegó a ACTIVE en 4,8 s y 4,4 s desde
una imagen CirrOS que sólo existía en Glance Go.
[Evidencia y flujo observado](../../evidence/sprint5/glance-b26-suite-20261112/README.md).
Para llegar ahí hubo que portar a Go el descubrimiento de versiones público, que
Nova necesita, y dar a Go su credencial de servicio en el proyecto `service`.
Las pruebas offline están en `tests/unit/test_glance_suite.py`. Antes de esto,
el repositorio no conservaba evidencia de un boot desde Glance Go (ver
[preflight v2](../../evidence/sprint5/benchmarks/glance-v2-functional-preflight-20261031/README.md)).

## Qué ejecuta

Un solo comando, `scripts/integration/glance_suite.py`, ejecuta estos casos en
orden. Se detiene al primer fallo y la limpieza corre siempre:

| # | Caso | Qué acredita |
|---|------|--------------|
| 1 | `preflight_and_isolation` | Controles de mantenimiento, identidad propia, snapshot del catálogo original, MySQL y store Go propios, y un validador temporal de Go en el proyecto `service` (ahí también está el usuario de servicio de Nova). Placement se lee (sólo lectura) como control de no interferencia: se aceptan allocations preexistentes, pero el estado final debe ser igual al inicial, sin contar `generation`. |
| 2 | `auth_fail_closed` | Go responde 401 sin token y con token inválido, y el descubrimiento de versiones (`GET /`) es público (300) como en el original: Nova lo necesita antes de cualquier llamada. |
| 3 | `not_found` | GET de un UUID nuevo devuelve 404. |
| 4 | `catalog_and_bytes_direct` | Ciclo completo contra Go directo: 404 → POST → PUT → active con tamaño/MD5/SHA-512 → GET verificando SHA-256 → PATCH → DELETE → 404. |
| 5 | `route_go_public` | Sustitución reversible de Apache `/image` con lease de rollback; original detenido; ruta pública protegida. |
| 6 | `catalog_and_bytes_public` | Ciclo 4 a través de la ruta pública de OpenStack. |
| 7 | `image_for_nova` | La imagen CirrOS de DevStack se sube sólo a Go (qcow2) y queda active con hashes iguales; no aparece en el store original. |
| 8 | `nova_boot_from_go` | Nova crea un servidor con esa imagen y llega a ACTIVE; se comprueban dueño e imagen. |
| 9 | `nova_glance_flow` | Captura y aceptación del flujo Nova → Glance en la ventana del boot ([detalle](nova-glance-flow.md)). |
| 10 | `server_and_image_removed` | Se borra el servidor y luego la imagen en Go (204 → 404). |

Limpieza obligatoria: servidor propio, ruta original restaurada, catálogo
original y sites de Apache sin cambios, Placement de control intacto, runtime,
identidades y raíz privada eliminados y controles restaurados. Sólo hay un pase
si se cumplen los 10 casos **y** toda la limpieza, sin imágenes pendientes.

## Cómo ejecutarla

Dentro de la VM, con el laboratorio exclusivo, como en B15:

```sh
make glance-build                       # en el host o la VM; produce bin/glance
source /opt/stack/data/venv/bin/activate
set +u; source /opt/stack/devstack/openrc admin admin
openstack flavor list; openstack network list   # elegir IDs existentes
make glance-integration GLANCE_INTEGRATION_ARGS="\
  --binary $PWD/bin/glance \
  --flavor-id $FLAVOR_ID --network-id $NETWORK_ID \
  --work-root /home/stack/tt-placement-b26-$(date +%s) \
  --output /home/stack/b26-out-$(date +%s) \
  --allow-mutations"
```

Opciones:
- `--payload-mib 1 16 64` repite el ciclo 4 con los tamaños de la campaña. El valor por defecto, `1`, es el más liviano.
- `--cirros-image` permite indicar otro archivo de imagen.
- `--wait-seconds` va de 10 a 300.
- `--lease-seconds` va de 300 a 1800 y tiene que cubrir toda la ventana con Go activo.

Salida, en el directorio `--output`:
- `report.json`: casos, tiempos, estados del boot, limpieza y solicitudes.
- `flow.json` y `flow.mmd`: el flujo capturado.
- Logs de cada acción.

Antes de publicar la salida en `evidence/`, hay que revisarla: no contiene tokens ni DSN.

## Recursos

Corre dentro de la VM: un MySQL propio (buffer de 128 MiB), Glance Go y un
servidor CirrOS con el flavor elegido. Hay que preferir el flavor más pequeño
disponible. En un host de 7 GiB, cerrar el resto de aplicaciones antes de
levantar la VM.

## Límites

- No mide rendimiento ni sustituye la campaña compacta.
- No prueba Placement Go al mismo tiempo; esa es otra pregunta experimental.
- Nova guarda una copia de la imagen en su caché `_base`, keyed por el ID de imagen. Por eso cada corrida usa un UUID nuevo: así la descarga ocurre de verdad. La suite no borra esa caché, porque pertenece a Nova; Nova la retira con su gestor de caché.
- No cubre notificaciones, import, multistore ni todas las rutas de Image API.
