# Guía de trabajo para agentes

## Contexto y fuentes

Este repositorio reimplementará un subconjunto de OpenStack Placement y Glance en Go. Placement corresponde a Paolo Castañon Barrera y Glance a Juan Cardenas Uribe. Consultar al comenzar:

- [Plan de implementación](PLANS.md): fases, alcance y criterios de aceptación.
- [Registro observado](harness/build-log.md): resultados reales y límites de las verificaciones.
- [Arquitectura Go](docs/sprint2/go-target-architecture.md) y [decisiones](docs/sprint2/design-decisions.md).
- Contratos de [Placement](docs/sprint2/placement-api-scope.md) y [Glance](docs/sprint2/glance-api-scope.md).
- [Trazabilidad](docs/sprint2/final-traceability.md) y [comparación diferencial](docs/sprint2/compatibility-strategy.md).

El Excel original conserva la planificación; el libro `Planificacion_Trabajo_Titulo_OpenStack_Placement_Glance_Sprint2.xlsx` contiene la última aceptación formal. No cambiar sus estados ni atribuir avances por la fecha del calendario. Los informes académicos de Sprint 2 son entregas históricas; el avance posterior se registra en el build log.

## Ciclo de trabajo

1. Inspeccionar el estado de Git, el plan, la evidencia y los archivos afectados.
2. Identificar el ID de backlog y un criterio observable antes de implementar. Para una corrección, reproducir el fallo; para una función Go, definir el contrato y su prueba.
3. Implementar un cambio acotado. No ampliar rutas o microversiones sin evidencia o justificación registrada.
4. Ejecutar `make check` y las pruebas específicas que correspondan. Una salida omitida o pendiente no cuenta como prueba aprobada.
5. Actualizar [el registro](harness/build-log.md) con fecha, tarea, comandos, resultados y límites. Separar lo propuesto, lo implementado y lo validado contra Nova.
6. Hacer un commit por tarea, como pidió el usuario. Conservar cambios ajenos.

Si una comprobación falla, corregir la causa; no debilitar el gate ni sustituir evidencia real por datos fabricados. Un pase de `make check` sin fuentes Go sólo verifica el harness y los artefactos existentes.

## Arquitectura y pruebas

- Procesos independientes `cmd/placement` y `cmd/glance`; dominio bajo `internal/placement` y `internal/glance`.
- Separar API, servicio, modelo y repositorio; Glance añade store con streaming. Los handlers no ejecutan SQL; los dos dominios no se importan entre sí.
- Compartir sólo auth, configuración, middleware y logging. Usar DB separadas del OpenStack original y las decisiones ADR existentes.
- Preservar status HTTP, tipos JSON, generation y errores de dominio. No normalizarlos para ocultar diferencias. Binarios se comparan por longitud y hash.
- Añadir pruebas del comportamiento modificado y de errores relevantes. `make go-check` exige formato, vet, pruebas con race detector y compilación cuando exista código Go.
- Los scripts `tests/compatibility/*_baseline.sh` capturan servicios originales; todavía no son un comparador Python↔Go.
- `scripts/placement_differential.py` sí compara endpoints vivos para B07: requiere directorio nuevo, tokens privados y opt-in de mutaciones; conserva diferencias y devuelve fallo si las hay. El gate local solo prueba el runner con dobles. Revisar la evidencia en vivo antes de ampliar contratos o declarar B07 aceptado.

## Laboratorio y evidencia

La VM `tt-openstack-lab` debe permanecer apagada mientras no se use, por instrucción del usuario. `make check` es local, no inicia la VM ni accede a OpenStack. Iniciar, aprovisionar o sustituir servicios sólo cuando la tarea del usuario lo requiera. No ejecutar DevStack en la estación de trabajo.

No guardar tokens, contraseñas, archivos `.env`, tfstate ni imágenes descargadas en Git. Sanear cabeceras y logs. Las capturas de Sprint 2 son la línea base aceptada: no sobrescribirlas al experimentar; usar otra carpeta para resultados posteriores y registrar cómo reproducirlos. Una revisión de archivos históricos no demuestra que los servicios estén activos hoy.
