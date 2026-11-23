# Plan de implementación

Este plan organiza el backlog existente; no reemplaza el Excel ni modifica sprints o puntos. Los resultados observados pertenecen al [build log](harness/build-log.md). La arquitectura y los contratos permanecen en [la documentación de Sprint 2](docs/sprint2/README.md).

## Fases y aceptación

| Fase | Backlog / sprint | Criterio para considerarla terminada |
|---|---|---|
| Laboratorio y análisis | B01–B06 y B19 / S2 | Instalación reproducible, smoke con boot ACTIVE, contratos y capturas del original. Evidencia aceptada: [matriz S2](docs/sprint2/sprint2-evidence.md). |
| Placement Go | B07–B12 / S3; B21 transversal | Providers/inventories, traits/classes, allocations, candidates, persistencia y auth dentro del alcance; pruebas unitarias/API, casos de error y comparación con el original. |
| Integración y Glance Go | B13–B16, B20, B22, B25 / S4 | Nova usa Placement Go y arranca una instancia; integración automatizada; método experimental definido; Glance gestiona catálogo y bytes con pruebas de integridad. |
| Sustitución Glance y evaluación | B17, B18, B23, B24, B26 / S5 | Nova arranca desde Glance Go; mediciones repetidas de latencia/throughput y CPU/RAM con metodología fija; resultados y memoria final. |

Las fases son agrupaciones de aceptación, no una prohibición de avanzar en paralelo en los dos módulos. B26 conserva S5. B21 continúa en progreso y B16 pendiente según la última aceptación; preparar estos documentos no los cierra.

## Integración y evaluación: avance B13–B18

**B16 completado técnicamente para Placement:** [protocolo experimental](docs/benchmarks/methodology.md),
instrumentos de lectura/CPU/PSS, tests y dos pilotos reales de calibración.
Resultados del piloto.
**B13 completado técnicamente:** [sustitución reversible](docs/placement/b13-integration.md),
publicación real del compute, cliente Nova 1.36 y rollback verificados.
**B14 completado técnicamente:** [boot Nova real](docs/placement/b14-acceptance.md),
ACTIVE, allocations/owner/usages correctos y liberación al borrar. Original
restaurado, identidad/runtime propios retirados y VM apagada.
**B15 completado técnicamente:** [suite integral](docs/placement/b15-integration.md),
dos corridas finales consecutivas 11/11, con ocho verificaciones de limpieza cada
una, recursos propios nuevos y original restaurado. VM apagada.
<!-- placement-b17-b18-status:start -->
**B17/B18 completados técnicamente, 2026-10-31:** 72/72 ventanas válidas, protocolo placement-compact-v2; 3 pares por celda, concurrencias [1, 16], warm-up 10s/carga 30s. Análisis CPU/PSS correlacionado aprobado para este perfil. Ver B17 y B18. Original restaurado, limpieza comprobada y VM apagada. Excel/S2 conservan su aceptación histórica.
<!-- placement-b17-b18-status:end -->

## Revisión compacta v2 autorizada, 2026-10-31

El usuario solicitó reducir duración y liberar espacio. Se prepararon perfiles
[compactos](docs/benchmarks/compact-campaigns-v2.md): Placement 72 ventanas, Glance
108 y conjunto 24 bloques/36 boots. Instrumentación adaptada a la matriz, rango
observado con n=3, controles de disco/logs y mantenimiento temporal; checks locales
aprobados. Placement v2 aceptado (72/72, CI PASS). Glance: preflight funcional
42/42 aprobado, piloto de carga y oficial pendientes; conjunto pendiente. Ningún intento
fallido de la matriz larga acredita v2 ni se mezcla con ella.

## Campañas posteriores autorizadas: Glance y funcionamiento conjunto

Solicitud del usuario, 2026-10-30: continuar después de la campaña Placement
actual. [Protocolos y runbook](docs/benchmarks/glance-joint-campaigns.md)
preparados en worktree independiente: Glance Python/Go con Placement original,
seguido de un diseño conjunto de cuatro variantes con boots Nova reales.
Instrumentos específicos, preflight, pilotos y resultados oficiales pendientes.
No cambiar la campaña Placement congelada ni ejecutar campañas simultáneas.
[Condiciones y continuación](docs/benchmarks/after-placement-handoff.md).

## Placement: B07–B12 completados en el alcance seleccionado

**Cierre técnico al 2026-10-29:** [B07 aceptado para el alcance priorizado
providers/inventories 1.0](docs/placement/b07-acceptance.md). La campaña final dio
**57/57** coincidencias estrictas, status y estados iguales contra Python fijado,
con MySQL/Keystone reales, reinicio de ejecutable, concurrencia y limpieza
verificados. [Evidencia y ensayos previos](evidence/sprint3/placement/b07-close-20261029/README.md).
El PR #6 ya fue integrado en b4d8300.

**B08 al 2026-10-29:** [catálogos y asociaciones classes/traits](docs/placement/b08-development.md)
implementados con migración v3, 21 clases y 377 traits estándar, CUSTOM, borrados
protegidos y generation. Las rutas seleccionadas incorporan los cambios 1.1–1.7:
**82/82** comparaciones estrictas y regresión B07 **57/57**, MySQL/Keystone reales,
reinicio y limpieza comprobados. [Evidencia B08](evidence/sprint3/placement/b08-close-20261029/README.md).
La asociación positiva de aggregates sólo se verificó en MySQL aislado.

