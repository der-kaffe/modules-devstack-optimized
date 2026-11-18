# Glance Go — fundamento de B20 (Entrega A)

Esta guía conserva la fundación A y su reproducción. La [Entrega B](b20-catalog.md)
añade LIST/PATCH/DELETE/esquemas y define sus nuevas pruebas y límites. Los
resultados reales de A no validan automáticamente B.

Actualización B25: [operación, configuración y pruebas opt-in](b25-operations.md).
Los límites de transferencia/store descritos abajo corresponden a la Entrega A
histórica; el CLI puede ahora conectar B25 explícitamente. La integración binaria
real con 002/Keystone sigue pendiente y no se deduce de la evidencia B20.

Alcance original de A: POST `/v2/images` y GET
`/v2/images/{id}`. A no incluía listado, PATCH, DELETE ni esquemas auxiliares (añadidos en B).
Siguen pendientes discovery, transferencia, store, integración Nova y sustitución
del original. La aceptación operativa de B20 requiere validar la Entrega B. Las capturas originales de Sprint 2 permanecen intactas.

## Compilación y pruebas aisladas

Módulo raíz compartido: `github.com/PaoloCastanon/modules-devstack-optimized`.
Directiva mínima Go 1.24.0; objetivo de validación Go 1.27.3. Las dependencias
directas están fijadas: mysql v1.10.1 y google/uuid v1.6.0; `go.sum` fija los hashes.
`GOTOOLCHAIN` selecciona una toolchain en la caché del usuario, sin instalación
global. La primera ejecución necesita acceso a los servidores de módulos de Go.

Desde la raíz:

```sh
make glance-fmt-check glance-vet glance-test glance-build
```

Para una comprobación adicional con el Go instalado, sin descarga de toolchain:

```sh
make GLANCE_TOOLCHAIN=local glance-vet glance-test glance-build
```

Los tests normales usan `httptest`, dobles de autenticación y repositorio, un
doble del protocolo SQL y el adaptador Keystone contra servidores HTTP de prueba. No conectan a MySQL ni al
laboratorio. Aprobarlos no demuestra persistencia SQL ni integración Keystone real.
El test de representación compara el JSON de creación y el error 404 con las
capturas históricas existentes, sin sobrescribirlas. No hay benchmark ni prueba
de carga. El paralelismo de compilación está limitado a uno y GOMAXPROCS a dos.
El build usa `-trimpath -buildvcs=false`; no incorpora estado Git ni rutas locales.
Las invocaciones Make establecen `GIT_OPTIONAL_LOCKS=0` sin cambiar configuración Git.

## Arquitectura

`cmd/glance` compone configuración, adaptador Keystone, servicio y repositorio
MySQL. `api` resuelve HTTP/JSON; `service` aplica permisos y negocio; `model`
define atributos y restricciones; `repository` contiene el contrato mínimo de
crear/consultar. MySQL implementa SQL parametrizado con contextos y transacciones.
Se usa net/http, encoding/json, database/sql y log/slog, sin ORM ni framework.

No hay autenticador falso en el ejecutable ni opción para desactivar validación.
Las identidades de prueba solo existen en archivos `_test.go`.

## Configuración

Consultar `configs/glance.env.example`; no contiene credenciales operativas.
Cargar las variables explícitamente o exportarlas desde una copia privada
revisada. No copiar secretos a Git ni imprimir la configuración completa.

| Variable | Valor predeterminado / función |
| --- | --- |
| `GLANCE_GO_LISTEN_ADDR` | `127.0.0.1:9293`; requiere host explícito |
| `GLANCE_GO_PUBLIC_BASE_URL` | `http://127.0.0.1:9293`; base de Location |
| `GLANCE_GO_DATABASE_DSN_FILE` | Obligatoria; archivo privado con DSN |
| `GLANCE_GO_KEYSTONE_AUTH_URL` | Obligatoria en serve; URL v3 completa |
| `GLANCE_GO_KEYSTONE_CREDENTIAL_FILE` | Obligatoria en serve; JSON privado |
| `GLANCE_GO_REQUEST_TIMEOUT` | `15s`; >0 y <=1m |
| `GLANCE_GO_DEPENDENCY_TIMEOUT` | `5s`; menor que el plazo de petición |
| `GLANCE_GO_MAX_BODY_BYTES` | `1048576`; límite operativo de metadatos |

