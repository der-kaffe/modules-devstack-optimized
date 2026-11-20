# Observación Nova y gates conjuntos

Protocolo propio: [protocol-v1.json](protocol-v1.json), estado borrador.
Plan y controles: [plan experimental](../glance/experimental-plan-v1.md).
Preparación, locks y restauración: [runbook Glance](../glance/runbook.md).

`scripts/benchmarks/integrated_boot.py` observa **una** creación secuencial en
la configuración ya preparada. No modifica Nova, Apache, Keystone ni endpoints;
no ejecuta switches A/B/C/D. No es un coordinador completo ni certifica D.

Con laboratorio disponible, en el invitado, SDK desplegado y openrc privado:

```sh
python3 scripts/benchmarks/integrated_boot.py observe \
  --configuration A --allow-mutations \
  --image-id UUID_IMAGEN --image-sha512 HASH_SHA512_VERIFICADO \
  --flavor-id UUID_FLAVOR --network-id UUID_RED \
  --placement-url http://192.168.122.10/placement \
  --output /ruta/privada/nueva/observacion-A
```

El instrumento verifica catálogo local, imagen ACTIVE/hash, flavor/red; guarda
baseline y CREATE_INTENT antes de POST; metadata tt_campaign y nombre exacto
permiten recuperar una respuesta perdida. Polling monotónico hasta ACTIVE,
verificación de allocations del flavor, DELETE sólo propio y espera de ausencia
y allocations vacías. Recursos originales retenidos forman parte del baseline.
Fallo conserva NEEDS_ATTENTION y clase de error sin cuerpo fault ni credenciales.
Timeouts de API SDK 15 s, boot<=600 s, delete<=180 s; no concurrencia Nova.

Recuperación con mismo ledger/output y lock VM:

```sh
python3 scripts/benchmarks/integrated_boot.py cleanup --allow-mutations \
  --output /ruta/privada/nueva/observacion-A
```

El resultado etiqueta configuration_claimed y mantiene deployment_identity_verified,
gate_accepted, official_accepted y restoration_verified en false. Es deliberado:
observar ACTIVE y allocations no demuestra qué binarios atendieron las requests
de Nova ni que se restauraron servicios. Hace falta evidencia externa correlacionada:

| Gate | Glance efectivo | Placement efectivo | Evidencia mínima adicional |
| --- | --- | --- | --- |
| A | Python | Python | Config y PIDs/hash originales, requests Nova, ACTIVE y cleanup |
| B | Go | Python | Hash binario Glance, DB/store propios, requests metadata/bytes correlacionadas |
| C | Python | Go | Runtime/route Placement propio, tracker/candidates/allocations de Nova |
| D | Go | Go | Ambas identidades y flujos **en el mismo boot**, no inferencia B+C |

Conservar imagen/hash/flavor/red iguales por bloque; imagen lógica puede tener
UUID distinto al copiar a stores propios, registrando mapa de IDs y hash igual.
Registrar caché Nova y si la descarga se observó; boot caliente/frío se separan.
No atribuir tiempo hasta ACTIVE exclusivamente a Glance. Verificar dentro de la
VM sólo cuando exista vía autorizada; este observador no inyecta SSH/user data.

Faltan probar el SDK real, lifecycle de procesos propios, switch combinado con
rollback/hash y recolección correlacionada por servicio. Tras superar A/B/C/D y
restauración, congelar una revisión nueva del protocolo y desarrollar/habilitar
el coordinador oficial; no basta cambiar un booleano en el JSON actual.
