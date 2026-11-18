# Filesystem store — B25, Acción 2/8

Implementa únicamente bytes y su contrato Go. No inicia servicios ni se conecta
desde `cmd/glance`; no conoce SQL, Keystone ni estados de imagen. No representa
B25 completo. Contrato superior: [B25](../../../docs/glance/b25-transfer.md).

## Uso y responsabilidad del llamador

`Initialize(root, scope)` requiere un directorio existente, vacío, 0700 y propiedad
del usuario efectivo. Es una operación explícita: no adopta contenido, no cambia
permisos y no repara configuraciones ajenas. Un fallo de inicialización puede dejar
preparación parcial que exige revisión. Nunca usar el store Python; se rechaza
también su ruta conocida `/opt/stack/data/glance/images` y descendientes.

`scope` es un UUID estable administrado por el llamador. El marcador exige la misma
identidad al reabrir; su vinculación al esquema/servidor corresponde al servicio
futuro, no se afirma que ya exista esa integración. Directorios: staging/objects
0700; archivos y marcador 0600. `NewFilesystem` verifica el marcador y obtiene un
flock exclusivo no bloqueante. Una segunda instancia requiere cerrar la anterior.

El proceso que posee la raíz y su UID son de confianza. No debe renombrar/remontar
el store, sustituir `.lock`, añadir hard links ajenos ni modificar contenidos
publicados. Estos controles no son un aislamiento frente a un proceso malicioso
con el mismo UID o root. No se siguen symlinks de clientes ni de entradas del store.

`Stage(ctx, key, reader, limit, expected)` escribe con buffer máximo de 1 MiB (antes 256 KiB),
cuenta bytes y calcula MD5/SHA-512. MD5 se calcula en una goroutine propia en paralelo con
SHA-512 y la escritura del mismo bloque. Esa goroutine no lee la fuente, se espera
antes de reutilizar el buffer y termina antes de volver. `expected=nil` admite longitud desconocida;
una declaración negativa o un límite negativo son inválidos. Cero permite un
objeto vacío en este componente: la admisión HTTP de upload vacío es una decisión
posterior, todavía NV en el contrato. Se lee como máximo un byte adicional para
detectar exceso o discrepancia. No se hacen reservas de RAM según Content-Length.

El lector es propiedad del llamador; Stage no lo cierra. Se comprueba contexto
entre lecturas/escrituras y al esperar una clave ocupada. Un `io.Reader` bloqueado
no es cancelable universalmente: el llamador debe usar deadlines/Body.Close o un
lector cooperativo para desbloquear Read. No se crean goroutines de lectura que
puedan quedar abandonadas. Tampoco se promete interrumpir un fsync bloqueado.

Operaciones sobre una misma clave se serializan y la espera respeta cancelación.
Otras claves pueden progresar simultáneamente. El límite global de transferencias
y el presupuesto de espacio pertenecen a la configuración/servicio posteriores;
no hay un semáforo global ni política de capacidad implementados en esta acción.
ENOSPC/EDQUOT reales se preservan y clasifican como ErrStoreFull.

## Identidad de archivos y publicación Linux

Las claves contienen UUID canónicos (se aceptan mayúsculas y se normalizan) de
imagen e intento; no contienen rutas. Sus nombres son `<image>-<attempt>` con
sufijos fijos `.part`, `.ready` y `.blob`. Cada componente de la raíz se abre
mediante openat/O_DIRECTORY/O_NOFOLLOW; las operaciones siguientes usan los
descriptores de los directorios. fstat verifica tipo, propietario, permisos y
mismo dispositivo. No se usa Lstat seguido de Open como defensa.

Stage crea `.part` con O_EXCL, escribe y sincroniza el archivo. Solo después de
validar longitud e integridad crea `.ready`, sincroniza staging, elimina `.part`
y vuelve a sincronizar staging. Un error puede dejar ambos nombres o un temporal
parcial; nunca se borra automáticamente evidencia de un intento fallido.

