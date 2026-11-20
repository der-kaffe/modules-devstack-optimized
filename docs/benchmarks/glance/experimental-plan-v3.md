# Enmienda v3: controles de MySQL

Antes de iniciar cualquier ventana oficial v2 se observó buffer pool de 16 MiB en MySQL original y 128 MiB en el backend Go. No se mezclan esas calibraciones con resultados oficiales. La v2 se conserva congelada sin ventanas oficiales; v3 la sustituye para igualar buffer pool (16 MiB), max_connections (1024), innodb_flush_log_at_trx_commit (1) y sync_binlog (1), con MySQL 8.0.46 en ambos.

El primer intento de reducción dinámica no pasó el gate: MySQL redondeó por el tamaño de chunk. Se conserva el fallo. Se configuró y reinició exclusivamente el servidor experimental de UUID verificado, con chunks de 8 MiB, y se verificaron los controles efectivos. El original se consultó mediante SELECT con su cuenta de servicio; no se escribió en él. Después se repiten los pilotos de consulta y descarga con el coordinador completo.

Se probaron tanto finalización normal como aborto por SIGTERM: ambos restauraron rutas y limpiaron recursos propios. El primer bloque oficial se limita a show/c1, cinco pares con el orden alternado y tiempos v3. Esto conserva margen de restauración frente al vencimiento del perfil. Download y los demás casos no se declararán completos por ejecutar únicamente este bloque.
