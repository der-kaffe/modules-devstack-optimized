# Glance compacto v2: piloto válido, oficial fallido

Piloto36/36 HTTP/bytes/estado y CPU/PSS válido. Oficial detenido con76 ventanas
válidas y una inválida (77 intentadas); **sin aceptación**. No mezclar muestras
con intentos/revisiones posteriores. Fuente80fcd1f y configuración stock
notifications messagingv2, límite2048FD. HTTP200 devolvió0bytes para imagen1MiB.
Journal confirma errno24 al abrir la imagen en caché. PID/start_ticks estables,
guardas de disco sin aborto; no hubo reinicio de workers ni fallo de integridad
normalizado. Originales, limpieza, controles y apagado comprobados.

Datos completos comprimidos separados en pilot/official; summary incluye errores.
Extracto mínimo saneado en incident. Journal completo privado fuera de Git.
Nueva calibración debe probar origen de acumulación y control sin notificaciones
antes de congelar otra revisión. V2 no se reinterpreta como aceptada.
