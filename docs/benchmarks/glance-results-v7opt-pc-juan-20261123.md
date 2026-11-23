# Glance Python frente a Go: glance-v7

Resultado: 144/144 ventanas válidas; HTTP/bytes/estado y CPU/PSS comprobados.
Tres pares por celda; concurrencias1/16; calentamiento10s y carga30s más drenaje completo.
Fuente medida: `12b53621b0a75be1fa98218a3af277697c538fbb`. Placement Python permanece como control.
Política de notificaciones: Python noop, Go noop. La revisión3 mide API/catálogo/bytes sin bus RabbitMQ; conserva caché Python y documenta por separado el fallo stock v2.

| Carga | C | Ciclos/s Python | Ciclos/s Go | MiB/s Python | MiB/s Go | PSS MiB Python | PSS MiB Go |
|---|---:|---:|---:|---:|---:|---:|---:|
| catalog-list | 1 | 21.055 | 138.797 | 0.000 | 0.000 | 239.514 | 16.694 |
| catalog-list | 16 | 41.176 | 345.355 | 0.000 | 0.000 | 238.305 | 21.678 |
| catalog-show | 1 | 104.789 | 493.015 | 0.000 | 0.000 | 233.780 | 17.609 |
| catalog-show | 16 | 202.051 | 1705.576 | 0.000 | 0.000 | 233.748 | 18.441 |
| catalog-lifecycle | 1 | 2.363 | 27.498 | 0.000 | 0.000 | 232.895 | 17.530 |
| catalog-lifecycle | 16 | 3.889 | 81.080 | 0.000 | 0.000 | 233.952 | 18.503 |
| upload-1m | 1 | 0.929 | 15.299 | 0.929 | 15.299 | 234.308 | 18.187 |
| upload-1m | 16 | 1.524 | 39.960 | 1.524 | 39.960 | 236.834 | 24.160 |
| upload-16m | 1 | 0.817 | 9.651 | 13.073 | 154.422 | 234.261 | 19.770 |
| upload-16m | 16 | 1.337 | 16.481 | 21.386 | 263.700 | 237.239 | 34.911 |
| upload-64m | 1 | 0.677 | 4.606 | 43.342 | 294.768 | 234.222 | 20.017 |
| upload-64m | 16 | 1.084 | 4.737 | 69.405 | 303.138 | 238.459 | 40.405 |
| download-1m | 1 | 66.222 | 328.408 | 66.222 | 328.408 | 235.410 | 16.793 |
| download-1m | 16 | 75.906 | 685.694 | 75.906 | 685.694 | 235.439 | 17.382 |
| download-16m | 1 | 30.963 | 48.132 | 495.410 | 770.118 | 235.109 | 16.769 |
| download-16m | 16 | 40.787 | 99.448 | 652.592 | 1591.167 | 235.214 | 17.285 |
| download-64m | 1 | 11.810 | 13.231 | 755.826 | 846.808 | 234.953 | 17.009 |
| download-64m | 16 | 17.558 | 28.274 | 1123.725 | 1809.505 | 234.965 | 17.366 |
| import-direct-16m | 1 | 0.410 | 5.489 | 6.560 | 87.819 | 264.377 | 21.849 |
| import-direct-16m | 16 | 0.808 | 11.355 | 12.935 | 181.683 | 268.986 | 34.897 |
| upload-qcow2 | 1 | 0.780 | 9.553 | 12.722 | 155.829 | 234.988 | 19.710 |
| upload-qcow2 | 16 | 1.308 | 18.086 | 21.339 | 295.020 | 237.038 | 36.058 |
| nova-mix | 1 | 18.861 | 38.409 | 301.776 | 614.542 | 238.260 | 20.306 |
| nova-mix | 16 | 27.670 | 84.145 | 442.727 | 1346.326 | 238.424 | 19.539 |

Se publican medianas de tres corridas. Rangos, ratios pareados, percentiles por operación, TTFB, CPU/op y muestras completas están en summary.json y evidencia oficial.

## Límites

- Notification policy: Python noop, Go noop; Python without bus notifications (v3/v4); does not validate stock messagingv2 stability
- Independent paired repetitions per cell; median and observed range, no statistical significance claim
- Closed-loop complete cycles including drain; upload cycle includes create/verify/delete and is not pure PUT throughput
- Download checks every full payload; upload load verifies active metadata/hash, worker canaries round-trip outside measured windows
- Public Apache path fixed; Python caches/workers/shared DB differ from Go isolated DB/store; no language-only attribution
- Python soft-delete tombstones are retained; only owned blobs and active metadata deleted via API, no SQL purge
- CPU/op uses all correct contract requests in common sample interval, including verification/404; PSS at1Hz, observed peak
- Per-operation percentiles preserve mixed-cycle meaning; n=3 boot or joint performance not measured here

La campaña conjunta se postergó por instrucción del usuario. No se atribuyen resultados de Nova ni de ambas sustituciones a esta campaña.