`Publish(ctx, key, receipt)` exige el Info devuelto por Stage (o conservado por el
llamador). Reabre `.ready` sin seguir symlinks y verifica tamaño, MD5 y SHA-512 con
memoria acotada antes de publicar. No publica un `.part` incompleto.

La primitiva es Linux **linkat**, que crea atómicamente un hard link o devuelve
EEXIST. Usa el descriptor verificado a través de `/proc/self/fd/<fd>` con
AT_SYMLINK_FOLLOW solo para esa referencia del kernel, no para una ruta del cliente.
Evita reabrir el pathname fuente y no requiere CAP_DAC_READ_SEARCH, a diferencia
de AT_EMPTY_PATH. No necesita dependencias nuevas: el wrapper pequeño utiliza
syscall.SYS_LINKAT de la biblioteca estándar. Las pruebas ejercitan el syscall
real y la sustitución del pathname fuente después de abrirlo.

Se vuelve a sincronizar el archivo, se crea `.blob`, se sincroniza objects y solo
entonces se retiran los nombres de staging y se sincroniza ese directorio.
Temporales y destino están en el mismo filesystem. Se requiere Linux con procfs,
hard links y fsync de directorios; un fallo no activa un fallback de rename que
pueda sobrescribir. No se afirma atomicidad conjunta de todas estas operaciones.

Publicar de nuevo devuelve ErrExists; nunca reemplaza el destino aunque la clave
sea la misma. Un error después de linkat puede dejar el objeto final completo.
El llamador debe resolver su estado/intención antes de confirmar o compensar:
**error no significa rollback**. No se implementa recuperación global ni fencing
en este paquete; ambas responsabilidades quedan en el servicio/repositorio.

## Apertura, integridad y eliminación

`OpenStream` (descargas) abre `.blob`, verifica el inodo y devuelve el descriptor y
su longitud **sin hashear**, como glance_store; el servicio compara la longitud con
SQL. `Open` (recuperación y borrado) abre únicamente `.blob`, verifica inode y
longitud, calcula hashes mediante streaming y devuelve un descriptor seekable en
offset cero. El llamador es dueño
del descriptor: debe cerrarlo. Delete/Close no invalidan un descriptor ya devuelto.
Open mide la integridad actual; solo compararla con el Info autoritativo esperado
permite detectar corrupción de igual longitud. No hay metadatos SQL ni manifest
de hashes adicionales en el store. No se procesa virtual_size ni formato alguno.

`Delete` elimina exclusivamente los tres nombres de la clave validada, sin buscar
archivos desconocidos ni recorrer directorios. ENOENT es idempotente. Un symlink
en ese nombre se elimina como enlace, sin tocar su destino; un directorio no se
borra recursivamente. Se sincronizan ambos directorios incluso en un reintento
sin archivos, porque un unlink previo puede haber precedido a un fallo de fsync.

Antes de Delete el servicio futuro debe confirmar autorización, intención y
fencing y cancelar/desbloquear un Stage en curso. No debe esperar ese escritor
manteniendo un bloqueo SQL o el mutex de publicación. El store no decide si una
imagen está eliminada ni si una petición HTTP debe devolver 410.

## Pruebas y límites de evidencia

`filesystem_linux_test.go` usa filesystem local temporal real: streaming, hashes,
límites, interrupción/cancelación, permisos, claves, symlinks, publicación sin
reemplazo, concurrencia y reapertura. `faults_linux_test.go` inyecta fallos por
instancia sobre operaciones reales: EIO, ENOSPC/EDQUOT, escritura corta, links,
unlinks y syncs. Esas pruebas no demuestran un disco físicamente lleno, una caída
del proceso, pérdida de energía o una transacción MySQL. La reapertura prueba
persistencia entre instancias del store, no reinicio del servidor MySQL.

```sh
GOMAXPROCS=2 GOTOOLCHAIN=local go test -p 1 ./internal/glance/store
GOMAXPROCS=2 GOTOOLCHAIN=local go test -race -p 1 ./internal/glance/store
```

El paquete no introduce handlers, migraciones, configuración global, almacenes
reales habilitados, recuperación automática, virtual_size inferido ni benchmarks.
