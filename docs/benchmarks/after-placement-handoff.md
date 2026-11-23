# Continuación autorizada después de Placement

Solicitud del usuario: al terminar la campaña Placement actual, crear campañas
de Glance Go y del funcionamiento conjunto de Glance Go y Placement Go. Incluye
preparación, instrumentos, pruebas, pilotos y ejecución secuencial en el mismo
laboratorio. Push ya autorizado en AGENTS.md; no merge automático.

## Estado guardado al preparar la cola

- Campaña predecesora: /tmp/b17-b18-official-status.json.
- Servicio: tt-placement-b17-b18-20261031-03.service (systemd de usuario).
- Worktree predecesor: /home/paolo/Proyectos/TrabajoTitulo/.worktrees/placement-b17-b18.
- HEAD congelado inicial: fa116a89b8bcd9e7c2d250c0a8b06800f0c29fa1.
- PR de resultados Placement: https://github.com/PaoloCastanon/modules-devstack-optimized/pull/16.
- Nuevo worktree: /home/paolo/Proyectos/TrabajoTitulo/.worktrees/glance-joint-campaigns.
- Rama: feat/glance-joint-benchmark-campaigns.
- Protocolos y runbook de esta carpeta preparados, no ejecutados ni aceptados.
- No se corrió make check ni se compiló mientras la campaña actual está midiendo.
  Validar estos cambios y hacer commit después de liberar el laboratorio.

## Pasos de continuación

1. Leer AGENTS.md, PLANS.md y harness/build-log.md; revisar el estado del
   predecesor y systemd sin tocar su ejecución. Mientras siga activo, esperar.
   Exigir stage=complete, accepted=true, b17=true, b18=true, ci=PASS y vm='shut off'
   en el estado, más servicio inactivo y restauración documentada. Si hay
   needs_attention, reparar primero el predecesor preservando evidencia.
2. Revisar worktree/rama propios y cambios ajenos. Reconciliar la base con los
   resultados finales Placement y la revisión realmente aceptada de Glance;
   conservar los archivos nuevos. No cambiar ni resetear el worktree anterior.
3. Implementar los dos protocolos y sus instrumentos conforme al runbook
   glance-joint-campaigns.md. Checks y commits por tarea; no modificar Excel,
   Sprint 2, archivos históricos ni atribuir a Paolo trabajo Glance ajeno.
4. Preflight y piloto completo de Glance; congelar revisión/manifiesto antes
   de medir las 360 ventanas. No usar el modo piloto como sustituto oficial.
5. Sólo con Glance aceptado y restauración probada, piloto conjunto en los cuatro
   estados y campaña de 120 bloques/280 boots. Misma exigencia de evidencia.
6. Para sesiones largas crear supervisores durables y un estado público separado
   por campaña, sin secretos y con apagado normal tras restauración validada.
   No ejecutar dos supervisores que muten la VM al mismo tiempo.
7. Actualizar B23/B24/documentación con resultados reales; push y PR revisable.
   No cerrar una campaña por tener un protocolo, un piloto o una mejora aparente.
   Notificar al usuario los hitos, finalización o bloqueos accionables.

## Reanudación sin duplicados

Conservar la etapa en estado externo a Git, por ejemplo
/home/paolo/Proyectos/TrabajoTitulo/reports/campaigns/followup-status.json.
Antes de iniciar un nuevo proceso, buscar servicio/proceso, lock, manifiesto y
estado existente. No relanzar campañas ya activas o aceptadas. Si falta un estado
o no coincide el dueño del laboratorio, inspeccionar antes de actuar. No pisar
directorios existentes; conservar fallos con su diagnóstico. Tras terminar ambas
campañas, desactivar el seguimiento de este chat.


## Recuperación del predecesor, 2026-10-31

El intento 02 se detuvo con 111/240 ventanas completadas: apt-daily-upgrade
reinició Apache y Keystone a las 06:19:55 UTC. Los datos, incluidos 503 y un
fallo de transporte en la ventana siguiente, se conservan sin aceptación.
La limpieza remota terminó después del fallo del supervisor; se verificaron
originales activos y retirada de runtime/identidad/estado privado, se archivó
la evidencia y se apagó normalmente la VM antes de reiniciarla.

