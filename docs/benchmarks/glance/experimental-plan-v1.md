# Plan experimental Glance e integración, revisión 1

Estado: **PROPUESTO; pendiente de calibración en laboratorio**. Preparado antes
de los instrumentos. Protocolos máquina en protocol-v1.json y
../integrated/protocol-v1.json. No se modifica Placement B16.

## Preguntas e hipótesis

RQ1: ¿Cómo cambian latencia, goodput, CPU y memoria del subconjunto RF10 al
sustituir Glance Python por Go? RQ2: ¿Cómo cambian boot y consumo de OpenStack
al sustituir Glance, Placement y ambos? H0 por métrica: diferencia entre
implementaciones igual a cero; H1 bilateral: diferencia distinta de cero.
No se presupone superioridad de Go. Se comparan implementaciones/despliegues,
no el efecto aislado del lenguaje.

Factor Glance {Python,Go} × Placement {Python,Go}: A=PP, B=GP, C=PG, D=GG.
Comparaciones A/B, A/C, A/D, B/D, C/D. Interacción aditiva por repetición:
D−B−C+A, en unidades originales; ratios logarítmicos requieren métricas positivas.
El boot mide el sistema completo; no se atribuye íntegro a Glance.

Variables dependientes: latencia por solicitud/ciclo, p50/p95/p99, media,
mediana/desviación; requests/s y operaciones correctas/s; bytes/s y MiB/s;
errores/timeouts; CPU-s y CPU/op correcta en ventana común; PSS/RSS reposo,
serie y máximo observado; tiempo POST Nova→ACTIVE, eliminación y allocations.
La observación independiente es una repetición/bloque, no cada request.

Controles: hardware/VM/kernel/disco/red, rutas proxy efectivas, Keystone real,
Nova/Neutron/flavor/imagen/red, semilla y payloads idénticos por par, concurrencia,
warm-up/duración y estado inicial. Registrar workers Python, GOMAXPROCS, pools,
schemas propios, store/fsync/durabilidad, caché de tokens, logging, timeouts,
versiones de SDK, toolchain/binarios y SHAs. Cache caliente y fría se separan;
no borrar caches ajenas. No compilación/tests durante ventanas oficiales.

## Cargas y límites

Metadata: create/list/show/patch/delete, paginación, filtro name/tag, lecturas
concurrentes, mezcla determinista y CRUD. Crear un conjunto estable por worker;
para ciclos de escritura crear/borrar un recurso por ciclo. Todo recurso lleva
UUID de campaña y etiqueta; UUID determinista antes de POST permite recuperar
respuestas perdidas sin adoptar recursos ajenos. Ledger persistente antes de
mutar, GET y propiedad exacta antes de DELETE; comprobar 404 después.

Binarios: upload secuencial/ciclo con borrado, download completo, rango único,
descargas concurrentes y mezcla metadata/download. Archivos idénticos por par,
generación y envío en bloques acotados; SHA256 y SHA512/MD5 independientes.
Transferencias negativas/límites en gate funcional separado, nunca goodput normal.
El límite Go de concurrencia no se aumenta silenciosamente: concurrencias que
rechacen carga quedan exploratorias y se conservan, no equivalentes oficiales.

No se fijan tamaños definitivos sin capacidad del invitado. Calibración propuesta:
1/16/64 MiB únicamente si max_upload>=64 MiB y reserva permite al menos
2×concurrencia×tamaño_mayor, más imagen boot/discos Nova y margen. Son candidatos
de piloto, no resultados ni protocolo oficial. Reducir/revisar antes de congelar
si disco/store/memoria efectivos lo requieren; ampliar tamaño necesita revisión.
No usar payload sintético como imagen boot: usar imagen original autorizada,
hash verificado y copia propia al backend Go, sin tocar la imagen original.

## Diseño, entrada y congelación

Carga closed-loop: una request pendiente por worker, sin tasa externa impuesta;
reportar limitación coordinated omission. Referencia: c1/4/8/16 metadata,
30 s warm-up, 120 s medición, cinco pares; binario empieza c1 y calibra c2;
no forzar c16 con dos slots. Piloto separado y corto, no agregado al oficial.
Orden alternado por repetición, inicio determinado por semilla 20261031.
Integrado: secuencial c1, cinco bloques completos A/B/C/D, orden balanceado;
boot timeout 600 s, delete timeout 180 s, polling 1 s (incertidumbre explícita).
Piloto valida una vez cada A/B/C/D y no cuenta como repetición oficial.

Antes de congelar: recursos del host/invitado suficientes; sin campaña ajena;
lock host y mismo lock B15 en VM; baseline/backups/hash de archivos/servicios,
catálogo efectivo y recursos previos; gates funcionales y cleanup/restore
verificados; versiones/configuración saneadas e instrumentos con tests aprobados.
El protocolo congelado debe conservar bytes/hash, tamaño y concurrencias
calibradas, evidencias piloto y gates. Una revisión produce otro fichero/versionado;
no mezclar revisiones. Las herramientas iniciales no habilitan oficiales hasta
que exista una ruta completa de despliegue/restauración ensayada.

## Limpieza, restauración e invalidez

Sólo IDs propios del ledger y ownership comprobado; no barrer por prefijo ni
editar tablas originales. Guardar intentos fallidos, snapshots, muestras, config
saneada, hashes y estados de restauración. Si falta confirmar ausencia o existe
ambigüedad, NEEDS_ATTENTION y retener medios privados de recuperación. No apagar
VM si falla restauración; apagar ordenadamente tras confirmar originales.
Config switch sólo con backup y comparación contra hash esperado; no reemplazar
archivo cambiado por otro actor. El orquestador integrado completo permanece
pendiente de ensayo seguro en la VM; no usar hooks arbitrarios no auditados.

Invalidar comparación oficial ante respuesta/estado inesperado, pérdida de
muestras, PID reciclado, cambios de configuración, token vencido, carga ajena,
presión de memoria o cleanup/restore no confirmado. Retener error y repetir bloque
entero tras corregir instrumento; no excluir por lentitud. Falta PSS se declara,
RSS no lo reemplaza silenciosamente. Sin A/B/C/D funcionales no hay integrado oficial.

## Análisis y reproducción

Percentiles nearest-rank por operación/repetición; p99 con <1000 éxitos lleva
soporte insuficiente. No promediar percentiles como percentil global. Resumir
medianas de métricas por repetición, dispersión y ratios/deltas pareados.
Bootstrap por bloques/repeticiones, 10000 remuestras, seed fija, IC95% sólo
con al menos cinco bloques completos; no prueba automática de significancia.
No mezclar stage pilot/local-test con official. Mantener errores y runs inválidos
en inventario; no generar gráficos finales con datos sintéticos.

Reproducción: ejecutar make check sin VM; calibrar disponibilidad y gates;
preparar endpoints efectivos con SDK/credenciales privados, conservar manifiesto;
ejecutar runner Glance sobre una implementación a la vez y ledger nuevo;
recolector placement_resources dentro del mismo invitado; analizar datos crudos
y comprobar checksums. Consultar runbook.md para CLI y límites implementados.
