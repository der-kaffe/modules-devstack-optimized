# B25 — operación y regresiones (Acción 8/8)

IMPLEMENTADO: cableado CLI, configuración, ownership, recovery y cierre; PUT/GET
binario y DELETE coordinado de Acciones 5–7. PROBADO LOCALMENTE: dobles SQL,
adaptador Keystone contra fixture HTTP, filesystem temporal real y sockets locales.
MySQL 8 con 002 y Keystone/binarios reales: **NO EJECUTADO — REQUIERE AUTORIZACIÓN**.
La evidencia real B20 no valida estas extensiones. No se declara B25 completo,
equivalencia íntegra Image API v2.18, boot Nova ni durabilidad física.

## Dependencias y arranque

`cmd/glance` → config → MySQL (cuenta runtime + VerifySchema) → binding de identidad
→ Filesystem exclusivo → UploadCoordinator → RecoverUploads → api.WithBinaryTransfers.
API → Keystone real y servicio; servicio → repositorio/transfers o store. No SQL
en handlers, ni transacciones SQL durante streaming/fsync. El constructor normal
no admite repositorio ni autenticador falsos mediante configuración.

1. Cargar configuración y archivos privados <=64 KiB, 0600 o más restrictivos.
2. Validar DSN con las guardas B20; abrir pool, Ping (presupuesto inicial 30s).
3. CheckAccount(serve): sólo CRUD directo en esquema permitido, sin roles/globales.
4. VerifySchema: historial InnoDB limpio, versiones 1/2 y checksums; estructura 002.
   **serve nunca llama Migrate**, ni crea esquema, usuarios o root.
5. Leer `@@GLOBAL.server_uuid, DATABASE()` al primario directo, con timeout SQL.
6. Derivar UUIDv5 bajo namespace OID con bytes UTF-8
   `glance-go-store-v1\x00<server_uuid>\x00<database>`. Comparar con STORE_SCOPE.
7. NewFilesystem verifica root existente, marcador/scope, permisos y flock exclusivo.
8. Crear propietario único y ejecutar RecoverUploads con plazo independiente.
   Un fallo cierra recursos, conserva evidencia y no abre listener.
9. Construir middleware Keystone B20 y rutas con WithBinaryTransfers; escuchar
   únicamente después de todas las precondiciones. Logs: startup_ms, duración y
   conteos de recuperación, sin DSN, tokens o rutas internas.

