# Glance Go frente al Glance oficial: CPU, memoria y descargas concurrentes

Fecha 2026-11-18 (Chile). VM local `tt-openstack-lab`: 4 vCPU y 6 GiB, con
Keystone y Nova reales. Glance oficial `a3e5f490` con su configuración de
DevStack (uwsgi con 2 procesos y notificaciones activas). **No es una campaña.**

## Qué faltaba medir

La campaña v3 del informe (revisión `d3286a6`) también midió CPU por solicitud,
memoria (PSS), la descarga de 16 MiB y descargas con 16 clientes, y en algunas
de esas Go perdía. La primera auditoría (`glance-audit-20261118`) sólo comparaba
latencias, así que no lo cubría. Ahora `glance_suite.py --audit` agrega:
- **CPU por solicitud** de cada bloque, desde el `cpu.stat` del cgroup del servicio: `devstack@g-api` (uwsgi) para el oficial y la unidad propia para Go. Igual que en la v3, no entran MySQL, Keystone ni Apache.
- **PSS** del servicio en reposo y su máximo por bloque, muestreado cada 0,5 s.
- Carga y descarga de **16 MiB** y descargas de 1, 16 y 64 MiB con **16 clientes**, más la CPU de Apache, el uso de la VM y la misma descarga **directo al servicio**, sin Apache.

## Lo que apareció: con 16 clientes y 64 MiB, Go era más lento (`res-audit-1/2`)

Con el binario de la ronda 2 (`84564884…`), 21 de 22 operaciones salieron a
favor de Go. La excepción fue la **descarga de 64 MiB con 16 clientes**: Go tardó
1,21× y 1,08× lo que el oficial (p50 de 1 274 frente a 1 052 ms en la corrida 1;
12,2 frente a 14,8 req/s), aunque gastaba 9 ms de CPU por descarga frente a 51.

**Diagnóstico** (`res2-audit-1` y los experimentos `exp-*`):

| 64 MiB, 16 clientes | Oficial | Go (ronda 2) | Uso de la VM (oficial / Go) |
|---|---|---|---|
| Por Apache | 971 ms, 15,2 req/s | 1 203 ms, 12,9 req/s | 75 % / 55 % |
| Directo al servicio | 796 ms, 19,2 req/s | 818 ms, 19,0 req/s | 62 % / 59 % |

- Directo al servicio empataban. Con Go, la VM estaba *menos* ocupada: no faltaba CPU, algo esperaba.
- **No era `sendfile`.** Copiar en espacio de usuario como glance_store (`exp-copy64`, `exp-copy1024`) lo empeoró: 1 340–1 366 ms por Apache y 971–1 068 ms directo, con 4–5 veces más CPU.
- **Era la concurrencia.** El oficial atiende con 2 procesos uwsgi y deja el resto en cola; Go enviaba las 16 descargas a la vez. Con un binario experimental limitado a 2 descargas en cola (`exp-queue2`), Go pasó a **572 ms y 25,4 req/s** por Apache y a 523 ms directo. La descarga de 16 MiB mejoró de 334 a 198 ms, y la de 1 MiB no cambió.

## Cambio aplicado (`16195b3`)

`GLANCE_GO_MAX_DOWNLOAD_STREAMS` limita cuántas descargas envían bytes a la vez,
y las demás esperan en cola, como en el oficial, en vez de recibir un 503.
- En disco, el valor por defecto es la mitad de las CPU, con mínimo 2 (2 en esta VM, como uwsgi).
- En RBD queda en 0, sin cola, porque sus lecturas esperan la red y no se midieron.
- La espera ocurre después de la autorización (un 403 o un 404 nunca espera), no ocupa la capacidad compartida y termina con el contexto del cliente o con el apagado.

Binario nuevo: sha256 `e3d48e98…`.

## Resultado final (`q-audit-1`, `q-audit-2`): Go no es más lento en ninguna

