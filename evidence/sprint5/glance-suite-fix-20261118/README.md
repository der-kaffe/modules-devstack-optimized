# Suite B26: correcciones de los dos fallos de la ronda 2

Fecha 2026-11-18 (UTC y Chile). Rama `fix/glance-suite-robustez`. Mismo binario
de la ronda 2 (sha256 `84564884…`). VM local de 6 GiB con Keystone y Nova reales.

Ninguno de los dos fallos era de Glance Go. Estaban en la herramienta de prueba,
que los informaba como fallos de Glance.

## 1. Boot en ERROR justo después de encender la VM (`glance-1`)

**Causa medida:** al encender la VM, RabbitMQ abre el puerto 5672 a los ~40 s y
`nova-compute` reporta a los ~65 s. Mientras tanto queda "down" con el heartbeat
del arranque anterior. La suite intentó el boot 47 s después del arranque, y el
scheduler registró `Filter ComputeFilter returned 0 hosts` (NoValidHost). El
reporte sólo decía que el servidor no llegó a ACTIVE.

**Corrección** (`glance_suite.py`):
- Antes de cualquier cambio, espera hasta `--nova-ready-seconds` (240 s por defecto, tope de 600) a que conductor, scheduler y compute estén habilitados y up, con un heartbeat posterior al arranque de la máquina.
- Si se acaba el tiempo, falla indicando el servicio que falta.
- Si aun así el boot termina en ERROR, el reporte guarda `boot_fault`: código y primera línea de la falla de Nova, con IDs reemplazados.

**Verificación (`fx-fresh`):** se apagó la VM, se encendió y la suite se lanzó
apenas respondió SSH (10 s). **Esperó 22,7 s a Nova**, el servidor llegó a ACTIVE
en 6,8 s y pasó 10/10, con limpieza completa. En las corridas siguientes, con
Nova ya listo, la espera fue de 0,1 s.

## 2. Una GET de Nova "sin atribuir" en la captura (`qc-r1`)

**Causa:** cuando no hay request id, la captura empareja cada respuesta de Glance
Go con una línea de Apache. Apache registra el segundo de llegada truncado y Go
registra al terminar. La captura tomaba la línea más cercana, una respuesta a la
vez. En `qc-r1`, tres GET iguales de Nova (a S.95, S+1.95 y S+2.05) quedaron en
Apache como S, S+1 y S+2. La primera respuesta tomó la línea de la segunda, la
segunda la de la tercera, y la tercera quedó sin pareja: la línea S estaba a
2,05 s, fuera de la tolerancia de 2 s.

**Corrección** (`glance_flow.py`):
- Una línea es candidata sólo si su segundo no es posterior a la respuesta (250 ms de holgura) ni anterior en más de 1 s más la duración de la solicitud (1 s de holgura).
- La asignación es un matching máximo: primero con las líneas de clientes OpenStack, después con el resto.

**Verificación con los logs reales** (`replay.txt`, generado con
`replay_flow.py` sobre el journal persistente y el log de Apache de la VM, con la
versión anterior y la nueva de `glance_flow.py`):

| Corrida | Versión anterior | Versión nueva |
|---|---|---|
| `qc-r1` | **rechazada** (5 de Nova y 1 sin atribuir) | **aceptada** (6/6 de Nova) |
| las otras 8 corridas con captura de la ronda 2 | aceptadas | aceptadas, con la misma atribución |

## Regresión con el código corregido

| Corrida | Modo | Resultado | Captura |
|---|---|---|---|
| `fx-fresh` | Glance Go, con la VM recién encendida | 10/10 | 7 llamadas de Nova, 0 ajenas |
| `fx-glance-2` | Glance Go | 10/10 | 0 ajenas |
| `fx-joint-1`, `fx-joint-2` | Glance Go + Placement Go | 14/14 y 14/14 | 0 ajenas |
| `fx-qc` | Glance Go con comparación rápida | 11/11 | 0 ajenas |

En todas la limpieza fue completa. `make check` dio OK (106 pruebas de Python,
entre ellas 6 nuevas para estas correcciones).

## Corrección de fechas

La auditoría y la ronda 2 se rotularon por error con el 2026-11-19, pero las
corridas fueron el 2026-11-18 (épocas `1795024660` y `1795026343`, 14:57 y 15:25
en Chile). Las carpetas se renombraron a `glance-audit-20261118` y
`glance-perf2-20261118`, y se corrigieron los textos. Los mensajes de commit ya
publicados conservan la fecha equivocada.
