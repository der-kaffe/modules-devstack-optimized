> Resultados posteriores y estado vigente: [evaluación experimental parcial](results-20261031.md), con SHOW oficial de cinco pares, evidencia A/B/C/D y límites del controlador.

> Actualización de continuidad: el diagnóstico de capacidad de este informe es histórico. La recuperación posterior y los gates A/B/C/D se documentan en [el informe de infraestructura](infrastructure-recovery-20261031.md) y [la evidencia funcional integrada](integrated/functional-gates-20261031.md). Los datos previos se conservan; su estado no se convierte en campaña oficial.

# Evaluación Glance, Placement y OpenStack integrado — estado verificable

Fecha: 2026-10-31, America/Santiago. Base de producción e instrumentos Placement:
`fa116a89b8bcd9e7c2d250c0a8b06800f0c29fa1`. Nuevos instrumentos sin commit,
aislados en `feat/glance-openstack-benchmarks`. No se modifica Excel/Sprint 2.

## 1. Introducción y objetivo

Se estudia sustituir dos servicios concretos de OpenStack por implementaciones
Go del subconjunto seleccionado: RF10/B20/B25/B26 de Glance y B07–B18 de
Placement. La comparación conjunta conserva Nova/Keystone/Neutron y el resto
originales. No evalúa una reescritura completa de OpenStack ni busca probar que
Go sea siempre superior. Esta entrega combina auditoría, instrumentos, validación
local, análisis de datos históricos y un ensayo real de capacidad.

## 2. Estado por nivel de evidencia

| Entrega | Estado observado |
| --- | --- |
| Auditoría y selección integrada | IMPLEMENTADO, refs públicas y ascendencia verificadas |
| Protocolos propios Glance/integrado | IMPLEMENTADO como borradores; sin congelación oficial |
| Generador Glance metadata/streaming/ledger | IMPLEMENTADO; PROBADO LOCALMENTE con HTTP independiente |
| Observador Nova secuencial | IMPLEMENTADO; PROBADO LOCALMENTE con gateway doble; SDK real pendiente |
| Planificador y analizadores pareado/factorial | IMPLEMENTADO; PROBADO LOCALMENTE, sin aceptación automática |
| Disponibilidad de VM reducida | PROBADO EN LABORATORIO; gate de capacidad FALLIDO |
| Piloto Glance real Python/Go | BLOQUEADO; no se ejecutó carga |
| Gate conjunto D | PENDIENTE; nunca demostrado en esta entrega |
| Placement piloto07 archivado | REANALIZADO: 24/24 HTTP/CPU/PSS válidos, no oficial |
| Campañas oficiales Glance/integrado/Placement | NO COMPLETAS; sin datos oficiales nuevos |
| Gráficos/tablas Placement piloto07 | ANÁLISIS DESCRIPTIVO COMPLETO del piloto, no de B17/B18 oficial |

## 3. Preguntas, hipótesis y diseño

Preguntas/hipótesis bilaterales, variables, controles, criterios y reproducción
están en [plan v1](glance/experimental-plan-v1.md). Protocolos separados:
[Glance](glance/protocol-v1.json), [integrado](integrated/protocol-v1.json).
Mantienen independencia respecto del protocolo B16 existente. Los tamaños y
concurrencias candidatos exigen calibración del store y límites efectivos.

## 4. Arquitectura y versiones

Host Debian, RAM física7.0 GiB; libvirt/KVM y VM Ubuntu dedicada de4 vCPU.
La configuración original era6 GiB y se restauró; sólo la sesión de capacidad
arrancó con4 GiB. No se ejecutó DevStack sobre el host. La reimplementación
separa procesos/dominios y persistencia; no se modificó producción.
La auditoría incluye contrato, auth/config, Makefile, harness/CI y evidencia
S2/B13–B18/Glance; [informe inicial](glance/audit-20261031.md).

Las versiones Python/Go/MySQL del piloto histórico están en su manifest y
snapshots archivados; no se atribuyen al boot actual. Go local se registra en
la salida de make check; matplotlib3.10.1+dfsg1 generó figuras. No se midieron
versiones actuales del SDK Nova mediante operaciones autenticadas en el invitado.

## 5. Matriz funcional A/B/C/D

| Configuración | Glance | Placement | Gate Nova en esta sesión | Evidencia anterior |
| --- | --- | --- | --- | --- |
| A | Python | Python | BLOQUEADO por capacidad; sólo health/probes | Boot S2 histórico |
| B | Go | Python | NO EJECUTADO; capacidad/despliegue pendiente | B26 privado: boot real, N7 parcial fallido |
| C | Python | Go | NO EJECUTADO; capacidad/despliegue pendiente | B14/B15 históricos con cleanup |
| D | Go | Go | NO EJECUTADO; capacidad y gate conjunto pendientes | No encontrada |

Los probes401/200 de originales no equivalen a gate A. Los éxitos históricos
independientes B/C no acreditan D. El instrumento conserva gate_accepted=false
hasta demostrar rutas/identidades y restauración, incluso si observa ACTIVE.

