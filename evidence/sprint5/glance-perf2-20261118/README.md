# Glance Go, ronda 2 de optimización: verificación de punta a punta

Fecha 2026-11-18 (15:25 en Chile). Rama `feat/glance-perf-2`, revisión `bec66a7`, árbol limpio.
VM local de 6 GiB con Keystone, Nova y el Glance oficial reales. Binarios
estáticos con Go 1.27.3:
- ronda 1 (`glance-r1`, `main` `ec86d0f`): sha256 `4fb59f8f…`;
- ronda 2 (`glance`): sha256 `84564884…`.

Qué cambió: ver la "Ronda 2" en [performance.md](../../../docs/glance/performance.md).

## Funcionamiento con Nova (suite B26)

| Corrida | Modo | Resultado |
|---|---|---|
| `glance-1` | Glance Go | **falló**: el servidor pasó a ERROR a los 2,3 s (ver abajo) |
| `glance-2`, `glance-3` | Glance Go | 10/10 y 10/10, limpieza completa (consecutivas) |
| `joint-1`, `joint-2` | Glance Go + Placement Go | 14/14 y 14/14, limpieza completa |
| `qc-r2`, `audit-1`, `audit-2` | Glance Go (con boot de Nova) | 11/11, 12/12 y 12/12 |

**Fallo de `glance-1`:** fue la primera corrida, apenas se encendió la VM. Los
logs de Nova muestran a RabbitMQ rechazando conexiones (`Connection refused`)
mientras los servicios terminaban de arrancar, así que Nova no pudo coordinar el
boot. No involucra a Glance Go; las corridas siguientes con el mismo binario
pasaron.

**Fallo de `qc-r1`** (binario de la ronda 1): Nova leyó los metadatos y
descargó la imagen completa, pero una `GET` quedó "sin atribuir" en la captura,
que empareja por el log de Apache con resolución de segundos. Es una
imprecisión de la herramienta, no de Glance. `qc-r1b` repitió la medición y pasó.

Los dos fallos quedaron corregidos en la suite y verificados (con `qc-r1`
reprocesado sobre sus logs reales): ver
[glance-suite-fix-20261118](../glance-suite-fix-20261118/README.md).

## Antes (ronda 1) y después (ronda 2): `qc-r1b` frente a `qc-r2`

Mismo runtime y token; sólo cambia `--binary`. **No es una campaña.**

| Medición | Ronda 1 | Ronda 2 |
|---|---|---|
| Metadatos, secuencial, p50 / p95 | 1,57 / 2,25 ms | 1,18 / 1,67 ms |
| Metadatos, 16 hilos, p50 | 11,1 ms | 9,8 ms |
| Throughput con 16 hilos | 1 099 req/s | 1 254 req/s |
| Descarga de 64 MiB, p50 | 56,4 ms | 52,4 ms |
| Primer byte de 64 MiB, p50 | 2,6 ms | 1,5 ms |
| **Carga de 64 MiB, p50 / p95** | 477 / 488 ms | **212 / 238 ms** |

## Auditoría contra el Glance oficial (`audit-1`, `audit-2`)

Go no es más lento en ninguna de las 14 operaciones. Las razones Go/oficial van
de 0,019 (crear) a 0,87 (descarga de 64 MiB). La carga de 64 MiB pasó de 0,51
(ronda 1) a 0,31–0,35.

En esta medición, la descarga de 64 MiB del oficial salió más rápida que en la
auditoría anterior del mismo día (72–74 frente a 101–111 ms, con la caché tibia). La de Go
mejoró (63–64 frente a 66–71 ms), por eso la razón subió sin que Go empeorara.

## Microbenchmarks del host (`microbench*.txt`)

Carga en el servicio con el store real: línea base 356 ms
(`microbench-baseline-service.txt`), ahora 139 ms. En `microbench.txt`, la
primera repetición (276 ms) se midió con la VM encendida compitiendo por disco y
CPU; sin la VM, en el desarrollo, fueron 135 ms. `Open` verificado de 64 MiB:
176 → 113 ms.