Los archivos de secretos deben ser regulares, de hasta 64 KiB, con permisos 0600
o más restrictivos. No hay contraseñas ni tokens constantes en producción.
Ejemplo de estructura del archivo de credencial (rellenar privadamente):

```json
{"application_credential_id":"REPLACE_PRIVATELY","application_credential_secret":"REPLACE_PRIVATELY"}
```

El DSN usa una cuenta exclusiva y el esquema `glance_go`, por ejemplo la forma
`glance_go_app:<secreto>@tcp(<host>:3306)/glance_go?tls=true`. El valor real se guarda
en el archivo privado, nunca en una línea de comandos compartida. TLS conserva la
validación normal de certificados (`tls=true`). TCP sin TLS solo se admite con IP
loopback literal (127.0.0.1 o ::1); también se permite un socket Unix local.
`localhost` sin TLS se rechaza para evitar depender de su resolución. Cualquier
otro destino requiere TLS verificado; las configuraciones TLS registradas por nombre
no se admiten en este ejecutable. HTTP de Keystone sigue permitido para un entorno
aislado; no ofrece protección de transporte para producción.
Se rechazan `allowFallbackToPlaintext`, `allowOldPasswords`,
`allowCleartextPasswords`, `allowAllFiles`, `tls=skip-verify`, `tls=preferred`,
multiStatements, root, admin, stack y cuentas de los servicios originales.

`Location` se construye desde la base configurada; no confía en Host ni en
X-Forwarded-*. Solo se admiten HTTP/HTTPS, un host DNS ASCII o IP válido y un
puerto válido cuando se indica. Se rechazan credenciales, query/fragmento (también
los separadores `?`/`#` vacíos), escapes `%`, segmentos `.`/`..`, barras interiores
duplicadas y barras invertidas. Se permiten bases como `https://host/image/` y
barras finales repetidas; se normalizan al unir componentes con `URL.JoinPath`.
Así, `/image/` produce `/image/v2/images/{uuid}`. La base debe apuntar al proceso
de pruebas, no al Glance original; el proxy debe quitar ese prefijo al reenviar.
Los enlaces self/file/schema siguen el formato `/v2/...` de la captura original.
El enlace file describe un recurso binario cuyo endpoint sigue fuera de alcance;
los enlaces schema se sirven en la Entrega B.

## Preparación de MySQL independiente — operación posterior del operador

Requiere una instalación MySQL 8.0.46 separada o un entorno de pruebas previamente
autorizado. No ejecutar esta preparación en la VM de tesis como parte de esta
entrega. El programa nunca crea bases ni usuarios y no toca `glance`.

El operador debe crear una base vacía `glance_go` y dos cuentas exclusivas:

| Cuenta | Grants explícitos sobre `glance_go`.* |
| --- | --- |
| Aplicación | SELECT, INSERT, UPDATE, DELETE |
| Migración | SELECT, INSERT, UPDATE, CREATE, ALTER, INDEX, REFERENCES |

