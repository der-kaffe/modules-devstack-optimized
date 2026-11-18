# N2: autenticación compuesta Nova y Glance Go

Alcance: autenticación y autorización de las rutas B20/B25 existentes. No cambia
Nova, Keystone, el catálogo, las políticas del laboratorio ni la observabilidad.
No acredita un arranque de instancia ni integración operativa Nova con Go.

## Contrato inspeccionado

La auditoría B26 verificó el Nova instalado, commit
`247ea5d774305b688997f8df81c011b249eb793c`, y Keystone mediante lectura de sus
fuentes/configuración. El plugin real de Nova emitió X-Auth-Token y X-Service-Token,
y Keystone validó ambos individualmente con 200. Eso no prueba que Go los admita.

Referencias de la instalación inspeccionada:

- `nova/service_auth.py`, `get_service_user_token_auth_plugin`, líneas 70–104:
  utiliza ServiceTokenAuthWrapper cuando send_service_user_token está activado.
- `keystonemiddleware/auth_token/__init__.py`, líneas 363–412: primero valida el
  servicio sin excepción de expiración; exige el rol service según la política
  observada, y sólo entonces solicita allow_expired para el usuario.
- `keystone/api/auth.py`, líneas 300–309: interpreta allow_expired como booleano
  y pasa token.allow_expired_window al proveedor.
- `keystone/token/provider.py`, validate_token y _is_valid_token, líneas 139–153
  y 197–253: valida la expiración dentro de esa ventana y comprueba revocaciones.

La ventana observada es 172800 segundos y la expiración normal es 3600 segundos.
Go no codifica esos valores ni intenta ampliar la política: Keystone decide en
cada validación. Cambiar la política del servidor exige una nueva revisión.

## Protocolo y alcance de confianza

Sin cabecera de servicio se conserva la validación B20, sin allow_expired.
Con ella se requieren exactamente un token de usuario y uno de servicio,
no vacíos, sin controles, espacios ni listas separadas por comas.

1. La credencial propia de Go obtiene el token validador. Sigue exigiéndose
   proyecto, rol service y ausencia de admin. Sólo este token se almacena en caché.
2. El token entrante de Nova se valida mediante GET /v3/auth/tokens?nocatalog,
   usando el token propio como X-Auth-Token y el de Nova como X-Subject-Token.
3. Se exige rol service y proyecto igual al del token validador de Go. Esta
   vinculación es una restricción adicional respecto del middleware Python:
   no se aceptan servicios de otros proyectos. No requiere una nueva variable
   de entorno y el proyecto se obtiene de Keystone, nunca de cabeceras cliente.
4. Se valida el usuario mediante una nueva petición con
   ?nocatalog&allow_expired=true. Un 404, incluso por revocación o por superar
   la ventana del servidor, rechaza la solicitud. Un fallo del validador falla
   cerrado. No hay fallback a una identidad anterior. Desde 2026-11-18, Glance Go
   reutiliza durante `GLANCE_GO_TOKEN_CACHE_TIME` (300 s) cada token validado con
   éxito, como keystonemiddleware: clave SHA-256, sin cachear rechazos y con la
   expiración revisada en cada uso. Igual que en el original, un token revocado
   puede servir hasta ese tiempo; `-1` desactiva la caché.
5. Se entrega únicamente la identidad del usuario, con su proyecto, roles y
   expires_at originales. Un campo privado permite representar la validación
   compuesta que Keystone acaba de confirmar; no puede construirse por JSON.
   Su vigencia tampoco supera la expiración del token entrante de servicio.

No se exige que el proyecto del usuario sea el del servicio. El servicio Nova
puede tener además admin, pero ese rol nunca se copia al usuario. Un reader sigue
sin poder escribir, un miembro no puede publicar y una imagen privada ajena sigue
oculta. Los controles de propietario, visibilidad y protected no cambian.

Las identidades son de uso por petición, no credenciales reutilizables. La ventana
y revocación se comprueban en Keystone al autenticar, como en el middleware
inspeccionado; no se promete detectar revocaciones durante una transferencia ya
autorizada. La excepción privada no altera expires_at ni valida otra petición.
Los tokens con restricciones access_rules o bind no implementadas se rechazan;
no se anuncia soporte de esas restricciones ni se convierten en acceso ilimitado.

## Resultados HTTP

| Condición | Resultado |
| --- | --- |
| Falta usuario; falta ambos; token vacío, duplicado o inválido | 401 |
| Servicio inválido, vencido o revocado | 401 |
| Servicio sin rol service, sin proyecto o con otro proyecto | 403 |
| Usuario revocado o fuera de la ventana que admite Keystone | 401 |
| Usuario vencido, servicio autorizado y Keystone acepta el usuario | Política B20 del usuario |
| Servicio admin con usuario reader intentando escribir | 403 |
| Imagen private/shared de otro proyecto | 404 |
| Keystone no disponible o respuesta inválida | 503 |
| Autenticador inyectado sin soporte compuesto | 403, sin ignorar la cabecera |

Los 401 conservan WWW-Authenticate. Los cuerpos y logs no contienen tokens,
credenciales, respuestas crudas de Keystone ni roles del servicio. El 403 para
un servicio con rol/proyecto no autorizado conserva la distinción local entre
autenticación y autorización; no se afirma igualdad de todos los errores Python.

## Verificación y límites

Las pruebas de internal/auth/composite_test.go usan un servidor de protocolo
independiente para verificar orden, ventana, revocación, revalidación, concurrencia
y errores. internal/glance/api/composite_test.go conecta el adaptador real a ese
tipo de servidor local, al servicio y a un repositorio doble. Comprueba códigos,
identidad original, aislamiento, rechazos sin escrituras y ausencia de secretos.
No son pruebas contra Keystone ni MySQL reales.

Antes de cambiar Nova se requiere validar de nuevo el cliente real contra Go,
con las credenciales y recursos autorizados para esa campaña. Siguen pendientes
los casos compuestos reales de expiración, revocación y permisos. No se crean
tokens revocados, identidades, esquemas ni instancias en las regresiones locales.