Corrida 1, p50; la última columna muestra las dos corridas.

| Operación | Oficial | Go | Go/oficial |
|---|---|---|---|
| Consulta de imagen | 10,0 ms | 1,8 ms | 0,18 / 0,18 |
| Consulta con 16 hilos | 80,2 ms (192 req/s) | 10,1 ms (1 001 req/s) | 0,13 / 0,13 |
| Crear (POST) | 306 ms | 5,7 ms | 0,02 / 0,02 |
| Carga de 16 MiB | 706 ms | 105 ms | 0,15 / 0,14 |
| Carga de 64 MiB | 983 ms | 330 ms | 0,34 / 0,29 |
| Descarga de 1 MiB | 17,5 ms | 2,4 ms | 0,14 / 0,17 |
| Descarga de 16 MiB | 30,6 ms | 18,3 ms | 0,60 / 0,56 |
| Descarga de 64 MiB | 88,0 ms | 67,0 ms | 0,76 / 0,92 |
| Descarga de 1 MiB, 16 clientes | 306 ms (50 req/s) | 38 ms (399 req/s) | 0,12 / 0,13 |
| Descarga de 16 MiB, 16 clientes | 498 ms (31 req/s) | 246 ms (64 req/s) | 0,49 / 0,44 |
| **Descarga de 64 MiB, 16 clientes** | 1 052 ms (14,5 req/s) | **576 ms (25,5 req/s)** | **0,55 / 0,58** |

Las 22 operaciones están en `q-audit-N/audit.json`. La descarga de 64 MiB con un
cliente es la más pareja (0,76 y 0,92).

### CPU por solicitud y memoria (`q-audit-1`)

| Bloque | CPU oficial | CPU Go | PSS máx. oficial | PSS máx. Go |
|---|---|---|---|---|
| Consulta | 7,9 ms | 0,7 ms | 237 MiB | 14 MiB |
| Ciclo de metadatos (POST, PATCH, DELETE) | 36,4 ms | 1,5 ms | 238 MiB | 15 MiB |
| Ciclo de carga de 64 MiB | 157 ms | 89 ms | 245 MiB | 15 MiB |
| Descarga de 64 MiB | 91 ms | 7,5 ms | 245 MiB | 15 MiB |
| Descarga de 64 MiB, 16 clientes | 52 ms | 7,9 ms | 325 MiB | 17 MiB |

En reposo: 233 MiB el oficial y 12,6 MiB Go. Go gasta menos CPU en todos los
bloques (`resources` en `audit.json`). En la v3, la descarga de 64 MiB costaba en
Go 188–208 ms de CPU porque hasheaba la imagen; ahora cuesta 7–9 ms.

## Regresión con el binario nuevo

- B26: Glance Go 10/10 ×2 (`q-glance-N`) y Glance Go + Placement Go 14/14 ×2 (`q-joint-N`), con limpieza completa.
- Comparación rápida (`q-qc`): la carga de 64 MiB dio 415 ms, frente a 212 en la ronda 2. Para descartar una regresión se alternaron los dos binarios (`qcmp-*`): ronda 2 228 y 244 ms; nuevo 244 y 244 ms. Fue ruido del disco de la VM. El resto coincide (metadatos 1,4 ms, descarga de 64 MiB 54–56 ms).
- `make check` OK y `make glance-test-rbd` 9/9.

## Límites

- El cliente corre dentro de la VM y compite por las mismas 4 vCPU que Apache y el servicio. La v3 lo ponía en el anfitrión. Por eso la ventaja de la cola puede ser menor con clientes remotos: la v3 tendría que confirmarla.
- El oficial conserva sus notificaciones (la v3 las desactivó), así que sus razones no se comparan con las de la v3. Sólo vale la dirección en las mismas condiciones.
- Hay pocas repeticiones. Los números sirven para comparar, no para afirmar significancia estadística.
