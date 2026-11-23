# Plan de reimplementación Placement Go

Actualizado: 2026-10-29. Responsable Placement: Paolo Castañon Barrera.

## Objetivo y punto de partida

Sustituir el subconjunto de Placement que utiliza Nova en el laboratorio:
publicar recursos, obtener candidatos, reservar capacidad y liberarla al borrar
la instancia. La aceptación final exige un boot ACTIVE con Nova original y
Placement Go, allocations correctas y restauración comprobada del original.

S2 aporta baseline y arquitectura aceptados. El
[PR #3](https://github.com/PaoloCastanon/modules-devstack-optimized/pull/3) ya está
integrado en `85bae56`. El [primer incremento B07](b07-development.md) reutiliza
su módulo/auth e implementa providers/inventories con MySQL y versión 1.0.
[B07 está completado técnicamente](b07-acceptance.md) para las operaciones
priorizadas providers/inventories **1.0**. La campaña final del 29 de octubre
compara 57 casos contra Python y Keystone reales: **57/57 status, respuestas y
estados coinciden**, con limpieza y reinicio verificados. La implementación está
integrada mediante PR #6 en b4d8300. [B08](b08-development.md) añade catálogos y
asociaciones con 82/82 comparaciones estrictas, regresión B07 57/57 y reinicio
verificado. [B09-A](b09-development.md) añade la base de allocations con 60/60 casos vivos y
regresiones B07/B08 aprobadas. El [cierre B09](b09-development.md) añade 24/24 casos modernos y CAS de consumer.
B10 completó 44/44 casos; B11 cerró durabilidad/restore con 9/9 verificaciones. B12 cerró Keystone/políticas con 143/143 y regresiones 267/267; no se cierra el conjunto Placement ni la integración Nova.
[Resultados y ensayos previos](../../evidence/sprint3/placement/b07-close-20261029/README.md).

No cambiar estados, fechas, puntos del Excel ni informes históricos. Este plan
desglosa B07–B15; B21 documenta las decisiones y resultados durante la ejecución.
Los resultados se registran en [build-log](../../harness/build-log.md).

## Arquitectura y orden de trabajo

`cmd/placement` configura un proceso independiente. El dominio vive en
`internal/placement/{api,service,model,repository}` y usa MySQL en `placement_go`,
con esquema/cuentas separados de OpenStack y Glance. API serializa y negocia
microversiones; service aplica reglas; repository controla SQL/transacciones.

Reutilizar `internal/auth` para validar tokens, con políticas propias Placement.
`internal/config.LoadPlacement` ya usa un namespace independiente; conservarlo
y reutilizar sólo helpers neutrales. No importar
`internal/glance` desde Placement ni copiar su política de owner/reader/member.
Las migraciones MySQL de Glance también son específicas de su dominio.

| Orden | Backlog | Entrega y criterio observable |
|---|---|---|
| 1 | B07 / B21 | Matriz de contrato corregida y casos iniciales de providers/inventories con status, headers, tipos, generation y efectos esperados |
| 2 | B07 + base B11/B12 | Primer recorrido HTTP→servicio→MySQL: discovery, CRUD providers, GET/PUT inventories y GET inventory individual; autenticación real al ejecutar el servicio |
| 3 | B08 | Catálogos y asociaciones traits/classes, incluidos OWNER_NOVA y COMPUTE_STATUS_DISABLED, con conflictos y generation comprobados |
| 4 | B09 | Consultar/reemplazar/liberar allocations por consumer, y consultar allocations por provider; reserva atómica sin exceder capacidad |
| 5 | B10 | Candidates derivados del estado persistido, con recursos y filtros usados por Nova; capacidad agotada produce respuesta vacía correcta |
| 6 | Cierre B11/B12 | Reinicio conserva datos; aislamiento, migración y concurrencia verificados con MySQL; Keystone/políticas positivos y negativos con identidad Nova |
| 7 | B13–B15 | Sustitución reversible en laboratorio, boot ACTIVE, limpieza, suite de integración reproducible y restauración del endpoint original |

B11 y B12 empiezan en el primer recorrido. No posponer persistencia o seguridad
hasta tener todos los endpoints. Sus cierres requieren pruebas transversales;
una implementación parcial en B07 no los da por terminados.

## Primer incremento: B07

1. Revisar el módulo y dependencias disponibles tras integrar el PR #3. Mantener
   una sola versión declarada y ejecutar también las regresiones Glance.
2. Escribir pruebas de contrato antes del comportamiento: crear, listar, consultar,
   renombrar y borrar provider; duplicados por nombre/UUID; UUID inválido y 404;
   inventario por clase, defaults, validaciones y reemplazo con generation obsoleta.
3. Crear el modelo, interfaces y servicios con errores tipados. Usar un repositorio
   de prueba únicamente en tests. La ejecución real usa MySQL y Keystone.
4. Crear migración inicial propia de providers/inventories y las clases mínimas
   que necesitan. Fijar unicidad provider+class, defaults y control atómico de
   generation. B08 completa los catálogos; no simular clases inexistentes.
5. Añadir configuración, arranque/cierre, límites HTTP, request ID y logging sin
   secretos. Discovery no debe anunciar versiones ni capacidades no verificadas.
6. Añadir pruebas reales MySQL en esquema descartable: migrar, repetir migración,
   CRUD, rollback, conflicto concurrente y lectura desde un pool nuevo. Separarlas
   de `make check` para que el gate local no inicie servicios.
7. Crear el comparador original↔Go y ejecutar los casos B07. Conservar request,
   status, headers estables, respuesta y efectos de estado de cada lado.

**B07 aceptado al 2026-10-29 para 1.0:** recorrido real persistente, pruebas unitarias/API/concurrencia,
`make check` PASS con Go ejecutado y comparación diferencial documentada.
Si faltan fixtures o acceso al original, registrar aceptación parcial y diferencias
pendientes; no declarar equivalencia ni cerrar la tarea por el scaffold.

## Contrato: resolver antes de codificar supuestos

La [tabla S2](../sprint2/placement-api-scope.md) es una propuesta, no un oráculo
completo. Contrastarla con el código desplegado fijado y el original:

- **Create provider:** para 1.0–1.19 la respuesta es 201 sin cuerpo, con Location;
  desde 1.20 es 200 con representación. El fixture `provider-create.response.json`
  vacío concuerda con el primer caso; no inventar JSON para llenar esa captura.
- **Consumer vacío:** la traza Nova ya contiene GET allocations 200 con respuesta
  corta antes de reservar; resolver el caso exacto contra el original, sin imponer
  el 404 genérico de la tabla a un consumer sin allocations.
- **Ruta omitida:** GET `/resource_providers/{uuid}/allocations` aparece realmente
  en `evidence/sprint2/placement/nova-requests.log`; incluirla en B09 y capturar
  status/body/headers específicos.
- **Candidates:** el request observado usa `resources`, `limit=1000` y
  `root_required=OWNER_NOVA,!COMPUTE_STATUS_DISABLED` con versión 1.36.
  Probar traits requeridos/prohibidos y no ignorar parámetros silenciosamente.
- **Microversiones:** cubrir cambios relevantes hasta la versión que se anuncie;
  probar versión ausente, inválida, fuera de rango y límites de cambio. Las
  versiones 1.0, 1.2, 1.6, 1.10, 1.28 y 1.36 son puntos de evidencia, no una
  autorización para tratar todas las versiones intermedias como equivalentes.
- **Nova compute:** capturar desde el arranque del resource tracker, además del
  boot. Revisar si requiere aggregates, usages, filtros/árboles u otras operaciones
  ausentes del resumen S2. Ampliar solo por evidencia y registrar la decisión.
- **Auth:** verificar roles/scope reales del usuario de servicio Nova. El rol
  `service` de la credencial que valida tokens no equivale automáticamente al
  permiso del cliente. Resolver `X-Service-Token` solo si el flujo observado lo usa.

Fuentes: [API oficial Placement](https://docs.openstack.org/api-ref/placement/),
[flujo Nova medido](../sprint2/nova-placement-flow.md),
[modelo](../sprint2/placement-data-model.md) y auditoría upstream archivada.
La documentación actual complementa el contrato; prevalece el comportamiento
contrastado de la revisión Python fijada para la comparación experimental.

## Invariantes que deben tener pruebas

- Un update con generation vieja falla con el error y status del original, sin
  cambiar inventario, traits ni allocations.
- Una reserva valida `total`, `reserved`, `allocation_ratio`, `min_unit`,
  `max_unit` y `step_size`. Definir redondeos y límites desde el original.
- Reemplazar allocations considera la reserva previa del mismo consumer.
  Un fallo en cualquier provider/clase revierte todo; dos solicitudes concurrentes
  no pueden reclamar más capacidad que la disponible.
- Inventario o provider en uso no se elimina. La reducción de capacidad conserva
  reservas aunque quede sobrecapacidad, como el original contrastado en B09-A.
  Una reserva que empeora esa sobrecapacidad falla; una igual o menor se admite
  si cumple constraints. El filtro resources descuenta el uso persistido.
- Consumer generation conserva la diferencia entre ausente, null, cero y obsoleta
  según microversión. DELETE y repetición siguen el contrato observado.
- Candidates y provider_summaries proceden de inventarios, usos y traits reales.
  Se prueban falta de clase, recursos insuficientes, trait prohibido y query inválida.
- Sin token/expirado se rechaza; identidad autenticada sin permiso también.
  No hay bypass de autenticación seleccionable al desplegar el servicio.

## Verificación, integración y evidencia

Cada incremento ejecuta formato, vet, race, build, `make check` y casos específicos.
El comparador normaliza solo IDs/fechas/orden permitidos por la estrategia S2;
preserva tipos, errores, generation y efectos. Guardar resultados posteriores en
carpetas distintas, por ejemplo `evidence/sprint3/placement/<caso>/`, saneados y
con comandos reproducibles; las capturas S2 permanecen intactas.

Para B13–B15, preparar primero backup de configuración, datos/semillas para la
DB Go, puerto independiente y restauración. Cambiar únicamente el endpoint/config
Placement usado por Nova; conservar Glance original para aislar la sustitución.
Verificar publicación del compute, candidates, claim, boot ACTIVE, allocations,
DELETE de VM y liberación; añadir fallo por capacidad y conflicto concurrente.
Restaurar y comprobar el original, y apagar la VM al terminar su uso.

No ejecutar DevStack en la estación de trabajo. El trabajo local de contrato y
pruebas aisladas mantiene apagada la VM; encenderla solo en las etapas que
necesitan capturas o integración real. Benchmarks y sustitución Glance conservan
sus tareas posteriores y su metodología; no condicionan el primer B07.

## Próximo paso concreto

B07–B12 están completados para el subconjunto y perfiles explícitos descritos en
sus entregas. [B10](b10-development.md) cierra candidates planos y resource tracker;
[B11](b11-acceptance.md) cierra durabilidad/restore y [B12](b12-acceptance.md)
identidad/políticas reales. Las regresiones B07–B10 pasan 267/267 sobre el binario
final; Keystone/políticas 143/143 y durabilidad 9/9. Laboratorio limpio y VM apagada.

El siguiente bloque es B13–B15: preparar sustitución reversible de Placement,
publicación del compute y boot ACTIVE con Nova original; verificar claims,
liberación, capacidad agotada y restauración del endpoint. Requiere contrastar
primero que los perfiles seleccionados cubren las llamadas reales del lab.
El soporte actual no incluye árboles/sharing, autenticación compuesta ni toda
la API upstream. No cambiar Excel ni evidencia histórica S2.

El [plan B13–B18](b13-b18-plan.md) detalla sustitución, boot, suite y metodología/
mediciones. Su orden operativo comienza por B16 y prepara B15 durante B13/B14.

Avance posterior: [B13](b13-integration.md) verifica sustitución/publicación y
[B14](b14-acceptance.md) el boot real y liberación. [B16](../benchmarks/methodology.md)
define el protocolo. [B15](b15-integration.md) cerró la suite con dos corridas finales 11/11 y limpieza
completa. El siguiente bloque pendiente es medir B17/B18 con el protocolo B16.

Preparación2026-10-30: [instrumentación completa](../benchmarks/campaign-runbook.md)
y tres pilotos24/24 válidos HTTP/CPU/PSS. El coordinador ejecuta las240 ventanas
oficiales B16 sin recortar duraciones/repeticiones. El piloto no cierra B17/B18;
el cierre exige datos oficiales, análisis, limpieza y restauración.

Actualización 2026-10-31: B17/B18 cerrados técnicamente con latencia/throughput y CPU/PSS, 72 ventanas válidas bajo placement-compact-v2, original restaurado y VM apagada.
