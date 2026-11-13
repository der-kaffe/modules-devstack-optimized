# Proyección: Ceph RBD como backend de Glance Go

Estado (2026-11-12): **proyección de diseño y esfuerzo. No está implementado ni
medido.** Se basa en el código actual del store y en los resultados de la
campaña `glance-compact-v3`. Ninguna cifra de este documento sale de una
ejecución con Ceph.

## Por qué RBD

En despliegues reales de OpenStack es habitual que Glance, Nova y Cinder
compartan un clúster Ceph. Cuando Nova usa `images_type=rbd` sobre el mismo
clúster que Glance, **no descarga la imagen por HTTP**: la clona copy-on-write
desde una snapshot protegida de la imagen. Eso cambia la pregunta experimental:
- El boot deja de transferir bytes a través de Glance.
- La API de carga y descarga sigue existiendo para clientes y otros backends.

## Punto de partida en el código

- `internal/glance/store/store.go` ya define una interfaz `Store` (Stage, Publish, Open, Delete) que separa los bytes de HTTP, SQL y estados. El coordinador de cargas usa esa interfaz (`bytes store.Store`), y la recuperación de cargas se guía por SQL y verifica a través de la misma interfaz. Ambas partes se pueden reutilizar.
- El acoplamiento concreto está en dos puntos: `NewUploadCoordinator(..., fs *store.Filesystem, ...)` y el arranque en `cmd/glance/startup.go`, que abre la raíz, toma el flock exclusivo y la cierra.
- **Hallazgo:** `Filesystem.Open` lee el objeto completo y recalcula MD5 y SHA-512 *antes* de entregar el primer byte. Esto explica las descargas grandes de la campaña v3. En la descarga de 64 MiB con C=1:
  - TTFB p50: 176 ms en Go contra 7 ms en Python.
  - CPU por solicitud: 188 ms en Go contra 29 ms en Python.
  - Throughput: Go logra entre 0,24 y 0,31 veces el de Python en las descargas de 16 y 64 MiB.

  Con RBD, ese diseño leería cada imagen dos veces por red, así que hay que cambiar el contrato antes de crear el backend. Además, cualquier cambio de este punto altera el objeto medido y exige una nueva versión del protocolo.
- `direct_url` y `locations` están reservados y Go no los publica. Para que Nova haga copy-on-write, Go tendría que exponer `direct_url` (`rbd://<fsid>/<pool>/<imagen>/snap`) sólo a roles de servicio. Es el equivalente a `show_image_direct_url` de Glance Python.

## Diseño: correspondencia con la interfaz

| Operación | Filesystem (actual) | RBD (propuesto) |
|-----------|---------------------|-----------------|
| Stage | `.part` con O_EXCL, escritura y fsync, MD5/SHA-512, luego `.ready` | Imagen RBD `<image>-<attempt>.part`. Se escribe en streaming con bloques grandes alineados, se calculan los hashes en el mismo paso y se amplía el tamaño por tramos si no hay Content-Length. Al validar, se renombra a `.ready`. |
| Publish | Relee `.ready` y verifica; luego `linkat` atómico (EEXIST → `ErrExists`) | Rename atómico dentro del pool (si el destino existe → `ErrExists`), luego snapshot `snap` y protect, que es lo que exigen los clones de Nova. Confía en el recibo de Stage, sin relectura completa. |
| Open | Abre y recalcula hashes antes de responder | Abre la imagen en `snap` en modo de sólo lectura (`ReadAt` y `Seek`). No vuelve a calcular los hashes antes de servir. |
| Delete | Borra los nombres de la clave | Unprotect de `snap`. Si existen clones (Nova usando la imagen), falla con un nuevo `ErrInUse`, que la API traduce a 409, como hace Glance Python. Después borra la snapshot y la imagen. |
| Exclusión | flock de la raíz y marcador de scope | Lock exclusivo de RADOS sobre un objeto marcador del pool o namespace, con el mismo UUID de scope. |
| Capacidad | ENOSPC/EDQUOT → `ErrStoreFull` | Cuota del pool → `ErrStoreFull`. |
| Durabilidad | fsync de archivo y directorio | El ack de RADOS llega después de escribir en las réplicas. No hay fsync de directorio. |

