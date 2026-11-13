# Registro de avance observado

Registrar resultados y límites de cada tarea; los planes y criterios viven en [PLANS.md](../PLANS.md). Las instrucciones persistentes están en [AGENTS.md](../AGENTS.md).

## 2026-10-29 — B07: cierre técnico del alcance priorizado 1.0

- **Criterio y alcance:** B07, «Operaciones priorizadas de providers e inventories superan las pruebas definidas» (consulta de sólo lectura al libro original, Backlog!H10). [Aceptación técnica](../docs/placement/b07-acceptance.md): providers/inventories 1.0 implementados y validados; PR #6 pendiente de merge. El Excel y los informes S2 permanecen intactos. No se confunde este cierre con B08–B12 o Nova.
- **Regresión previa:** ocho errores de esquema reproducidos contra una exportación intacta de `4d1976c`, con salida 1. La validación nueva usa cuatro esquemas upstream 1.0 embebidos, JSON con orden/lexemas y diagnósticos compatibles con jsonschema/WebOb inspeccionados; producción enteramente Go. Se contrastó un oráculo adicional de 61 casos sintéticos y se preservó el parser decimal exacto.
- **Prioridades:** se amplió el runner de 38 a 57 casos, sin cambiar canonical/differences ni relajar campos. El ensayo intermedio de 53 casos tuvo 47 coincidencias estrictas, 48 status y 53 estados, salida 1: seis diferencias de prioridad. Se corrigieron existencia antes del body, generation antes de capacidad/clase, capacidad antes de resolver clases, orden de entrada y Content-Type antes de política; escrituras conservan la semántica Accept original. Lock/CAS final permanecen en MySQL.
- **Campaña final real:** [57/57 status, respuestas estrictas y estados iguales](../evidence/sprint3/placement/b07-close-20261029/README.md), runner salida **0**, toda limpieza de providers verificada. Python `f4a89d3`, MySQL Go 8.0.46 aislado y Keystone desplegado. Usuario/proyecto temporales, application credentials con expiración y roles admin/service/reader separados; validador sólo service. Proxy temporal loopback monta /placement, sin modificar bodies/responses ni original/Nova.
- **Persistencia:** `make placement-test-mysql`, `make placement-test-binary` y `make placement-test-upgrade` PASS con race, tres esquemas nuevos independientes por ensayo. Además, binario con Keystone real conserva provider/inventory/generation después de SIGTERM y reinicio; canario eliminado y 404 comprobado. No se reinicia el servidor MySQL ni se prueba corte de energía.
- **Gate y regresiones:** `make check` PASS, incluyendo ambos dominios, formato/vet/race/build; API con race y casos de snapshots/headers PASS; regresión de orden/Accept repetida 30 veces PASS. Capturador del oráculo con SHA/versiones exigidos produce el mismo fixture; sintaxis Python y `git diff --check` PASS. [Fuentes/hash del candidato probado](../evidence/sprint3/placement/b07-close-20261029/attempt-3/provenance.json).
- **Limpieza:** se retiraron canarios/providers, application credentials, usuario/proyecto y archivos privados; ausencia verificada. Procesos/proxies y contenedores MySQL descartables retirados. `tt-openstack-lab` volvió a `shut off`; [pruebas de limpieza](../evidence/sprint3/placement/b07-close-20261029/keystone-cleanup.json).
- **Siguiente:** B08 classes/traits, sin anunciar versiones superiores hasta cubrir sus contratos. Permanecen fuera del cierre API íntegra, HTML/todas las combinaciones de negociación, Unicode/números fuera de límites locales, allocations/candidates, cierre transversal B11/B12, Nova y benchmarks.

## 2026-10-29 — B07: errores de negociación y capacidad

- **Base y criterio:** `git pull --ff-only` dejó main al día en `dfd7624`; incremento en `fix/placement-b07-errors`. Reproducir exactamente las tres respuestas Python archivadas reserved-total, unsupported-media y unacceptable, manteniendo estado y headers estables.
- **Reproducción previa:** la nueva prueba sobre una exportación de main falla en los tres casos con salida 1, antes de modificar el código productivo. [Evidencia antes/después](../evidence/sprint3/placement/b07-errors-20261029/README.md).
- **Cambios:** 406 por Accept insatisfecho usa texto UTF-8 y cuerpo capturado, separado del 406 de microversión; 415 conserva el media type real; el modelo distingue capacidad inválida y el servicio añade UUID/clase al detalle sin escrituras. Se conserva 1.0, SQL/migraciones, auth y el comparador.
- **Verificación local:** `make check` PASS (Glance y Placement, formato/vet/race/build); `go test -race ./internal/placement/... -count=1` PASS; regresiones dirigidas PASS con cuerpos/headers originales, invariancia de estado y variantes de UUID/clase/capacidad/media type. `git diff --check` PASS.
- **MySQL real:** `make placement-test-mysql` PASS, 8.0.46 aislado en tmpfs con puerto loopback, esquema nuevo y migrador/runtime separados; nuevos rechazos HTTP para VCPU/MEMORY_MB conservan inventario/generation por lectura SQL. Keystone fixture. El primer intento de aprovisionamiento falló antes de tests por un readiness prematuro; se conservó el log y el reintento exigió SELECT 1 autenticado por TCP. Se retiraron contenedores/datos/credenciales al terminar.
- **Límites:** no se repitió la campaña en vivo con Python/Keystone real ni Nova; las cifras 27/38 y 11 diferencias describen el código del 28 de octubre. Tres diferencias se corrigen contra capturas; quedan ocho de JSON Schema. B07 continúa en progreso. Sin cambios a Excel/S2. VM apagada durante toda esta tarea.

## 2026-10-29 — Revisión e integración del PR #5: Glance B25

