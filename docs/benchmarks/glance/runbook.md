# Reproducción de instrumentos RF10

Estado: instrumentos para **piloto** y validación local. No existe todavía un
coordinador Glance A/B/C/D de despliegue/restauración validado. Los protocolos son
borradores y la CLI no admite stage official. No confundir este runbook con
autorización para saltar gates de capacidad o para cerrar B20/B25/B26/B17/B18.

## Validación local

Desde la raíz del worktree, sin VM ni servicios externos:

```sh
GOMAXPROCS=2 GOTOOLCHAIN=local GOFLAGS=-p=1 GOPROXY=off GOSUMDB=off MAKEFLAGS=-j1 make check
python3 -m unittest discover -s tests/unit -p test_glance_benchmark.py -v
python3 -m unittest discover -s tests/unit -p test_experiment_analysis.py -v
python3 -m unittest discover -s tests/unit -p test_integrated_boot.py -v
python3 -m unittest discover -s tests/unit -p test_experiment_schedule.py -v
```

Los tests HTTP usan un servidor independiente de protocolo; las pruebas de
boot usan un gateway doble. Ninguno acredita Keystone/MySQL/Nova reales.

## Preparación antes del piloto real

1. Medir host y VM. Reservar memoria del host y del invitado, espacio del store,
   disco Nova y capacidad de transferencias. El ensayo de capacidad de esta
   sesión **falló** con 4 GiB; no repetir cargas en ese estado.
2. Confirmar identidad de VM, SSH y usuario. Si cambia la host key, verificarla
   por canal independiente (guest-agent de la VM identificada), sin reemplazar
   el known_hosts anterior. No usar StrictHostKeyChecking=no/accept-new.
3. Capturar hashes/configuración/servicios, catálogo efectivo y recursos previos.
   Respaldos y tokens en directorio privado fuera de Git. Leer configuraciones
   sólo con extracción de claves permitidas; nunca publicar DSN/password.
4. Adquirir lock host de campaña y lock **dentro de la VM**
   `/run/lock/tt-placement-integration.lock`, mismo inode/UID/mode usados por
   B15/B17. No borrar ese inode durante operaciones. Un lock host no excluye
   campañas dentro del invitado. Las CLI sólo toman el lock indicado localmente:
   por eso el piloto de laboratorio debe ejecutarse dentro del lock de la VM.
5. Desplegar código/binarios propios en carpeta nueva; comprobar SHA y hashes.
   Preparar MySQL/schema/store/identidad Go nuevos, sin adoptar backend B26 ni
   borrar sus recursos retenidos. Validar startup/recovery/Keystone y equivalencia
   funcional antes de medir. Esta preparación todavía no está automatizada aquí.
6. Probar restauración con backups/hash y cierre de procesos propios antes de
   habilitar un protocolo oficial. No ejecutar complete_placement_campaign.py:
   su supervisor publica en Git/PR y está prohibido por la solicitud humana.

## Payload y ventana Glance

En un directorio persistente privado, con espacio comprobado. Valores son de
ejemplo para un piloto, no tamaños oficiales ya aceptados:

```sh
python3 scripts/benchmarks/glance_benchmark.py payload \
  --output /ruta/privada/nueva/payload-1m.bin --bytes 1048576 --seed 20261031
python3 scripts/benchmarks/glance_benchmark.py run \
  --url http://192.168.122.10/image --implementation python \
  --token-file /ruta/privada/member.token --allow-mutations \
  --scenario download --payload /ruta/privada/nueva/payload-1m.bin \
  --max-payload-bytes 1048576 --concurrency 1 --warmup 1 --duration 4 \
  --timeout 30 --max-requests 10000 --stage pilot \
  --lock /run/lock/tt-placement-integration.lock \
  --output /ruta/privada/nueva/run-python
```

La URL es la base **sin /v2**. CLI restringida a loopback y al IP identificado
del laboratorio, sin credenciales/query/fragmento en URL. HTTP por red privada
del laboratorio; HTTPS verifica certificados cuando se usa. Cada ventana admite
máximo 16 workers, 120 s de carga, 30 s warm-up, 100000 requests y payload<=1 GiB;
un timeout por request <=600 s limita headers/body, con watchdog de socket.
La duración real incluye drain y helpers del ciclo, registrada en manifest.
Presupuestar el máximo de teardown antes de un supervisor de apagado; esta CLI
no autoriza un job remoto durable ni realiza shutdown por sí sola.