El binding detecta otro servidor/esquema y una configuración errónea; **no es
una identidad de generación de backups**. No detecta recrear el mismo nombre de
base dentro del mismo server_uuid ni clones que copian ese UUID. Se exige un
primario directo, sin proxy que alterne servidores, y gestión conjunta del par
SQL/store. Restauraciones/clones y cambios de primario requieren revisión humana;
no reutilizar un root anterior con una nueva generación de metadatos. No se añade
una migración ni un protocolo de replicación para resolverlo silenciosamente.
MySQL documenta [server_uuid como global de sólo lectura persistido en auto.cnf](https://dev.mysql.com/doc/mysql-replication-excerpt/8.0/en/replication-options.html).
Esa propiedad del identificador no prueba la durabilidad de los datos de imágenes.

## Configuración

Conservar las variables B20 de [development](development.md) y usar el ejemplo
sin secretos [glance.env.example](../../configs/glance.env.example).

| Variable | Predeterminado / validación |
| --- | --- |
| GLANCE_GO_BINARY_ENABLED | false; sólo true/false literales; opt-in de rutas y DELETE B25 |
| GLANCE_GO_STORE_ROOT | Obligatorio con B25; ruta absoluta limpia, existente, privada 0700 |
| GLANCE_GO_STORE_SCOPE | UUID canónico no nulo; debe coincidir con SQL y marcador |
| GLANCE_GO_MAX_UPLOAD_BYTES | 1073741824; 1..1099511627776, límite del stream real |
| GLANCE_GO_MAX_CONCURRENT_TRANSFERS | 16 (antes 2); 1..64, compartido PUT/GET/DELETE, sin cola |
| GLANCE_GO_UPLOAD_TIMEOUT | 10m; >0 y <=1h, mayor que dependencia SQL/Keystone |
| GLANCE_GO_DOWNLOAD_TIMEOUT | 10m; >0 y <=1h, incluye hashing previo y envío |
| GLANCE_GO_RESOLUTION_TIMEOUT | 10s; >0 y <=1m, resolución/compensación detached acotada |
| GLANCE_GO_RECOVERY_TIMEOUT | 5m; >0 y <=10m; fallo impide admisión |
| GLANCE_GO_SHUTDOWN_TIMEOUT | 30s; >0 y <=5m, gracia antes de cancelar conexiones |
| GLANCE_GO_REQUEST_TIMEOUT | 15s; metadatos, >0 y <=1m |
| GLANCE_GO_DEPENDENCY_TIMEOUT | 5s; SQL y cliente Keystone, menor que petición/transferencia |
| GLANCE_GO_MAX_BODY_BYTES | 1048576; límite JSON independiente del binario |

Con B25 deshabilitado, root/scope configurados causan error: no se ignoran.
Sin B25 se conserva catálogo y DELETE B20, que sólo elimina queued sin contenido.
No usar ese modo como alternativa para saltar una recuperación binaria fallida.
Header timeout 5s, idle 30s; ResponseController aplica deadlines absolutos de
lectura y escritura según ruta antes de autenticar. El plazo de escritura añade
ResolutionTimeout+1s para resolución/respuesta; al cancelar se vencen ambos para
desbloquear sockets. No hay timeout de lectura global corto que corte uploads.
Los deadlines permanecen hasta que net/http termina su flush y drenaje del body
no leído; no se borran al retornar del handler. Una regresión con cliente que no
termina el cuerpo comprueba ese límite, también en respuestas de rechazo.
El contexto no pretende interrumpir un fsync del kernel. No existen plazos de
transferencia infinitos ni buffers proporcionales a Content-Length.

### Preparación explícita del operador (NO ejecutada en esta acción)

Sólo en un MySQL independiente y previamente autorizado. Aprovisionamiento y
migraciones reales requieren autorización específica. No tocar VM/DevStack.
Archivos DSN separados y privados para migración/runtime; no guardar secretos en
el entorno de comandos ni mostrar su contenido. Desde la raíz del proyecto:

```sh
GOMAXPROCS=2 GOTOOLCHAIN=local make GLANCE_TOOLCHAIN=local glance-build
# Configurar privadamente las variables B20; DSN migrador con DDL sólo en su DB.
GLANCE_GO_DATABASE_DSN_FILE=/private/migrator.dsn bin/glance migrate
# Cambiar a la cuenta runtime, sin DDL. No imprimir el archivo DSN.
export GLANCE_GO_DATABASE_DSN_FILE=/private/runtime.dsn
bin/glance store-scope
# Copiar el UUID verificado a STORE_SCOPE. Preparar un root NUEVO y VACÍO, 0700,
# separado de Python; autorizar la ruta exacta antes de crearlo.
export GLANCE_GO_STORE_ROOT=/private/authorized-new-root
export GLANCE_GO_STORE_SCOPE=UUID_VERIFICADO
bin/glance store-init
export GLANCE_GO_BINARY_ENABLED=true
bin/glance serve
```

store-scope y store-init requieren CheckAccount(serve) y VerifySchema; no requieren
Keystone. store-init sólo inicializa el root vacío ya existente: no convierte,
repara o adopta contenido. Un fallo puede dejar setup parcial para revisión, no
se borra automáticamente. Nunca crear otro root como workaround de recovery.
serve comprueba credenciales, aunque el primer acceso a Keystone ocurre al
autenticar una petición; una dependencia caída produce 503, sin bypass.

## Cierre y recuperación

SIGINT/SIGTERM: cerrar admisión HTTP y del coordinador, dejar terminar requests
durante la gracia, cerrar listener/keepalives mediante Server.Shutdown. Si vence
la gracia, cancelar contextos y cerrar conexiones para desbloquear I/O de red.
Después **esperar handlers**, incluyendo compensaciones detached, y WaitIdle de
uploads, downloads con fd abierto, DELETE y recovery. Sólo entonces Close del
coordinador, Close del filesystem/flock y Close del pool SQL, en ese orden.

Un kernel/fsync bloqueado puede prolongar esa espera después de la gracia: se
registra y se retiene ownership; no se abandona una goroutine que escribe ni se
informa cierre limpio. Una cancelación forzada produce salida de error aunque
se hayan drenado los handlers. Un supervisor puede matar el proceso como crash;
el siguiente arranque debe reconciliar intenciones, nunca asumir limpieza.
No es una prueba de pérdida de energía, almacenamiento físico ni borrado seguro.

No hay endpoint de recovery mientras el servicio está atendiendo. Cada arranque
exclusivo vuelve a recorrer el inventario SQL y relee DataState antes de actuar.
No TTL, barrido del root o promoción saving automática. 503 significa capacidad,
dependencia o reconciliación pendiente según el JSON saneado y error_kind del log.
No borrar .part/.ready/.blob manualmente para ocultarlo. Registrar estado SQL,
intento y errores saneados con acceso privado, detener sólo procesos propios y
obtener revisión/autorización para cualquier reparación.

## Matriz de fallos y garantías locales

| Escenario | Resultado exigido / evidencia local |
| --- | --- |
| Cuenta/esquema viejo o dirty | verifyBackend corta el arranque; verificación real cubierta por dobles SQL existentes |
| Scope ajeno, root inseguro/no inicializado, flock ocupado | Sin listener; tests startup y store reales temporales |
| Recovery falla o se repite | Sin admisión; libera ownership; nueva instancia puede reconciliar idempotentemente |
| .part, .ready antes/después SQL, .blob antes/después published | Cleanup de clave confirmada a queued; recuperación con FS real y SQL doble |
| active/committed íntegro | Conservar/verificar; nunca limpiar por mera presencia en inventario |
| Blob activo ausente/corrupto | Bloqueo sin reparación; no falso 200 ni promoción |
| Tombstone/cleanup pendiente | Delete idempotente + fsync y cleaned; nunca queued |
| Unlink hecho/fsync falla | Mantener cleanup, 503; reintento sincroniza incluso con ENOENT (fallo inyectado) |
| Commit incierto/finalizador antiguo | Relectura autoritativa; no unlink o activación sin fence exacto |
| Writer vivo y DELETE | 503 sin tombstone; no abandonar Stage/Publish; política Acción 7 |
| Download abierto y DELETE | El fd Linux puede terminar; nuevas lecturas 404; cierre espera fd |
| Shutdown con writer/reader/DELETE | Sin nueva admisión; WaitIdle/Close no liberan ownership antes de terminar |
| Cliente lento/cancelado | Deadline efectivo/cierre Body; resolución acotada conserva evidencia |

Rutas habilitadas con B25: POST/GET/LIST/PATCH/DELETE de B20 y PUT/GET `/file`.
Se conservan autorización, roles/owner, protected y cuerpos/cabeceras de Acciones
6/7. GET verifica completo antes de emitir bytes; rango único/MD5 completo según
contrato. raw usa virtual_size=size; QCOW2 y formatos sin inspector: null.
Errores JSON 401, schema owner no anulable, 503 de blob perdido, exclusión de
writer vivo y reglas estrictas de tamaños siguen siendo diferencias declaradas
frente al Python fijado. No hay nueva captura ni declaración de equivalencia.

## Integración real opt-in: autorización independiente

**No ejecutada.** Preparar exactamente tres esquemas NUEVOS independientes
`glance_go_test_b25_<id>_{upgrade,sql,http}` en un servidor MySQL 8 autorizado,
sin bases originales, sin IP/alias/túnel de MySQL DevStack. No compartir esquemas.
Los tests abortan si no están vacíos. Reutilizar un servidor necesita confirmar
su identidad/aislamiento, recursos y autorización; no se inicia automáticamente.

Operaciones que deben autorizarse: crear esos esquemas; cuentas nuevas migradoras
por esquema con SELECT/INSERT/UPDATE/DELETE/CREATE/ALTER/INDEX/REFERENCES y runtime
HTTP sólo SELECT/INSERT/UPDATE/DELETE; grants directos literales (escapar `_` si
partial_revokes=OFF), sin roles/globales. Preparar archivos 0600 fuera de Git.
Comprobar roles, autocommit real, TLS (TCP loopback/socket o verificado remoto),
`innodb_flush_log_at_trx_commit`, `sync_binlog`, versión exacta, engines y límites.
Los tests no modifican configuración global ni cuentas/grants previos.

Cada comando siguiente escribe sólo sobre SU nuevo esquema mediante el DSN
privado. Las rutas son ejemplos a sustituir después de autorizar recursos exactos:

```sh
# Esquema upgrade: crea v1 y añade 002; conserva metadata/historia y rechaza
# checksum modificado y dirty. DEJA dirty a propósito: nunca usarlo para API.
GLANCE_GO_B25_MYSQL_TEST=1 GLANCE_GO_TEST_DSN_FILE=/private/upgrade-migrator.dsn \
  make GLANCE_TOOLCHAIN=local glance-test-b25-upgrade
# Esquema sql: CAS Stage/Publish/Commit concurrentes, finalizador/DELETE,
# CHECK real que falla + rollback SQL, reapertura. Recibos sintéticos, sin bytes.
GLANCE_GO_B25_MYSQL_TEST=1 GLANCE_GO_TEST_DSN_FILE=/private/sql-migrator.dsn \
  make GLANCE_TOOLCHAIN=local glance-test-b25-sql
# Esquema http: migración inicial 000/001/002 + idempotencia y runtime sin DDL;
# root EXISTENTE vacío 0700 autorizado. SQL y FS reales, AUTH DOBLE explícito.
GLANCE_GO_B25_MYSQL_TEST=1 GLANCE_GO_TEST_DSN_FILE=/private/http-migrator.dsn \
  GLANCE_GO_TEST_RUNTIME_DSN_FILE=/private/http-runtime.dsn \
  GLANCE_GO_B25_TEST_ROOT=/private/authorized-http-root \
  make GLANCE_TOOLCHAIN=local glance-test-b25-pipeline
```

El último test usa arranque real de componentes, coordinador/API/store/SQL reales,
POST, PUT, GET, PATCH atómico, DELETE/tombstone/cleaned, pool nuevo y recovery.
La reapertura **no es un reinicio de proceso o servidor**. Conserva SQL/root;
detiene sólo su servidor HTTP. `t.TempDir` sólo se usa en tests locales normales.
Para Keystone REAL agregar `GLANCE_GO_B25_KEYSTONE_TEST=1`,
GLANCE_GO_KEYSTONE_AUTH_URL, GLANCE_GO_KEYSTONE_CREDENTIAL_FILE privado con la
app credential service preexistente autorizada sin admin/access_rules y
GLANCE_GO_TEST_CLIENT_TOKEN_FILE privado con token vigente member+reader de
proyecto. No emite ni renueva tokens, no crea identidades; fallo aborta, no cae
a un autenticador falso. Se requiere otra base/root HTTP nuevos para reejecutar.

Escenarios adicionales preparados por suites existentes (cada uno otro esquema
nuevo): TestMySQLTransferOperations (reservas concurrentes),
TestMySQLCoordinatedDeletion (tombstones/UUID/children y DELETE concurrentes).
Pérdida controlada del ACK COMMIT exige proxy/fallo de red autorizado; no se
atribuye a un CHECK fallido ni se afirma ejecutada. Crash/SIGKILL entre fronteras,
reinicio de MySQL y pérdida de energía real son campañas separadas pendientes:
anotar fase SQL, archivos y recibos, matar únicamente proceso propio autorizado,
reabrir mismo par y verificar no promoción/limpieza indebida. No ejecutar esos
comandos contra servicios existentes. Un shutdown limpio no sustituye esa campaña.

Guardar fuera de Git versiones, configuración saneada, resultados completos,
consultas físicas SQL, hashes de payload, fases, estados y recursos. Nunca
tokens/DSN/passwords en logs. No eliminar esquemas, cuentas, roots o evidencia
automáticamente; la limpieza posterior exige autorización específica.

## Comparación experimental posterior (no ejecutada)

Los logs existentes añaden response_bytes a request_id/método/ruta/status/duración;
arranque y recovery publican duración/conteos. No hay payload, tokens ni SQL.
Preparar dataset fijo por formato/tamaño y semilla; hardware efectivo, versión
Go/Python/MySQL, filesystem/montaje, TLS/auth, concurrencia, warm-up, caché y
repeticiones explícitos. Separar metadatos, upload, download completo/rango,
startup vacío, startup/recovery con objetos incompletos y committed.

Medir desde cliente latencia p50/p95/p99, errores y throughput de bytes verificados;
muestrear `/proc/<pid>/status`, CPU de `/proc/<pid>/stat`, RAM/RSS y `/usr/bin/time -v`
en procesos propios autorizados. Contabilizar por separado MySQL y filesystem.
Hasta la revisión medida en la v3, GET hacía una pasada completa de hashes ANTES
de transmitir. Desde 2026-11-18 sirve sin rehash, como glance_store (ver
[performance.md](performance.md)). Recovery puede recorrer blobs grandes. Incluir esos costes, no quitarlos para
favorecer Go. El buffer fijo no equivale a una medición de RSS o throughput.
No hay benchmark ni resultados empíricos inventados en esta acción.
