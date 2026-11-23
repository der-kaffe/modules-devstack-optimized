# Campaña Glance v7: diseño

Estado: **protocolo congelado** (`docs/benchmarks/glance-protocol-v7.json`, 2026-11-23). Mismo diseño que la campaña de Placement. Reemplaza a la v6 como medición de
referencia de Glance Go frente al Glance original.

## Por qué medir de nuevo

- La v6 (360/360 ventanas, 2026-11-22) midió el binario `a69474d`. Desde
  entonces, la PR #28 completó la API v2 y dejó Glance 100 % en Go por defecto,
  y la PR #29 optimizó el código nuevo. El binario que DevStack usa hoy no es
  el medido.
- El plugin ya no instala el Glance de Python por defecto. Para comparar, la v7
  instala DevStack con `GLANCE_GO_INSTALL_ORIGINAL=True`: el original queda
  instalado y apagado, igual que en la v6. Al terminar, DevStack se reinstala
  sin Python de Glance (orquestador, paso 7).

## Métricas mínimas: las del informe de Placement

El informe `overleaf-placement-python-vs-go` fija lo mínimo que se reporta por
celda. Todas estas métricas ya las registra el arnés de Glance (`summary.json`):

| Métrica (informe de Placement) | Campo en el `summary.json` de Glance |
|---|---|
| Peticiones correctas por segundo | `correct_ops_s` |
| Latencia p50 / p95 / p99 | `p50_ms`, `p95_ms`, `p99_ms` |
| Errores (0 exigido) | `requests` − correctas; la ventana se invalida si hay error |
| CPU del servicio por petición | `resources.cpu_s_per_correct_request` |
| CPU como % de 4 núcleos | `resources.cpu_fraction_of_four_cores` |
| PSS media, pico y en reposo | `resources.pss_mean_bytes`, `pss_peak_observed_bytes`, `idle_pss_mean_bytes` |
| RSS pico (solo referencia) | `resources.rss_peak_observed_bytes` |
| CPU de la pila por petición (MySQL, Apache, Keystone) | `controls.{mysql-*,apache,keystone}.cpu_seconds` / `correct_requests_common_interval` |
| PSS de MySQL | `controls.mysql-*.pss_mean_bytes` |
| Escalamiento de 1 a 16 clientes | `correct_ops_s` con C=16 / C=1 |
| Comparación entre binarios (octubre/noviembre en Placement) | v6 (`a69474d`) frente a v7, solo para Go |

**Glance ya mide más que Placement:**
- el CPU y la PSS de memcached (la caché de tokens del original), que el
  informe de Placement declara no haber medido;
- el TTFB de las descargas (`download_ttfb_ms`) y los MiB/s validados;
- los descriptores abiertos (crecimiento por ventana);
- la memoria del cgroup (anónima y de archivo);
- las peticiones de más de 10 s.

## Cómo se mide (igual que en la v6 y en Placement)

- **Entorno:**
  - VM de 4 vCPU con DevStack;
  - Glance original (uWSGI) y Glance Go detrás de la misma ruta `/image` de
    Apache, con el mismo MySQL y la misma base `glance`, el mismo store de
    archivos, el mismo usuario de servicio de Keystone (Go lee el
    `glance-api.conf` del original) y notificaciones `noop` en los dos.
- **Carga:**
  - lazo cerrado: cada cliente mantiene una petición pendiente, sin pausas;
  - 10 s de calentamiento y 30 s medidos por ventana;
  - un ciclo empezado antes del plazo se completa y cuenta en el denominador.
- **Validación:**
  - cada respuesta se valida completa: estado HTTP, campos de metadatos,
    bytes y hashes (MD5, SHA-512) de cada subida y descarga;
  - cada imagen creada se limpia y se verifica su ausencia;
  - un error invalida la ventana.
- **CPU:** ticks de todos los procesos del servicio (master y workers de uWSGI,
  o el proceso de Go) dentro de la ventana, divididos por las peticiones
  correctas del intervalo común. Los controles (MySQL, Apache, Keystone,
  memcached) se miden igual, en la misma ventana.
- **Memoria:**
  - PSS muestreada a 1 Hz;
  - RSS solo como referencia, porque cuenta dos veces las páginas compartidas
    entre los workers de uWSGI;
  - PSS en reposo antes de la carga.
