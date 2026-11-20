# Gates A/B/C/D observados en el laboratorio

Los cuatro tratamientos alcanzaron ACTIVE, verificaron sus allocations y eliminaron exclusivamente la instancia propia. La evidencia se conserva en `functional-gates-20261031.json`, con rutas y hashes. Es evidencia funcional de pilotos, no una campaña integrada oficial ni una comparación estadística de tiempos.

Para D se verificaron las dos rutas Apache hacia los backends Go, los servicios Python correspondientes inactivos y tráfico HTTP correcto de ambos backends durante la misma creación Nova. Placement atendió candidates, claim, lectura y liberación de allocations. Glance atendió metadatos de la imagen usada. La transferencia de 21.692.416 bytes desde Glance Go se observó en B; D reutilizó la caché Nova. `qemu-img compare` devolvió cero entre la caché raw y el archivo original, cuya identidad SHA-512 también se verificó al importar y descargar la imagen Go.

El observador conserva `gate_accepted=false`: por diseño no controla ni certifica endpoints. La evidencia externa de rutas, estado de servicios y logs complementa esa observación. No se cambia ese campo retrospectivamente. Una ruta exitosa no sustituye la revisión de toda la evidencia de despliegue.

Se conserva el primer B fallido por discovery de Glance y el segundo B con ERROR de Nova durante inicialización de compute. Se fijó explícitamente el mismo endpoint Glance en las tres configuraciones Nova relevantes, respaldando sus bytes y restaurándolos al terminar. El tercer B pasó después de esperar la inicialización. No se modificó producción Go para ampliar discovery.

La instancia histórica SHUTOFF conserva sus recursos. En la base experimental Go se replicó mediante APIs su ocupación, para que Nova no percibiera capacidad libre distinta. Se compararon cantidades sin normalizar ni ocultar generations propias de cada backend. No se editó SQL original.

A se calibró a 4 GiB; los gates posteriores incluyeron 4,5 GiB. Cambiaron además estados temporales de servicios durante la calibración. Por esas diferencias, los tiempos se publican como diagnósticos y no se calculan mejoras ni interacción con ellos. No se verificó login dentro de CirrOS. Una campaña integrada repetida con perfil y caché constantes sigue siendo un gate distinto.
