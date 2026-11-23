# Glance Python frente a Go: glance-compact-v5

Resultado: 360/360 ventanas válidas; HTTP/bytes/estado y CPU/PSS comprobados.
Tres pares por celda; concurrencias1/16; calentamiento10s y carga30s más drenaje completo.
Fuente medida: `7e922cd4047fc5c23b7892680e6894dd7b9efe8d`. Placement Python permanece como control.
Política de notificaciones: Python noop, Go original. La revisión3 mide API/catálogo/bytes sin bus RabbitMQ; conserva caché Python y documenta por separado el fallo stock v2.

| Carga | C | Ciclos/s Python | Ciclos/s Go | MiB/s Python | MiB/s Go | PSS MiB Python | PSS MiB Go |
|---|---:|---:|---:|---:|---:|---:|---:|
| catalog-list | 1 | 21.206 | 168.802 | 0.000 | 0.000 | 238.036 | 15.900 |
| catalog-list | 4 | 41.450 | 339.868 | 0.000 | 0.000 | 238.135 | 16.828 |
| catalog-list | 8 | 41.227 | 360.300 | 0.000 | 0.000 | 238.132 | 18.033 |
| catalog-list | 16 | 41.539 | 365.329 | 0.000 | 0.000 | 238.241 | 20.511 |
| catalog-show | 1 | 110.007 | 603.888 | 0.000 | 0.000 | 233.656 | 16.716 |
| catalog-show | 4 | 216.048 | 1640.511 | 0.000 | 0.000 | 233.697 | 17.060 |
| catalog-show | 8 | 215.081 | 2082.788 | 0.000 | 0.000 | 233.738 | 17.282 |
| catalog-show | 16 | 215.294 | 2339.988 | 0.000 | 0.000 | 233.690 | 17.453 |
| catalog-lifecycle | 1 | 2.520 | 38.405 | 0.000 | 0.000 | 232.835 | 16.544 |
| catalog-lifecycle | 4 | 4.309 | 94.752 | 0.000 | 0.000 | 233.814 | 16.936 |
| catalog-lifecycle | 8 | 4.330 | 136.089 | 0.000 | 0.000 | 233.884 | 17.090 |
| catalog-lifecycle | 16 | 4.313 | 173.889 | 0.000 | 0.000 | 233.956 | 17.348 |
| upload-1m | 1 | 1.074 | 13.853 | 1.074 | 13.853 | 234.299 | 17.078 |
| upload-1m | 4 | 1.795 | 28.696 | 1.795 | 28.696 | 236.317 | 18.238 |
| upload-1m | 8 | 1.786 | 42.519 | 1.786 | 42.519 | 236.432 | 19.174 |
| upload-1m | 16 | 1.795 | 54.612 | 1.795 | 54.612 | 237.614 | 22.009 |
| upload-16m | 1 | 0.980 | 6.830 | 15.685 | 109.281 | 234.305 | 19.053 |
| upload-16m | 4 | 1.636 | 11.450 | 26.171 | 183.200 | 236.329 | 19.722 |
| upload-16m | 8 | 1.610 | 12.716 | 25.753 | 203.462 | 236.464 | 24.468 |
| upload-16m | 16 | 1.586 | 13.172 | 25.370 | 210.744 | 237.693 | 32.565 |
| upload-64m | 1 | 0.773 | 2.729 | 49.477 | 174.685 | 234.246 | 20.886 |
| upload-64m | 4 | 1.281 | 4.417 | 81.952 | 282.676 | 236.278 | 24.966 |
| upload-64m | 8 | 1.263 | 4.885 | 80.845 | 312.670 | 236.398 | 31.208 |
| upload-64m | 16 | 1.230 | 3.987 | 78.692 | 255.144 | 237.627 | 39.353 |
| download-1m | 1 | 72.396 | 343.532 | 72.396 | 343.532 | 234.557 | 16.145 |
| download-1m | 4 | 84.082 | 660.072 | 84.082 | 660.072 | 234.667 | 16.421 |
| download-1m | 8 | 85.028 | 769.151 | 85.028 | 769.151 | 234.717 | 16.552 |
| download-1m | 16 | 84.460 | 830.062 | 84.460 | 830.062 | 234.629 | 16.698 |
| download-16m | 1 | 32.405 | 50.643 | 518.473 | 810.283 | 234.217 | 16.105 |
| download-16m | 4 | 42.480 | 103.691 | 679.676 | 1659.061 | 234.353 | 16.325 |
| download-16m | 8 | 42.288 | 103.195 | 676.604 | 1651.124 | 234.280 | 16.575 |
| download-16m | 16 | 42.098 | 84.127 | 673.566 | 1346.037 | 234.392 | 16.661 |
| download-64m | 1 | 12.074 | 13.231 | 772.765 | 846.808 | 233.970 | 16.439 |
| download-64m | 4 | 17.580 | 27.839 | 1125.106 | 1781.697 | 234.057 | 16.312 |
| download-64m | 8 | 17.556 | 27.898 | 1123.608 | 1785.487 | 234.099 | 16.412 |
| download-64m | 16 | 17.406 | 28.190 | 1113.975 | 1804.186 | 234.071 | 16.640 |

Se publican medianas de tres corridas. Rangos, ratios pareados, percentiles por operación, TTFB, CPU/op y muestras completas están en summary.json y evidencia oficial.

La evidencia cruda de esta campaña (`summary.json`, observaciones y muestras de las 360 ventanas, ~5 000 archivos) no se sube al repositorio: queda en el PC de Juan, en `evidence/sprint5/benchmarks/glance-compact-v5-pcjuan-20261121/`, y se entrega a pedido.

## Límites

- Notification policy: Python noop, Go original; Python without bus notifications (v3/v4); does not validate stock messagingv2 stability
- Independent paired repetitions per cell; median and observed range, no statistical significance claim
- Closed-loop complete cycles including drain; upload cycle includes create/verify/delete and is not pure PUT throughput
- Download checks every full payload; upload load verifies active metadata/hash, worker canaries round-trip outside measured windows
- Public Apache path fixed; Python caches/workers/shared DB differ from Go isolated DB/store; no language-only attribution
- Python soft-delete tombstones are retained; only owned blobs and active metadata deleted via API, no SQL purge
- CPU/op uses all correct contract requests in common sample interval, including verification/404; PSS at1Hz, observed peak
- Per-operation percentiles preserve mixed-cycle meaning; n=3 boot or joint performance not measured here

La campaña conjunta se postergó por instrucción del usuario. No se atribuyen resultados de Nova ni de ambas sustituciones a esta campaña.
