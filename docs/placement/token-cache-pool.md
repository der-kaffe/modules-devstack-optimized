# Placement Go: caché de tokens y pool de MySQL

2026-11-20. Rama `feat/placement-token-cache-pool`. Motivo: en la campaña
compact-v2 (B17), Go quedó por debajo de Python en las lecturas simples
(provider y allocations-read, ~0,61–0,67× en throughput). Dos causas probables
estaban en la configuración, no en el lenguaje:

1. **Validación de tokens.** Go validaba cada token contra Keystone en cada
   petición. Placement oficial usa keystonemiddleware con
   `token_cache_time = 300` s y reutiliza la validación. Glance Go ya tenía ese
   caché (`internal/auth/token_cache.go`), pero Placement no lo activaba.
2. **Pool de MySQL.** El pool estaba fijo en 4 conexiones abiertas y 2
   inactivas. Con 16 clientes concurrentes, las peticiones esperaban conexión.

## Cambio

| Variable | Valor por defecto | Rango | Efecto |
|---|---|---|---|
| `PLACEMENT_GO_TOKEN_CACHE_TIME` | `300` | `-1` (desactivado) o `1..86400` s | Igual que `token_cache_time` de keystonemiddleware |
| `PLACEMENT_GO_DB_MAX_CONNECTIONS` | `16` | `1..256` | Conexiones abiertas e inactivas del pool de `serve` |

- El caché es el mismo que usa Glance Go: LRU acotado a 10 000 entradas, guarda
  solo el hash SHA-256 del token, nunca cachea fallos y revisa la expiración del
  token en cada uso.
- El modo `migrate` conserva el pool pequeño de `mysql.Open`.
- Las variables de Glance no configuran Placement, y `DB_MAX_CONNECTIONS` no
  aplica a Glance. Lo verifican las pruebas de `internal/config`.
- El log de arranque informa los dos valores efectivos.

## Efecto sobre pruebas anteriores

**Revocación (B12).** Con el caché activo, un token que Go ya validó sigue
aceptado hasta 300 s después de revocarse. Es el mismo comportamiento que
keystonemiddleware en el original. La fase `revoked` de B12 validaba el token en
Go *antes* de revocarlo y esperaba 401. Para repetirla tal cual hay que usar
`PLACEMENT_GO_TOKEN_CACHE_TIME=-1`, o bien comparar ambos servicios con el token
ya visto por los dos.

## Resultado en la VM (2026-11-20)

Protocolo `placement-compact-v2-reads`: 36/36 ventanas válidas, 0 errores,
original restaurado y VM apagada. Evidencia.
Medianas de tres pares; entre corchetes, el rango observado del cociente pareado.

| Carga | Conc. | Python ops/s | Go ops/s | Go/Python | p50 Py/Go (ms) | p99 Py/Go (ms) |
|---|---:|---:|---:|---:|---:|---:|
| provider | 1 | 302 | 2064 | 6,83 [6,75–6,84] | 3,1 / 0,4 | 5,0 / 0,7 |
| provider | 16 | 721 | 3516 | 4,88 [4,87–4,88] | 21,4 / 4,2 | 36,6 / 8,6 |
| inventories | 1 | 191 | 1403 | 7,28 [7,24–7,47] | 5,0 / 0,6 | 7,7 / 1,1 |
| inventories | 16 | 440 | 3285 | 7,47 [7,14–9,52] | 35,3 / 4,5 | 52,6 / 9,2 |
| allocations-read | 1 | 201 | 1339 | 6,67 [6,16–7,02] | 4,6 / 0,6 | 8,0 / 1,8 |
| allocations-read | 16 | 519 | 3289 | 6,26 [6,23–6,45] | 29,2 / 4,6 | 48,5 / 9,5 |

CPU por petición correcta: Go/Python 0,060–0,065. PSS: Python ~182–185 MiB,
Go ~15–16 MiB.

En compact-v2 (31-oct, sin caché y con pool de 4) Go hacía 0,61–1,04× el
throughput de Python en estas celdas; ahora hace 4,9–7,5×. Python mide
valores similares a los de octubre (provider c1: 280 → 302 ops/s), lo que
indica un entorno comparable. La desventaja anterior se debía a la
validación de tokens contra Keystone en cada petición y al pool reducido, no
al lenguaje.

### Candidates, claim-cycle y nova-mix

Protocolo `placement-compact-v2-nova`, mismo binario: 36/36 ventanas válidas,
0 errores, original restaurado y VM apagada. Evidencia.

| Carga | Conc. | Python ops/s | Go ops/s | Go/Python | p50 Py/Go (ms) | p99 Py/Go (ms) | Go oct → nov |
|---|---:|---:|---:|---:|---:|---:|---:|
| candidates | 1 | 41 | 389 | 9,40 [8,60–9,60] | 22,5 / 1,8 | 43,7 / 3,6 | 120 → 389 (3,2×) |
| candidates | 16 | 68 | 647 | 9,47 [9,35–9,53] | 227,9 / 22,2 | 323,4 / 48,7 | 404 → 647 (1,6×) |
| claim-cycle | 1 | 39 | 121 | 2,98 [2,93–3,20] | 25,4 / 5,9 | 81,2 / 25,6 | 97 → 121 (1,25×) |
| claim-cycle | 16 | 60 | 849 | 14,14 [13,94–15,19] | 235,6 / 19,5 | 844,9 / 58,4 | 320 → 849 (2,65×) |
| nova-mix | 1 | 54 | 294 | 5,44 [5,34–5,58] | 14,8 / 1,9 | 56,4 / 19,9 | 108 → 294 (2,7×) |
| nova-mix | 16 | 99 | 1070 | 10,86 [10,27–11,26] | 170,0 / 11,7 | 392,2 / 44,7 | 401 → 1070 (2,7×) |

CPU por petición correcta: Go/Python 0,060–0,106. PSS: Python ~181–194 MiB,
Go ~15–17 MiB.

**Cautela:** en esta campaña Python rindió 20–30 % menos que el 31-oct en
candidates c16, claim-cycle y nova-mix (por ejemplo, nova-mix c16: 143 → 99
ops/s), de forma estable entre repeticiones y sin causa identificada. Esa baja
infla los cocientes Go/Python. La comparación Go contra Go (última columna)
no depende de Python: el binario nuevo es 1,25–3,2× más rápido que el de
octubre en estas cargas.

## Conclusión

Con la misma política de caché de tokens que el original y un pool adecuado,
Placement Go supera a Python en las seis cargas medidas: 4,9–7,5× en lecturas
y 3–14× en candidates, claims y la mezcla tipo Nova, con ~6–10 % de la CPU por
petición y ~8 % de la memoria.

Límites: tres pares por celda y dos concurrencias, sin significancia
estadística. Ambos servicios cachean tokens (Python en memcached, Go en
memoria del proceso), pero workers, pools y configuración MySQL siguen siendo
distintos. Se comparan despliegues, no lenguajes aislados.

## Verificación

`go test -race ./...` y `make check` aprobados. Pruebas nuevas:
`TestPlacementTokenCacheTime`, `TestPlacementDatabasePoolSize` y
`TestConfigurePoolSetsOpenAndIdleLimits`.
