# Glance Go optimizado: antes/después y regresión E2E

Fecha local 2026-11-18. Rama `feat/glance-performance` (código en la VM: `983a75e`).
VM local `tt-openstack-lab`: 4 vCPU y 6 GiB, con Keystone y Nova reales.
Binarios estáticos con Go 1.27.3:
- antes: `glance-before`, sha256 `ca63516f…`, igual a `main` `41e2a3c`;
- ahora: `glance`, sha256 `2ec2f231…`;
- `placement`: sha256 `60f507a8…`.

Qué cambió y por qué: [docs/glance/performance.md](../../../docs/glance/performance.md).

## Comparación rápida (`quick-before/`, `quick-after/`)

Se usa `glance_suite.py --quick-compare`: el mismo runtime (MySQL y store
propios, 16 transferencias), el mismo token de Keystone real, directo a Glance
Go, y sólo cambia `--binary`. **No es una campaña.**

| Medición | Antes | Ahora |
|---|---|---|
| 200 lecturas de metadatos secuenciales: p50 / p95 | 11,87 / 22,44 ms | 1,87 / 2,25 ms |
| 400 lecturas con 16 hilos: p50 / p95 | 135,0 / 219,8 ms | 10,4 / 21,2 ms |
| Throughput de metadatos con 16 hilos | 109 req/s | 1 137 req/s |
| Descarga de 64 MiB, total p50 / p95 (10 veces) | 262 / 364 ms | 55 / 58 ms |
| Descarga de 64 MiB, primer byte p50 / p95 | 203 / 293 ms | 2,5 / 3,9 ms |
| Carga de 64 MiB, p50 / p95 (5 veces) | 685 / 1 061 ms | 508 / 717 ms |

Cada descarga verificó la longitud y el SHA-256 en el cliente. Las dos corridas
pasaron 11/11 casos, con limpieza completa.

Microbenchmarks del host: `microbench-store.txt` y `microbench-auth.txt`.

## Regresión con Nova (`glance-1/2`, `joint-1/2`)

Corridas consecutivas con el binario nuevo:

| Corrida | Modo | Casos | Limpieza |
|---|---|---|---|
| `glance-1` | Glance Go | 10/10 | completa |
| `joint-1` | Glance Go + Placement Go | 14/14 | completa |
| `glance-2` | Glance Go | 10/10 | completa |
| `joint-2` | Glance Go + Placement Go | 14/14 | completa |

## Límites

- VM de 6 GiB, cliente dentro de la VM y pocas repeticiones: los números sirven para comparar antes contra ahora, no para compararse con la campaña v3 ni para afirmar significación estadística.
- La caché de tokens y la descarga sin rehash cambian garantías de forma deliberada, para igualar al Glance oficial (ver `performance.md`).
