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

## Auditoría medida contra Glance oficial (2026-11-18)

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
[glance-audit-20261118](../../evidence/sprint5/glance-audit-20261118/README.md).

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

## Ronda 2 (2026-11-18): revisión de todo el código de Glance Go

Cada hallazgo se calificó como alto, medio o bajo. Sólo se aplicó lo que mejora
sin romper nada y sigue igual o más cerca del original.

| # | Calificación | Cambio | Estado | Efecto medido |
|---|---|---|---|---|
| A1 | Alto | La carga no relee ni rehashea la imagen después de un `Publish` exitoso (sólo comprueba presencia y tamaño); si `Publish` falla, verifica completo | Aplicado | Carga de 64 MiB en el servicio: 356 → 193 ms |
| A2 | Alto | MD5 en paralelo con SHA-512 y la escritura (un worker acotado por llamada, sin goroutines colgadas) | Aplicado | 193 → 146 ms; `Open` verificado 176 → 113 ms |
| A3 | Alto → medio | Las 8 lecturas estáticas frecuentes se preparan una vez al arrancar y se reutilizan por conexión (`tx.StmtContext`); los parámetros siguen enlazados en el servidor | Aplicado | Metadatos p50 en la VM: 1,57 → 1,18 ms |
| M1 | Medio | Propiedades y tags con un `INSERT` multi-fila (lotes de 500) | Aplicado | De N sentencias a 1 por tabla |
| M2 | Medio | GET de bytes con una sola transacción de lectura (`DownloadView`: imagen y estado, sin propiedades ni tags) | Aplicado | Primer byte de 64 MiB: 2,6 → 1,5 ms |
| M3 | Medio | Quitar la relectura de estado tras cada escritura confirmada de la carga | **No aplicado** | Las pruebas de la carga dependen de ella para detectar cambios concurrentes con el error exacto; la carga ya es unas 12× más rápida que el oficial |
| M4 | Medio | Omitir el fsync del archivo en el camino rápido de `Publish` | **No aplicado** | Es un límite de durabilidad probado (`publish-file-sync`) y la ganancia es marginal |
| B1 | Bajo | Serialización JSON | **No aplicado** | El handler completo tarda 24 µs, alrededor del 1 % de una consulta |
| B2 | Bajo | Log por solicitud | **No aplicado** | Igual que el original |
| B3 | Bajo | Buffer de transferencia de 1 MiB (antes 256 KiB) | Aplicado | 145 → 135 ms en la carga de 64 MiB |

**Resultado en la VM** (ronda 1 frente a ronda 2): carga de 64 MiB 477 → 212 ms,
metadatos 1,57 → 1,18 ms y 1 099 → 1 254 req/s con 16 hilos. La auditoría contra
el oficial vuelve a dar ninguna operación más lenta (carga de 64 MiB en 0,31–0,35
del oficial). La regresión con Nova pasa en los dos modos.
[Evidencia](../../evidence/sprint5/glance-perf2-20261118/README.md).

## Auditoría con CPU y memoria, y cola de descargas (2026-11-18)

La auditoría ahora mide, además, lo que la campaña v3 midió y la primera
auditoría no:
- CPU por solicitud y PSS del servicio, desde su cgroup (sin MySQL, Keystone ni Apache);
- carga y descarga de 16 MiB;
- descargas con 16 clientes, por Apache y directo al servicio.

Con eso apareció un caso donde Go era más lento: **64 MiB con 16 clientes**
(12,9 frente a 15,2 req/s por Apache). Directo al servicio empataban, y quitar
`sendfile` lo empeoraba. La causa era la concurrencia: el oficial atiende con 2
workers uwsgi y deja el resto en cola, y Go enviaba las 16 a la vez en una VM de
4 vCPU.

**Cambio** (como el original): `GLANCE_GO_MAX_DOWNLOAD_STREAMS` deja en cola las
descargas que pasan del límite, en vez de rechazarlas. En disco el límite es la
mitad de las CPU con mínimo 2; en RBD no hay cola.

**Resultado:**
- 64 MiB con 16 clientes pasa a 25,5 req/s, frente a 14,5 del oficial (Go/oficial 0,55–0,58).
- Go no es más lento en ninguna de las 22 operaciones.
- Go gasta menos CPU en todos los bloques. En la descarga de 64 MiB, 7,5 frente a 91 ms; en la v3 Go gastaba 188–208 ms.
- Go ocupa 13–17 MiB de PSS, frente a 233–325 MiB del oficial.

[Evidencia](../../evidence/sprint5/glance-resources-20261118/README.md).
