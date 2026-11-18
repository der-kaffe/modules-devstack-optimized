# Rendimiento de Glance Go: optimizaciones alineadas con Glance oficial

Fecha: 2026-11-18. Criterio de Juan: para cada comportamiento, se pregunta si se
hace igual que el **Glance oficial de OpenStack**, que es la revisión fijada en el
laboratorio, `a3e5f490`, con keystonemiddleware y glance_store. Si Go lo hace
distinto y además más lento, se lleva al comportamiento del original. Si ya es
igual, se deja así, salvo que haya certeza de que un cambio lo acelera sin errores.

La referencia se leyó en el código instalado en la VM:
- keystonemiddleware `auth_token`: `token_cache_time` = 300, la clave es el SHA-256 del token, sólo se cachean validaciones exitosas y `_validate_token` revisa la expiración en cada uso;
- `glance_store/_drivers/filesystem.py`: `get` sirve el archivo en trozos sin hashear, y `add` calcula MD5 y SHA-512 en una pasada;
- oslo.db: pool de 5 conexiones más 50 de overflow.

## Cambios

| # | Antes en Go | Glance oficial | Ahora en Go |
|---|---|---|---|
| 1 | Validaba cada token contra Keystone | Caché de 300 s (keystonemiddleware) | Caché LRU con la misma semántica, `GLANCE_GO_TOKEN_CACHE_TIME` (`-1` la desactiva). Sólo en Glance; Placement Go no cambia |
| 2 | GET hasheaba el blob completo (MD5+SHA-512) antes del primer byte y copiaba en espacio de usuario | Sirve sin rehash | `OpenStream` sin hash y *sendfile*; el tamaño se compara con SQL. `Open` verificado se conserva para la recuperación y el borrado |
| 3 | GET abría 2 transacciones de escritura con `SELECT … FOR UPDATE` | Lectura simple | `DataStateSnapshot`, una lectura consistente sin lock; las escrituras conservan el lock |
| 4 | Pool MySQL de 4 abiertas y 2 ociosas | 5 + 50 | 55 abiertas y 16 ociosas |
| 5 | Publish releía y rehasheaba lo que Stage acababa de hashear | Una sola pasada | Se omite la relectura sólo si el `.ready` es el mismo archivo medido (dev, inodo, tamaño y ctime) y el recibo coincide. Si no, se verifica completo |
| 6 | 2 transferencias concurrentes por defecto | Sin ese límite (sólo los workers) | 16 (de 1 a 64) |
| 7 | Cliente Keystone con 2 conexiones ociosas por host | requests/keystoneauth: 10 | 32 |
| 8 | Al arrancar rehasheaba cada imagen activa: el arranque era proporcional al total de bytes | No hace escaneo al arrancar | Las imágenes confirmadas se comprueban por existencia y tamaño; las cargas a medio camino se resuelven como antes |

Lo que **no** cambió: la API, los status HTTP, el JSON, la autorización, el
modelo de estados, la recuperación al arranque y las garantías de escritura
(fsync y publicación atómica).

Lo que cambió deliberadamente, para igualar al original:
- un token revocado puede seguir sirviendo hasta `token_cache_time`;
- una corrupción del mismo tamaño en disco ya no se detecta al descargar (sí en la recuperación).

## Resultados

Antes/después en la VM local (6 GiB) con Keystone real, mismo runtime y sólo
cambiando el binario. Es la opción `--quick-compare` de la suite B26 y **no es
una campaña**. La evidencia está en [glance-performance-20261118](../../evidence/sprint5/glance-performance-20261118/README.md).

| Medición | Antes (`ca63516f`) | Ahora (`2ec2f231`) |
|---|---|---|
| Lectura de metadatos, secuencial, p50 | 11,9 ms | 1,9 ms |
| Lectura de metadatos, 16 en paralelo | 109 req/s | 1 137 req/s |
| Descarga de 64 MiB, total p50 | 262 ms | 55 ms |
| Descarga de 64 MiB, primer byte p50 | 203 ms | 2,5 ms |
| Carga de 64 MiB, p50 | 685 ms | 508 ms |

Microbenchmarks del host (`go test -bench`):

| Medición | Antes | Ahora |
|---|---|---|
| Abrir y leer 64 MiB | 176 ms | 12 ms |
| Stage + Publish de 64 MiB | 365 ms | 201 ms |
| Autenticación, Keystone simulado en loopback | 71 µs | 0,3 µs |

Los 176 ms de antes coinciden con el TTFB de 64 MiB que midió la campaña v3.

**Alcance:** los resultados de la campaña `glance-compact-v3` corresponden a la
revisión medida `d3286a6`, no a esta. Una campaña nueva (de Paolo) tendría que
usar una versión de protocolo nueva.

## Auditoría medida contra Glance oficial (2026-11-19)

Se midieron las mismas 14 operaciones en el Glance oficial y en Glance Go, por
la misma ruta pública, con el mismo cliente y el mismo token, en dos corridas:
- descubrimiento;
- consulta y listado;
- esquema;
- crear, PATCH y DELETE;
- cargas y descargas de 1 y 64 MiB, con su TTFB;
- consulta con 16 hilos.

**Go no es más lento en ninguna.** La razón Go/oficial va de 0,02 (crear) a 0,66
(descarga de 64 MiB), y con 16 hilos Go atiende 1 149 req/s frente a 215. Así
que, por la regla, no queda nada que llevar al original. Evidencia en
[glance-audit-20261119](../../evidence/sprint5/glance-audit-20261119/README.md).

### Revisión por código (previa a la medición)


Se aplicó la regla a cada operación restante. Sin una mejora segura, se deja.

| Operación | ¿Igual al original? | ¿Más lento? | Decisión |
|---|---|---|---|
| Listar imágenes | Distinto (SQL propio) | No: en la v3, Go fue de 2,9 a 4,5× más rápido | Se deja |
| Crear, PATCH y DELETE de metadatos | Distinto | No: en la v3, de 4,1 a 7,5× más rápido | Se deja |
| Carga: fsync del archivo y los directorios | Distinto: glance_store no hace fsync | No: Go ya era más rápido en las cargas, y el fsync le da durabilidad ante caídas | Se deja (más seguro y más rápido) |
| Lectura de metadatos en una transacción de sólo lectura (3 SELECT) | Distinto: el ORM sin transacción explícita | No: ahora es de 1,9 ms p50 | Se deja (lectura consistente) |
| Esquemas `/v2/schemas/*` | Igual: se construyen en cada solicitud | No | Se deja |
| Descubrimiento `/` y `/versions` | Igual: público, 300/200 | No | Se deja |
| Log de cada solicitud (JSON a journald) | Equivalente: el original también registra cada solicitud | No | Se deja |
| Capacidad compartida PUT/GET/DELETE (16) | Distinto: el original no tiene ese límite | No es velocidad, es coordinación (recuperación y cierre) | Se deja como diferencia conocida; es configurable de 1 a 64 |
| Resolución de un commit incierto (`verifyBlob`) | Distinto | Sólo ocurre ante un error de commit | Se deja: vale más la seguridad |
