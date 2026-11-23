# modules-devstack-optimized

Reimplementación en Go de dos servicios de OpenStack, **Glance** (imágenes) y **Placement** (inventario de recursos), compatibles con los originales en Python. Trabajo de Título INFO-1197, Universidad Católica de Temuco.

## Resumen

- **Glance Go** reemplaza al servicio de imágenes de OpenStack con el mismo catálogo MySQL, el mismo directorio de imágenes y el mismo `glance-api.conf`. Nova, Horizon y la CLI no notan el cambio.
- **Placement Go** reimplementa providers, inventories, clases, traits, allocations y candidates del perfil plano, comparados respuesta por respuesta contra Placement Python.
- Se instalan como un **plugin de DevStack** (`enable_plugin`) y se miden contra los servicios originales bajo las mismas condiciones: misma ruta de Apache, mismo MySQL, mismo Keystone.
- Cada comportamiento se verifica contra el servicio original antes de darlo por bueno: pruebas con `-race`, integración con MySQL real y comparación diferencial con el servicio vivo.

## Estado

- **Glance Go:** catálogo (crear, listar, ver, editar, borrar), subida y descarga, autenticación Keystone con caché de tokens, notificaciones `image.*` y plugin de DevStack que sustituye a `g-api` por `g-go`. Detalle en [`docs/glance/`](docs/glance/replace-original.md) y [`docs/glance/devstack-plugin.md`](docs/glance/devstack-plugin.md).
- **Placement Go:** providers, inventories, clases y traits, allocations con generation y consumers, candidates del perfil plano y políticas de Keystone. Detalle en [`docs/placement/`](docs/placement/b07-acceptance.md).
- **Pendiente de llegar a `main`:** la API v2 completa de Glance (miembros, tareas, importación, metadefs, caché, cuotas, políticas, varios stores), `glance-manage`, `glance-cache-pruner`, `glance-cache-cleaner` y `glance-scrubber` en Go, y la instalación por defecto sin Glance en Python. Está implementado y mergeado en la rama `perf/glance-full-api` del repo de Paolo.

## Uso rápido

En `local.conf` de un DevStack (siempre en la VM de laboratorio, nunca en la estación de trabajo):

```ini
[[local|localrc]]
enable_plugin glance-go https://github.com/PaoloCastanon/modules-devstack-optimized main
```

Desarrollo local, sin iniciar la VM ni contactar OpenStack:

```bash
make check          # formato, vet, pruebas con -race, build y chequeos del harness
make glance-build   # compila cmd/glance
make placement-build
```

`make check` necesita Python 3, Make, Bash, ShellCheck, Terraform, Ansible y Go.

## Estructura

| Ruta | Contenido |
|---|---|
| `cmd/glance`, `cmd/placement` | Los ejecutables de cada servicio |
| `internal/glance`, `internal/placement` | Dominio, API, servicio, repositorio MySQL y store; separados por capas, sin importarse entre sí |
| `internal/auth`, `internal/config` | Keystone y lectura de `glance-api.conf` / `placement.conf`, compartidos |
| `devstack/` | Plugin `glance-go` para `enable_plugin` |
| `infra/` | Laboratorio: Terraform, Ansible y guía de DevStack |
| `scripts/`, `tests/` | Pruebas de integración, comparación diferencial y arnés de campañas |
| `docs/` | Diseño, decisiones y guías de cada servicio |
| `harness/` | Registro fechado de cada verificación (`build-log.md`) |

## Documentación

- [Plan por fases](PLANS.md) y [reglas para agentes](AGENTS.md)
- [Arquitectura objetivo](docs/sprint2/go-target-architecture.md) y [decisiones de diseño](docs/sprint2/design-decisions.md)
- [Estrategia de compatibilidad](docs/sprint2/compatibility-strategy.md)
- [Registro de verificaciones](harness/build-log.md)
- [Guía del laboratorio](infra/devstack/README.md)

La VM `tt-openstack-lab` permanece apagada mientras no se usa. Que pasen los controles locales no acredita por sí solo la disponibilidad del laboratorio.
