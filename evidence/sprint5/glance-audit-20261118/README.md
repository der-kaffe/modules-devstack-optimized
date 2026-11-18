# Auditoría medida: Glance oficial frente a Glance Go

Fecha 2026-11-18 (UTC en la VM; 14:57 en Chile). VM local `tt-openstack-lab` con 6 GiB, Keystone y
Nova reales.
- Glance oficial `a3e5f490`, con su configuración real de DevStack (incluidas las notificaciones).
- Glance Go: binario `4fb59f8f…` (`main` `594a024`). Código de la suite `b3ef1e4`.

Comando: `glance_suite.py --audit`.
- **Misma carga, misma ruta pública** de Apache (`/image`), mismo cliente (`glance_http.Client`) y mismo token de Keystone.
- Primero el oficial, antes del switch (`audit_original`), y después Glance Go, tras el switch (`audit_go`).
- Las descargas verifican longitud y SHA-256 en el cliente.
- El listado filtra por un tag propio, para que los dos devuelvan las mismas 10 imágenes.

**No es una campaña:** hay pocas repeticiones, la VM es de 6 GiB y el cliente corre dentro de la VM.

## Resultado: Go no es más lento en ninguna operación

p50 en ms (corrida 1 / corrida 2). En la última columna, un valor menor que 1 significa que Go es más rápido.

| Operación | Oficial | Go | Go/oficial |
|---|---|---|---|
| Descubrimiento `GET /` | 2.66 / 1.88 | 0.82 / 0.81 | 0.308 / 0.431 |
| Consulta de imagen | 19.4 / 8.92 | 2.02 / 1.9 | 0.104 / 0.213 |
| Listado (10 imágenes con tag) | 13.36 / 15.32 | 2.34 / 2.47 | 0.175 / 0.161 |
| Esquema `image` | 3.61 / 3.83 | 0.54 / 0.54 | 0.15 / 0.141 |
| Crear (POST) | 312.76 / 298.32 | 5.34 / 5.16 | 0.017 / 0.017 |
| PATCH | 48.2 / 45.63 | 7.19 / 6.8 | 0.149 / 0.149 |
| DELETE | 40.87 / 39.65 | 10.07 / 8.4 | 0.246 / 0.212 |
| Carga de 1 MiB | 561.04 / 575.39 | 44.12 / 46.22 | 0.079 / 0.08 |
| Carga de 64 MiB | 983.75 / 970.21 | 500.19 / 517.32 | 0.508 / 0.533 |
| Descarga de 1 MiB | 16.24 / 17.55 | 3.68 / 3.88 | 0.227 / 0.221 |
| Primer byte, 1 MiB | 15.13 / 16.19 | 2.81 / 2.78 | 0.186 / 0.172 |
| Descarga de 64 MiB | 100.92 / 111.22 | 66.27 / 71.43 | 0.657 / 0.642 |
| Primer byte, 64 MiB | 18.93 / 16.89 | 3.04 / 3.07 | 0.161 / 0.182 |
| Consulta con 16 hilos | 72.68 / 76.11 | 10.88 / 10.72 | 0.15 / 0.141 |

Throughput de la consulta con 16 hilos: oficial 215 / 204 req/s; Go 1 149 / 1 134 req/s.

Las dos corridas pasaron 12/12 casos, con limpieza completa: catálogo y store
originales sin cambios, y runtimes, identidades y controles retirados. El detalle
por operación (n, p50 y p95) está en `run-N/audit.json`.

## Decisión

Por la regla de Juan, sólo se cambia lo que en Go es distinto y más lento que el
original. En esta medición no hay ninguna operación así, y con eso termina la
auditoría.

La diferencia de coordinación que queda (16 transferencias compartidas,
configurable) no es de velocidad. Está documentada en
[performance.md](../../../docs/glance/performance.md).