## 6. Instrumentación implementada

- `glance_benchmark.py`: 13 escenarios de metadata/transferencias/mezclas;
  semillas propias, JSON Patch, paginación/filtros, generación SHAKE256 de payload,
  streaming256 KiB, hashes independientes, deadlines/socket watchdog, límites,
  tokens privados, raw por request, ledger write-ahead y cleanup recuperable.
- `integrated_boot.py`: observación de un boot vía SDK, inputs/hash, ledger antes
  de POST, recuperación de respuesta perdida, estado/tiempo ACTIVE y allocations,
  eliminación por propiedad y preservación del inventario previo. No switch.
- `plan_experiments.py`: orden reproducible, alternancia por par y bloques
  balanceados A/B/C/D; genera planes, no ejecuta campañas ni las acepta.
- `experiment_analysis.py`: verifica artefactos/raw, exporta CSV, calcula latencia,
  errores/goodput/binarios; CPU/PSS opcional en ventana común con recolector
  existente. Análisis pareado/factorial por repeticiones y bootstrap descriptivo.
- `export_placement_pilot.py`: gráficos PNG/PDF y CSV derivados del reanálisis
  real, rotulados como histórico/piloto; no publica en Git.

Se reutilizan placement_http.summarize y placement_resources.interval_summary.
No se duplica el recolector ni se ejecuta el supervisor que hace commits/push/PR.
Separación: carga Glance, observación Nova, planificación, recolección existente,
análisis y exportación local. Falta el coordinador reversible Glance+Placement
completo y ensayado. Los instrumentos nuevos no pretenden sustituirlo.

## 7. Escenarios y límites

Glance: create/list/show/patch/delete/pagination/filter/metadata-mix/crud,
upload/download/range/binary-mix; concurrencia por ventana acotada. Cada worker
mantiene dos semillas y como máximo un recurso adicional por ciclo, sin purga
SQL. Los ciclos upload/delete incluyen helpers: goodput es el flujo observado,
no un POST ilimitado. Los escenarios negativos invalidan, no suman éxitos.
Nova se limita a una VM propia por observación, sin nuevas funciones productivas.

## 8. Control y seguridad experimental

No se cambiaron los binarios productivos para favorecer resultados. No se
desactivó auth. No hubo dos campañas competidoras ni carga oficial durante tests.
Locks, UUID/name/tags y metadata protegen recursos ajenos. Los cambios de Git se
limitan al fetch autorizado y a la rama/worktree nuevos; cero staging/commits/
push/PR. Main y rama histórica Glance se verifican contra snapshots iniciales.

## 9. Ensayo real de capacidad y restauración

La presión inicial del host impedía arrancar6 GiB. Tras liberarse memoria por
actividad externa, se verificaron>=5 GiB disponibles y se ensayó4 GiB con margen
host, supervisor finito y restauración inmediata del XML persistente. El guest
quedó con MemAvailable144288 KiB y swap usada1623368 KiB, sin OOM observado.
Esa condición impide añadir cargas/Go/MySQL/VM Nova con seguridad y comparabilidad.
No se redujo un protocolo oficial ni se simuló una campaña completa.

Ocho servicios originales activos, Glance sin token401, Placement raíz200; una
instancia anterior apagada, preservada. VM apagada ordenadamente y XML igual al
original. [Informe de restauración](integrated/restoration-20261031.md) y evidencia
externa describen exactamente lo comprobado y la lectura de hashes no repetida.

## 10. Datos obtenidos y procedencia

No hay latencias de benchmark Glance real ni tiempos nuevos de boot. Las 14
ventanas conservadas de validación local son tests de protocolo, stage local-test,
sin valor para comparar implementaciones. No se convierten en pilotos de servicio.

Se reanalizaron raw HTTP gzip y procesos del piloto07 existente:
`evidence/sprint5/benchmarks/b17-b18-pilots-20261030/pilot-07`.
Resultado:24 ventanas válidas, cero errores del analizador; stage pilot,
b17_accepted=false, b18_accepted=false, frente a240 ventanas oficiales requeridas.
Pilotos05/06 también están archivados; no se agruparon como repeticiones oficiales.

## 11. Tabla ilustrativa de datos reales del piloto07

Sólo providers, concurrencia1; un par, warm-up1 s y carga4 s. Estas cifras se
recalcularon de evidencia histórica; no son mediciones de la sesión4 GiB.

| Métrica | Python | Go |
| --- | ---: | ---: |
| p50 HTTP, ms | 4.693083 | 6.726713 |
| p95 HTTP, ms | 7.423821 | 9.452858 |
| Requests correctas/s | 198.308850 | 139.927233 |
| Tasa de error | 0 | 0 |
| CPU ms/request correcta, intervalo común | 3.733108 | 0.692124 |
| PSS media del proceso, bytes | 187012096 | 15528960 |