No otorgar privilegios globales, roles delegados, ALL PRIVILEGES ni GRANT OPTION.
El programa inspecciona `@@GLOBAL.mandatory_roles`, `CURRENT_ROLE()` y
`SHOW GRANTS` **sin FOR**, en una misma conexión. La variante `FOR CURRENT_USER`
omite roles obligatorios; cambiar la consulta tampoco expandiría privilegios de
roles. Esta entrega permite únicamente grants directos: se rechaza cualquier rol
obligatorio, activo/default o asignado aunque esté inactivo, incluso un rol limitado.
No hay roles permitidos ni expansión parcial del grafo de roles en A. Si no puede
leer estas variables/grants o interpretar el formato, falla cerrado. Esto puede
rechazar configuraciones seguras basadas en roles; usar grants directos sobre una
instancia independiente sin roles obligatorios. No conceder acceso a `mysql.*`
ni privilegios administrativos para sortear la comprobación. También se rechazan
grants por tabla, globales (salvo USAGE), PROXY, GRANT OPTION y sentencias REVOKE;
no se intenta demostrar aislamiento mediante restas de privilegios globales.
La cuenta HTTP rechaza permisos DDL, incluso si corresponden al esquema de Go.
La comprobación es de arranque; no sustituye el control administrativo de cambios
de permisos en el servidor.
Ver [SHOW GRANTS](https://dev.mysql.com/doc/refman/8.0/en/show-grants.html) y
[roles MySQL](https://docs.oracle.com/cd/E17952_01/mysql-8.0-en/roles.html).
No reutilizar credenciales administrativas. Ninguna cuenta nueva fue creada por
esta entrega. Usar secretos nuevos y permisos de archivo privados.

En grants de base, los guiones bajos pueden ser comodines incluso entre backticks.
El verificador consulta `@@GLOBAL.partial_revokes` sin modificarlo: si está OFF,
el operador debe escapar cada `_` al otorgar permisos (ámbito `glance\_go` y,
para tests, por ejemplo `glance\_go\_test\_a`). Si está ON, debe usar el nombre
literal sin esos escapes. Nunca cambiar opciones globales para sortear la guardia.
Consultar la [semántica oficial de grants](https://dev.mysql.com/doc/refman/8.0/en/grant.html).
La forma exacta de SHOW GRANTS del servidor real aún requiere la suite MySQL.

Para aplicar las migraciones, seleccionar el archivo DSN de la cuenta migradora:

```sh
GLANCE_GO_DATABASE_DSN_FILE=/absolute/private/glance-go-migrate.dsn ./bin/glance migrate
```

Para ejecutar HTTP, seleccionar la cuenta de aplicación y configurar Keystone:

```sh
GLANCE_GO_DATABASE_DSN_FILE=/absolute/private/glance-go-app.dsn ./bin/glance serve
```

`serve` verifica versión/checksum/dirty y rechaza un esquema sin migrar. Nunca
ejecuta DDL. El listener tiene plazos y apagado ordenado por SIGINT/SIGTERM.

## Esquema y migraciones

`images` conserva metadatos tipados y campos calculados anulables. Propiedades y
etiquetas tienen PK compuesta y FK a images. Índices por propietario/visibilidad,
borrado y fecha soportan el listado añadido en B. Hay checks de visibilidad, mínimos,
tamaños y consistencia de borrado. No se almacenan bytes ni ubicaciones.

`000_history.sql` inicializa explícitamente `schema_migrations`; `001_metadata.sql`
crea las otras tres tablas. La versión 1 guarda su checksum. Un bloqueo MySQL de
sesión impide migradores concurrentes. Inspección, bloqueo, DDL e historial usan
una única conexión reservada. El DSN exige autocommit y lo fija en 1; antes de
cualquier DDL/DML se comprueba también `@@SESSION.autocommit=1` en esa conexión.
No basta con confiar en el DSN. Con autocommit, la confirmación correcta de cada
sentencia confirma su DML; la marca dirty se persiste antes del DDL y solo se limpia
al finalizar. La actualización final exige una fila con el checksum esperado y
se relee el historial antes de devolver éxito. Si falla la liberación del advisory
lock se descarta la conexión y no se anuncia éxito. Un fallo de transporte durante
el DML final puede dejar resultado incierto: revisar el historial, sin asumir rollback.
Ver [autocommit MySQL](https://docs.oracle.com/cd/E17952_01/mysql-8.0-en/innodb-autocommit-commit-rollback.html). MySQL hace commits implícitos en DDL: ante un fallo,
detener y revisar manualmente; no editar dirty para fingir éxito. No hay down ni
recuperación destructiva automática. Una versión/checksum desconocida impide servir.

Antes de aceptar el historial, el migrador y `VerifySchema` inspeccionan
`information_schema.TABLES`, `COLUMNS` y `STATISTICS`. Se exige una tabla base
InnoDB, exactamente cuatro columnas y PK únicamente sobre `version`:
`version BIGINT` firmado NOT NULL, `checksum CHAR(64)` ASCII/ascii_bin NOT NULL,
`dirty TINYINT` firmado NOT NULL y `applied_at DATETIME(6)` NULL. No se admiten
columnas extra, valores por defecto no nulos, generación ni atributos automáticos.
Los anchos de presentación de enteros de MySQL no cambian su dominio y se aceptan.

Una tabla preexistente con otro motor o estructura incompatible se rechaza antes
de DDL/DML de historial; no se convierte, borra ni reconstruye. Si no existe,
únicamente `migrate` la crea y comprueba después la estructura creada. Un error de
inspección también impide continuar. La comprobación estructural se repite antes
de la confirmación final del historial. Una migración repetida con historial válido
no recrea la tabla. El SQL original y el checksum de la versión 1 no cambian.
Estas consultas no requieren grants administrativos adicionales: cada cuenta debe
poder inspeccionar los metadatos de su propio esquema.

Las regresiones con dobles SQL comprueban el rechazo y orden de operaciones; no
demuestran durabilidad física. La validación no sustituye las garantías del servidor,
filesystem y hardware, ni protege contra un administrador que altere objetos durante
la inspección. No se inspeccionan políticas arbitrarias de triggers ni todas las
estructuras de las tablas de imágenes como parte de esta comprobación del historial.

Crear imagen/propiedades/etiquetas es una sola transacción; un error de hijo revierte
todo. Consultar usa una transacción de lectura con snapshot consistente. El UUID
duplicado se traduce a 409. Las consultas excluyen borrados; DELETE de B usa esas mismas columnas con tombstones; ver su contrato. Los valores UTC se conservan con microsegundos y se exponen con segundos,
como el fixture. El pool abre hasta cuatro conexiones y aplica plazos por operación.

## Integración MySQL explícita — NO comprobada por tests aislados

No se presupone una DB autorizada ni se arranca un contenedor. En otra instancia
independiente autorizada, el operador debe crear un esquema **vacío y descartable**
`glance_go_test_<sufijo>` y una cuenta exclusiva con SELECT, INSERT, UPDATE, DELETE,
CREATE, ALTER, INDEX y REFERENCES únicamente sobre ese esquema. Guardar su DSN en
un archivo 0600. No reutilizar `glance_go`, `glance` ni credenciales de servicios.
No apuntar a 192.168.122.10/10.0.2.15 ni usar túneles/alias hacia esa VM. La guardia
de nombres/IP reduce errores, pero el operador debe verificar el destino real.

```sh
GLANCE_GO_MYSQL_TEST=1 \
GLANCE_GO_TEST_DSN_FILE=/absolute/private/glance-go-test.dsn \
make glance-test-mysql
```

El build tag `integration` separa esta suite del gate normal. La ausencia del opt-in
o la configuración es un fallo explícito, no un skip aprobado. Primero verifica
grants y que la DB esté vacía. Prueba migración, idempotencia, nulabilidad, tags,
propiedades, UUID duplicado, rollback por fallo SQL de hijo, cancelación, persistencia
entre conexiones, exclusión de borrado, UUID reservado, dirty y checksum alterado.
También desactiva autocommit en la sesión aislada para comprobar el rechazo antes
de DDL y comprueba el historial con un pool nuevo inmediatamente tras migrar.
Restaura el historial alterado y deja los datos de prueba para revisión, sin borrar
bases, usuarios o tablas. Utilizar una base vacía nueva para repetirla; el operador
administra su eliminación fuera de esta suite.

## Escenarios MySQL adicionales opt-in — NO EJECUTADOS

Los dobles SQL prueban orden/confirmaciones y fallo cerrado; no simulan un motor
MySQL ni demuestran commits, permisos efectivos, DDL o recuperación reales.
Los siguientes tests requieren recursos independientes preparados y autorizados
por el operador; nunca preparan roles, usuarios ni variables globales:

- `TestMySQLAccountBoundaryFixtures`: manifest JSON privado (0600) indicado por
  `GLANCE_GO_TEST_ACCOUNTS_FILE`; array de objetos con `dsn_file`, `reject`,
  `partial_revokes`, `mandatory_roles` y `active_roles`. Cada DSN debe usar
  `glance_go_test_<sufijo>` y una cuenta exclusiva. Las precondiciones se leen y
  comparan antes de la guardia. Preparar fixtures para grants directos seguros,
  globales, esquema ajeno, roles obligatorios excesivos/limitados, roles default
  activos e inactivos, y comodines con partial_revokes ON/OFF. Roles limitados
  también deben tener `reject: true`. Las variantes de mandatory_roles requieren
  servidores separados: no modificar el laboratorio. Los errores de inspección
  se prueban localmente; no requieren conceder administración al test.
- `TestMySQLPartialDDLRemainsDirtyAcrossConnections`: DSN separado en
  `GLANCE_GO_TEST_PARTIAL_DSN_FILE`, esquema vacío. Crea una tabla de colisión,
  provoca DDL parcial y comprueba dirty persistente al reabrir el pool; no borra
  tablas ni limpia dirty. No equivale a probar un corte de energía o caída del
  proceso: esas recuperaciones siguen pendientes de una prueba controlada.

Con `GLANCE_GO_MYSQL_TEST=1` y los archivos privados correspondientes, seleccionar
**un** escenario explícitamente desde la raíz (operación posterior autorizada):

```sh
GOMAXPROCS=2 GOTOOLCHAIN=go1.27.3 go test -p 1 -count=1 -tags=integration \
  ./internal/glance/repository/mysql -run '^TestMySQLAccountBoundaryFixtures$' -v
# En otro esquema vacío, con su DSN privado independiente:
GOMAXPROCS=2 GOTOOLCHAIN=go1.27.3 go test -p 1 -count=1 -tags=integration \
  ./internal/glance/repository/mysql -run '^TestMySQLPartialDDLRemainsDirtyAcrossConnections$' -v
```

La compilación con `-tags=integration -run '^$'` no ejecuta estos tests ni valida
MySQL. No ejecutar toda la suite integration contra una sola DB reutilizada.

### Caso preparado de historial tras reiniciar MySQL — NO EJECUTADO

Usar otra instancia desechable autorizada, un esquema vacío
`glance_go_test_history_<sufijo>` y dos cuentas con grants directos solo sobre él:
migración con SELECT/INSERT/UPDATE/CREATE/REFERENCES y lectura con SELECT.
No usar los esquemas API, persistencia o dirty de otras pruebas.

1. Registrar UUID del servidor, datadir, PID y configuración de durabilidad. Aplicar
   `glance migrate` explícitamente con la cuenta migradora, y guardar la fila de
   historial completa, el checksum y `ENGINE=InnoDB` como baseline saneado.
2. Ejecutar el checkpoint de lectura siguiente con la cuenta SELECT y guardar el
   resultado. El archivo DSN debe ser privado (0600) y apuntar al esquema dedicado.
3. El operador, en otra fase autorizada, detiene y arranca **solo esa instancia
   desechable**. Conservar evidencia del cambio de PID/inicio y del mismo UUID/datadir.
   El test no ejecuta comandos de control de servicios.
4. Repetir el checkpoint en un proceso nuevo y comparar la fila completa con el
   baseline. Debe conservar versión, checksum, dirty=false y applied_at. Un PASS
   sin evidencia externa del reinicio no demuestra persistencia después de reiniciar.

```sh
GLANCE_GO_MYSQL_TEST=1 \
GLANCE_GO_TEST_HISTORY_DSN_FILE=/absolute/private/history-reader.dsn \
GLANCE_GO_TEST_HISTORY_SERVER_UUID="<UUID_VERIFICADO_DE_LA_INSTANCIA_DESECHABLE>" \
GOMAXPROCS=2 GOTOOLCHAIN=local go test -p 1 -count=1 -tags=integration \
  ./internal/glance/repository/mysql -run '^TestMySQLHistoryReadOnlyVerification$' -v
```

No ejecutar este procedimiento sobre el MySQL del laboratorio. No cambiar dirty
para superar un fallo ni limpiar datos automáticamente. La recuperación frente a
caída abrupta o pérdida de energía requiere pruebas adicionales; un reinicio
ordenado no equivale a esas condiciones.

## Keystone real

El adaptador obtiene un token de servicio con application credentials y valida el
token cliente con GET `/v3/auth/tokens?nocatalog`: X-Auth-Token identifica al servicio;
X-Subject-Token al usuario. Requiere una credencial de servicio con permiso de
validación de tokens, rol service y sin admin. No crea esa credencial ni modifica
políticas. La disponibilidad y permisos de tal identidad real quedan por verificar.

El token propio se guarda en memoria hasta su renovación. Desde 2026-11-18 los
tokens entrantes validados con éxito también se reutilizan durante
`GLANCE_GO_TOKEN_CACHE_TIME` (300 s por defecto), igual que keystonemiddleware en
Glance oficial ([performance.md](performance.md)). Placement Go no usa esta caché. La renovación comparte únicamente el resultado del
token de servicio mediante un canal de finalización; no mantiene un mutex durante
HTTP ni crea goroutines propias. Cada solicitud en espera puede cancelar su contexto.
Si quien renueva falla o vence, quienes esperaban reciben ese fallo; una petición
posterior puede renovar de nuevo. Las identidades cliente nunca se comparten.
No se siguen redirects que puedan reenviar secretos.
En autenticación simple se rechazan tokens vencidos o malformados, scope sin
proyecto y respuestas inválidas.
Un 404 de validación significa token inválido; un 401 del validador renueva el token
del servicio una sola vez. Errores de configuración/red/servidor producen 503 y no
permiten acceso. No se confía en X-Roles, X-User-Id ni X-Project-Id del cliente.
N2 añade validación independiente de X-Service-Token cuando está presente, sin
transferir sus roles al usuario. El servicio debe tener rol service, un token
vigente y el mismo proyecto que la credencial validadora de Go. El usuario puede
pertenecer a otro proyecto. La excepción para un usuario vencido depende de una
validación nueva de Keystone con allow_expired=true, nunca de un plazo local ni
de omitir la revocación. Ver [contrato N2](composite-auth.md), incluidos sus límites
y las pruebas operativas todavía pendientes.

HTTP sin token/incorrecto/vencido sin delegación válida produce 401;
identidad sin proyecto o permiso produce 403;
imagen privada/shared de otro proyecto produce 404. Crear requiere member o admin;
publicar requiere admin. Consultar requiere reader o admin y acceso por propietario
o visibilidad public/community. Se utilizan los roles efectivos del token Keystone,
sin inventar jerarquías locales. No se implementan membresías de imágenes shared.

La integración con Keystone real está **NO VERIFICADA** hasta ejecutar pruebas
autorizadas con esa identidad. Tests de protocolo no equivalen a integración real.
Las application credentials restringidas mediante access_rules no están soportadas:
no se anuncia la cabecera de soporte ni se implementa su enforcement.

## Contrato, diferencias y pendientes

POST devuelve 201 con estado queued, UUID generado o indicado, owner del proyecto,
visibility shared por defecto, mínimos cero, flags falsos, tags [], campos calculados
null y propiedades al mismo nivel. GET devuelve el objeto directamente y 404 si
no existe/no es visible. JSON inválido/tipos incorrectos dan 400; campos reservados
403; conflicto UUID 409; media type 415; cuota/cuerpo excesivo 413; dependencia
indisponible 503. El cuerpo completo debe ser UTF-8 válido antes de decodificar JSON;
bytes inválidos producen 400, sin sustitución silenciosa. Unicode válido y propiedades
extra conservan sus valores. Primero se aplica el límite del cuerpo (413). El texto
del error UTF-8 no tiene fixture Python y no se declara idéntico al original.
Errores usan message/code/title, incluyendo el sufijo observado
en el fixture 404. Nunca se devuelve ni registra SQL, DSN, token o cuerpo secreto.

Cada petición registra un evento estructurado con request_id generado por el
servidor, método, plantilla de ruta, estado HTTP, duración y categoría de error.
Los fallos de identidad/metadatos tienen categorías separadas y los 5xx nivel ERROR.
No se registran rutas arbitrarias, UUID de imagen, queries, cabeceras de identidad,
tokens, cuerpo o mensajes crudos del driver. El fallo al escribir la respuesta queda
categorizado sin cambiar la respuesta HTTP. El gate de formato comprueba el exit
code de gofmt; sus regresiones usan archivos temporales fuera del repositorio.

Ausente y null convergen solo en nombre y formatos. null no es válido para UUID,
owner, visibility, mínimos, flags, tags o propiedades extra. Nombre vacío y propiedad
vacía permanecen vacíos; cero/falso/[] permanecen presentes. UUIDs se canonizan a
minúsculas; no se exige versión 4 a UUIDs indicados, solo a los generados. Los formatos
permitidos siguen las listas del original auditado, aunque A no procesa sus bytes.

Diferencias deliberadas/limitaciones que deben verificarse contra nuevos fixtures:

- Owner explícito solo si coincide con el proyecto; no override administrativo ni
  owner null. No protege propiedades mediante reglas configurables por atributo.
- Se exige JSON entero para mínimos; no números decimales equivalentes como 1.0.
- Propiedades extra son cadenas; no se incluyen excepciones de schema-image.json
  para propiedades tipadas/anulables como kernel_id.
- Nombre/tag/clave hasta 255 caracteres; valor de propiedad hasta 65535 bytes;
  cuotas fijas 128 tags únicos/propiedades y límite de cuerpo configurable. Son
  límites de esta entrega, sin equivalencia negativa completa demostrada.
- No se anuncian métodos de importación ni soporte completo v2.18. Discovery sigue pendiente; schema, listado, PATCH y DELETE están en B. No afirmar compatibilidad completa de cliente.
- No se replica el puerto loopback del Location histórico ni Server/Date de Apache.
- La cabecera 401 se corrige y contrasta en B; el cuerpo conserva JSON frente al
  texto original. Mensajes negativos adicionales y negociación Accept requieren
  fixtures antes de afirmar equivalencia. El 404 histórico y la creación sí se
  comparan automáticamente a nivel de representación JSON.
- La Entrega A se validó contra MySQL/Keystone reales el 27/10/2026, incluido
  DDL parcial e historial tras reinicio del servidor temporal. Las nuevas rutas
  de B requieren su propia integración; no se ha probado recuperación ante
  corte de energía ni se extrapolan los resultados funcionales a rendimiento.

No usar `make baseline`, el wrapper antiguo de compatibilidad o `make smoke` para
validar A: modifican datos/evidencias del original o dependen del ciclo binario.
La [Entrega B](b20-catalog.md) implementa listado, PATCH, DELETE y esquemas
auxiliares; su integración real y fixtures diferenciales adicionales siguen pendientes. B25/B26 y benchmarks permanecen fuera.
