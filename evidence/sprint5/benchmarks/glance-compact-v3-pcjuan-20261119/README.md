# Campaña v3 en el PC de Juan: el piloto que encontró el deadlock

Fecha 2026-11-19. El piloto de la campaña (`glance-compact-v3`, revisión
`376f3af`) se detuvo en `upload-1m-c16-r01-go`. Durante el calentamiento,
Glance Go respondió **409** a un `PUT` (worker 9, primer ciclo).

**Causa:** con 16 cargas simultáneas de imágenes distintas, el `SELECT ... FOR
UPDATE` sobre la fila de operación que todavía no existe toma un gap lock de
InnoDB. Dos transacciones que después insertan en el mismo hueco quedan en
deadlock (1213), y el repositorio lo informaba como conflicto. El Glance oficial
reintenta esas transacciones con oslo.db (`retry_on_deadlock`).

- `intento1-pilot-409/`: las muestras del piloto, conservadas sin aceptación.
- `reproduccion-sql-contention.log`: la prueba nueva con MySQL real falla con el código anterior (`concurrent metadata transaction conflict`).
- `reproduccion-run-tests.log`: la salida completa de `run-tests.sh`.

Lo corrigió el commit `525608f`: las transacciones de escritura se reintentan.
Con la corrección, un piloto posterior pasó 36/36. La campaña completa se
repetirá sobre el código final, que incluye las notificaciones.
