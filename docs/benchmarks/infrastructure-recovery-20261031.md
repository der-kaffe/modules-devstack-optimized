# Recuperación del laboratorio y presupuesto de memoria

El bloqueo anterior no era una demostración de imposibilidad del hardware. El nuevo censo encontró APIs con dos workers y servicios auxiliares residentes. Se aplicó un perfil reversible de un worker por API, se separaron las ventanas HTTP de las de Nova y se mantuvieron los componentes necesarios durante los gates integrados. No se cerraron aplicaciones ajenas, no se compraron recursos y no se instaló DevStack sobre el host.

La estación sigue siendo Debian 13, Ryzen 5 7520U y aproximadamente 7 GiB físicos. Se observaron inicialmente unos 5,2 GiB disponibles, zram con 439 MiB lógicos comprimidos en unos 122 MiB físicos y swap de disco sin uso. No se cambió swappiness ni se presentó swap como RAM. La VM identificada por UUID `385591e9-02ea-4209-b09e-49f3f882a8e6` conserva persistentemente 6 GiB y cuatro vCPU. Los ensayos HTTP usan 4 GiB; parte de los pilotos Nova usó 4,5 GiB live después de comprobar la reserva del host.

| Censo del invitado | MemAvailable MiB | Swap usada MiB | PSS de procesos MiB |
|---|---:|---:|---:|
| Original tras arranque | 135,28 | 684,70 | 3385,94 |
| Workers reducidos, transición | 599,75 | 1311,39 | 2864,40 |
| Perfil auxiliar reducido | 704,98 | 998,39 | 2754,21 |

Son instantáneas de transición, no una curva causal ni una aceptación por sí solas. Las páginas previamente intercambiadas siguen en swap aunque ya no exista presión; importa medir actividad y PSI durante la carga. El servidor MySQL original aportó inicialmente unos 391 MiB PSS y 146 MiB SwapPSS. Neutron API rondó 290 MiB PSS, Nova API 249 MiB y Glance 224 MiB. Los censos completos conservan cada cgroup y proceso.

En las 36 ventanas HTTP reales, el invitado mantuvo al menos 1920 MiB disponibles y registró cero páginas swap-out durante unos 231 segundos de colección; hubo 2671 páginas swap-in, que se conservan como limitación de ese piloto corto. La repetición con SSH persistente validó seis ventanas de CPU/PSS con el límite de reloj intacto. Los gates Nova A/B/C/D alcanzaron ACTIVE y limpiaron allocations. Eso demuestra viabilidad funcional y de cargas HTTP pequeñas, sin demostrar todavía estabilidad prolongada del sistema integrado.

El presupuesto HTTP separa memoria del host, QEMU, APIs, dos MySQL, Keystone, Apache, generador y recolector. El coordinador mantiene reservas de 768 MiB en el host, 512 MiB en el invitado, 1 GiB en el filesystem de evidencia y 512 MiB en el store. Tiene límite de memoria virtual de 768 MiB, duración absoluta finita, lock propio y watchdog; el perfil mantiene además el lock B15 del laboratorio. Un exceso sostenido de swap-out, OOM, colector obsoleto o pérdida de capacidad aborta la carga. Los datos fallidos no se borran.

No fue necesaria una segunda VM: dos instalaciones simultáneas competirían por la misma RAM. Se conservaron el disco y XML originales. El espacio observado (aproximadamente 17 GiB en /home y 35 GiB en el filesystem del disco VM) permite evidencia acotada, no duplicaciones ilimitadas. El volumen NTFS con más espacio no aumenta RAM y no se usó. No se encontró un segundo equipo de cómputo autorizado y accesible. Una instalación mínima nueva sigue siendo alternativa si falla la estabilidad prolongada, no un requisito ya satisfecho.

Se descubrió otra variable controlable: MySQL experimental tenía buffer pool de 128 MiB frente a 16 MiB original. La reducción dinámica falló por granularidad; se conservó el intento. Sólo el servidor experimental, de UUID distinto, se reinició con 16 MiB y controles de durabilidad/conexiones iguales al original. La v3 documenta ese cambio antes de medir oficialmente. La v1 y v2 y sus pilotos permanecen separados.

Las evidencias voluminosas residen fuera de Git, bajo el workspace privado bio-harness `6fda7b8234e406137f334ff3f21c9a99`, directorios `infrastructure-recovery-20261031a` y `infrastructure-recovery-20261031b`. Las credenciales viven únicamente en subdirectorios privados y quedan excluidas de manifiestos públicos. El informe final de cada sesión debe verificar configuración, servicios, recursos propios, XML y apagado; un fallo deja NEEDS_ATTENTION, nunca una afirmación de restauración.
