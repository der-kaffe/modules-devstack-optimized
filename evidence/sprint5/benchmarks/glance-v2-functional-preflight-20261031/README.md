# Glance: preflight funcional real, 2026-10-31

Fuente probada: `85a78deb985c3ca0a2543afea604026746278f8c`. Binario y hashes de
artefactos en provenance.json. Base Placement aceptada: `13bb013`.

Resultado: 42/42 solicitudes con status esperado y sin errores de transporte o
integridad. Keystone real; Python desplegado y Go con MySQL/store propios.
Ciclos: UUID nuevo/404, crear, cargar, consultar estado y hashes MD5/SHA512,
descargar verificando longitud/SHA256, renombrar, eliminar y comprobar 404.
Archivos raw deterministas: Python 1 MiB; Go directo 1/16/64 MiB; Go a través de
Apache /image 1 MiB. Placement original se mantuvo como control.

Original-before/after idénticos, recursos/identidad/runtime propios retirados,
controles temporales restaurados y servicios originales activos antes de apagar.
VM apagada normalmente, sin apagado forzado; ver vm-final-state.json.
No se publican credenciales, application credentials, payloads ni logs privados.
Los registros identity-prepare sólo contienen IDs, roles y expiraciones.

Reproducción: desplegar scripts de esa revisión y binario cmd/glance en la VM;
con openrc admin cargado de forma privada, ejecutar glance_preflight.py con
--allow-mutations, --work-root nuevo, --binary, --output nuevo y --url público.
Exige laboratorio exclusivo, rutas privadas nuevas y restauración/apagado posterior.
El binario se compiló con Go 1.27.3, GOEXPERIMENT=nodwarf5, -trimpath y
-buildvcs=false; esos atributos deben preservarse al congelar la campaña.

Este ensayo no es el piloto de carga ni acepta las 108 ventanas compactas.
No mide CPU/PSS comparables ni valida un boot Nova nuevo, auth compuesta o toda
la API. Se conserva separada la evidencia histórica informada por el autor de
Glance. Siguiente: coordinador, RPC, análisis de CPU/PSS, supervisor durable,
piloto completo, congelación y ejecución oficial antes de la campaña conjunta.
