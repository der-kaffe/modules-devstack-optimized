# Reproducción de la campaña v3

Leer primero `protocol-v3.json`, las enmiendas v2/v3 y el informe de infraestructura. La fuente productiva permanece en el SHA base; los instrumentos sin commit se identifican adicionalmente por SHA-256 y una copia congelada. No sustituir ese identificador por HEAD solamente.

## Preparación

1. Inventariar host, VM UUID, XML, discos, espacio, PSI, swap, servicios y recursos OpenStack existentes. Confirmar ausencia de campaña ajena y adquirir el lock de laboratorio B15. No borrar la instancia histórica ni sus allocations.
2. Abrir una sesión finita del perfil `lab_capacity_profile.py` en la VM identificada. Su `session` requiere `--allow-mutations`, `--maximum-seconds` y una unidad root con `ExecStopPost` que invoque `restore` con el mismo opt-in. Registrar la raíz privada de backups y hashes. La primera llamada de continuación omitió ese opt-in: falló sin aplicar el perfil y se conserva como intento fallido.
3. Preparar `combined_runtime.py prepare` con binarios verificados y raíz nueva `tt-placement-*`. Esto crea exclusivamente una base MySQL independiente, dos schemas Go, store Glance y validador Keystone de servicio propio. Guardar datos sensibles fuera del repositorio, con permisos 0700/0600.
4. Ejecutar `combined_runtime.py match-db-baseline` antes de pilotos v3. Lee sólo variables del original y configura únicamente el servidor experimental de UUID distinto. Su reinicio es un cambio de calibración: nunca hacerlo durante una campaña iniciada. Conservar intentos y volver a pilotar.
5. Separar HTTP de Nova/Neutron con presupuesto systemd finito y `ExecStopPost` que restaure los estados iniciales. No modificar servicios indispensables durante ventanas integradas. Cinder y los demás servicios presentes deben ser constantes en los pares.
6. Verificar pilotos autenticados, integridad, limpieza y restauración. El coordinador debe pasar una ejecución corta y un aborto controlado por SIGTERM. Congelar protocolo y fuentes antes de la primera ventana oficial.

## Entrada y ejecución

`glance_benchmark.py run --stage official` exige protocolo congelado, parámetros exactos y recibo de entrada. Ese recibo liga hashes de fuentes de evidencia, protocolo, instrumento, SHA de producción y vencimiento; no certifica por sí solo todo el trabajo. Su preparación requiere revisar las pruebas referenciadas.

`glance_campaign.py --config CONFIG.json` usa configuración externa. Los campos son: `output`, `protocol`, `entry_evidence`, `cases`, `deadline_utc_s`, `ssh` (argv con host verificado), `remote_code`, `runtime`, `resource_groups`, `url`, `token_file`, `code_sha`, `operation_lock`, `campaign_lock`, `payloads` y `max_address_space_mib`. Cada caso es `[escenario, concurrencia, bytes_o_null]`. Los payloads se indican por tamaño en bytes como claves de `payloads`. El archivo de token no se incluye en el recibo público.

El coordinador crea salida nueva, rechaza un bloque cuyo tiempo no cabe en la sesión y conserva todas las ventanas. Por repetición usa UUID nuevos, compartidos sólo entre los dos backends independientes. Reutilizar UUID borrados contra Glance Python produce tombstones y puede responder 409; no hacerlo entre intentos. Los retries requieren namespace y directorio nuevos, sin borrar el fallo.

Ejecutar con un inhibidor temporal de suspensión/idle y límites finitos. La campaña instrumenta controles de host e invitado, CPU del generador/coordinador, CPU del recolector, RSS/PSS por cgroup, espacio, swap y OOM. La incertidumbre de reloj máxima es 100 ms, con probes antes/después de cada ventana mediante SSH persistente. Los errores de reloj no se corrigen alterando timestamps retrospectivamente. `--http-only` permite exportar exclusivamente HTTP y deja explícitamente sin aceptación los recursos.

La restauración de rutas ocurre en `finally` y mediante guardian systemd independiente. Esto no sustituye la limpieza total del laboratorio ni certifica un supervisor de infraestructura nuevo. El controlador de capacidad usado durante la calibración tiene apagado al límite; el agente debe verificar y terminar la restauración con margen antes de ese límite. No reutilizarlo como supervisor desatendido de campañas largas sin añadir y probar un gate de restauración del invitado antes del apagado.

## Cierre y aceptación

1. Detener carga/colector y cerrar el socket SSH propio. Guardar `campaign.json`, todos los datos crudos y fallos.
2. Restaurar A con `api_routes.py restore`, verificar hashes Apache y Nova cuando se haya fijado su endpoint. Detener el guardian propio antes de retirar su raíz de evidencia.
3. Eliminar por API únicamente imágenes boot importadas por el ledger, si existen. Ejecutar `combined_runtime.py cleanup` después de verificar rutas originales. Mantener datos privados si falla.
4. Detener presupuesto HTTP y perfil de workers; verificar ocho hashes originales y servicios activos. Confirmar que sólo permanecen imágenes, instancias y allocations históricas. Limpiar únicamente drop-ins, tokens y cachés inequívocamente propios.
5. Verificar XML persistente original. Apagar ordenadamente y comprobar estado OFF. Construir `restoration-verified.json` con `restored`, `errors`, `vm_state` y lista `evidence` de rutas/hashes de comprobaciones reales. No establecerlo en true por suposición.
6. Ejecutar `validate_glance_campaign.py --config CONFIG --restoration RESTORE --output SALIDA_NUEVA`. Recalcula raw, orden, pares, duración, hashes, reloj, membresía de procesos y swap-out. La aceptación es del subconjunto declarado. `full_protocol_complete` sigue false mientras falten casos.
7. Ejecutar `export_glance_results.py` sobre las rutas crudas verificadas, producir CSV/PNG/PDF y análisis pareado por operación. Cinco repeticiones son cinco unidades independientes, no miles de muestras HTTP independientes. No mezclar pilotos ni versiones.
8. Ejecutar validaciones locales y revisión Git final fuera de la ventana de medición. No staging, commits, push ni publicación de PR. Conservar diffs íntegros y sus hashes fuera del repositorio.
