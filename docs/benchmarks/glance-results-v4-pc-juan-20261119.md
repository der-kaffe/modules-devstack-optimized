# Glance Python frente a Go: glance-compact-v4

Resultado: 108/108 ventanas válidas; HTTP/bytes/estado y CPU/PSS comprobados.
Tres pares por celda; concurrencias1/16; calentamiento10s y carga30s más drenaje completo.
Fuente medida: `4290e64e65a284ca539f5fd251598314f34adb74`. Placement Python permanece como control.
Política de notificaciones: Python noop, Go original. La revisión4 repite la matriz v3; Go además envía sus notificaciones (más trabajo que Python).

| Carga | C | Ciclos/s Python | Ciclos/s Go | MiB/s Python | MiB/s Go | PSS MiB Python | PSS MiB Go |
|---|---:|---:|---:|---:|---:|---:|---:|
| catalog-list | 1 | 20.699 | 145.015 | 0.000 | 0.000 | 239.687 | 15.774 |
| catalog-list | 16 | 39.381 | 356.222 | 0.000 | 0.000 | 239.292 | 20.624 |
| catalog-show | 1 | 105.515 | 554.747 | 0.000 | 0.000 | 234.833 | 16.485 |
| catalog-show | 16 | 203.562 | 2002.772 | 0.000 | 0.000 | 234.808 | 17.346 |
| catalog-lifecycle | 1 | 2.535 | 37.834 | 0.000 | 0.000 | 234.069 | 16.470 |
| catalog-lifecycle | 16 | 4.373 | 164.854 | 0.000 | 0.000 | 235.093 | 17.224 |
| upload-1m | 1 | 1.084 | 13.571 | 1.084 | 13.571 | 235.482 | 16.819 |
| upload-1m | 16 | 1.807 | 53.278 | 1.807 | 53.278 | 238.787 | 20.515 |
| upload-16m | 1 | 1.002 | 6.721 | 16.031 | 107.540 | 235.499 | 17.477 |
| upload-16m | 16 | 1.609 | 10.659 | 25.750 | 170.540 | 238.374 | 29.979 |
| upload-64m | 1 | 0.791 | 2.517 | 50.627 | 161.083 | 235.513 | 18.562 |
| upload-64m | 16 | 1.265 | 3.419 | 80.933 | 218.836 | 239.325 | 40.980 |
| download-1m | 1 | 69.828 | 329.654 | 69.828 | 329.654 | 236.280 | 17.999 |
| download-1m | 16 | 81.252 | 786.455 | 81.252 | 786.455 | 236.493 | 18.575 |
| download-16m | 1 | 31.375 | 46.883 | 502.007 | 750.136 | 235.826 | 17.971 |
| download-16m | 16 | 40.958 | 100.974 | 655.325 | 1615.582 | 235.964 | 18.367 |
| download-64m | 1 | 12.041 | 13.128 | 770.623 | 840.192 | 235.653 | 17.957 |
| download-64m | 16 | 17.403 | 23.915 | 1113.796 | 1530.564 | 235.756 | 18.222 |

Se publican medianas de tres corridas. Rangos, ratios pareados, percentiles por operación, TTFB, CPU/op y muestras completas están en summary.json y evidencia oficial.

## Límites

- Notification policy: Python noop, Go original; Python without bus notifications (v3/v4); does not validate stock messagingv2 stability
- Three independent paired repetitions per cell; median and observed range, no statistical significance claim
- Closed-loop complete cycles including drain; upload cycle includes create/verify/delete and is not pure PUT throughput
- Download checks every full payload; upload load verifies active metadata/hash, worker canaries round-trip outside measured windows
- Public Apache path fixed; Python caches/workers/shared DB differ from Go isolated DB/store; no language-only attribution
- Python soft-delete tombstones are retained; only owned blobs and active metadata deleted via API, no SQL purge
- CPU/op uses all correct contract requests in common sample interval, including verification/404; PSS at1Hz, observed peak
- Per-operation percentiles preserve mixed-cycle meaning; n=3 boot or joint performance not measured here

La campaña conjunta se postergó por instrucción del usuario. No se atribuyen resultados de Nova ni de ambas sustituciones a esta campaña.
