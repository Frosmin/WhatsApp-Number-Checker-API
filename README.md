# WhatsApp Number Checker API

API local en Go para comprobar si un número está registrado en WhatsApp.
Una consulta correcta devuelve **solo `true` o `false`** en JSON. Una consulta
que falla devuelve un error HTTP, nunca un `false` inventado.

## Ejecutar en Windows

Requiere Go 1.26 o posterior, conexión a internet y una cuenta de WhatsApp
que puedas vincular como dispositivo. No necesita Node.js, Docker, un servidor
de bases de datos ni un compilador C.

Desde la carpeta del proyecto, en PowerShell:

```powershell
go run .
```

En el teléfono abre **WhatsApp → Dispositivos vinculados → Vincular un
dispositivo** y escanea el QR que aparece en la terminal. Espera el mensaje
`WhatsApp conectado; API lista para consultar`. Si el QR caduca, detén la API
con Ctrl+C y vuelve a ejecutarla.

La sesión queda en `data/session.db`; al volver a ejecutar la API se recupera
sin pedir otro QR, siempre que WhatsApp no haya revocado la vinculación.
Ejecuta desde la misma carpeta para usar la misma ruta de sesión. También puedes
fijar otra ubicación:

```powershell
go run . -session "D:\mis-datos\whatsapp-session.db"
```

La API escucha exclusivamente en `http://127.0.0.1:8080`. Antes de conectar
WhatsApp, las consultas válidas reciben `503`. Mantén abierta la terminal.

## Consultar

En otra terminal, sustituye `NUMERO_INTERNACIONAL` por el número real, con
código de país y sin espacios, guiones ni ceros iniciales:

```powershell
curl.exe -i "http://127.0.0.1:8080/check?number=NUMERO_INTERNACIONAL"
```

Ejemplo de formato boliviano: `591` seguido del número nacional. El prefijo
es obligatorio: la API no adivina el país ni añade `591` automáticamente.
Se permiten hasta 15 dígitos. Para incluir `+`, codifícalo como `%2B`;
un `+` literal en la query se interpreta como espacio y se rechaza.

Una respuesta HTTP `200` tiene como cuerpo:

```json
true
```

O bien:

```json
false
```

`false` significa que WhatsApp respondió explícitamente que ese número no
está registrado. No indica si la línea telefónica existe o está activa,
ni si el usuario está conectado. No se envían mensajes al número consultado.

## Errores

Todos los cuerpos son JSON. Ejemplo: `{"error":"whatsapp_unavailable"}`.

| HTTP | Código | Significado / acción |
| --- | --- | --- |
| 400 | `invalid_number` | Falta el número, está repetido o tiene formato inválido. |
| 404 | `not_found` | La única ruta es `/check`. |
| 405 | `method_not_allowed` | Usa GET. |
| 429 | `rate_limited` | Espera los segundos de `Retry-After` antes de reintentar. |
| 502 | `upstream_error` | Fallo de WhatsApp o respuesta incompleta/ambigua. |
| 503 | `whatsapp_unavailable` | Falta vincular, está desconectado o se revocó la sesión. |
| 504 | `timeout` | La consulta superó los 10 segundos. |

Los resultados y errores llevan `Cache-Control: no-store`. Trata cualquier
respuesta distinta de `200` como **resultado desconocido**, no como `false`.

## Listas y frecuencia

Tu programa recorre la lista mediante un GET por número. La API permite una
consulta activa y un inicio por segundo; el exceso devuelve `429` sin
encolarse. Una consulta puede tardar varios segundos: esperar un segundo no
garantiza que ya esté libre la conexión. No hay caché de resultados ni
reintentos automáticos de consultas.

Ante un límite explícito de WhatsApp (`429` o `419` en la consulta), se
pausan todas las nuevas consultas durante al menos 60 segundos. Si la
respuesta incluye una espera mayor, se respeta. Los clientes deben leer
`Retry-After` y reintentar después, sin bucles inmediatos.

La conexión usa el protocolo de WhatsApp Web mediante whatsmeow y no es una
API oficial de Meta. Una sesión puede perder acceso o necesitar una nueva
vinculación. El límite local de una consulta por segundo es control de carga,
**no una cuota garantizada o aprobada por WhatsApp**. Más de 10.000 consultas
diarias queda pendiente de validación sostenida con la cuenta real.

## Sesión y problemas de conexión

- Conserva `data/` y protégelo: contiene las credenciales del dispositivo.
  No publiques esa carpeta ni compartas los QR. Los datos están excluidos de Git.
- Ante cortes temporales, la biblioteca intenta reconectar. Mientras tanto
  la API responde `503`. No se reintenta la consulta que ya falló.
- Si aparece `Sesión revocada`, reinicia y vincula nuevamente. No borres
  manualmente una sesión activa.
- Si el puerto está ocupado, detén la otra instancia. No ejecutes dos procesos
  usando la misma sesión ni compartas su archivo mientras está en uso.
- Con `upstream_error` persistente, pausa el procesamiento y revisa la conexión
  y compatibilidad antes de seguir; no clasifiques esos números como negativos.

## Compilar y verificar

```powershell
go test ./...
go vet ./...
$env:CGO_ENABLED = "0"
go build -o bin/whatsapp-checker.exe .
.\bin\whatsapp-checker.exe
```

Las pruebas usan respuestas controladas para comprobar booleanos, formatos,
resultados ambiguos, desconexión, timeout, concurrencia, cooldown y persistencia
real de SQLite entre aperturas. No consultan números reales.

El adaptador reproduce la consulta de contacto de `IsOnWhatsApp` y valida la
respuesta original mediante `DangerousInternals().SendIQ` con `NoRetry: true`.
Esto también evita que el helper `Usync` reenvíe una consulta al reconectar.
La versión fijada
de `IsOnWhatsApp` convierte cualquier tipo distinto de `in` en `false`; aquí
solo se acepta el negativo explícito `out`. Esta dependencia interna está
aislada en `internal/whatsapp/lookup.go`: una actualización requiere repetir
las pruebas y la comprobación real de ambos resultados.

## Validación real pendiente

1. Vincula tu cuenta y consulta un número controlado que tenga WhatsApp y
   otro confirmado que no lo tenga. No uses números inventados como negativos.
2. Comprueba respectivamente `200 true` y `200 false`. Cualquier error HTTP
   deja la comprobación pendiente.
3. Detén y reinicia: debe recuperar la sesión sin otro QR. Después prueba
   desconexión temporal y recuperación.
4. Con una muestra pequeña y controlada, registra duración, consultas `200`,
   errores y límites. Los logs muestran estado HTTP y duración en milisegundos
   sin registrar los números consultados. No extrapoles una muestra breve a
   capacidad diaria garantizada.

La entrega solo queda validada con WhatsApp real después de completar estos
pasos. Las pruebas automáticas y la compilación no sustituyen esa evidencia.
