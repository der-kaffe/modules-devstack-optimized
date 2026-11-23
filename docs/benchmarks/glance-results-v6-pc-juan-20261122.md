# Glance Python frente a Go: glance-compact-v6

Resultado: 360/360 ventanas válidas; HTTP/bytes/estado y CPU/PSS comprobados.
Cinco pares por celda (Python y Go en orden aleatorio); concurrencias 1, 4, 8 y 16; calentamiento de 10 s y carga de 30 s más drenaje completo; los dos por Apache `/image`.
Fuente medida: `a69474da0f76d6b50825a09b723201a66dcbc5dd`. Placement Python permanece como control.
Política de notificaciones: Python noop, Go noop. Glance Go sirve el mismo catálogo (`glance`), el mismo MySQL, el mismo store y lee el mismo `glance-api.conf` que el original.
Binario `a082d95d…` (Go 1.27.4). DevStack reinstalado desde cero antes de la campaña (`clean.sh` + `stack.sh` con el plugin `glance-go`).

**Interrupción y reanudación.** La campaña se cortó en la ventana 231 (el PC se quedó sin memoria y el proceso que la dirigía se detuvo). Se reanudó desde el primer par incompleto (par 116), que se repitió entero con ID de imagen nuevos (los anteriores quedaron reservados por el borrado lógico del original); se conservaron las 230 ventanas válidas anteriores. Los recursos (CPU y memoria) de las primeras 230 ventanas se recuperaron de la VM. Queda registrado en el `manifest.json` (`resumed`).

| Carga | C | Ciclos/s Python | Ciclos/s Go | MiB/s Python | MiB/s Go | PSS MiB Python | PSS MiB Go |
|---|---:|---:|---:|---:|---:|---:|---:|
| catalog-list | 1 | 21.325 | 155.093 | 0.000 | 0.000 | 238.193 | 15.671 |
| catalog-list | 4 | 41.482 | 330.811 | 0.000 | 0.000 | 238.149 | 16.778 |
| catalog-list | 8 | 39.793 | 307.925 | 0.000 | 0.000 | 238.164 | 17.687 |
| catalog-list | 16 | 40.005 | 314.163 | 0.000 | 0.000 | 238.206 | 19.822 |
| catalog-show | 1 | 104.476 | 561.824 | 0.000 | 0.000 | 233.670 | 16.504 |
| catalog-show | 4 | 202.215 | 1505.216 | 0.000 | 0.000 | 233.695 | 16.822 |
| catalog-show | 8 | 205.779 | 1887.224 | 0.000 | 0.000 | 233.712 | 17.027 |
| catalog-show | 16 | 206.227 | 2171.664 | 0.000 | 0.000 | 233.728 | 17.296 |
| catalog-lifecycle | 1 | 2.420 | 30.376 | 0.000 | 0.000 | 232.867 | 16.464 |
| catalog-lifecycle | 4 | 3.933 | 66.692 | 0.000 | 0.000 | 233.804 | 16.789 |
| catalog-lifecycle | 8 | 3.688 | 87.293 | 0.000 | 0.000 | 233.881 | 16.883 |
| catalog-lifecycle | 16 | 3.448 | 86.388 | 0.000 | 0.000 | 233.872 | 17.206 |
| upload-1m | 1 | 0.755 | 16.893 | 0.755 | 16.893 | 234.262 | 16.949 |
| upload-1m | 4 | 1.274 | 33.873 | 1.274 | 33.873 | 236.354 | 19.306 |
| upload-1m | 8 | 1.210 | 44.285 | 1.210 | 44.285 | 236.427 | 20.036 |
| upload-1m | 16 | 1.148 | 45.404 | 1.148 | 45.404 | 237.603 | 21.984 |
| upload-16m | 1 | 0.622 | 10.003 | 9.948 | 160.056 | 234.261 | 21.164 |
| upload-16m | 4 | 1.038 | 17.049 | 16.602 | 272.781 | 236.253 | 24.419 |
| upload-16m | 8 | 1.033 | 19.322 | 16.529 | 309.153 | 236.770 | 29.208 |
| upload-16m | 16 | 0.999 | 15.640 | 15.982 | 250.233 | 237.814 | 36.297 |
| upload-64m | 1 | 0.513 | 4.680 | 32.862 | 299.492 | 234.206 | 18.007 |
| upload-64m | 4 | 0.857 | 5.431 | 54.854 | 347.609 | 236.295 | 29.112 |
| upload-64m | 8 | 0.835 | 5.856 | 53.441 | 374.764 | 237.206 | 36.488 |
| upload-64m | 16 | 1.198 | 3.653 | 76.688 | 233.782 | 238.978 | 52.758 |
| download-1m | 1 | 70.670 | 352.076 | 70.670 | 352.076 | 235.931 | 16.862 |
| download-1m | 4 | 82.172 | 696.674 | 82.172 | 696.674 | 235.930 | 17.113 |
| download-1m | 8 | 81.228 | 812.647 | 81.228 | 812.647 | 236.111 | 17.262 |
| download-1m | 16 | 82.442 | 837.598 | 82.442 | 837.598 | 235.977 | 17.461 |
| download-16m | 1 | 31.929 | 47.951 | 510.858 | 767.220 | 235.619 | 16.694 |
| download-16m | 4 | 41.178 | 100.979 | 658.853 | 1615.657 | 235.800 | 17.085 |
| download-16m | 8 | 40.731 | 100.408 | 651.701 | 1606.523 | 235.705 | 17.132 |
| download-16m | 16 | 40.477 | 82.076 | 647.638 | 1313.211 | 235.813 | 17.370 |
| download-64m | 1 | 11.743 | 12.817 | 751.522 | 820.265 | 235.481 | 17.712 |
| download-64m | 4 | 17.045 | 27.366 | 1090.890 | 1751.450 | 235.437 | 17.023 |
| download-64m | 8 | 16.986 | 27.126 | 1087.072 | 1736.040 | 235.457 | 17.233 |
| download-64m | 16 | 17.028 | 27.383 | 1089.762 | 1752.499 | 235.412 | 17.214 |

Se publican medianas de cinco pares. Rangos, ratios pareados, percentiles por operación, TTFB, CPU/op y muestras completas están en summary.json y evidencia oficial.

## Límites

- Notification policy: Python noop, Go noop; Python without bus notifications; does not validate stock messagingv2 stability
- Independent paired repetitions per cell; median and observed range, no statistical significance claim
- Closed-loop complete cycles including drain; upload cycle includes create/verify/delete and is not pure PUT throughput
- Download checks every full payload; upload load verifies active metadata/hash, worker canaries round-trip outside measured windows
- Same public Apache path, catalog, MySQL server, store and configuration for both; Python workers/caches still differ from Go; no language-only attribution
- Python soft-delete tombstones are retained; only owned blobs and active metadata deleted via API, no SQL purge
- CPU/op uses all correct contract requests in common sample interval, including verification/404; PSS at1Hz, observed peak
- Per-operation percentiles preserve mixed-cycle meaning; Nova boot and joint performance not measured here

La campaña conjunta se postergó por instrucción del usuario. No se atribuyen resultados de Nova ni de ambas sustituciones a esta campaña.
