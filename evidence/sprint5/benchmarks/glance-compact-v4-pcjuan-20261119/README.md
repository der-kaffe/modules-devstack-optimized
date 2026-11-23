# Campaña Glance compact v4 en el PC de Juan

Fecha 2026-11-19. Protocolo `glance-compact-v4` (`docs/benchmarks/glance-protocol-compact-v4.json`,
sha256 `f53c9cdd…`). Fuente medida `4290e64`: la rama `bench/glance-v4-pcjuan`, que es el PR #21 más
el protocolo. Binario Glance Go sha256 `fa62bdd7…`.

**Resultado: piloto 36/36 y oficial 108/108 ventanas válidas, sin errores.** En cada
ventana se verificaron estado, bytes, hashes, CPU, PSS y descriptores, y al final se comprobaron la
limpieza y la restauración del oficial.

## Qué mide

Es la misma matriz que la v3 aceptada: 9 cargas, concurrencias 1 y 16, 3 pares por celda, 10 s de
calentamiento y 30 s de medición, con el mismo piloto y las mismas guardas.
- **Glance oficial (Python):** notificaciones en `noop` y caché de imágenes conservada, como en la v3.
- **Glance Go:** envía sus notificaciones (`messagingv2`) al mismo RabbitMQ y exchange que el oficial, con la configuración copiada de él.

Go hace más trabajo que el oficial. Placement Python queda fijo como control.

## Resultado: Go gana en las 18 celdas

Razón Go/Python de ciclos completos por segundo: mediana de los 3 pares y, entre corchetes, el rango
observado. Un valor mayor que 1 favorece a Go. La columna v3 es la campaña aceptada (`d3286a6`, hardware de
Paolo). La CPU (ms por solicitud) y la memoria (PSS en MiB) son de la v4.

| Carga | C | v3 Go/Py | v4 Go/Py | CPU Py / Go | PSS Py / Go |
|---|---|---|---|---|---|
| Listado | 1 | 2,873 | **7,00** [6,77; 7,35] | 42,0 / 3,51 | 240 / 15,8 |
| Listado | 16 | 4,471 | **9,07** [9,04; 9,21] | 45,5 / 4,45 | 239 / 20,6 |
| Consulta | 1 | 0,814 | **5,27** [5,18; 5,28] | 6,9 / 0,65 | 235 / 16,5 |
| Consulta | 16 | 0,941 | **9,84** [9,57; 9,84] | 8,0 / 0,54 | 235 / 17,3 |
| Ciclo de metadatos | 1 | 4,126 | **15,09** [14,93; 15,49] | 19,9 / 1,05 | 234 / 16,5 |
| Ciclo de metadatos | 16 | 7,545 | **37,78** [37,65; 37,82] | 22,6 / 0,88 | 235 / 17,2 |
| Carga de 1 MiB | 1 | 4,964 | **12,46** [12,32; 12,58] | 27,2 / 3,17 | 235 / 16,8 |
| Carga de 1 MiB | 16 | 7,328 | **29,51** [29,40; 29,69] | 37,3 / 2,94 | 239 / 20,5 |
| Carga de 16 MiB | 1 | 2,381 | **6,71** [3,60; 6,78] | 37,9 / 14,03 | 235 / 17,5 |
| Carga de 16 MiB | 16 | 2,921 | **6,62** [5,90; 7,15] | 52,4 / 15,79 | 238 / 30,0 |
| Carga de 64 MiB | 1 | 1,135 | **3,21** [3,10; 3,41] | 72,5 / 48,29 | 236 / 18,6 |
| Carga de 64 MiB | 16 | 1,582 | **2,70** [2,58; 3,09] | 101,8 / 67,08 | 239 / 41,0 |
| Descarga de 1 MiB | 1 | 0,700 | **4,74** [4,69; 4,82] | 14,3 / 1,09 | 236 / 18,0 |
| Descarga de 1 MiB | 16 | 1,980 | **9,66** [9,55; 9,77] | 15,1 / 1,16 | 236 / 18,6 |
| Descarga de 16 MiB | 1 | 0,298 | **1,49** [1,49; 1,59] | 23,5 / 3,28 | 236 / 18,0 |
| Descarga de 16 MiB | 16 | 0,305 | **2,47** [2,46; 2,51] | 29,2 / 4,62 | 236 / 18,4 |
| Descarga de 64 MiB | 1 | 0,236 | **1,09** [1,08; 1,15] | 51,6 / 10,50 | 236 / 18,0 |
| Descarga de 64 MiB | 16 | 0,242 | **1,37** [1,37; 1,41] | 67,1 / 12,41 | 236 / 18,2 |

**Lo que en la v3 perdía Go:**
- consulta: de 0,81–0,94 a 5,3–9,8;
- descarga de 1 MiB con un cliente: de 0,70 a 4,7;
- descargas de 16 MiB: de 0,30 a 1,5–2,5;
- descargas de 64 MiB: de 0,24 a 1,09–1,37.

La CPU de Go en la descarga de 64 MiB bajó de 188–208 a 10–12 ms por solicitud. La celda más pareja es la
descarga de 64 MiB con un cliente (1,09, rango 1,08–1,15).

## Límites

- **Hardware distinto:** este PC tiene 8 CPU y 7 GiB, con una VM de 4 vCPU y 6 GiB; la v3 corrió en el equipo de Paolo, con una VM de ~7,75 GiB. **Los valores absolutos no se comparan con la v3**; sí la dirección Go/Python dentro de cada campaña.
- Tres pares por celda: se publican la mediana y el rango observado, sin afirmar significancia estadística.
- El Python sigue sin notificaciones (como en la v3) porque, con `messagingv2` y su CacheFilter, agotó los descriptores en la v2.

## Archivos

- `pilot/` y `official/`: observaciones comprimidas, `summary.json`, `campaign.log` y `vm-final-state.json`.
- Las rutas privadas del host aparecen como `<host-privado>`.
- Resultados publicados: `docs/benchmarks/glance-results-v4-pc-juan-20261119.md`.