- **Diseño (como Placement):**
  - cargas × concurrencias 1 y 16 × 3 pares Python/Go, con la misma semilla
    (`order_seed`) y orden aleatorio de pares;
  - cada valor del informe es la mediana de 3 pares; se publica también cada
    resultado y el rango;
  - los cocientes son cocientes de medianas;
  - ya no hay concurrencias 4 y 8, que sí tenía la v6.

## Cargas

### 1. Las 9 de la v6 (comparables con la v6 en C = 1 y 16)

`catalog-list`, `catalog-show`, `catalog-lifecycle`, `upload-1m|16m|64m` y
`download-1m|16m|64m`. La comparación v6 frente a v7 usa solo las celdas
C = 1 y 16 de la v6 (mediana de sus 5 repeticiones frente a la de 3 pares).

### 2. Cargas propias de Glance (3 nuevas)

Ejercitan lo que la v6 no tocaba y lo que usan Nova, Horizon y la CLI:

| Carga | Ciclo por cliente | Qué mide |
|---|---|---|
| `import-direct-16m` | crear → `PUT /stage` → `POST /import` (glance-direct) → `GET` hasta `active` (pausa de 20 ms entre consultas, fuera de la latencia) → verificar hashes → borrar | importación interoperable con tareas (la que usa `openstack image create --import`) |
| `upload-qcow2` | crear con `disk_format=qcow2` → subir un qcow2 real (CirrOS) → verificar `virtual_size` → borrar | inspección de formato (`require_image_format_match`, activa en DevStack) |
| `nova-mix` | lo que hace Nova al arrancar desde una imagen: `GET` de la imagen, `GET /file` de 16 MiB y listado `status=active&visibility=private` | carga realista del cliente principal |

### Matriz

12 cargas (9 de la v6 y 3 nuevas) × C = 1 y 16 × 3 pares × 2
implementaciones = **144 ventanas oficiales**, el mismo número que el informe
de Placement. **Sin piloto:** solo la campaña oficial. Si una ventana falla,
la campaña se reanuda desde el primer par incompleto.

Quedan fuera de la matriz, por ser lecturas de metadatos parecidas a
`catalog-show` y `catalog-list`: compartir con miembros, el listado con filtros
y metadefs. Su compatibilidad ya está cubierta por la comparación diferencial
(449/450) y la integración.

## Duración estimada

La v6 tomó 6,4 h para 360 ventanas (64 s por ventana).

| Parte | Tiempo |
|---|---|
| 144 ventanas oficiales | ≈ 2,6 h |
| Dos `stack.sh` y las suites | ≈ 1,5 h |
| **Total** | **≈ 4 h** |

La v6 se quedó sin memoria en la ventana 231. La v7 se lanza con `setsid`,
fuera de Claude Code y con lo mínimo abierto en el PC. Además se puede
reanudar desde el primer par incompleto (`reanudar.sh`).

## Informe

Con la misma estructura que el informe de Placement, generado con un
`extract.py` y un `generar_tex.py` propios:
1. un recuadro con el resultado principal;
2. una tabla de indicadores (rango de cocientes);
3. la sección «Cómo se midió»;
4. memoria (PSS media, pico y reposo; RSS pico);
5. CPU por petición;
6. CPU de toda la pila por componente (Glance, MySQL, Apache, Keystone y,
   en Glance, memcached);
7. peticiones por segundo;
8. latencia p50/p95/p99 y TTFB de descarga;
9. escalamiento de 1 a 16 clientes;
10. v6 frente a v7 (solo Go);
11. limitaciones;
12. la tabla completa en un anexo.

## Limitaciones conocidas

- Se comparan **despliegues**, no lenguajes: workers de uWSGI frente a un proceso de Go,
  y pools de conexiones distintos.
- Tres pares por celda y dos concurrencias no dan significancia
  estadística. Se reportan medianas y rangos.
- Sin C = 4 y 8, el escalamiento se ve solo entre 1 y 16 clientes.
- Es un laboratorio de un nodo con store de archivos. No hay rbd, swift,
  cinder ni s3.
- El PC de Juan no reproduce el hardware de la campaña de Paolo: los
  cocientes se comparan dentro de cada campaña.