**Verificación de integridad sin releer todo.** Se proponen dos alternativas, que son decisiones del contrato:
- Calcular los hashes **mientras** se envía la respuesta y abortar la conexión si no coinciden. El cliente detecta la longitud o el hash incorrecto, tal como lo hace hoy la suite.
- Confiar en los hashes publicados y verificar con un scrub periódico.

En ambos casos conviene una opción `verify_on_open` que conserve el comportamiento actual por defecto, para no invalidar lo ya medido.

## Librería

[`github.com/ceph/go-ceph`](https://github.com/ceph/go-ceph) (paquetes `rados` y `rbd`) es un binding cgo a librados y librbd. Tiene tres consecuencias:
- Exige `CGO_ENABLED=1` y las cabeceras de desarrollo de Ceph.
- Su versión queda atada a la del clúster.
- El binario deja de ser estático.

Por eso el backend iría detrás de un build tag `rbd`, de modo que `make glance-build` siga produciendo el binario actual sin dependencias de Ceph.

Se descarta invocar la CLI `rbd`: crea un proceso por operación y no da control sobre el streaming.

## Fases y estimación

Se estima en story points, con la misma escala del backlog:

| Fase | Contenido | SP |
|------|-----------|----|
| F1 | Contrato: el constructor y el arranque aceptan `store.Store` y una interfaz de exclusión y cierre; se agrega `verify_on_open`. Los tests actuales no cambian. | 3 |
| F2 | Store RBD con go-ceph detrás del build tag. Tests contra un clúster mínimo de un OSD en contenedor. | 8 |
| F3 | `direct_url` con política sólo de servicio, con forma compatible con Glance Python. | 3 |
| F4 | DevStack con `devstack-plugin-ceph` (Glance y Nova con `images_type=rbd`). Se extiende la suite B26 con un boot copy-on-write, cuyo flujo esperado tiene metadatos pero **ninguna** descarga `/file`. | 5 |
| F5 | Nueva versión del protocolo compacto: las mismas cargas más el tiempo de boot con copy-on-write frente a descarga. | 5 |
| | **Total** | **≈ 24** |

Es un esfuerzo comparable a B20 + B25 (26 SP), así que no cabe antes del cierre del 4 de diciembre. Se propone:
- Presentarlo en el informe final como trabajo futuro, con este diseño.
- Hacer sólo **F1** si queda tiempo, porque también sirve para estudiar el hallazgo del TTFB con un protocolo nuevo.

## Factibilidad en el laboratorio

Un Ceph mínimo (1 MON, 1 MGR y 1 OSD) necesita memoria propia. Por defecto, el
OSD apunta a `osd_memory_target` = 4 GiB. Se puede bajar, a costa de
rendimiento.
- **PC de Juan:** tiene 7 GiB en total y la VM de DevStack ya usa casi todo. **No es realizable ahí.**
- **Máquina de Paolo o un nodo aparte:** haría falta una VM de unos 12 GiB o un nodo Ceph separado.

## Riesgos

- Dependencia cgo y compatibilidad de versiones con el clúster.
- Borrar una imagen que tiene clones activos: se maneja con `ErrInUse` y la respuesta 409, y hay que probarlo.
- Consistencia entre SQL y RBD: se reutiliza el modelo actual de fencing por intento y de recuperación dirigida por SQL.
- `direct_url` revela el pool y el fsid, así que nunca debe mostrarse a usuarios.
- Comparabilidad: con copy-on-write, el boot ya no mide la descarga. Los resultados no son comparables con la campaña v3 y deben publicarse aparte.