CPU y goodput HTTP usan las ventanas apropiadas respectivas; no dividir CPU por
operaciones de otra ventana. Datos de los seis escenarios y c1/c16 en CSV externo.

## 12. Tratamiento estadístico

Percentiles nearest rank por request/operación y repetición; p99 indica soporte
insuficiente bajo1000 éxitos. Mediana/media/desviación se calculan sin llamar
percentil global a una media de percentiles. Pares completos independientes para
diferencias/ratios/porcentajes; integrado estima D−B−C+A por bloque. Se rechazan
protocolos/stages mezclados y duplicados. Nuevos analizadores omiten IC con n<5.
El analizador histórico produce IC degenerados para n1: no se interpretaron ni
mostraron como incertidumbre válida en la exportación. No hay prueba de significancia.

## 13. Resultados e interpretación

El piloto07 muestra respuestas dependientes del escenario. En providers c1,
Go reduce CPU/PSS y empeora latencia/goodput respecto de Python en ese ensayo.
No se declara ventaja global ni causalidad del lenguaje. Sin Glance repetido y
sin D no se puede estimar impacto integrado ni interacción. Cualquier número
de mejora combinada sería especulación y no se presenta.

## 14. Gráficos y tablas

Se generan latency/goodput/cpu/memory, PNG y PDF, cada uno con unidades,
concurrencia, n1, duración, procedencia y límites. No hay gráficos finales Glance
o A/B/C/D por ausencia de datos reales. CSV conserva escenarios/implementaciones
sin pooling y provenance.json identifica hash del summary reanalizado.

## 15. Amenazas a la validez

Interna: middleware/caché Keystone, workers Python/GOMAXPROCS, pools/MySQL,
filesystem/fsync/hash previo, proxy, disco compartido, swap y actividad ajena.
Constructo: closed-loop incluye coordinated omission; tiempo boot no es latencia
Glance; máximos PSS/RSS a1 Hz no son picos absolutos. Estadística: n1 piloto,
colas cortas y bootstrap descriptivo; cero evidencia para significancia o interacción.
Externa: portátil único y perfil reducido; no extrapolar a datacenter ni escalas
no medidas. Reproducibilidad: SHA base fijado, nuevas herramientas sin commit
identificadas por hashes/diff; protocolos aún no congelados.

## 16. Limitaciones y pendientes reales

Recursos insuficientes del laboratorio activo; preparar capacidad estable sin
OOM/swap dominante. Despliegue Go nuevo aislado y switch combinado con restore
probado; ensayar SDK/collector Glance reales y gates actuales A/B/C/D. Calibrar
tamaños/concurrencias, congelar protocolos nuevos; completar campañas sin
reducir repeticiones a escondidas. Placement oficial aún240 ventanas; no ejecutar
su supervisor publicador. Análisis/figuras finales dependen de esas mediciones.

## 17. Conclusiones respaldadas

Hay instrumentos piloto verificables y datos Placement históricos analizables;
no existe aceptación experimental completa. El entorno reducido observado no
ofrece margen seguro para las campañas. El gate D permanece sin demostrar.
Veredicto: APTO para revisar instrumentos locales; **NO APTO** para cerrar
benchmark Glance, B17/B18 o integrado. No se cambia ningún estado formal del Excel.

## 18. Reproducción

[Runbook Glance](glance/runbook.md), [observador integrado](integrated/runbook.md)
y runbook Placement preexistente. El planificador referencia300 pares Glance
(600 ventanas,25 h mínimas) y5 bloques A/B/C/D; es planificación pendiente de
calibración, no job activo. No hay proceso durable de campaña dejado en ejecución.

## 19. Anexos y almacenamiento

Raíz externa exacta de esta entrega:
`/home/juanc/.local/share/bio-harness/workspaces/d64feb577c19cd022749420b3c83db8a/benchmarks-endtoend-20261031/`.

Incluye snapshots de worktrees/refs; make-check logs; local-instrument-validation/
y su análisis; reanálisis Placement y placement-pilot-07-exports-final/ con8 figuras y
CSV; planes de referencia; preflight/restauración VM; review/ con diffs íntegros,
inventario y hashes. MANIFEST.sha256 cubre artefactos de entrega; no secretos.
Los datos voluminosos existentes permanecen en sus ubicaciones originales.

## 20. Revisión humana y commits candidatos

Cambios completos e inventario se entregan fuera del repositorio, incluyendo
diff de nuevos contra /dev/null. No hay staged ni archivos eliminados. Propuestas
de títulos, ejecutadas exclusivamente por el humano tras revisión:

1. `docs(benchmarks): define Glance and integrated experimental protocols`
2. `feat(benchmarks): add bounded Glance and Nova pilot instruments`
3. `feat(benchmarks): add paired analysis and historical pilot exports`
4. `docs(benchmarks): record capacity gate and reproducible evaluation status`

Repository root: `/home/juanc/Proyectos/modules-devstack-benchmarks-endtoend`.
No se propone mezclar datos locales sintéticos con evidencia oficial ni publicar
automáticamente estos cambios.