**B09 completado para las rutas priorizadas:** [entrega A](docs/placement/b09-development.md) implementa
allocations atómicas, consultas por consumer/provider, usages y capacidad restante,
con migración v4. **60/60** casos vivos, B07 **57/57** y B08 **82/82**; rollback,
concurrencia y reinicio verificados. El [cierre B09](evidence/sprint3/placement/b09-close-20261029/README.md) añade
formato 1.12, generación 1.28 y mappings 1.34 para PUT 1.36: **24/24** casos
modernos estrictos. El rango alto se limita a allocations hasta ampliar los
perfiles del resource tracker en B10.
[Evidencia B09-A](evidence/sprint3/placement/b09a-20261029/README.md).

**B10 completado para el perfil plano:** [candidates](docs/placement/b10-development.md)
derivados de inventarios/uso/traits/aggregates persistidos, filtros Nova y perfiles
del tracker. **44/44** casos vivos y ocho seeds sin diferencias; regresiones
24/60/57/82 completas. [Evidencia B10](evidence/sprint3/placement/b10-close-20261029/README.md).

**B11 completado:** [durabilidad y restore](docs/placement/b11-acceptance.md), nueve verificaciones reales aprobadas.
**B12 completado:** [Keystone y políticas](docs/placement/b12-acceptance.md), 143/143 casos vivos, tres verificaciones fail-closed, 267/267 regresiones y B11 final 9/9. VM apagada y limpieza comprobada. El alcance sigue siendo el subconjunto para Nova; no se acredita
sustitución Nova, toda la API 1.8 ni se cambia el Excel histórico.

El [plan B13–B18](docs/placement/b13-b18-plan.md) propone sustituir Placement,
validar boot/suite Nova y medir latencia, throughput, CPU y memoria. B16 se define
antes de medir; B13–B15 acreditan integración y B17/B18 usan campañas controladas.
Es planificación, sin cambios al estado formal del libro ni ejecución de laboratorio.

El [plan Placement B07–B15](docs/placement/implementation-plan.md) conserva el
orden. Los resultados anteriores de 27/38 y ocho diferencias pendientes describen
etapas intermedias; el build log conserva esa historia sin sustituir capturas.

### Estado histórico previo a implementar B07

**Estado al revisar el repositorio el 2026-10-26:** sin iniciar. No existe `go.mod` ni fuentes `.go`. La infraestructura sí está implementada y tiene evidencia de Sprint 2; la reimplementación de los servicios aún no comenzó.

### Entradas de B07

- [Arquitectura](docs/sprint2/go-target-architecture.md), [modelo Placement](docs/sprint2/placement-data-model.md), [ADR-01 y ADR-03](docs/sprint2/design-decisions.md).
- [Contrato de rutas y microversiones](docs/sprint2/placement-api-scope.md) y [estrategia de pruebas](docs/sprint2/test-strategy.md).
- [Capturas originales Placement](evidence/sprint2/placement/). Completar casos que aún no tienen fixture; no inventar respuestas de Nova.

### Alcance de B07

Reutilizar el módulo Go integrado y completar el recorrido API→servicio→repositorio de providers/inventories, con descubrimiento y negociación de las microversiones necesarias. El almacenamiento sigue ADR-01; no escribir en la DB original. Auth de producción se verifica transversalmente en B12: cualquier identidad de prueba debe quedar restringida a tests, sin un bypass desplegable.

### Verificación y aceptación de B07

1. **Prueba inicial:** definir casos de create/list/show/update/delete, validación, duplicados y errores de generation según el contrato original. Registrar el comportamiento que falta antes de implementarlo.
2. **Implementación:** hacer pasar esos casos con la separación de capas prevista y persistencia consistente; B11 completa y verifica los requisitos transversales de persistencia y reinicio.
3. **Refactor:** mantener el contrato y volver a ejecutar las pruebas.
4. **Verificación local:** `make check`; los tests Go deberán ejecutarse, sin salida de pendiente.
5. **Compatibilidad:** ejecutar casos equivalentes sobre original y Go, conservando estado inicial, request, status, campos estables y efectos. Las pruebas locales aisladas no cierran la integración Nova de B13–B15.

La [aceptación B07](docs/placement/b07-acceptance.md) registra resultados, tests, casos diferenciales y límites en el build log. No se considera completado por crear carpetas, un health endpoint o respuestas HTTP constantes.

## Comandos y límites

- `make check`: verificación local del harness, sintaxis/lint, infraestructura, evidencia archivada y Go cuando exista. No contacta el laboratorio.
- `make harness-status`: inventario actual de fuentes y referencia al estado aceptado.
- `make evidence-check`: valida sólo las capturas archivadas de S2; no indica disponibilidad actual de servicios.
- `make go-check`: con código Go, exige módulo, pruebas de los dominios presentes, formato, vet, race detector y build. Sin código, informa explícitamente que la implementación está pendiente.
- `make lab-status`: consulta libvirt; los comandos de instalación, smoke y baseline se ejecutan sólo para tareas que requieran el laboratorio.

El [workflow de trabajo](AGENTS.md) conserva evidencia y registra los comandos ejecutados antes de cada commit. Las verificaciones locales no reemplazan la sustitución real con Nova ni los benchmarks.
