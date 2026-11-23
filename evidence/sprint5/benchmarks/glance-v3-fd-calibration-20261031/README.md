# Diagnóstico y calibración nativos FD, 2026-10-31

Fuente07b03de. No son datos de rendimiento: cliente dentro de la VM.
Original messagingv2:1500/1500 descargas1MiB/hash correctas y +1500FD.
Control noop:6000/6000 descargas1MiB/hash correctas y +4FD. Conteos/sockets por
PID en report.json. CacheFilter crea nuevo Notifier/transport por cached request;
el crecimiento de sockets desaparece con el driver noop. Concuerda con el fallo
stock v2 errno24/HTTP200 vacío en límite2048. No se aumentó ese límite ni se
modificó código/configuración original. Copia privada y dropin /run retirados;
hashes originales iguales, recursos propios eliminados, controles restaurados y
VM apagada normalmente. Todas las copias/credenciales y payloads quedan fuera de Git.

Nuevo protocolo v3 conserva108/36, pero sólo compara API/catálogo/bytes sin
notificaciones RabbitMQ en ambas implementaciones; la caché Python se conserva.
No acredita estabilidad/compatibilidad de messagingv2 ni se mezcla con v2.
Piloto y oficial v3 pendientes tras esta calibración. Se añade guarda FD por
ventana con crecimiento máximo256 sobre el primer sample, por separado de
HTTP/bytes y CPU/PSS. Replica negativa del guard y configuración v3 probadas.