- **Revisión:** [PR #5](https://github.com/PaoloCastanon/modules-devstack-optimized/pull/5), HEAD `fbb4a3d24bdd8b102e2e24c1a7b052e1563a41b5` sobre `0279987`. [Informe](../github-pr-5-review.md): auth compuesta, aislamiento de configuración, streaming, filesystem, intentos SQL, recuperación, DELETE y migraciones. Sin defectos bloqueantes detectados.
- **Verificación del HEAD aislado:** `make check` PASS (vet/race/build incluidos); `go test -coverprofile=/tmp/pr5-coverage.out ./...` PASS, 79,6% global; `git diff --check origin/main...origin/pr-5` PASS. [CI 40876937535](https://github.com/PaoloCastanon/modules-devstack-optimized/actions/runs/40876937535) success en el mismo SHA.
- **MySQL real:** 8.0.46 descartable, tmpfs y puerto loopback; tres esquemas nuevos separados. `TestB25MySQLVersionedUpgrade`, `TestB25MySQLConcurrentFences` y `TestB25MySQLFilesystemHTTP` con `-tags=integration -race -count=1`: PASS. El pipeline verifica bytes, recibo persistido, reapertura y borrado físico con migrador/runtime separados; auth usa doble. Se retiraron contenedor, datos y credenciales privadas al terminar.
- **Remoto:** [APPROVED](https://github.com/PaoloCastanon/modules-devstack-optimized/pull/5#pullrequestreview-5698455549) ligado al HEAD. Con autorización del usuario, merge `4c07cb5fcc1b023b3f607ffc9f3788f40176f41f`, confirmado merged. Se usó `gh` autenticado para escritura ante la limitación de permisos conocida del conector. main local actualizado por fast-forward; su árbol coincide exactamente con el HEAD probado.
- **Verificación posterior:** `make check` PASS sobre main integrado con el informe y este registro; `git diff --check` PASS. No se modificó código funcional durante la revisión.
- **Límites:** no se repitió Keystone real ni boot Nova. Los resultados CLI/Nova del cuerpo del PR pertenecen al autor; falta incorporar/enlazar evidencia N3–N7 y alinear documentación operativa. Siguen pendientes los casos compuestos reales de expiración/revocación, instalación independiente y fallos físicos. No se cambió Excel, backlog formal ni S2. VM `tt-openstack-lab`: `shut off`.

## 2026-10-28 — B07: comparación viva y correcciones de contrato

- **Criterio del incremento:** comparar el subconjunto 1.0 contra el Python fijado con identidad real, observar negativos y efectos, corregir diferencias funcionales y conservar las de representación sin simular un pase.
- **Runner implementado:** `scripts/placement_differential.py`, con tests de no coerción, normalización limitada, archivos privados y protección de recursos existentes. Cada escenario usa UUID/nombre nuevos; preflight exige ausencia, finally elimina únicamente esos UUID y verifica 404. No sigue redirects ni guarda tokens. El gate local usa dobles; el runner vivo es explícito y mutante.
- **Primer ensayo sobre `1e95402`:** 34 casos, 32 status y 9 comparaciones estrictas coincidentes; limpieza verificada. Diferencias observadas: enteros `3.0`, query repetida, collation de nombres, precisión FLOAT, UUID de query y errores. Se reprodujeron fallos en tests antes de corregirlos.
- **Cambios:** parser entero decimal exacto para no redondear generation; último query value en 1.0; UUID de path case-insensitive y query canónico; shape 401 de Keystone y varios errores estables. La migración **v2** cambia igualdad/uniqueness de nombre y FLOAT; v1 y su checksum permanecen intactos. La lectura CAST a CHAR conserva el número textual que devuelve MySQL al driver Python.
- **Laboratorio real:** se inició la VM existente y se verificó Placement `f4a89d3`, MySQL y Keystone; no hubo reprovisión. Se creó un proyecto/usuario temporal con credenciales de aplicación de roles limitados y expiración. Go validó tokens admin/service/reader mediante Keystone desplegado; MySQL Go corrió en contenedor descartable separado, con cuentas app/migrador de esquema propio. No se cambiaron endpoints de Nova ni la DB/config del original.
- **Resultado final:** [38 casos](../evidence/sprint3/placement/b07-live-20261028/README.md), **38/38 status**, **38/38 snapshots antes/después**, **27/38 coincidencias estrictas**. Diez diferencias permanecen en detail de validación y una en body/Content-Type de 406. El comparador devolvió **1**, no PASS; todas las limpiezas de providers quedaron verificadas. Las capturas S2 se conservaron.
- **Persistencia:** upgrade real v1→v2 conservó un canario y generation. La suite de colisión comprobó que dos nombres de v1 que colisionan en v2 dejan dirty y conservan ambas filas; serve rechaza v1 incompleto y no se repara dirty automáticamente. No se probó corte de energía ni reinicio del servidor MySQL.
- **Verificación:** `make check` PASS (Glance y Placement: formato, vet, race, build; harness y 16 tests Python unit); `make placement-test-mysql` PASS; `make placement-test-binary` PASS; `TestV1CollisionStopsUpgrade` con race PASS; `git diff --check` PASS. Las suites locales de integración siguen usando Keystone fixture, distinto de la matriz real. [Metadatos y evidencia](../evidence/sprint3/placement/b07-live-20261028/provenance.json).
- **Limpieza:** providers, application credentials, usuario/proyecto temporales, contenedor MySQL y archivos privados retirados. Se comprobó ausencia del usuario/proyecto y que `tt-openstack-lab` volvió a `shut off`.
- **Pendiente:** cerrar las 11 diferencias estrictas, ampliar microversiones según Nova y sus pruebas. B07 sigue en progreso; B11/B12 parciales. No se modificó el Excel ni se declaró integración/boot Nova.

## 2026-10-28 — B07: primer recorrido Placement Go persistente

- **Base:** PR #3 integrado por el usuario en `85bae56`. Se conservaron y reaplicaron los dos commits locales de revisión/plan sobre esa base; trabajo funcional en `feat/placement-b07`.
- **Criterio del incremento:** CRUD providers y lectura/reemplazo inventories mediante HTTP→servicio→MySQL, con roles Placement, errores y control de generation. Se escribieron primero tests de modelo/servicio y API; fallaron por ausencia de implementación (`NewProvider`, interfaces y handler `New`) antes de añadir el código.
- **Contrato contrastado:** fixtures S2 y handlers/schemas/objetos/policies upstream fijados en `f4a89d3`. POST provider 1.0 devuelve 201 vacío; PUT provider no acepta ni incrementa generation; reemplazar inventories hace CAS e incrementa una vez. Se anuncia únicamente 1.0 y tres clases observadas; [guía de alcance y límites](../docs/placement/b07-development.md).
- **Implementado:** `cmd/placement`, capas `internal/placement`, migración v1 y guardias para esquema/cuentas Placement separados. Se reutiliza `internal/auth`; `internal/config` comparte el loader con namespace Placement independiente y conserva la interfaz Glance. No hay bypass de auth desplegable; root discovery permanece público según upstream. Se corrigió la afirmación contradictoria del README sobre integración real de Glance B.
- **Prueba real MySQL:** instancia descartable 8.0.46 en contenedor con puerto solo loopback y datos tmpfs; schemas nuevos y cuentas migrador/runtime separadas por suite. Migración repetida, rechazo de cuenta DDL para HTTP, duplicados, generation obsoleta, rollback tras fallo SQL del segundo hijo, concurrencia, reapertura de pool, borrado y dirty: PASS. El primer ensayo observó código FK 1216, no solo 1452; ambos se traducen y el rollback se comprobó en un esquema nuevo.
- **Ejecutable real:** suite compila el binario y ejecuta migrate/serve; creación HTTP con adaptador Keystone, SIGTERM, reinicio del proceso y consulta del estado persistido: PASS. La prueba corrigió su reutilización de un contexto HTTP que podía expirar durante shutdown; el request posterior usa su propio plazo. Keystone es un servidor HTTP de fixture en ambas suites, no el servicio desplegado.
- **Verificación final:** `make check` PASS, incluidos formato/vet/race/build Go para Glance y Placement y los gates preexistentes; tests históricos API/404 PASS; `make placement-test-mysql` PASS; `make placement-test-binary` PASS; `make placement-build` PASS; `git diff --check` PASS. [Salidas saneadas](../evidence/sprint3/placement/b07/README.md). La comparación 404 omite solo request_id; las capturas S2 no se modificaron.
- **Límites y siguiente paso:** B07 en progreso, con B11/B12 parciales. Falta comparación en vivo Python↔Go, completar negativos/microversiones y verificar Keystone real/Nova. Sin allocations, no se acredita inventario en uso; B09 debe añadir sus invariantes antes de reservar. No se reinició el servidor MySQL ni se probó corte de energía. No se cambió el Excel ni se declaró boot ACTIVE con Go.
- **Limpieza:** se eliminó la instancia descartable con sus datos y archivos de credenciales temporales al terminar. La VM `tt-openstack-lab` permaneció `shut off`; no se inició ni reaprovisionó.

## 2026-10-28 — Revisión y aprobación PR #3: Glance B20

- **Revisión:** [PR #3](https://github.com/PaoloCastanon/modules-devstack-optimized/pull/3), HEAD `d0d1b3217b5b0982cf73d8872b6a423a6b83da3c`, sobre base `27d5b77`. [Informe](../github-pr-3-review.md) con alcance, cobertura y límites.
- **Pruebas sobre copia aislada del HEAD:** `make check` PASS, incluidos Go vet/race/build; `go test -coverprofile=... ./...` PASS, 84,8% global. Compilación con `-tags=integration -run '^$'` PASS, sin ejecutar integración. `git diff --check origin/main...origin/pr-3` PASS desde el repo.
- **Remoto:** [CI 40613658362](https://github.com/PaoloCastanon/modules-devstack-optimized/actions/runs/40613658362) success. [Review APPROVED](https://github.com/PaoloCastanon/modules-devstack-optimized/pull/3#pullrequestreview-5691720702) publicada con SHA explícito mediante `gh` después de que el conector devolviera 403 por permisos. El PR sigue abierto, sin merge.
- **Límites:** no se detectaron defectos bloqueantes. README y documentación discrepan sobre la integración real de B; el CLI no tiene cobertura de statements. No se conectó a MySQL/Keystone, no se validó Nova ni se modificó el estado formal B20/Excel. La base main local todavía no incorpora código Go de este PR.
- **VM:** `virsh -c qemu:///system domstate tt-openstack-lab` devolvió `shut off`; no se inició.

## 2026-10-28 — Plan de reimplementación Placement

- **Propuesto:** [plan B07–B15](../docs/placement/implementation-plan.md), enlazado desde PLANS.md. Prioriza contrato y providers/inventories, con persistencia B11 y auth B12 desde el primer recorrido; luego traits/classes, allocations, candidates y sustitución Nova. No se escribió código Placement ni se modificó el Excel.
- **Hallazgos de contrato:** la traza Nova incluye GET allocations por provider, ausente de la tabla propuesta; candidates requiere root_required y versión 1.36. La API oficial distingue POST provider 201 vacío antes de 1.20 y 200 con cuerpo después. El plan exige resolver estos casos y los del resource tracker antes de asumir compatibilidad.
- **Dependencia:** PR #3 aprobado pero sin merge. Las piezas Go reutilizables existen en su HEAD; main local no contiene ese código.
- **Verificación local de los documentos de revisión y plan:** `make check` PASS y `git diff --check` PASS. En main, Go sigue explícitamente PENDING por ausencia de fuentes; el pase Go de esta sesión corresponde únicamente a la copia aislada del PR #3. No se inició el laboratorio.

## 2026-10-02 — Línea base aceptada de Sprint 2

- **Hecho:** B01–B06 y B19, 7 tareas y 33 puntos. La [matriz de evidencia](../docs/sprint2/sprint2-evidence.md) y la [trazabilidad](../docs/sprint2/final-traceability.md) relacionan las capturas con los criterios.
- **Implementado:** Terraform/Ansible, instalación DevStack, smoke y captura del baseline original. Commits de referencia: `6abd728`, `30491e9`, `6344e3f`.
- **Documentado:** arquitectura/contratos y propuestas Go; informes individuales revisados en `84a8196` y `1032506`.
- **Límite:** el boot ACTIVE corresponde al OpenStack original. No hay sustitución por Go ni benchmark comparativo.
- **Último estado formal del libro derivado:** 7/26 tareas y 33/185 puntos terminados; B21 en progreso y B16 pendiente. Este registro no modifica el Excel.

## 2026-10-26 — Auditoría y adopción del harness

- **Situación encontrada:** había automatización del laboratorio, baselines y gates S2. Faltaban instrucciones de agente en el repositorio, un plan de ejecución recuperable y CI. No había `go.mod`, fuentes `.go` ni pruebas Go. Los wrappers de compatibilidad sólo capturan el original.
- **Fase real:** laboratorio/análisis S2 cerrado; transición a la implementación S3, con B07 aún sin iniciar. El paso del calendario no acredita ejecución.
- **Cambio de esta tarea:** instrucciones persistentes, fases ligadas al backlog, registro de evidencia y verificación local reproducible. El gate distingue código Go ausente de pruebas aprobadas.
- **Verificación previa:** `make docs-check` y `python3 scripts/verify_sprint2.py` aprobados; `terraform fmt -check -recursive infra/lab-vm/terraform` y `ansible-playbook --syntax-check -i localhost, infra/lab-vm/ansible/site.yml` aprobados. Son verificaciones de archivos locales.
- **Laboratorio:** `virsh -c qemu:///system domstate tt-openstack-lab` devolvió `shut off`. No se inició ni aprovisionó la VM.
- **Verificación posterior:** `make check` aprobado: sintaxis/ShellCheck, documentos, evidencia archivada, navegación del harness, 13 pruebas de regresión, formato Terraform y sintaxis Ansible. `make harness-status` confirmó 0 fuentes y 0 tests Go; `make go-check` informó `PENDING Go` y no ejecutó pruebas/build Go. `git diff --check` aprobado.
- **Siguiente tarea funcional:** B07, providers/inventories Go y sus pruebas, según [el alcance definido](../PLANS.md).

Referencia del enfoque: [guía oficial de OpenAI sobre flujos de desarrollo con Codex](https://developers.openai.com/cookbook/examples/codex/iterating-development-workflows-with-codex). Se adapta al repositorio usando su documentación existente y controles ejecutables.

## 2026-10-26 — Verificación continua

- **Cambio:** [GitHub Actions](../.github/workflows/verify.yml) ejecuta `make check` y `make harness-status` en Ubuntu 24.04 para pushes a `main`, PR y ejecución manual. Acciones fijadas por SHA; Terraform fijado en 1.15.9; Go se configura desde `go.mod` cuando exista.
- **Límites:** permisos de lectura, sin secretos del laboratorio, sin arranque de VM ni despliegue. Un workflow fallido informa el fallo; no se configuraron reglas de protección de rama.
- **Verificación local:** YAML parseado y comprobados triggers, permisos, comando y acciones fijadas por SHA. `make check` aprobado nuevamente (13 tests); `git diff --check` aprobado. La VM sigue `shut off`. La ejecución remota sólo se puede confirmar después del push.

## 2026-10-26 — Resultado remoto y compatibilidad del runner

- **Resultado observado:** [CI 40332648391](https://github.com/PaoloCastanon/modules-devstack-optimized/actions/runs/40332648391), commit `036379c`, aprobado en GitHub. Ejecutó los gates locales y omitió explícitamente setup Go porque no existe módulo; no validó servicios Go.
- **Ajuste:** el runner avisó que checkout/setup-terraform usaban Node 20. Se verificaron los `action.yml` oficiales y los tags remotos; checkout v6, setup-terraform v4 y setup-go v6 usan Node 24. Se fijaron sus SHAs para eliminar la dependencia del runtime obsoleto.
- **Verificación local:** YAML y SHAs aprobados; `make check` aprobado (13 tests) y `git diff --check` sin incidencias. El resultado de una ejecución anterior no acredita esta nueva revisión del workflow; consultar el check asociado a su commit en GitHub.

## 2026-10-26 — Revisión PR #1: pin de Nova

- **Revisión:** [PR #1](https://github.com/PaoloCastanon/modules-devstack-optimized/pull/1), HEAD `bbe1d2bdc1aff51a8ee0b6534d5830bee47df00a`. [Informe independiente](../github-pr-1-review.md) conservado como resultado de la revisión; las observaciones describen el estado inspeccionado.
- **Resultado local:** sobre una copia temporal de `main` combinada con el PR, `make check`, `bash tests/compatibility/nova_pin.sh` y `git diff --check HEAD` aprobados. No se detectaron defectos bloqueantes. DevStack fijado admite SHA en `NOVA_BRANCH`.
- **Ajuste del harness:** `make check` incorpora `nova-pin-check` cuando la prueba del PR existe. La ausencia del test se informa explícitamente; no se ejecutan los scripts de captura que contactan OpenStack.
- **Observación de operación:** un `local.conf` antiguo con la rama de Nova debe revisarse antes de reintentar manualmente el instalador: el guard de configuración rechaza diferencias. La reprovisión con marcador completo omite esa instalación. No se modificó configuración ni evidencia de la VM.
- **Límites:** el PR no tiene checks remotos registrados en su HEAD; la verificación anterior es local. No se ejecutó instalación completa ni se inició la VM. La aprobación técnica no equivale a integrar el PR ni a validar una reinstalación.
- **Verificación del ajuste:** `make check` aprobado en main y en el checkout temporal main+PR #1+PR #2; allí ejecutó correctamente el test de Nova. Las 12 pruebas MySQL también aprobaron por separado. `git diff --check` aprobado. Se publicó una revisión APPROVED para el HEAD indicado; el PR sigue abierto, sin merge.

## 2026-10-26 — Revisión PR #2: autenticación MySQL

- **Revisión:** [PR #2](https://github.com/PaoloCastanon/modules-devstack-optimized/pull/2), HEAD `8c84fe87ec30685ed0f8a2dbccf1ac467c3db708`. [Informe independiente](../github-pr-2-review.md) de sólo lectura; los resultados ejecutados por el agente principal se registran aquí.
- **Resultado local:** `make check`, las 12 pruebas `python3 -m unittest discover -s tests/unit -p 'test_mysql_auth.py' -v` y `git diff --check HEAD` aprobados sobre main+PR #2 y sobre main+ambos PR en copias temporales. No se detectaron defectos bloqueantes; se revisó la transición, la idempotencia, los fallos y la protección de credenciales.
- **Ajuste del harness:** `unit-test` descubre `tests/unit` explícitamente desde `make check`. La suite estaba fuera del descubrimiento anterior porque el directorio no es un paquete Python importable. Se informa su ausencia en las revisiones que aún no contienen el PR.
- **Límites:** no hay checks remotos en el HEAD del PR. Las pruebas usan dobles de MySQL/sudo; no se ejecutó una instalación completa con esta revisión exacta. La VM permanece apagada. No se cambió el estado formal del backlog.
- **Verificación del ajuste y aprobación:** `make check` aprobado en main y en el checkout combinado. En este último ejecutó 13 tests del harness, 12 MySQL y la regresión Nova; Terraform/Ansible y `git diff --check HEAD` también aprobados. Se publicó revisión APPROVED sobre el HEAD indicado; el PR permanece abierto, sin merge.

## 2026-10-26 — Integración de los PR #1 y #2

Los estados abiertos indicados en las revisiones anteriores describen ese momento. Ambos PR quedaron posteriormente fusionados, de forma secuencial y con autorización del usuario.

| PR | HEAD actualizado con main | CI aprobado antes del merge | Commit de merge |
|---|---|---|---|
| [#1: pin Nova](https://github.com/PaoloCastanon/modules-devstack-optimized/pull/1) | `57744fe43933f9a4fe5a6821a3e70928b9181aad` | [40336772207](https://github.com/PaoloCastanon/modules-devstack-optimized/actions/runs/40336772207) | `e4baefa7bfa7cbc310f1ad982e06db0baf552447` |
| [#2: auth MySQL](https://github.com/PaoloCastanon/modules-devstack-optimized/pull/2) | `1c6e47adb9e2e465495abb82c1db5e0be0c12023` | [40337050891](https://github.com/PaoloCastanon/modules-devstack-optimized/actions/runs/40337050891) | `652a9ca8e7d81c3a0ae13ae43c3d3676a281d64e` |

- **Procedimiento:** cada rama se actualizó con main; la segunda incorporó también el merge del primer PR. Se comprobó que los archivos funcionales coincidían con las revisiones aprobadas, que main era ancestro y que GitHub informaba `MERGEABLE`, `CLEAN`, aprobación y checks completos sin fallos. Los merges quedaron ligados al SHA comprobado y se confirmó `MERGED` antes de avanzar.
- **Resultado en main:** `make check` aprobado tras ambos merges: 13 pruebas del harness, 12 MySQL, regresión Nova, evidencia archivada, ShellCheck, formato Terraform y sintaxis Ansible. La copia local se actualizó con fast-forward.
- **Documentación:** se corrigió el alcance de versiones fijadas (DevStack y Nova) y se documentó la migración de un `local.conf` anterior al pin, sin cambiar el laboratorio.
- **Límites:** son pruebas de código y archivos; MySQL usa dobles. No se ejecutó instalación completa, no se recapturó evidencia S2 y no se modificó el backlog. `make go-check` sigue informando `PENDING Go`; B07 continúa sin iniciar.
- **VM:** `virsh -c qemu:///system domstate tt-openstack-lab` devolvió `shut off` después de la integración.

## 2026-10-28 — B25, Acción 2/8: filesystem store aislado

- **Implementado:** paquete `internal/glance/store` con inicialización explícita de
  raíz privada, scope UUID y flock; Stage streaming con límite/longitud y MD5/SHA-512;
  Publish con recibo verificado y linkat sin reemplazo; Open seekable con medición
  de integridad; Delete de claves exactas, idempotente y con fsync de directorios.
  No se conectó a cmd/glance ni se implementaron SQL, estados o recuperación global.
- **Seguridad:** aperturas openat/O_NOFOLLOW y verificación del inode; publicación
  desde descriptor mediante procfs. Linux, mismo filesystem, 0700/0600; sin nuevas
  dependencias. El README del paquete documenta garantías, requisitos y límites.
- **Validación local, Go 1.24.4:** gofmt, `go vet -p 1 ./...`, `go build -p 1 ./...`
  y `go mod verify` aprobados. `go test -p 1 -count=1 -json ./...` ejecutó 59
  resultados PASS del store (16 tests y 43 subtests) y 368 PASS de otros paquetes,
  pero falló en el primer test de internal/auth: httptest no pudo abrir un socket
  local (`operation not permitted`). El resto de esa suite no llegó a ejecutarse.
- **Race:** `go test -race -p 1 -count=1 -json ./cmd/glance ./internal/config
  ./internal/glance/...` aprobó esos mismos 427 resultados. No acredita race de
  internal/auth. Se usaron GOMAXPROCS=2, GOTOOLCHAIN=local, GOPROXY=off, GOFLAGS=-p=1
  y GOCACHE privado escribible fuera del repositorio, sin configuración global.
- **Gate completo:** `make check` avanzó por documentación/evidencia, 13 pruebas del
  harness, 12 pruebas MySQL con dobles, regresión Nova y sintaxis/formato de infra;
  falló en go-check por el socket prohibido del mismo test Keystone. No se debilitó
  el gate ni se modificaron las pruebas B20. No se informa make check como aprobado.
- **Límites:** filesystem temporal real y fallos de I/O inyectados están separados;
  ENOSPC es simulado. Reapertura entre instancias no demuestra corte de energía o
  reinicio MySQL. Sin VM, servicios, contenedores, DB real, benchmarks ni integración
  Nova. El gate integral B20 queda pendiente de ejecutarse donde httptest pueda
  abrir sockets; esta acción no cierra B25 ni autoriza la siguiente.

Revalidación del 28/10/2026: se ejecutó `GOMAXPROCS=2 GOTOOLCHAIN=local make check` en `/home/juanc/Proyectos/modules-devstack-main`, con código de salida 0. Pasaron los controles de documentación y evidencias, pruebas Python, verificación Nova, comprobaciones Terraform/Ansible, análisis estático Go, pruebas Go con race detector y compilación. El bloqueo anterior de `httptest` por permisos de sockets correspondía al entorno restringido de la primera ejecución. Esta revalidación no constituye una prueba de integración MySQL real ni de durabilidad ante pérdida de energía.

## 2026-10-28 — B25, Acción 6/8: HTTP binario autenticado inyectable

- **Implementado:** PUT/GET `/v2/images/{id}/file` mediante opción explícita de
  construcción del handler, reutilizando Keystone B20 y UploadCoordinator. PUT
  valida octet-stream, declaraciones y stream real; GET exige active/committed,
  coteja tamaño/MD5/SHA-512 antes de cabeceras y transmite con buffer fijo.
  Incluye rangos únicos 206, rechazo 400/416, 204 sin contenido y 503 para blob
  confirmado perdido/corrupto. Después de cabeceras, aborta errores sin anexar JSON.
- **Seguridad:** recuperación obligatoria también para download, capacidad
  compartida y cierre de descriptores antes de liberar propiedad. Se distingue
  fallo de fuente (400) de fallo filesystem (503) sin exponer diagnósticos.
  No cambia cmd/glance: scope SQL/store, recuperación al arrancar y deadlines de
  transferencia siguen pendientes de Acción 8; DELETE coordinado, de Acción 7.
- **Compatibilidad:** se leyeron image_data.py y wsgi.py del commit Python fijado
  a3e5f490b7a28b1161714c94acffe5308d24b567. Se confirmó el MD5 completo también
  para 206, PUT 204, media type 415 y rangos múltiples 400/incorrectos 416.
  Las diferencias y límites están en `docs/glance/b25-http.md`; no se ejecutó Python.
- **Validación local Go 1.24.4:** gofmt; pruebas focalizadas API (203 PASS: 32 tests
  y 171 subtests) y servicio (185 PASS: 33 tests y 152 subtests), sin FAIL/SKIP;
  `go vet -p 1 ./...`, `go mod verify` y `make check` con código 0. Se usaron
  GOMAXPROCS=2, GOTOOLCHAIN=local, GOFLAGS=-p=1 y GOPROXY=off. El gate completo
  incluye documentación/evidencias, 25 pruebas Python, Nova, Terraform/Ansible,
  pruebas Go con race detector y compilación. No se alteraron gates ni tests previos.
- **Límites:** httptest y dobles de SQL/identidad; un recorrido local utiliza
  coordinador y filesystem reales sobre directorios temporales. No acredita
  integración MySQL 8/002, Keystone binario, reinicio ni durabilidad física. Sin
  VM, contenedores, servicios originales, DDL real, Git de escritura o Acción 7/8.

## 2026-10-28 — B25, Acción 7/8: DELETE coordinado y recuperación

- **Implementado:** DELETE del recurso imagen mediante el mismo coordinador
  exclusivo de PUT/GET. TombstoneData bloquea imagen antes de operación, repite
  política/protected y confirma tombstone + cleanup/deleted en una transacción.
  Conserva UUID, recibos, propiedades y tags; queued sin operación no inventa
  intentos. Relee SQL antes de Store.Delete y después de CompleteDataCleanup.
  Un resultado incierto no acredita rollback ni permite unlink a ciegas.
- **Concurrencia:** DELETE y uploads se excluyen durante toda su ejecución.
  Un writer vivo produce 503 controlado sin tombstone ni I/O destructivo; esta
  opción autorizada en Acción 7 difiere de la cancelación prevista en el diseño.
  Se comprueban intentos tardíos, dos DELETE, Close ocupado y GET con fd abierto,
  que puede terminar tras unlink en Linux. No se mantiene SQL durante filesystem.
- **Recuperación:** bajo arranque exclusivo, cleanup/deleted termina en tombstone
  cleaned y cleanup/killed confirmado termina en killed. Reintenta Delete/fsync
  si los nombres ya desaparecieron; no promueve saving ni limpia active válido.
  Inventario obsoleto, estados contradictorios o dependencias fallidas conservan
  evidencia y bloquean admisión. No usa TTL, barridos ni reconstrucciones.
- **Validación local Go 1.24.4:** gofmt; MySQL 280 PASS (32 tests/248 subtests),
  servicio 257 PASS (43/214), API 227 PASS (38/189): 764 resultados, sin FAIL/SKIP.
  `go vet -p 1 ./...` y `make check` terminaron con código 0, incluido race ./...,
  compilación, documentación/evidencias, 25 Python, Nova y Terraform/Ansible.
  GOMAXPROCS=2, GOTOOLCHAIN=local, GOFLAGS=-p=1, GOPROXY=off.
- **Desarrollo:** el primer caso terminal falló porque su fixture conservaba
  campos calculados de active al simular saving/cleanup; se corrigió ese fixture,
  manteniendo el rechazo de estados contradictorios. La negativa previa se
  conserva para killed/cleanup incompatible y se añade el caso válido positivo.
- **Límites:** SQL y auth simulados; HTTP/coordinador con filesystem temporal real.
  TestMySQLCoordinatedDeletion se preparó y compiló con tag integration y run
  vacío, pero NO SE EJECUTÓ: requiere esquema nuevo independiente y autorización
  posterior. No demuestra MySQL/Keystone reales, reinicio de servicios ni pérdida
  de energía. Se leyó DELETE Python del commit fijado; no se ejecutó el original.
  000/001/002 y store intactos. Sin VM, contenedores, DDL real, infraestructura,
  Git de escritura ni Acción 8. Protocolo y divergencias: docs/glance/b25-deletion.md.

## 2026-10-28 — B25, Acción 8/8: arranque operativo y regresiones locales

- **Implementado:** cmd/glance conecta B25 mediante opt-in de configuración;
  verifica cuenta runtime y esquema sin migrar, vincula server_uuid/esquema con
  scope/marcador, adquiere flock y completa RecoverUploads antes de escuchar.
  store-scope es lectura; store-init exige root existente vacío e inicialización
  explícita. No hay reparación, esquema/root alternativo o bypass de auth.
- **Lifecycle:** plazos de metadatos, upload/download, SQL, resolución, recovery y
  shutdown separados. StopAdmission + WaitIdle conservan ownership de writers,
  readers y DELETE. Shutdown cancela conexiones al vencer la gracia y espera
  handlers/compensación antes de cerrar filesystem y SQL. Un fsync del kernel
  no interrumpible puede prolongar esa espera; no se afirma cierre limpio en ese caso.
- **Regresión encontrada durante esta acción:** limpiar deadlines al retornar
  el handler dejaba sin plazo el drenaje posterior de un body HTTP incompleto.
  El test nuevo reprodujo FAIL y pasó al conservar deadlines durante flush/drain.
  La evidencia externa conserva ambas ejecuciones, sin ocultar la primera.
- **Validación final local:** Go 1.24.4, GOMAXPROCS=2, GOTOOLCHAIN=local,
  GOFLAGS=-p=1, GOPROXY=off. 1042 PASS (162 tests principales + 880 subtests),
  0 FAIL, 0 SKIP; cobertura de sentencias 88,1 %. gofmt, go vet -p 1 ./...,
  make check (incluye race ./..., build, 25 Python, Nova, Terraform/Ansible y
  documentación/evidencias), go mod verify y build explícito terminaron en 0.
  La cobertura no acredita integración real ni durabilidad.
- **Filesystem real temporal:** reapertura de componentes, .part/.ready/.blob,
  corrupción/ausencia, tombstones y limpieza idempotente. Test de flock entre
  procesos propios temporales. Recorrido POST/PUT/GET/PATCH/DELETE usa SQL doble
  y adaptador Keystone contra un fixture HTTP: no es Keystone real.
- **Preparado, NO EJECUTADO:** TestB25MySQLVersionedUpgrade,
  TestB25MySQLConcurrentFences y TestB25MySQLFilesystemHTTP. Compilación con tag
  integration y run vacío aprobada, sin ejecutar casos. Cada suite requiere su
  nuevo esquema independiente y autorización posterior; la última exige además
  runtime separado y root propio. Auth simulada se etiqueta; modo Keystone real
  exige credencial/token preexistentes autorizados y nunca hace fallback a fake.
- **Límites:** 000/001/002, store y código MySQL previo intactos. Sin VM,
  contenedores, servicios reales, DDL/DML de laboratorio, cuentas/grants,
  benchmarks, Nova o Git de escritura. Sin prueba de proceso terminado durante
  transferencia, reinicio MySQL B25 o pérdida de energía. No se declara B25
  completo. Binding no distingue generaciones recreadas del mismo esquema en
  el mismo server_uuid; requiere gestión humana conjunta SQL/store y primario
  directo. Guía reproducible: docs/glance/b25-operations.md.

## 2026-10-29 — B26, N2: autenticación compuesta acotada

- **Implementado:** validación independiente del token entrante de servicio y
  del usuario, preservando la identidad y roles originales del usuario. El
  servicio requiere rol service, proyecto igual al validador y vigencia normal;
  su rol admin no se delega. La credencial propia sigue sin admitir admin.
- **Contrato leído:** Nova fijado 247ea5d y keystonemiddleware del laboratorio;
  Keystone api/auth.py y token/provider.py confirman allow_expired=true con la
  ventana del servidor y comprobación de revocaciones. No hay ventana local
  codificada. Sólo tras validar el servicio se solicita esa validación de usuario.
- **Seguridad:** duplicados, vacíos y listas de tokens se rechazan; identidades
  cliente no se cachean. Restricciones bind/access_rules no implementadas fallan
  cerrado. Sin cambios de observabilidad, SQL, store, endpoints o servicios.
- **Validación dirigida:** 326 resultados PASS, incluidos 72 de los tests y
  subtests nuevos; go test -p 1 -count=1 -json ./internal/auth
  ./internal/glance/api y go vet -p 1 ./... terminaron en 0 con Go local,
  GOMAXPROCS=2 y GOPROXY=off. Evidencia y códigos de salida fuera del repositorio
  en b26-n2-composite-20261028a. Ver contrato en docs/glance/composite-auth.md.
- **Regresión general:** make check terminó en 0; incluye formato, documentación
  y evidencia histórica, 25 pruebas Python, pin Nova, sintaxis Terraform/Ansible,
  vet Go, tests de todos los paquetes con race detector y compilación. go mod
  verify terminó en 0. No se ejecutó ninguna suite opt-in de infraestructura.
- **Límites:** servidores Keystone de protocolo locales y repositorios dobles;
  no valida autenticación compuesta contra Keystone real, MySQL real, cliente
  Nova contra Go ni arranque de Ubuntu. N3–N7 no ejecutados en esta acción.

### 2026-10-29 — Revisión final de las aserciones HTTP N2

- Se sustituye la comparación de referencias compartidas por una instantánea
  JSON independiente de todos los valores de Image antes de la solicitud. El
  fixture incluye nombre no nulo, tags y propiedades para ejercitar aliasing.
- PATCH autorizado exige persistir el nombre y updated_at, conservar los demás
  metadatos y reflejar el cambio tanto en la respuesta como en un GET posterior.
  DELETE exige marcar el borrado, respuesta vacía, GET 404 y exclusión del listado.
- Cinco fallos inyectados sólo en copias externas del doble de repositorio
  (puntero, mapa, slice, PATCH sin efecto y DELETE sin efecto) fueron detectados
  por las aserciones esperadas; cada ejecución negativa terminó en 1. No se
  modificó el doble original ni código productivo para esas inyecciones.
- Pruebas dirigidas y dirigidas con race terminaron en 0, con paralelismo 1 y
  GOMAXPROCS=2. Logs y códigos de la revisión en b26-n2-final-review-20261029a,
  separados de la evidencia N2 previa. Sin cambios de semántica productiva,
  infraestructura, servicios ni avance a N3. No acredita integración real.

## 2026-10-29 — Resolución local de conflictos PR #5

- **Base:** merge iniciado por el usuario entre `feat/glance-b25` (`aa6e466`)
  y main de Paolo (`0279987`), con base común `d0d1b32`. No se inició ni se
  finalizó el merge en esta revisión.
- **Resolución:** README conserva el estado de Glance B25 y Placement B07.
  El loader compartido conserva los namespaces y defaults de ambos servicios;
  Glance mantiene `store-scope`/`store-init` y su política binaria, mientras
  Placement sólo acepta `serve`/`migrate` y no consulta variables `GLANCE_GO_*`.
  Se conservaron el código, pruebas, documentación y evidencia entrantes.
- **Regresiones:** `internal/config/namespace_test.go` comprueba independencia
  en ambos sentidos y rechazo temprano de comandos de store en Placement.
  El paquete config aprobó 82 resultados de tests/subtests (9 tests principales).
  Un overlay externo sin el aislamiento binario terminó en 1, como se esperaba;
  los archivos productivos no se modificaron para esa ejecución negativa.
- **Verificación:** `GOMAXPROCS=2 GOTOOLCHAIN=local GOFLAGS=-p=1 MAKEFLAGS=-j1
  make check` terminó en 0: documentación, evidencia histórica, pruebas Python,
  pin de Nova, formato/sintaxis de infraestructura, formato/vet Go, pruebas con
  race detector y compilación de ambos servicios. Sin integración MySQL o
  Keystone real ni operaciones sobre VM, contenedores o servicios.
- **Git:** el índice y sus entradas no se modifican; los dos conflictos siguen
  marcados `UU` hasta que el usuario revise y marque sus resoluciones.
  No se ejecutaron staging, commits, push ni finalización del merge.

## 2026-10-29 — B08: catálogos y asociaciones Placement

- **Base y alcance:** PR #6 integrado en b4d8300; rama feat/placement-b08.
  Reimplementación parcial para Nova: classes/traits estándar y CUSTOM,
  asociaciones por provider y cambios seleccionados 1.1–1.7 de sus rutas.
  Default 1.0, latest 1.7; no se afirma cobertura completa de la API.
  Nova fijado usa traits 1.6 y ensure resource classes 1.7 según report.py.
- **Implementación:** catálogo persistido (21 clases y 377 traits fijados a
  os-resource-classes 1.1.0 y os-traits 3.9.0 del laboratorio); migración v3
  append-only, v1/v2 intactas. CUSTOM CRUD, protección de estándar/en uso,
  renombre de clase con cascade sin cambiar generation, traits con CAS y
  no-op sin incremento, filtros asociados. Aggregates 1.1, member_of 1.3,
  resources 1.4 y DELETE inventories 1.5 cubren dependencias seleccionadas.
  API/servicio/repositorio separados, sin importar dominios Glance.
- **Pruebas previas y correcciones:** nombre CUSTOM y protocolos inicialmente
  sin implementar; tests antes de código. MySQL real detectó error 1826 al
  recrear una FK con igual nombre en un ALTER; v3 separa las sentencias.
  Cada reintento usa DB nueva; el historial dirty sigue bloqueado. La primera
  campaña dio 75/75 status y 0/75 estricto por Vary en snapshots 404 y textos
  de errores 404/405. Ocho respuestas Python capturadas se reprodujeron en
  tests rojos y se corrigió Go sin flexibilizar errores, tipos o generation.
- **Comparación viva final:** scripts/placement_catalog_differential.py,
  opt-in --allow-mutations, tokens privados y salida nueva: **82/82** status,
  respuestas y estados estrictos frente a Python f4a89d3 con MySQL/Keystone
  reales. Regresión del runner B07 sin modificarlo: **57/57** status, respuestas
  y estados en 1.0. Los recursos originales eran frescos (GET 404 previo) y
  quedaron ausentes tras limpiar. Se conservan también intentos 1 y 2.
- **Normalización explícita:** request_id/orígenes/Vary como B07; ordenar listas
  de traits/aggregates y catálogo de clases sin deduplicar. Sólo listados
  globales excluyen CUSTOM ajenos al ensayo; todos los estándar y propios se
  comparan, con respuestas crudas completas. Las asociaciones positivas de
  aggregates/member_of se verifican en MySQL aislado; en el original sólo PUT
  [] porque HTTP no permite borrar filas globales de aggregates. Esta parte
  no se atribuye a la comparación viva.
- **MySQL y proceso:** TestB08MySQLCatalogs con race en schema descartable,
  cuentas migrate/serve distintas: upgrade v2→v3 conserva inventario/generation,
  idempotencia, estándar/CUSTOM, CAS mixto traits/inventories, rollback tras
  overflow SQL después de modificar hijos y protección por FK. Pasaron además
  make placement-test-binary y make placement-test-upgrade en schemas nuevos
  separados. SIGTERM exit 0 y proceso nuevo conservan clase CUSTOM, inventario,
  trait y asociación con generation; no se reinició el servidor MySQL.
- **Gate local:** make check terminó en 0, con documentación/evidencia histórica,
  13 tests harness y 19 tests unitarios Python, pin Nova, sintaxis/fmt de
  infraestructura, formato/vet Go, pruebas de ambos módulos con race y build.
  La VM no se inicia desde este gate. Se verifica además que los 127 digests
  de fuentes/inputs guardados corresponden a los archivos ensayados.
- **Limpieza y límites:** proyecto/usuario/credenciales temporales Keystone
  eliminados, recursos de ambos Placement ausentes, contenedores/datos/procesos
  locales y secretos descartados; tt-openstack-lab apagada. No se cambió el
  endpoint original, Excel ni capturas S2. No se acredita Nova contra Go,
  allocations, candidates, benchmarks ni cierre transversal B11/B12. B09 debe
  restar usage al filtro resources antes de habilitar claims.
- **Evidencia y continuación:** evidence/sprint3/placement/b08-close-20261029/
  conserva reportes, errores iniciales, logs, restart, hashes y limpieza.
  docs/placement/b08-development.md documenta contrato/operación; PLANS.md y
  implementation-plan.md dejan B09 allocations como siguiente incremento.

## 2026-10-29 — B09-A: allocations atómicas y uso persistido

- **Base:** B08 6587805, PR #7 abierto al comenzar; rama feat/placement-b09.
  Entrega A implementada; **B09 sigue en progreso**. Incluye GET/PUT/DELETE
  allocations por consumer, GET allocations/usages por provider, reemplazo
  atómico y capacidad restante. Default 1.0, máximo seleccionado 1.8; falta
  formato por provider 1.12 y consumer generation 1.28 que exige Nova (PUT
  observado en 1.36). No se acredita integración Nova ni candidates.
- **Contrato y prueba inicial:** se inspeccionó código fijado f4a89d3 de handlers,
  schemas, objetos, políticas y excepciones. TestB09AllocationProtocols falló
  inicialmente con 404 para consumer vacío; luego pasa con 200 y allocations {}.
  Se mantienen diferencias entre GET inválido/vacío, DELETE repetido 404,
  array legacy, duplicado de provider que conserva la última entrada y roles.
  Nueve errores Python capturados se reproducen literalmente como regresión.
- **Microversión 1.8 justificada:** la DB original no tenía registros globales
  incomplete-project/user. Una escritura legacy los crearía sin limpieza HTTP.
  Se añadió el cambio puntual 1.8 (project_id/user_id obligatorios y persistidos
  junto a las reservas) y se usaron IDs preexistentes en Placement para la
  campaña. GET aún no muestra propietario; ese cambio pertenece a 1.12.
  Las escrituras 1.0–1.7 tienen pruebas locales, no comparación viva positiva.
- **Persistencia:** migración v4 append-only, historial v1–v3 y hashes intactos;
  serve rechaza v3/dirty. Consumers y allocations propios, FK a inventories,
  cascade para renombre de clase CUSTOM y GC de consumer al liberar. La fixture
  de upgrade B08 siembra v2 mediante SQL propio antes de migrar, evitando usar
  el repositorio actual contra un schema histórico sin allocations.
- **Transacciones:** lock de consumer y unión ordenada de providers anteriores/
  nuevos; READ COMMITTED permite leer uso actual tras ganar un lock bloqueado.
  Se descuentan las reservas anteriores del consumer. Fallar en cualquier clase,
  provider o actualización SQL revierte reservas, generaciones y propietario.
  Un PUT exitoso aumenta cada provider solicitado una vez; old-only y DELETE
  no lo aumentan, como el original. La comparación de capacidad evita redondear
  used a float (caso 2^53+1) y conserva límites fraccionarios/min/max/step.
- **Corrección al plan:** el original permite reducir inventario por debajo de
  uso y conserva la reserva en sobrecapacidad. Go reproduce ese comportamiento:
  no permite empeorar el exceso mediante una nueva reserva, pero admite igualar
  o reducirlo cumpliendo constraints. Se bloquean borrados de provider/inventario
  en uso. El filtro resources ahora resta allocations persistidas.
- **MySQL real:** tres incrementos de suites en schemas nuevos; la ejecución
  final TestB09MySQLAllocations con race prueba upgrade v3→v4 preservando B08,
  rollback por overflow final de provider y consumer, propietario sin cambios
  parciales, carrera entre dos consumers, carrera de varios providers, reemplazo
  concurrente del mismo consumer, pool nuevo y renombre de clase asignada.
  También PASS TestB08MySQLCatalogs, TestB07MySQLHTTP, TestBinaryMySQLLifecycle
  y TestV1CollisionStopsUpgrade, cada una en su propio schema vacío y con cuentas
  migrate/serve diferentes. MySQL 8.0.46 descartable en tmpfs, sin DB original.
- **HTTP real:** scripts/placement_allocation_differential.py con opt-in, tokens
  privados y directorio nuevo: primera campaña **52/52**, ampliación final
  **60/60** status/respuestas/estados estrictos; regresiones B07 **57/57** y
  B08 **82/82** con sus runners intactos. Keystone/MySQL reales; sólo recursos
  propios nuevos. Normalización B07 exclusivamente (request_id, origen Location,
  tokens Vary); no se cambian errores, generation, tipos o identidades.
- **Reinicio:** SIGTERM exit 0 y nuevo proceso conserva provider, inventario,
  allocations, usage y generaciones; DELETE y limpieza comprobados. No se
  reinició el servidor MySQL ni se simuló una caída abrupta.
- **Gate local:** make check terminó en 0: formato/vet Go, pruebas de ambos
  dominios con race y build, 13 tests harness y 21 unitarios Python, documentos/
  evidencia histórica, pin Nova y sintaxis/fmt de infraestructura. No inicia VM.
- **Limpieza:** lectura SQL posterior del original confirma 0 providers, 0
  consumers y 0 allocations del ensayo, mismos registros globales project/user
  y ausencia de nuevas filas incomplete. No hubo escrituras SQL directas al
  original. Identidad/credenciales de ensayo eliminadas, procesos/contenedores/
  datos/archivos privados locales descartados y VM apagada. Sin cambios de
  endpoints Nova, Excel ni evidencia S2.
- **Evidencia y siguiente:** evidence/sprint3/placement/b09a-20261029/ conserva
  60 casos, regresiones, ensayos previos, rollback/concurrencia, hashes, restart
  y limpieza. docs/placement/b09-development.md documenta operación y B09-B:
  formatos modernos, consumer generation y dependencias antes de anunciar más
  versiones. El rollback binario a B08 requiere restaurar su DB propia previa;
  no se implementa down migration automática.

## 2026-10-29 — Cierre técnico B09 y continuación autorizada hasta B12

- El usuario pidió terminar B09 y amplió el trabajo hasta B12. Se conserva el
  subconjunto Nova; no se acredita sustitución ni boot de B13–B15.
- B09-B añade formato por provider 1.12, owner en GET, consumer generation 1.28
  distinguiendo ausente/null/zero/obsoleta y mappings 1.34. PUT 1.36 y lecturas
  modernas forman el perfil priorizado. POST batch y reshaper quedan fuera.
- CAS del consumer dentro de la misma transacción que reservas y propietario;
  un conflicto no tiene efectos. PUT vacío libera y elimina el consumer, con
  incremento de los providers anteriores; DELETE sigue sin incrementarlos.
  El provider generation opcional del payload se ignora como en upstream.
- Migración v5 append-only añade created_at a allocations. Se conservan v1–v4
  y hashes; Last-Modified proviene de reservas persistidas y Cache-Control es
  no-cache desde 1.15. Desde 1.23 los errores incluyen code; conflictos de
  consumer usan placement.concurrent_update.
- Comparación real con MySQL/Keystone: **24/24** casos modernos estrictos,
  regresiones B09-A **60/60**, B07 **57/57**, B08 **82/82**, con cleanup.
  Se conserva el valor crudo de Last-Modified, se valida HTTP date y sólo se
  normaliza el timestamp además de request_id/orígenes/Vary permitidos. Sin
  normalizar códigos, status, generación, owner o tipos.
- Cinco suites MySQL independientes PASS: B09 añade carrera de creación del
  mismo consumer, CAS stale/null, owner inmutable y liberación vacía; también
  pasan B08/B07, lifecycle binario y guard de colisión. make check PASS con
  race/vet/build de ambos módulos, documentos, Python e infraestructura.
- El máximo 1.36 se aplica a allocations; otras rutas mantienen 406 explícito
  fuera de su perfil 1.8 hasta B10. No se afirma compatibilidad íntegra 1.36.
- Evidencia en evidence/sprint3/placement/b09-close-20261029/, con hashes, raw
  HTTP, SQL, restart y limpieza. SQL original sólo lectura; metadata global no
  cambió. Identidad temporal eliminada y VM apagada antes de seguir trabajo
  local B10. No se modificaron Nova/endpoints, Excel ni capturas S2.

## 2026-10-29 — Cierre técnico B10 del perfil plano

- Base cierre B09 0e59cf3; rama feat/placement-b10. Candidates procede de una
  instantánea RR de MySQL (providers, inventory, usage, traits y aggregates).
  Evalúa capacidad remanente, min/max/step, traits requeridos/prohibidos,
  root_required, limit, pertenencia/negación de aggregate, grupos/policy,
  in_tree y same_subtree para providers planos. No hay respuestas constantes.
- Representaciones 1.10/1.12/1.17/1.27/1.29/1.34; máximo 1.36 en candidates.
  No se promete toda API ni árboles/sharing. Parent no nulo y estado compartido
  se rechazan explícitamente; dos grupos isolate no comparten un root plano.
- Perfiles Nova contrastados: providers 1.14/1.20 (root/parent, links, required,
  in_tree y POST 200), aggregates 1.19 con CAS y aumento aun en no-op, inventory
  1.26 con capacidad cero/reserved=total. Cada ruta tiene máximo explícito y
  devuelve 406 fuera del perfil. Migración v6 conserva v1–v5 y persiste tiempos
  de provider/inventory; timestamps históricos desconocidos son de migración.
- Test inicial de candidates falló con 406; ahora devuelve 200 y resultado vacío
  al no existir inventario. Suite SQL real prueba uso/traits/aggregate, CAS de
  aggregate y 40 lecturas concurrentes con relación generation/usage que detecta
  mezclas de snapshots. Las cinco suites previas también pasan en DB nuevas.
- Comparación viva MySQL/Keystone: **44/44** estricto, ocho seeds sin diferencias.
  Regresiones B09 moderno **24/24**, B09-A **60/60**, B07 **57/57**, B08 **82/82**.
  El CUSTOM resource fresco excluye providers originales ajenos a la prueba.
  Last-Modified validado y normalizado como timestamp; se ordenan conjuntos sin
  deduplicar; no se cambian códigos, generaciones, tipos ni capacity/used.
- make check PASS: vet/race/build ambos módulos, Python, documentos e infra.
  Sólo [] en aggregates original para no dejar filas globales; asociación
  positiva se verifica en MySQL propio. Cleanup HTTP y SQL sólo de lectura
  confirman ausencia de filas propias y metadatos globales intactos.
- Evidencia: evidence/sprint3/placement/b10-close-20261029/. Identidad/VM quedan
  temporalmente en uso por B11/B12; su limpieza final se registrará al terminar.
  No se cambió Nova, endpoints, Excel ni S2; B13–B15 conservan boot/sustitución.

### 2026-10-29 — B11 cierre de persistencia

- Base B10 b42c273; campaña reproducible scripts/placement_persistence_campaign.py.
  MySQL 8.0.46, volumen y tres schemas propios, cuentas DDL/DML y Keystone real.
- Nueve verificaciones PASS: privilegios mínimos, aislamiento, SIGKILL Go/MySQL,
  rollback InnoDB de owner sin confirmar, dump/restore íntegro en otra cuenta,
  sentinel intacto y cleanup. Dump de todas las filas idéntico byte a byte.
- Primer intento falló por binding Docker aleatorio tras restart; evidencia
  conservada. Segundo intento usa puerto explícito y ownership create/start.
- make check PASS (vet/race/build ambos dominios y harness). Guards Python
  rechazan output existente y ausencia de opt-in antes de iniciar Docker.
- Evidencia: evidence/sprint3/placement/b11-close-20261029/. No DB original,
  HA ni sustitución Nova; Keystone/VM continúan en uso por B12.

### 2026-10-29 — B12 cierre de Keystone/políticas y B07–B12

- Base B11 19854f7; se ejecutó el binario normal con MySQL propio y Keystone
  real. Identidad Nova obtenida de nova.conf (nova/service), sin copiar secretos
  al repo; credenciales por rol propias y validador service sin admin.
- Primer intento 99/134 conservado: 29 rechazos unscoped correctos en permiso/
  efectos pero con error/versión genéricos; tres GET inventory sin cache headers;
  tres expectativas del runner incorrectas (GET trait devuelve 204 en ambos).
  Tests rojos reproducidos antes de corregir. Sin cambios a auth/Glance compartida.
- Se conserva microversión/política en 403; inventory individual lee su timestamp
  persistido por clase en RR. SQL real prueba dos timestamps y filtro parametrizado.
- scripts/placement_auth_differential.py: **143/143** estricto: políticas 134,
  revocación 3, trust válido 3 y expirado 3. Sin efectos ante denegaciones.
  Expiración real de trust a 25s; token revocado ya se había validado en Go.
  El original no conocía ese token antes de revocarlo: no se acredita su caché.
- Tres verificaciones sólo Go: validador privilegiado 503, fallo de validación
  local 503, composite header 403. Hash de todas las filas intacto; recuperación
  tras restaurar credencial. No se alteró Keystone original ni se añadió bypass.
- make check PASS (vet/race/build ambos dominios). MySQL opt-in PASS.
  Regresión viva final **267/267**: B07 57, B08 82, B09-A 60, B09 moderno 24,
  B10 44 y seeds sin diferencias. B11 sobre binario final **9/9** con SIGKILL
  Go/MySQL, rollback no confirmado y backup/restore íntegro byte a byte.
- Evidencia: evidence/sprint3/placement/b12-close-20261029/, incluye reportes
  crudos, hashes, helpers archivados sin secretos, intentos fallidos y limpieza.
  Consulta SQL sólo lectura: 134 IDs provider/16 consumer de los ensayos ausentes;
  metadatos projects/users intactos. HTTP confirma cleanup de clases/traits.
- Se eliminaron trusts/trustee, user/project y app creds propios, se revocó el
  token de Nova emitido para el ensayo y se eliminó el directorio privado remoto.
  Contenedores/volúmenes propios ausentes; tt-openstack-lab **shut off**.
- B07–B12 completados para el subconjunto parcial del laboratorio. B13–B15
  (sustitución/boot con Nova) pendientes. Sin cambiar Nova/endpoints/Excel/S2
  ni ejecutar SQL de escritura contra DB original. No se acredita API completa,
  árboles/sharing, composite, federación, system scope, HA o policies custom.

### 2026-10-29 — Plan B13–B18

- Solicitado: plan para B13–B18, sin ejecutar todavía esos bloques. Base B12
  5c682af; rama docs/b13-b18-plan. Se consultó el libro Sprint2 sólo en lectura
  (Backlog A16:M21 y requisitos RF09/RF12/RNF02/RNF03), conservando su hash.
- Propuesto en docs/placement/b13-b18-plan.md: definir B16 primero; sustituir
  Placement de forma reversible (B13), boot ACTIVE y liberación (B14), cerrar
  runner repetible con negativos/rollback (B15), medir latencia/throughput (B17)
  y CPU/memoria (B18) con las mismas campañas. Se conservan dependencias formales,
  puntos, responsables y sprints del libro. Valores de carga/repeticiones son
  propuesta para piloto, no resultados experimentales ni calendario aprobado.
- La primera comparación mantiene Glance original como control. B20/B25/B26
  conservan implementación/aceptación Glance y boot con imágenes del servicio Go;
  no se declaran cerrados por integrar Placement ni por preparar este plan.
- Se enlazó desde PLANS.md y el plan Placement; se corrigió el estado corriente
  del documento de trabajo B09–B12 para reflejar sus cierres ya registrados.
- Validación: make check PASS (harness/infra y formato/vet/race/build Go de ambos
  dominios), git diff --check PASS, hash del Excel intacto. Sin código funcional,
  despliegue, benchmarks, modificación S2 o cambios al estado formal del libro.
  tt-openstack-lab permanece shut off; no se inició durante esta planificación.

### 2026-10-29 — B16 protocolo e instrumentos; inicio B13

- El usuario autorizó comenzar el plan B13–B18. Base integrada B09–B12
  c2cadc7 en feat/placement-b08; main 45b91ba aún no contiene esos cierres.
  Se inició feat/placement-b16 desde la base integrada y se conservó el plan.
- B16: protocolo v1 con matriz/semilla, cargas, warm-up, ventanas, repeticiones,
  métricas/errores y análisis por repetición antes de medir. Instrumentos HTTP
  readonly con snapshot completo y /proc con CPU/PSS/RSS y PID identity.
- TDD rojo al faltar módulo, seis tests finales PASS con HTTP y /proc reales;
  status 200 con estado erróneo no cuenta como éxito y no se guardan tokens.
  make check PASS (harness, infra, vet/race/build de ambos dominios).
- Dos pilotos Python con Keystone real: cada uno cuatro concurrencias
  1/4/8/16, warm-up 1s/carga 4s, cero errores. Segundo piloto usa instrumento
  final con UTC y denominador hasta deadline/respuesta final, sin teardown.
  Muestreo de master+dos workers uWSGI, PSS disponible. Cifras sólo de piloto,
  no benchmark B17/B18 ni comparación con Go. Fuentes y muestras conservadas
  en evidence/sprint4/benchmarks/b16-20261029/.
- VM encendida en uso por B13. Preflight readonly: un provider plano, sin
  sharing/allocations y sin instancias; compute up. Placement tiene unidad
  uWSGI independiente y ruta Apache propia. Identidad temporal por roles
  service/admin/reader con archivos privados; sigue en uso por B13.
- B16 cierra protocolo para Placement. Claim/mezcla y campañas B17/B18 se
  implementan/pilotan después de integrar Nova; no se acreditan por estos pilotos.
  Sin cambios al Excel, evidencia S2 o código funcional de Placement/Glance.

### 2026-10-29 — B13 sustitución reversible del perfil plano

- Base B16 d7c89e6; scripts/integration configura user/project tt-placement y
  app creds por rol, preflight readonly, runtime nativo propio y cambio Apache
  con backup privado/hash, reemplazo atómico y timer de rollback a 600s.
- MySQL 8.0.46 dentro de la VM: datadir/socket/puerto 13316, UUID/schema/
  migrador/app propios. Perfil AppArmor independiente; original sin cambios.
  Fallos iniciales de confinamiento y tmpdir conservados y limpiados.
- Readiness/probe mysql CLI con defaults-extra-file heredó ~/.my.cnf y leyó
  original mediante SELECT. Detectado al coincidir UUID; corregido a defaults-file
  y SOCKET. Bootstrap PyMySQL y runtime Go usaron siempre socket/DSN explícitos
  propios. Ver prueba válida de UUIDs/puerto distintos. Sin SQL original de escritura.
- Python uWSGI detenido durante sustitución. Nova compute/scheduler reiniciados
  con configuración intacta. Nova publicó provider real, tres inventarios y
  traits en Go; misma identidad/root, constraints, traits, aggregates y reservas
  vacías. Generaciones independientes 3/2 conservadas, sin declararlas iguales.
- Cliente Nova desplegado y RequestSpec/ResourceRequest reales consultaron
  candidates 1.36 con nova.conf. Probe es biblioteca Nova, no boot del scheduler.
  Logs Go añaden IDs de identidad validada; prueba de spoofing y tests de
  discovery/401 corrigen acceso sin identidad sin guardar token/cabeceras falsas.
- cycle-01 falla por construcción del objeto del helper y restaura original;
  probe corregido probado primero en Python. cycle-02 PASS completo, con
  publicación Nova, candidates y original restaurado/hash Apache exacto.
- make check PASS (vet/race/build ambos dominios), API y guards PASS. Evidencia
  evidence/sprint4/placement/b13-20261029/. Sin editar Nova, Excel o S2 ni afirmar
  API completa. B14 continúa boot; B15 suite/inyectores y benchmarks pendientes.
  Original restaurado; VM/runtime/identidad permanecen temporalmente en uso B14.

### 2026-10-29 — B14 boot real con Placement Go y cierre del laboratorio

- Base B13 55b0019, binario 83433d9b… identificado en evidencia B13.
  Helper SDK real con opt-in, output nuevo e IDs explícitos; no guarda adminPass
  ni objetos Server enteros. Original uWSGI detenido, sólo Go en /placement.
- Server 1af1c553-3927-431a-be9b-d7a1f3010006 BUILD→ACTIVE a 00:39:02Z
  (21:39:02 del 29 en Santiago), imagen CirrOS original, m1.tiny y red shared.
  Nova hizo candidates/claim; logs Go registran identidad Nova validada.
- Allocations verificadas en consumer real: VCPU1/MEMORY_MB512/DISK_GB1,
  owner de la instancia admin (distinto de identidad Nova), consumer gen1 y
  provider gen3. Usages cero→reserva→cero. DELETE Nova y liberación PASS.
- Original restaurado/hash Apache exacto. Probe Nova real volvió a obtener
  candidates en Python. Preflight final conserva provider/inventarios/traits/
  aggregates/allocations y UUID MySQL original. Listado all-projects sin VMs,
  imagen original ACTIVE y cinco unidades originales activas antes de shutdown.
- Runtime Go/MySQL propio detenido; datadir/schema/cuentas/DSN/archivos privados
  y perfil AppArmor independiente eliminados. Perfil original conserva SHA.
  User/project/app creds temporales de Keystone y root privado eliminados.
  Sin unidades/perfiles tt-placement activos; tt-openstack-lab shut off.
- make check PASS (guards, harness/infra, vet/race/build de ambos dominios).
  Evidencia evidence/sprint4/placement/b14-20261029/. Scripts/headers no guardan
  credenciales. Sin cambios Nova/Excel/S2 ni SQL Placement original de escritura.
- B16, B13 y B14 cerrados técnicamente para el alcance; B15 sigue pendiente
  (dos suites integrales y negativos), B17/B18 no medidos, B26 fuera de este boot.

### 2026-10-29 — B15 suite integral, dos corridas finales consecutivas

- Base B14 1a2efa7; scripts/integration/placement_suite.py y make
  placement-integration ejecutan dentro de la VM el flujo completo opt-in con
  IDs explícitos, output nuevo, user/project/app creds, MySQL/Go propios y ledger
  privado. Snapshot/hash original y MySQL propio vía DSN app explícito.
- Once casos obligatorios: preflight/aislamiento, publicación tracker, auth sin
  efectos, persistencia tras restart Go, CAS, claims contra capacidad, NoValidHost
  real por disco insuficiente, boot normal, dos boots concurrentes, excepción
  controlada con rollback por timer y restauración del original/cliente Nova.
- En este lab, capacidad VCPU16: cinco claims concurrentes de4 producen cuatro
  204 y un409, sin sobreasignación. CAS de un consumer: 204/409 y una reserva.
  Cada corrida completa crea tres VMs ACTIVE y una ERROR/NoValidHost, luego
  comprueba borrado/liberación por Nova y usos devueltos al baseline.
- Guards rojos/verdes: PASS incompleto prohibido, ownership exacto, output/opt-in,
  retener backend/credenciales/ledger si falla borrar VM, lease exclusiva entre
  coordinadores y restore idempotente con timer --collect ya desaparecido.
- Prototipos01/02 pasaron;03 añadió retención;04 exclusión.05 falló OSError al
  preparar runtime y limpió todo, sin contarse como PASS. El chequeo bind podía
  rechazar TIME_WAIT; prueba TCP real reproduce el problema y valida SO_REUSEADDR
  sin aceptar listeners activos/SO_REUSEPORT. No se conserva errno del fallo
  histórico, sí su ubicación y estado; nuevos OSError registran errno. MySQL
  --version ahora usa --no-defaults para no leer ~/.my.cnf del operador.
- Versión final, sin cambiar fuente/binario entre corridas06/07: **22/22** casos,
  ocho flags cleanup true por corrida; MySQL UUID/schema y recursos nuevos.
  Fuente/hashes y prototipos (incluido05 fallido) conservados por separado en
  evidence/sprint4/placement/b15-20261029/. No se debilitaron casos/error codes.
- make check PASS: guards/tests, harness/infra, vet/race/build ambos dominios.
  Auditoría real final: cero instancias/dominios, flavors/user/projects propios,
  work roots, unidades o perfiles AppArmor propios; imagen original ACTIVE, cinco
  servicios originales activos y hash Apache/perfil original intactos. Lock de
  coordinación vacío persiste por diseño; tt-openstack-lab **shut off**.
- B15 cierra integración del subconjunto; B17/B18 no medidos, B26 fuera.
  No se cambió Excel/S2/Nova ni se escribió SQL en Placement original.

### 2026-10-30 — B17 instrumentación completa y pilotos; campaña oficial pendiente

- Base integrada2046a95 (B13/B14/B15/B16 mergeados). Generador host cerrado por
  worker, seis cargas B16, validación exacta de generaciones/owner/estado y
  candidates bajo claims concurrentes. Deadline por request, drain y limpieza de
  ciclos parciales fuera de la ventana; raw JSONL gzip sin credenciales.
- Coordinador SSH/SDK con lock B15, semilla/orden congelados antes de mutar,
  16 providers propios/clase CUSTOM por par, ruta Apache común, MySQL Go propio
  y owner SQL ya presente en Python. Sólo SELECT en la DB original. Tokens
  renovados por par; app creds propias24h para cubrir una campaña de más de10h.
- Prototipos01/02 PUT415 y03 PUT400 corrigieron Content-Type y creación de clase
  1.7.04 pasó dos lecturas pero falló cleanup DELETE provider1.36; se restauró
  original y retuvieron medios de reparación. DELETE1.0 con propiedad verificada
  retiró backend/identidad/privados. No se reclasifican esos fallos como PASS.
- Pilotos05 y06: cada uno24/24 HTTP/estado PASS (seis cargas, c1/16, warm1s/load4s).
  Hashes finales de generador/coordinador/recolector coinciden con06. Original,
  owners SQL, hash Apache y perfil MySQL intactos; recursos privados retirados.
- make check PASS (52 tests de unidad, harness/infra y vet/race/build ambos
  dominios). Cuatro tests B17 incluyen HTTP real, estado falso tras204, no adopción
  de recursos, generaciones/tipos y matriz de120 pares. Evidencia nueva Sprint5.
- B17 no cierra por estos pilotos. Se mantiene protocolo B16 de240 ventanas,
  warm30s/load120s y cinco pares por celda. Excel/S2 y Glance original intactos.

### 2026-10-30 — B18 instrumentación y cierre autónomo; mediciones oficiales pendientes

- Recolector por cgroup completo con PID/start_ticks, CPU/PSS/RSS a1Hz, reposo
  inicial5s, membresía antes/durante/después y ventanas UTC alineadas con reloj
  monotónico cliente. Grupos separados MySQL, Keystone, Apache, Nova y Glance;
  VM CPU/steal/memoria/load y costo del recolector/generador registrados.
- Análisis usa CPU y completadas correctas del mismo intervalo interior; PSS
  primario y máximo observado, RSS secundario. Bootstrap por repetición/par,
  n=5 y10000 remuestras; p99 marca soporte insuficiente. Heap/GC Go no expuesto.
- Pilotos05/06/07:24/24 ventanas CPU/PSS válidas cada uno, además de HTTP/estado.
  07 añade publicación del compute de control y probes Nova reales antes de
  cargar; identidad/capacidad/traits/aggregates equivalentes, gen original3/Go2
  preservadas. Los cuatro instrumentos finales coinciden por SHA con07.
- make check PASS, incluidos cuatro tests B18: CPU excluye warm-up/otros servicios,
  PID reciclado/PSS ausente rechazados, bootstrap reproducible, publicación
  parcial bloqueada y apagado sin cinco servicios originales verificados prohibido.
- Supervisor de sesión larga en worktree aislado: requiere HEAD limpio/congelado
  y opt-ins campaña/publicación/apagado. Guarda progreso fuera de Git. Al terminar
  sólo publica cierre si las240 ventanas oficiales y ambos análisis pasan,
  original/limpieza quedan verificados, VM apagada, make check y CI aprobados.
  Commits de resultados B17/B18 separados; PR ready, sin merge automático.
- B17/B18 siguen pendientes de la campaña oficial de más de10h. No se reduce
  B16 para cerrar antes. Sin cambios a producción Go, Nova, Excel/S2 o Glance.

### 2026-10-30 — B17 guard de confianza SSH en worktrees

- Arranque oficial01 sin ventanas: known_hosts ignorado no existía en el nuevo worktree; SSH falló antes de ejecutar el coordinador. Intento y prueba del original activo/directorios remotos ausentes conservados.
- Guard de archivos SSH/protocolo legibles antes de crear outputs/estado o lanzar SSH; test reproduce ausencia de la caché y exige cero efectos. Ruta absoluta original de confianza para el nuevo intento. No se deshabilita verificación de host.
- Instrumentos HTTP, coordinador VM y recolector iguales al piloto07; sólo cambia preflight del driver. Campaña240 pendiente, sin reclasificar este fallo como éxito.


### 2026-10-30 — Cola de campañas Glance y sustitución conjunta

- Solicitud explícita: al finalizar Placement, crear campañas para Glance Go y
  para ambos servicios Go juntos. Preparación en rama/worktree independientes
  feat/glance-joint-benchmark-campaigns, base fa116a8; no se cambió el worktree
  congelado ni el protocolo/supervisor de la campaña Placement en ejecución.
- Propuesto: glance-v1 compara Python/Go con Placement original, nueve cargas,
  cuatro concurrencias y cinco pares (360 ventanas). Diseño conjunto 2x2:
  cuatro variantes, dos perfiles de caché, lotes 1/2/4 y cinco repeticiones
  (120 bloques y 280 boots medidos). Preflight y pilotos separados, sin aceptación
  anticipada ni atribución de rendimiento sólo al lenguaje.
- Preparado: protocolos JSON, runbook, condiciones de continuación y vínculo en
  PLANS.md. Estado durable fuera de Git en reports/campaigns/followup-status.json
  del workspace superior. Seguimiento horario en este chat creado mediante
  automation_update; ID continuar-con-campa-as-glance-y-conjunta. Espera el cierre
  aceptado/restaurado de Placement y evita campañas o procesos duplicados.
- Verificación ligera ejecutada: JSON, unicidad de escenarios, productos de las
  matrices/pilotos y enlaces relativos. PASS: Glance 360/36 ventanas; conjunto
  120/16 bloques y 280/24 boots oficiales/piloto. Sin acceso a la VM.
- make check, compilación, instrumentos específicos, pilotos, mediciones y commit:
  PENDIENTES hasta que termine la campaña actual, para evitar interferencia.
  No se declaró B17/B18 Glance/conjunto terminado ni se modificó Excel/Sprint2.
  Localizar código y evidencia real de B25/B26 antes de usar Glance como baseline
  aceptado: los documentos de este checkout todavía contienen límites históricos.


### 2026-10-31 — Recuperación de la campaña Placement predecesora

- Intento 02: detenido con 111/240 ventanas; la siguiente ventana presentó
  38706 respuestas válidas, un fallo de transporte y 15 HTTP 503. Journal de la
  VM correlaciona apt-daily-upgrade con reinicios Apache/Keystone a 06:19:55 UTC.
  No se aceptan ni se combinan esos resultados con el entorno actualizado.
- Limpieza remota tardía comprobada: originales activos/inalterados, runtime,
  identidad y estado privado retirados. Evidencia pública copiada fuera del
  worktree congelado; apagado normal verificado. Reinicio para un nuevo ensayo.
- Control operacional temporal: apt-daily y apt-daily-upgrade, servicios y timers,
  masked-runtime y timers inactivos; habilitación persistente sin cambios.
  Guardado estado previo/efectivo y versiones de paquetes fuera de Git.
- Intento 03 iniciado con supervisor durable y 240 ventanas nuevas; HEAD fa116a8,
  protocolo y fuentes sin modificar. Seguimiento del chat actualizado al servicio
  tt-placement-b17-b18-20261031-03.service. Campañas Glance/conjunta en espera.
- No cambios en el código Go, Excel, Sprint2 ni aceptación B17/B18. No se ejecutó
  make check o compilación durante las ventanas. Controles operacionales y
  preflight existentes no sustituyen la aceptación de la matriz completa.


### 2026-10-31 — Limpieza autorizada y perfiles de campaña compactos

- Usuario pidió liberar disco/cachés y campañas de menor duración. No había
  respaldo completo confirmado para destruir el laboratorio; se preservó la VM.
- Cachés Chromium HTTP/código, pnpm metadata y pip HTTP: 1,56 GiB netos liberados.
  En Ubuntu: syslog 28,6 GiB, acceso Horizon 2,1 GiB y journal 4 GiB. Se preservaron
  colas recientes comprimidas, journals de incidentes y toda evidencia de pruebas;
  se redujeron logs, se aspiró journal a 256 MiB y se limpió apt.
- Domain XML respaldado; discard=unmap sólo en vda. Dos ejecuciones fstrim del
  espacio libre reclamaron bloques. qcow2 de 44,79 a ~6,76 GiB; host ~39 GiB libres.
  Capacidad 55 GiB y DevStack conservados; controles originales activos verificados
  durante mantenimiento y apagado normal final comprobado. No se midió rendimiento.
- Perfiles v2 preparados: Placement 72 ventanas, Glance 108, conjunta 24 bloques
  y 36 boots. Verificados productos de matrices. No ejecutados ni aceptados.
  Supervisor/publicación, estadísticas con n=3, límites de logs/espacio y IaC
  requieren adaptación y checks antes de pilotos/campañas. Datos v1 no se mezclan.
- Revisión 2 documenta alcance preliminar y duración menor; no presenta un recorte
  como cumplimiento de las 240 ventanas históricas. VM apagada al terminar.


### 2026-10-31 — B16 v2 e instrumentación compacta B17/B18

- El usuario confirmó continuar con perfiles compactos. Matriz Placement de 72
  ventanas, tres pares y 10/30s; conteos/status/publicación derivados del protocolo.
  Análisis de n=3 publica mediana/rango sin intervalo bootstrap de confianza.
  Aceptación comprueba los IDs completos y únicos de la matriz aprobada.
- Guardia anfitriona: 20 GiB antes de iniciar, vigilancia a 2s durante carga y
  aborto controlado bajo 10 GiB. Los workers consultan sólo un Event en memoria;
  finalización conserva las muestras y limpia recursos fuera de la medición.
- Helper VM: máscara apt temporal, límites journald temporales y retención
  acotada de syslog/Horizon entre ventanas. Conserva colas privadas recientes,
  restaura timer/enablement/configuración y fstrim antes de apagar. No interrumpe
  un job apt activo ni elimina máscaras ajenas.
- EOF/RPC corregido: recoge cleanup tardío tras cierre y copia evidencia; una
  recuperación de limpieza no convierte una campaña inválida en aceptada.
- Terraform driver.discard=unmap validado contra esquema libvirt 0.9.9 y terraform
  validate PASS; coincide con la configuración operativa de vda, sin apply.
- Validación: make check PASS (harness, evidencia S2, Python, infraestructura,
  Go vet/race/build de ambos dominios). Ocho pruebas compactas específicas PASS,
  incluida restauración al fallar controles, falta de espacio, retención acotada,
  limpieza tardía y publicación sin 240 ni confianza inferencial con n=3.
  Suite Python final repetida tras los últimos ajustes PASS.
- No campaña oficial nueva aún; próximo paso piloto real de 24 ventanas con
  original/Go y muestras CPU/PSS. No se declaró cierre B17/B18 o Glance.


### 2026-10-31 — Piloto compacto v2 Placement aceptado para instrumentación

- Fuente 789648e, binario Go idéntico al previamente validado (20b7b92d...f63db).
  Campaña independiente: 24/24 HTTP válidas y 24/24 CPU/PSS válidas, sin errores
  del analizador. Seis cargas, concurrencias 1/16 y un par por celda; piloto 1/4s.
- Todos los archivos y muestras comprimidas conservados en
  evidence/sprint5/benchmarks/compact-v2-pilot-20261031. Guardia de disco no abortó.
  Revisión 2 oficial todavía no medida: 72 ventanas, tres pares y 10/30s.
- Cleanup true, controls_restored true; originales/owners SQL y rutas sin cambios,
  backend/identidad/estado privado retirados. VM apagada normalmente comprobada.
  El resumen piloto mantiene b17_accepted/b18_accepted false, como corresponde.
- CI del PR #17 PASS para instrumentación. Siguiente hito: congelar esta revisión
  y ejecutar la matriz oficial compacta en supervisor durable, sin builds ni
  cambios de fuentes mientras mida. Glance/conjunto continúan pendientes.


### 2026-10-31 — Corrección de apagado tardío y reanudación sin medir otra vez

- La campaña compacta terminó con 72/72 ventanas válidas, cleanup y controles
  restaurados true. El apagado normal superó el límite de 120s del supervisor;
  libvirt confirmó después shut off (shutdown). No se forzó la VM ni se perdió
  la matriz. El analizador confirmó B17 y B18 aceptables, 72/72, sin errores.
- El supervisor admite VM ya apagada y espera hasta 600s el apagado normal.
  Nueva reanudación de postproceso no lanza un generador: exige medición completa,
  manifest igual al original, protocolo inalterado y fuentes de la revisión
  medida verificadas contra Git. Guarda la revisión de postproceso por separado.
- make check PASS; dos regresiones nuevas para apagado ya completado y rechazo
  de duplicados/revisión incorrecta al reanudar. Diez pruebas compactas PASS.
  Fuente medida 345ca56 conservada; datos y aceptación publicados en pasos aparte.


### 2026-10-31 — Diagnóstico de locale del apagado

- El primer diagnóstico de apagado tardío se precisó al reanudar: systemd de
  usuario heredó locale español y virsh devolvía `apagado`, mientras el script
  comparaba `shut off`. La VM estaba apagada; no falló la medición ni cleanup.
- Virsh de la comprobación/solicitud de apagado ahora usa LC_ALL=C/LANG=C.
  Regresión simula salida española sin ese entorno y exige no volver a apagar
  un dominio ya detenido. make check PASS y diez pruebas compactas PASS.
- Se conserva la historia de alertas al reanudar y se limpian error/reason actuales
  después de validar los datos. Próximo paso: reanudar sólo análisis/publicación.

### 2026-10-31 — B17 medición oficial Placement completada

- 72/72 ventanas válidas, protocolo placement-compact-v2; 3 pares por celda, concurrencias [1, 16], warm-up 10s/carga 30s.
- Requests crudas, status/errores/generaciones, goodput y percentiles conservados. Semilla equivalente, owner SQL reutilizado, limpieza y original restaurado.
- make check PASS; VM shut off. Informe docs/benchmarks/b17-results.md. Excel y S2 intactos; no inferencia causal exclusiva del lenguaje.

### 2026-10-31 — B18 consumo CPU/PSS correlacionado completado

- Las mismas 72 ventanas de B17: cgroups completos, PID/start_ticks estables, ticks CPU y PSS a1Hz; reposo, carga, máximo observado y RSS secundario.
- CPU/op usa requests completadas en intervalo interior común; relojes y overhead del recolector registrados. MySQL/Keystone/Apache/Nova/Glance/generador separados.
- Mediana y rango observado; tres repeticiones, sin afirmar precisión inferencial. Datos crudos y resumen reproducible; auth/cache/pools/DB declarados.
- make check PASS. Restauración, cleanup y VM shut off comprobados; B17/B18 cerrados técnicamente para este subconjunto de Placement.


### 2026-10-31 — Instrumentación Glance v2 y preparación de preflight real

- Base 13bb013: Placement v2 aceptado 72/72, CI PASS en el mismo SHA, originales
  restaurados y VM apagada. PR5 fbb4a3d integrado en 4c07cb5: se preserva autoría
  Glance y se distingue lo informado por su autor de la verificación nueva.
- Cliente HTTP con payload determinista, streaming acotado, hashes completos,
  keepalive por worker y TTFB real para descargas. Ventanas de ciclos completos,
  drenaje incluido en el denominador y muestras/intentos comprimidos sin tokens.
  UUID propios, estado/tipos estrictos y cleanup sin adoptar imágenes ajenas.
- Backend de laboratorio extiende helper existente con dominio glance: MySQL
  propio, migrador/runtime separados, store ligado a UUID SQL e inicialización
  explícita. Sin cambios en los dominios Go ni bases originales. Proxy /image
  reversible y lease antes de sustituir; Placement original permanece control.
- Preflight preparado: original y Go, archivos raw 1/16/64 MiB, metadata y hashes,
  paso por Apache, eliminación de bytes propios y restauración. Aún no ejecutado;
  no se presentan sockets locales como Keystone/MySQL/Glance reales.
- make check PASS, seis regresiones Glance de instrumentos PASS. Se rechaza
  dominio/puerto no admitido, 200 binario corrupto y metadatos de propietario/tipo
  incorrectos. VM apagada durante desarrollo; siguiente paso preflight opt-in.


### 2026-10-31 — Glance: preflight funcional real aprobado

- Fuente 85a78de, sin cambios productivos Go: 42/42 solicitudes correctas con
  Keystone real, MySQL/store propios, metadatos y bytes íntegros; Python 1 MiB,
  Go directo 1/16/64 MiB y Go público Apache 1 MiB. Payloads y credenciales fuera
  de Git. [Evidencia](../evidence/sprint5/benchmarks/glance-v2-functional-preflight-20261031/README.md).
- Placement original se mantuvo como control; catálogo original y hashes Apache
  iguales antes/después. Runtime/identidad/bytes propios retirados, controles
  restaurados, todos los originales activos y VM apagada normalmente.
- make check PASS para instrumentos; se repite para esta entrega de evidencia.
  No es piloto de carga ni aceptación Glance108 o joint24/36. Coordinador/RPC,
  análisis CPU/PSS y supervisor durable siguen pendientes antes de medir.


### 2026-10-31 — B16/B23 Glance: coordinador y análisis compacto

- Usuario limitó esta ejecución a automatización, piloto36 y oficial108 de Glance;
  conjunta postergada. Seguimiento actualizado y Placement Python permanece fijo.
- RPC con lock, seeds propios por namespace/owner, equivalencia entre implementaciones,
  roundtrip por worker fuera de ventanas, restauración y apagado por sesión.
- Collector Glance g-api/Go separado de Placement; análisis reconstruye ciclos,
  muestras/bytes, percentiles, TTFB y CPU/PSS con PID/start_ticks e intervalo común.
- Supervisor durable piloto->oficial, revisión/protocolo/binario congelados,
  guardas disco host/VM y CI exacto antes de publicar PR ready. No merge ni conjunta.
- Diez pruebas específicas PASS, incluyendo matriz108/36 reproducible, rechazo
  de ownership ajeno y analizador que rechaza muestras incompletas/no acepta piloto.
  Datos de esos tests son sintéticos y no acreditan mediciones reales.
- make check PASS para esta revisión antes de congelar. Piloto/campaña aún
  pendientes; VM apagada durante implementación. No se anuncian resultados nuevos.

- Guarda adicional antes de cada encendido:20GiB libres; make check PASS.
- Preparación tras boot espera todos los servicios originales activos y apt inactivo; make check PASS.


### 2026-10-31 — Glance v2: hallazgo real de estrés, sin aceptación oficial

- Piloto36/36 válido, incluido CPU/PSS. Oficial se detuvo en77:76 válidas y
  descarga Python1MiB HTTP200/0bytes inválida, payload_mismatch. Se preservó
  toda la evidencia, sin sustituir muestra, reintento oculto ni aceptación.
- Journal nativo confirma errno24 Too many open files al abrir el archivo de
  caché, con LimitNOFILE2048. PID/start_ticks estables y guardas disco sin aborto.
  CacheFilter crea Notifier/get_transport por cached request; acumulación de
  transports/sockets es hipótesis a contrastar mediante calibración dedicada.
- Diagnóstico de sólo lectura, originales/restauración verificados y VM apagada.
  [Evidencia](../evidence/sprint5/benchmarks/glance-compact-v2-20261031-01/README.md).
- Se prepara v3 (misma matriz108) para comparar API/catálogo/bytes sin bus de
  notificaciones, fuera del alcance Go. Driver oficial noop en copia privada,
  OS_GLANCE_CONFIG_DIR mediante dropin sólo /run; originales/caché/límites sin
  modificación. Todo se retira/restaura por sesión. Debe calibrarse primero;
  no se presentan v3 ni el control propuesto como solución ya validada.


### 2026-10-31 — Glance: calibración FD confirma causa y control v3

- Ensayo nativo fuente07b03de:1500 descargas1MiB messagingv2, +1500FD;
  6000 descargas1MiB noop, +4FD. Todas con bytes/hash completos. Conteos de
  sockets por worker respaldan acumulación de transportes por cached request.
- Control temporal OS_GLANCE_CONFIG_DIR en /run restaurado y hashes originales
  iguales; caché y límite2048 intactos. Recursos propios retirados y VM apagada.
  [Evidencia](../evidence/sprint5/benchmarks/glance-v3-fd-calibration-20261031/README.md).
- No es benchmark de rendimiento (cliente VM-local). V3 mide API/bytes sin bus
  RabbitMQ; piloto36 y oficial108 todavía pendientes. V2 fallido preservado.
- Se añade guarda de crecimiento FD máximo256 por ventana, sin aceptar datos
  sin contador. Prueba positiva/negativa y make check para nueva congelación.


### glance-compact-v3: piloto y campaña oficial completados

- Piloto36/36 y oficial108/108, bytes/estado/CPU/PSS válidos; Placement Python fijo.
- Restauración, limpieza, controles y apagado normal verificados tras ambas sesiones.
- Datos independientes, n=3 mediana/rango; no atribución exclusiva al lenguaje.
- Resultado reproducible en docs/benchmarks/glance-results.md y evidencia de esta campaña.
- Campaña conjunta postergada por el usuario; no se ejecutó.


### 2026-11-12 — B26: suite automática Glance Go, captura Nova→Glance y proyección RBD

- Nueva suite `scripts/integration/glance_suite.py` (`make glance-integration`).
  Tiene 10 casos fail-fast: contrato Go, sustitución reversible de `/image`, imagen
  CirrOS sólo en Go, boot Nova y flujo capturado. La limpieza obligatoria impide
  un pase parcial. [Uso y límites](../docs/glance/b26-integration-suite.md).
- Captura `scripts/integration/glance_flow.py`: journald Go (request_id) ↔
  keystoneauth Nova (request id, presencia de X-Service-Token), con Apache como
  respaldo; saneada. [Método](../docs/glance/nova-glance-flow.md).
- Verificado sólo offline: `python3 -m unittest discover -s tests/unit` con 89 OK
  (13 nuevos), py_compile y `--help`. **Sin corrida en laboratorio**: B26 y el
  boot Nova desde Glance Go siguen sin evidencia en el repositorio.
- Proyección de diseño y esfuerzo (≈24 SP) de un store Ceph RBD; no implementado.
  Hallazgo: `Filesystem.Open` relee y hashea el objeto antes del primer byte, lo
  que es coherente con el TTFB y la CPU de las descargas de 16/64 MiB en v3.
  [Proyección](../docs/glance/ceph-rbd-projection.md).
- Primera corrida real (VM local, 6 GiB): pasan 7/10 casos. Nova falla al crear
  el servidor (HTTP 500, `MissingAuthPlugin`): keystoneauth necesita el
  descubrimiento de versiones de `/image/`, y Glance Go no lo tenía (respondía
  401). Se implementa en Go `GET|HEAD /` (300) y `/versions` (200) públicos,
  idénticos a la captura original de Sprint 2; los tests Go lo comparan con
  `evidence/sprint2/glance/versions.response.json`. Se quita también de la suite
  la exigencia de cero allocations del control de Placement (ahora sólo lectura).
- Corridas reales posteriores en la VM local (6 GiB):
  - 403 de Go a nova-api: la credencial validadora de Go estaba en el proyecto de
    laboratorio, y Go exige que el X-Service-Token de Nova sea de su mismo
    proyecto. Ahora la suite crea un validador temporal en el proyecto `service`.
  - Nova llega a ACTIVE desde Go, pero la captura falla por tres razones: la
    ventana de journalctl es de segundos completos, `/v2/schemas/image` quedaba
    como unmatched y Apache tiene empates dentro del mismo segundo. Además, la
    `generation` de Placement sube con cada claim y release (11→12) y rompía el
    control. Todo corregido, con tests.
- **Resultado: dos corridas consecutivas sobre `c6684fd` pasan 10/10 casos con
  limpieza completa.** Nova queda ACTIVE en 4,8 s y 4,4 s desde una imagen que
  sólo está en Glance Go. El flujo capturado tiene tres lecturas de metadatos,
  dos del esquema y una descarga completa de 21 692 416 B.
  [Evidencia](../evidence/sprint5/glance-b26-suite-20261112/README.md).
- Verificado también: 94 tests Python unitarios OK, `make glance-test` OK y
  gofmt/vet OK. No se corrió `make check` completo (race detector) en este host.