El intento 03 repite las 240 ventanas completas sobre el entorno actualizado;
no mezcla datos de antes/después de actualizar paquetes. Mismo HEAD fa116a8,
binario y protocolo. Servicio actual tt-placement-b17-b18-20261031-03.service;
estado público central sin cambios de ruta. Configuración en
/tmp/tt-placement-b17-b18-supervisor-20261031-03/config.json. Se suspendieron los
dos timers apt y se enmascararon sus unidades/timers sólo en /run; no se cambiaron
opciones persistentes. Las máscaras desaparecen en el apagado/reinicio de la VM.
Registro: reports/campaigns/placement-maintenance-control-20261031-03.json en el
workspace superior. Mantener ese control también en campañas posteriores,
registrando/restaurando estado, para que no reinicien dependencias al medir.
No relanzar nada mientras el intento 03 siga activo. El intento 02 está
archivado en /tmp/b17-b18-recovery-20261031-01 y su directorio de datos original.


## Disco anfitrión agotado, intento 03

El intento 03 se detuvo tras 58/240 ventanas. libvirt informó paused (I/O error),
vda: no space; el filesystem anfitrión de 196 GiB estaba al 100 %. Se vació sólo
la caché Go regenerable, se reanudó la VM para recuperación, se verificó cleanup
remoto tardío y servicios originales activos, se archivó evidencia y se apagó
normalmente la VM. No hay aceptación ni campaña Glance iniciada.

Antes de relanzar exigir al menos 20 GiB disponibles en el filesystem que aloja
el disco qcow2, y añadir guardas de espacio al preflight y durante la campaña
para abortar/restaurar antes de un nuevo I/O error. No borrar imágenes ajenas,
snapshots, datos personales ni caches de otras aplicaciones. sudo del anfitrión
requiere contraseña; paccache dry-run estimó 4.24 GiB recuperables manteniendo dos
versiones, pero no se ejecutó esa limpieza. Estado durable y diagnóstico en
reports/campaigns/placement-recovery-20261031-02.json del workspace superior.
El servicio 03 ya no está activo; no confundirlo con una medición en curso.
Si sigue faltando espacio, conservar la VM apagada y no repetir notificaciones
idénticas; continuar preparación local ligera que no agrave la falta de disco.


## Revisión vigente tras recuperar espacio y solicitud de campañas cortas

2026-10-31: el usuario pidió limpiar y reducir la duración de campañas. Leer
compact-campaigns-v2.md antes de continuar: perfiles v2 preparados (Placement 72,
Glance 108 ventanas; conjunta 24 bloques/36 boots). No relanzar el perfil largo
v1 por defecto. El espacio fue recuperado sin borrar la VM: anfitrión ~39 GiB
libres, qcow2 ~6,8 GiB, servicios originales conservados y VM apagada. Estado
actual compact_profiles_prepared. La condición antigua de disco lleno quedó
resuelta; conservarla sólo como historia.

Antes de ejecutar v2, parametrizar constantes de matriz y publicación, validar
alcance/estadística y make check, limitar logs, actualizar IaC para discard y
pilotar. El perfil compacto es preliminar y no acredita la matriz larga. No
se ejecutó ninguna nueva campaña durante la limpieza. Debe verificarse siempre
el estado vivo y las guardas de almacenamiento antes de iniciar procesos.


## Instrumentación compacta y piloto ya validados

Commit de instrumentación 789648e y PR #17 (apilado sobre #16), CI PASS. Piloto
Placement v2 final: 24/24 HTTP y 24/24 CPU/PSS válidos, controls_restored y cleanup
true, originales preservados, VM apagada. Evidencia versionada en
evidence/sprint5/benchmarks/compact-v2-pilot-20261031. El piloto no acepta 72 ventanas oficiales. La siguiente
sesión usa el supervisor complete_placement_campaign.py con perfil v2 y PR #17.
Estado/servicio actuales se consultan en followup-status.json del workspace
superior; no usar los servicios 02/03 fallidos como si siguieran ejecutándose.


## Estado vigente: Placement aceptado y preflight Glance aprobado

2026-10-31: Placement compacto v2 completado, 72/72 HTTP y CPU/PSS; aceptación,
restauración, limpieza, controles y apagado verificados. CI PASS sobre 13bb013;
PR #17 listo, sin merge. La alerta de apagado fue lectura locale español de
virsh, corregida con LC_ALL=C; no requiere repetir mediciones.

Trabajo nuevo en /home/paolo/Proyectos/TrabajoTitulo/.worktrees/glance-compact-v2,
rama feat/glance-compact-v2. Instrumentos 85a78de y preflight real 42/42 aprobado,
con evidencia.
Glance v2 todavía NO tiene piloto de rendimiento ni ventanas oficiales.
Pendientes: RPC/coordinador, análisis CPU/PSS, supervisor durable, piloto36,
congelación y oficial108; luego conjunta24bloques/36boots. No usar las matrices
v1 de los apartados históricos anteriores. VM apagada al cerrar preflight.
Consultar primero followup-status.json superior para estados que avancen.
