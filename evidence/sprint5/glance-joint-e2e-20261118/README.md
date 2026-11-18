# E2E con Nova: Glance Go solo y Glance Go + Placement Go

Fecha local 2026-11-18. Revisión `f7ea161` (rama `feat/glance-integration-suite`).
Binarios estáticos `CGO_ENABLED=0`, Go 1.27.3: `glance` sha256 `ca63516f…` y
`placement` sha256 `c342bb64…`. VM local `tt-openstack-lab`: 4 vCPU, 6 GiB.
Flavor `m1.nano`, red `private`, imagen CirrOS 0.6.3 subida **sólo** a Glance Go.

Comandos ([uso](../../../docs/glance/b26-integration-suite.md)):

```sh
python3 scripts/integration/glance_suite.py ... --allow-mutations                      # glance-N
python3 scripts/integration/glance_suite.py ... --with-placement-go \
  --placement-binary bin/placement --allow-mutations                                     # joint-N
```

## Resultado

| Corrida | Modo | Casos | Limpieza | Nova BUILD→ACTIVE |
|---|---|---|---|---|
| `glance-1` | Glance Go | 10/10 | completa | ver `report.json` |
| `joint-1` | Glance Go + Placement Go | 14/14 | completa | 6,6 s |
| `glance-2` | Glance Go | 10/10 | completa | ver `report.json` |
| `joint-2` | Glance Go + Placement Go | 14/14 | completa | ver `report.json` |

Las dos corridas de cada modo son consecutivas, sobre la misma revisión y
cada una con recursos privados nuevos, igual que el criterio de B15.

## Qué acredita el modo conjunto (`joint-1`)

1. **Placement Go activo:**
   - la ruta `/placement` de Apache apunta a Go y el Placement original está detenido;
   - tras reiniciar nova-compute y nova-scheduler (como en B13), Nova publicó su proveedor en Placement Go en 4,2 s, con inventario `VCPU 4`, `MEMORY_MB 5918` y `DISK_GB 52`;
   - el proveedor empezó sin uso.
2. **Glance Go activo:** la ruta `/image` apunta a Go y el Glance original está detenido. La imagen sólo existe en Go.
3. **Nova arrancó la VM** con los dos servicios Go a la vez (`BUILD → ACTIVE`).
4. **La allocation vive en Placement Go** y coincide con el flavor: `VCPU 1`, `MEMORY_MB 192`, `DISK_GB 1`.
5. **Flujo Nova → Placement Go** (`placement-flow.json`): seis llamadas, todas del usuario de servicio de Nova y ninguna ajena.

   ```
   t=0 ms    GET /allocation_candidates               200   (scheduler)
   t=82 ms   GET /allocations/{consumer}               200
   t=127 ms  PUT /allocations/{consumer}               204   (claim)
   t=539 ms  GET /allocations/{consumer}               200
   t=598 ms  GET /resource_providers/{id}/allocations  200
   t=611 ms  GET /allocations/{consumer}               200
   ```

6. **Flujo Nova → Glance Go** (`flow.json`, `flow.mmd`): siete llamadas atribuidas a Nova, tres lecturas de metadatos, descarga completa de la imagen y ninguna solicitud ajena.
7. Al borrar el servidor, **la allocation se liberó en Placement Go** y el proveedor volvió a quedar sin uso.
8. **Restauración y limpieza:**
   - las dos rutas vuelven a los originales y Nova se reinicia contra el Placement original;
   - el catálogo de Glance Python y el estado de Placement original quedan iguales que al inicio (sin contar `generation`);
   - se retiran los dos runtimes Go (MySQL propios), las identidades temporales y el validador del proyecto `service`;
   - los controles de mantenimiento se restauran.

## Límites

- Prueba funcional, no de rendimiento. VM de 6 GiB, no el perfil de 8 GiB de las campañas.
- La atribución dentro de Glance (nova-api frente a nova-compute) es por orden temporal: keystoneauth registra en WARN (ver [nova-glance-flow](../../../docs/glance/nova-glance-flow.md)).
- No se tocó el servidor preexistente `b26-ubuntu-20261029a` (SHUTOFF). Placement Go no recibe sus allocations y no hacen falta para la prueba.
- Las imágenes de esta prueba usan el store de disco. RBD se probó aparte ([glance-rbd-20261118](../glance-rbd-20261118/README.md)).
