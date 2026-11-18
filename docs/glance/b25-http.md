# B25 — HTTP binario inyectable (Acción 6/8)

Actualización Acción 8: `cmd/glance serve` conecta estas rutas con configuración
explícita, binding SQL/store y recuperación previa. Véase [operación](b25-operations.md).
La descripción del CLI pendiente más abajo conserva el estado histórico de Acción 6.

El alcance histórico de esta acción se conserva a continuación. La integración
posterior del DELETE existente se describe en [Acción 7](b25-deletion.md).

Se añaden PUT y GET `/v2/images/{id}/file`. No se amplía DELETE.
El middleware Keystone B20 sigue siendo la única entrada de identidad; no se
confía en cabeceras de proyecto/roles. Los errores mantienen el JSON B20 y
WWW-Authenticate en 401, sin diagnósticos SQL, paths o secretos.

## Construcción y estado de ejecución

`api.New` conserva su modo predeterminado de catálogo B20. La opción explícita
`api.WithBinaryTransfers(coordinator)` conecta las rutas al UploadCoordinator.
No cambia su estado: ambas transferencias rechazan con 503 hasta que se complete
RecoverUploads. El límite se obtiene de MaxUploadBytes, política inmutable del
coordinador. No existe un parámetro HTTP para attempt_id, root o cleanup.

El coordinador conserva la propiedad exclusiva del Filesystem. Los downloads
comparten MaxConcurrent con uploads, incluyendo preflight y streaming; se rechaza
capacidad ocupada sin colas. Close rechaza lectores abiertos, además de writers y
recuperación. El llamante debe cerrar cada Download.Body.

`cmd/glance` sigue sin conectar el store ni estas rutas. El cableado operativo
de Acción 8 debe configurar root propio y scope vinculado al servidor/esquema,
adquirir exclusividad, verificar esquema sin migrar automáticamente, recuperar
antes de admitir tráfico y cerrar ordenadamente lectores/writers. Debe separar
plazos de upload/download de los actuales plazos cortos de metadatos y aplicar
deadlines de red efectivos: un contexto por sí solo no desbloquea cualquier I/O.
Esta acción no habilita un arranque binario operativo mediante el CLI existente.

## PUT y errores

Exige application/octet-stream, permite parámetros MIME válidos y rechaza
cabeceras Content-Type repetidas. Usa el ContentLength ya validado por net/http;
no reinterpreta framing. Admite longitud desconocida y la declaración opcional
X-Openstack-Image-Size (un decimal int64 no negativo, sin signos ni duplicados,
coincidente con ContentLength si ambos existen). Cuenta y limita el stream real,
sondeando un byte adicional, sin aceptar un prefijo truncado ni reservar RAM
según el tamaño. La entrada se cierra incluso si falla autenticación. Cancelar
la petición cierra Body para desbloquear su Read; un Body personalizado debe
cumplir el contrato de net/http de Close concurrente que desbloquea Read.

El coordinador autoriza member propietario/admin antes de reservar o escribir;
private/shared ajenas dan 404, public/community ajenas dan 403. Un formato ausente
da 400, estado incompatible 409, longitud incorrecta/interrupción de fuente 400,
límite o store lleno 413, resultado confirmado 204. Capacidad/recuperación pendiente,
dependencias o incertidumbre no resuelta dan 503. Si falla una compensación, 503
tiene prioridad sobre el error inicial 400/413. Un tombstone ganador da 410 según
la resolución existente, sin implementar aquí la eliminación coordinada.

## GET y rangos

OpenDownload exige reader/admin y visibilidad B20 antes de abrir. Relee DataState
al primario, valida intento/fase y exige active/committed con recibos coincidentes.
Store.Open mide tamaño, MD5 y SHA-512 con buffers acotados; antes de entregar el
descriptor se vuelve a comprobar visibilidad y fence SQL. No hay transacciones
SQL durante hashing ni streaming. No se exponen claves internas ni se ejecutan
reparaciones o limpiezas desde GET.

Un estado coherente sin contenido confirmado devuelve 204. Active contradictorio
o blob confirmado perdido/corrupto devuelve 503 antes de cabeceras binarias. Un
objeto confirmado vacío devuelve 200 y longitud cero. Tras autorizar/verificar,
GET completo da 200 y rango único da 206 con Content-Range. Se aceptan inicio-fin,
inicio- y -sufijo; se recorta el final al tamaño. Rangos múltiples dan 400;
inválidos/insatisfacibles, 416 y Content-Range bytes */N cuando se conoce N.

Se emiten application/octet-stream, Content-Length exacto y Content-MD5 del
contenido completo, también para 206. No se anuncian ETag/If-Range, multipart ni
HEAD (405). El Content-Range legado como cabecera de solicitud no se interpreta.
Se copia con un buffer fijo de 1 MiB (antes 256 KiB; en disco se usa sendfile) y se cierra el descriptor en todas las
rutas. Si falla lectura/escritura después de cabeceras, se registra binary_stream
y se aborta mediante http.ErrAbortHandler: nunca se anexa JSON ni cambia el status.
Un fd ya abierto puede terminar después de un unlink posterior en Linux; la
coordinación de DELETE/publicación pertenece a Acción 7.

## Contraste y límites de evidencia

Se leyó el código upstream fijado en
[`a3e5f490`](https://github.com/openstack/glance/blob/a3e5f490b7a28b1161714c94acffe5308d24b567/glance/api/v2/image_data.py)
y su
[`wsgi.py`](https://github.com/openstack/glance/blob/a3e5f490b7a28b1161714c94acffe5308d24b567/glance/common/wsgi.py).
El deserializador confirma 415 para media type ausente/incorrecto; el serializador
confirma PUT 204, Content-Length y el MD5 completo también en 206. wsgi confirma
400 para rangos múltiples y 416 para rangos inválidos. Esto amplía la inspección
de fuentes del contrato B25, sin ejecutar el servicio Python en esta acción.

Diferencias seleccionadas: validación decimal más estricta de tamaño y exigencia
de coincidencia entre declaraciones; errores JSON B20; blob confirmado perdido
da 503, mientras el serializador Python puede convertir Store.NotFound en 204;
no se reproduce Content-Range legado de solicitud. No se afirma equivalencia de
todas las excepciones, sintaxis aceptadas por WebOb o política configurable.
Upload vacío se comprueba localmente en Go; su aceptación Python sigue sin fixture.
QCOW2/otros formatos sin inspector conservan virtual_size null, según Acción 5.

Las pruebas httptest comprueban códigos, cabeceras, bytes, límites y cierre. Hay un
recorrido con el coordinador y filesystem reales sobre t.TempDir, pero SQL e
identidad son dobles: no es integración MySQL/Keystone, reinicio ni prueba de
durabilidad física. MySQL 8 con 002 y el flujo binario real siguen pendientes.
