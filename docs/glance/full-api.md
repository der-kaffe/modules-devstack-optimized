# Glance Go: API completa sobre el Glance original

Además de las 7 operaciones que usa Nova (crear, listar, ver, editar, borrar,
subir y bajar), Glance Go implementa el resto de la API v2 que DevStack deja
activa. Todo funciona sobre las mismas tablas, archivos y `glance-api.conf`
del original.

| Parte | Rutas | Tablas del original | Configuración que lee |
|---|---|---|---|
| Miembros | `/v2/images/{id}/members[/{member}]` | `image_members` | — |
| Tags por ruta | `PUT/DELETE /v2/images/{id}/tags/{tag}` | `image_tags` | — |
| Desactivar / reactivar | `POST /v2/images/{id}/actions/{deactivate,reactivate}` | `images` | — |
| Cuotas de Keystone | `/v2/info/usage`; límites al crear y subir | uso desde `images`, `image_locations`, `image_properties` | `use_keystone_limits`, `[oslo_limit]` |
| Varios stores | `/v2/info/stores[/detail]`, `X-Image-Meta-Store`, `copy-image`, `stores`/`all_stores`, `DELETE /v2/stores/{store}/{id}`; campo `stores` | `image_locations` (`meta_data.store`) | `enabled_backends`, `[glance_store] default_backend`, `[<store>] filesystem_store_datadir`, `store_description`, `weight`, `[os_glance_staging_store]` |
| Ubicaciones | `GET/POST /v2/images/{id}/locations`, PATCH `/locations`, `direct_url` | `image_locations`, `tasks` | `show_image_direct_url`, `show_multiple_locations` |
| Inspección de formato | al subir e importar: rechazo 415 y `virtual_size` | `images` | `[image_format] require_image_format_match`, `gpt_safety_checks_nonfatal` |
| Políticas y protecciones | todas las acciones; propiedades protegidas | — | `[oslo_policy] policy_file`, `property_protection_file`, `property_protection_rule_format` |
| Plugins de importación | `image_conversion` (qemu-img), `image_decompression`, `inject_image_metadata`, `no_op` | `images`, `image_properties` | `glance-image-import.conf` |
| Cuotas locales | límites al crear, subir y editar | `images` | `user_storage_quota`, `image_size_cap`, `image_{member,property,tag}_quota` |
| Borrado diferido | `DELETE` deja `pending_delete`; `glance-scrubber` lo completa | `images`, `image_locations` | `delayed_delete`, `scrub_time` |
| Límites de Keystone en caché | los límites de un proyecto se reutilizan unos segundos | — | `GLANCE_GO_LIMITS_CACHE_SECONDS` (5 por defecto; 0 pregunta siempre, como el original) |
| Otros | `POST /v2/tasks` (tipo `import`), `/healthcheck` | `tasks` | `[DEFAULT] max_retries`/`retry_interval` de `[database]` |
| Importación interoperable | `PUT /stage`, `POST /import`, `/v2/info/import` | `images`, `image_properties` (`os_glance_import_task`) | `node_staging_uri`, `enabled_import_methods`, `[import_filtering_opts]` |
| Tareas | `/v2/tasks`, `/v2/tasks/{id}`, `/v2/images/{id}/tasks` | `tasks`, `task_info` | `[task] task_time_to_live` |
| Caché de imágenes | `/v2/cache[/{image_id}]`; descargas servidas desde la caché | `cached_images`, `node_reference` | `[paste_deploy] flavor`, `image_cache_dir`, `worker_self_reference_url`, `cache_prefetcher_interval` |
| Metadefs | `/v2/metadefs/namespaces/...`, `/v2/metadefs/resource_types` | `metadef_*` (6 tablas) | — |
| Schemas | `/v2/schemas/*` (16 documentos) | — | — |

## Cómo se verifica

- `make check`: pruebas unitarias con `-race` de cada capa.
- `make glance-test-integration`: 11 suites sobre una copia del DDL real.
  `members` cubre miembros, tags, acciones y uso. `imports` cubre importación,
  tareas, recuperación y caché de punta a punta por HTTP.
- `scripts/integration/glance_differential.py`: la misma secuencia contra el
  original vivo y contra Glance Go, con las respuestas normalizadas.
  Resultado del 2026-11-23: 449 de 450 casos idénticos
  (`evidence/sprint5/glance-full-api-20261123/differential.json`).

## Diferencias declaradas

- Un 400 de validación de metadefs no incluye el volcado de `jsonschema` que
  el original agrega después de la primera línea.
- La importación escribe los datos por el mismo camino que una subida:
  durante la escritura la imagen se ve `saving` (el original la muestra
  `importing` hasta quedar `active`).
- La caché copia una descarga completa en segundo plano desde el store (el
  original la copia mientras responde). El resultado es el mismo, y la primera
  descarga conserva `sendfile`.

## Herramientas en Go

`glance-manage` (`db sync|version|upgrade|check|purge|purge_images_table`,
`db load_metadefs|unload_metadefs|export_metadefs`), `glance-cache-pruner`,
`glance-cache-cleaner` y `glance-scrubber`. El plugin DevStack las instala en
`/usr/local/bin` y, por defecto (`GLANCE_GO_INSTALL_ORIGINAL=False`), no
instala el Glance de Python: el esquema de la base lo crea `glance-manage` Go
con el DDL de la revisión `2024_1_contract01`.

## Lo que no implementa todavía

- `glance-download` (importación entre regiones) y stores que no son de
  archivos (rbd, swift, cinder, s3, http): `enabled_backends` acepta solo
  `file`.
- `/v2/info/stores/detail` informa `chunk_size` 65536 y
  `thin_provisioning` false fijos para los stores de archivos.
