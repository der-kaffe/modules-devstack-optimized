> Informe histórico de la primera calibración. Las sesiones posteriores sí modificaron temporalmente configuraciones y rutas, y tienen sus propias pruebas de restauración. Consultar [resultados posteriores](../results-20261031.md), especialmente `restoration-final` y `normal-01/session-01/restoration-repaired`. Este texto no describe el alcance de todas las sesiones.

# Estado de restauración — 2026-10-31

Ámbito real: sesión de calibración de capacidad de tt-openstack-lab. No hubo
cambio de rutas de Glance/Placement, despliegue Go, tokens, SQL de escritura ni
creación de imágenes/instancias. No se inició una campaña de rendimiento.

Estado final observado por libvirt: **shut off**. Supervisor terminó con
`phase=RESTORED_OFF`, sin errores. XML persistente coincide byte a byte con el
respaldo previo: SHA256
`e4cecb489fa28b68c5267ec9e803b1736d1d85e225fc3ad5e65dd78c97c028d5`.
UUID: `385591e9-02ea-4209-b09e-49f3f882a8e6`.

## Operaciones y controles

1. Confirmar VM apagada, UUID/disco correctos y sin QEMU/campaña observados.
2. Backup externo del XML; lock host exclusivo; preflight >=5 GiB disponibles.
3. Cambiar currentMemory persistente a 4 GiB sólo para arrancar calibración.
   Inmediatamente después del start, restaurar configuración persistente de
   6 GiB y comparar XML. La sesión activa conservó 4 GiB; no se confundió con 6.
4. Supervisor finito de 900 s, muestreo host cada 5 s, corte por memoria<512 MiB
   y shutdown ACPI, sin destroy. La parada se solicitó antes del límite.
5. SSH rechazó known_hosts histórico. Verificar key pública mediante guest-agent
   libvirt de esa VM y MAC/IP del invitado. Crear known_hosts **nuevo externo**;
   conservar intacto el existente. Fingerprint verificado:
   `SHA256:eFLh7QOqGVyX737yituVNnK4jAnJRhQ7RURuIeiuJiU`.
6. Preflight dentro de lock B15 por `flock`, sólo lecturas. Glance/Placement,
   Nova API/compute/scheduler, Keystone, MySQL y Apache activos. `/image/v2/images`
   sin token401; `/placement/`200. Ninguno es benchmark ni gate de boot.
7. Presión del invitado impide cargas: MemAvailable144288 KiB (140.91 MiB),
   swap usada1623368 KiB (1585.32 MiB). No hubo OOM en la consulta del boot.
   Se conservó instance-00000001 apagada. No iniciar experimentos con ese margen.
8. Solicitar shutdown ordenado de la VM externa; esperar hasta shut off;
   comparar XML persistente otra vez. El supervisor y sus generadores finalizaron.

Los hashes de configuraciones originales del invitado se capturaron sin contenido
en lab-capacity-preflight.json. No se editaron esas configuraciones ni endpoints.
No se hizo una segunda lectura de sus archivos tras apagar el invitado; no se
afirma una comparación antes/después inexistente. Sí se verificó dos veces el XML
persistente. El lock vacío creado en /run/lock pertenece al tmpfs del boot y no se
retiró durante operaciones para evitar carreras; desaparece al apagar/reiniciar.

## Evidencia persistente externa

Base exacta:
`/home/juanc/.local/share/bio-harness/workspaces/d64feb577c19cd022749420b3c83db8a/benchmarks-endtoend-20261031/`

- vm-before.xml, vm-persistent-restored.xml y capacity-state.json.
- capacity_session.py y capacity-host-memory.jsonl (supervisor limitado).
- guest-agent-key-result.json y known_hosts-guest-agent (sólo clave pública).
- lab_readonly_preflight.py y lab-capacity-preflight.json.

No quedan recursos sintéticos de laboratorio de esta tarea. La imagen/backend y
la instancia históricos B26 permanecen ajenos a la limpieza de esta misión.
La sesión de capacidad está restaurada; esto no repara ni revalida el N7 histórico
de la instancia interior. No quedó job de benchmark ni publicación Git en curso.
