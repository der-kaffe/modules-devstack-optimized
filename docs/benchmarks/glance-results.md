# Glance Python frente a Go: glance-compact-v3

Resultado: 108/108 ventanas válidas; HTTP/bytes/estado y CPU/PSS comprobados.
Tres pares por celda; concurrencias1/16; calentamiento10s y carga30s más drenaje completo.
Fuente medida: `d3286a61c20181560a00023a2b2e26be30b3f93b`. Placement Python permanece como control.
Política de notificaciones: noop. La revisión3 mide API/catálogo/bytes sin bus RabbitMQ; conserva caché Python y documenta por separado el fallo stock v2.

| Carga | C | Ciclos/s Python | Ciclos/s Go | MiB/s Python | MiB/s Go | PSS MiB Python | PSS MiB Go |
|---|---:|---:|---:|---:|---:|---:|---:|
| catalog-list | 1 | 31.843 | 91.420 | 0.000 | 0.000 | 239.323 | 14.834 |
| catalog-list | 16 | 67.034 | 299.708 | 0.000 | 0.000 | 237.325 | 16.891 |
| catalog-show | 1 | 170.023 | 138.225 | 0.000 | 0.000 | 232.932 | 15.423 |
| catalog-show | 16 | 434.558 | 409.090 | 0.000 | 0.000 | 232.878 | 16.188 |
| catalog-lifecycle | 1 | 4.197 | 17.354 | 0.000 | 0.000 | 232.824 | 15.466 |
| catalog-lifecycle | 16 | 7.750 | 57.456 | 0.000 | 0.000 | 234.114 | 16.295 |
| upload-1m | 1 | 1.847 | 9.169 | 1.847 | 9.169 | 234.217 | 15.585 |
| upload-1m | 16 | 3.419 | 25.136 | 3.419 | 25.136 | 237.124 | 16.888 |
| upload-16m | 1 | 1.626 | 3.857 | 26.022 | 61.707 | 234.257 | 16.030 |
| upload-16m | 16 | 2.895 | 8.706 | 46.327 | 139.302 | 236.945 | 16.982 |
| upload-64m | 1 | 1.206 | 1.368 | 77.174 | 87.556 | 234.150 | 15.767 |
| upload-64m | 16 | 1.893 | 2.995 | 121.158 | 191.705 | 237.814 | 18.182 |
| download-1m | 1 | 107.930 | 75.879 | 107.930 | 75.879 | 234.988 | 15.073 |
| download-1m | 16 | 114.769 | 220.406 | 114.769 | 220.406 | 235.011 | 16.455 |
| download-16m | 1 | 51.252 | 15.282 | 820.037 | 244.517 | 234.525 | 16.072 |
| download-16m | 16 | 73.944 | 22.522 | 1183.111 | 360.349 | 234.645 | 16.612 |
| download-64m | 1 | 19.179 | 4.502 | 1227.448 | 288.150 | 234.252 | 16.378 |
| download-64m | 16 | 22.260 | 5.387 | 1424.647 | 344.790 | 234.362 | 16.599 |

Se publican medianas de tres corridas. Rangos, ratios pareados, percentiles por operación, TTFB, CPU/op y muestras completas están en summary.json y evidencia oficial.

## Límites

- Notification policy: noop; v3 compares API/catalog/bytes without bus notifications; does not validate stock messagingv2 stability
- Three independent paired repetitions per cell; median and observed range, no statistical significance claim
- Closed-loop complete cycles including drain; upload cycle includes create/verify/delete and is not pure PUT throughput
- Download checks every full payload; upload load verifies active metadata/hash, worker canaries round-trip outside measured windows
- Public Apache path fixed; Python caches/workers/shared DB differ from Go isolated DB/store; no language-only attribution
- Python soft-delete tombstones are retained; only owned blobs and active metadata deleted via API, no SQL purge
- CPU/op uses all correct contract requests in common sample interval, including verification/404; PSS at1Hz, observed peak
- Per-operation percentiles preserve mixed-cycle meaning; n=3 boot or joint performance not measured here

La campaña conjunta se postergó por instrucción del usuario. No se atribuyen resultados de Nova ni de ambas sustituciones a esta campaña.