Escenarios: create/list/show/patch/delete/pagination/filter/metadata-mix/crud y
upload/download/range/binary-mix. Uploads son secuenciales por worker, sin
acumulación: crear/subir/verificar/borrar. Las semillas estables son dos imágenes
por worker; un ciclo escritor añade una. No se crean instancias Nova aquí.
Crear/delete aislados miden esas requests, pero su goodput incluye helpers que
mantienen el estado acotado; no interpretarlo como capacidad de POST sin cleanup.
El CRUD conserva requests por operación, sin confundir requests/s con ciclos/s.

Ambas implementaciones reciben payload/IDs deterministas iguales por par usando
el mismo campaign-id y ledger/output distintos; no ejecutar simultáneamente.
El generador carga bloques de 256 KiB y no mantiene el binario completo en RAM.
SHA256/SHA512/MD5 de origen se calculan antes de carga; download compara longitud,
SHA256 y SHA512; upload comprueba metadata y las semillas añaden un download.
Rangos comprueban Content-Range, longitud y SHA256 del segmento. El payload
sintético raw no es una imagen de boot.

## Ledger, abort y recuperación

Cada intención de POST persiste UUID/name/tags **antes** de enviar. Los nombres
estables identifican campaña/worker/secuencia; la limpieza revisa GET y propiedad
exacta antes de DELETE y exige 404 después. Un POST con respuesta perdida se
recupera por el UUID de intención, sin barrer imágenes por prefijo. No se purgan
tombstones SQL. Las credenciales nunca entran al ledger ni a las muestras.

SIGINT/SIGTERM detiene nuevos ciclos, drena requests acotadas y entra en cleanup.
Una pérdida de energía/SIGKILL necesita recuperación explícita con el mismo
ledger y token vigente. Si no hay certeza se conserva NEEDS_ATTENTION:

```sh
python3 scripts/benchmarks/glance_benchmark.py cleanup \
  --output /ruta/privada/nueva/run-python --token-file /ruta/privada/member.token \
  --lock /run/lock/tt-placement-integration.lock
```

Recovery escribe recovery.json sin convertir un intento fallido en exitoso.
Muestras/report/manifest/ledger llevan SHA256SUMS. Guardar por separado intentos
fallidos; no modificar sus muestras ni borrar outliers.

## Recursos y análisis

Reutilizar `scripts/benchmarks/placement_resources.py` dentro de la VM. Entregar
JSON de cgroups explícitos con grupo `glance` para el proceso completo del
servicio y grupos separados Nova/Keystone/MySQL/Apache/generador. Verificar
permisos de smaps y membresía; no confundir Apache completo con Glance Python.
Conservar reposo, warm-up, medición y drain a 1 Hz. No se incorporó un nuevo
recolector duplicado. No se ejecutó muestreo de rendimiento Glance en esta sesión.

Opcionalmente, tras detener el recolector, copiar resources.jsonl a la carpeta de
la ventana y adjuntar clock-alignment.json con `verified`, `uncertainty_ns`,
`remote_minus_generator_ns` y `service_vcpus`. Estos valores requieren medición
real antes/después; no establecer verified=true por conveniencia. Regenerar
checksums conscientemente antes del análisis. El analizador exige todos los
artefactos manifestados y ventana interior/membresía/PSS correctas. Sin esos
archivos sólo genera métricas HTTP, marcando ausencia de recursos.

```sh
python3 scripts/benchmarks/experiment_analysis.py glance \
  /ruta/privada/nueva/run-python /ruta/privada/nueva/run-go \
  --output /ruta/privada/nueva/analisis
python3 scripts/benchmarks/plan_experiments.py --kind glance \
  --protocol docs/benchmarks/glance/protocol-v1.json \
  --output /ruta/privada/nueva/plan.json
```

El plan genera orden reproducible y alternancia por repetición; **no ejecuta**
ni congela una campaña. Matriz de referencia Glance actual: 300 pares/600 ventanas,
unas 25 horas sólo de warm-up+carga, antes de calibrar. No reducirla a escondidas.

Para `paired` y `factorial`, proporcionar JSON de bloques independientes con
`protocol_sha256`, `stage`, `repetition`, `values`. Python/Go usan claves
`python`,`go`; integrado usa `A`,`B`,`C`,`D`. Separar operación/tamaño/concurrencia
y configuración antes de formar bloques. Rechaza protocolos/stages mezclados,
repeticiones duplicadas/incompletas y valores no finitos. IC bootstrap descriptivo
sólo con >=5 bloques. El analizador nunca acredita gates/despliegue por sí mismo.
