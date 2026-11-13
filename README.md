# WhatsApp Number Checker API (Multi-Account Pool & Dashboard)

API en Go de alto rendimiento para comprobar si números telefónicos están registrados en WhatsApp mediante un **Pool de múltiples cuentas** y un **Panel Web de control y monitoreo en tiempo real**.

- **Distribución inteligente de carga:** Algoritmo *Longest Idle* (selecciona la cuenta que más tiempo ha descansado para evitar patrones robóticos).
- **Control estricto de cuotas por variables de entorno:** Ajusta el límite diario de consultas por chip (ej. 1.000) y la meta total (ej. 10.000).
- **Aislamiento de Cooldown:** Si WhatsApp responde con un `429` (Rate Limited) en un chip, solo ese chip entra en pausa temporal; los demás siguen respondiendo sin interrumpir el servicio.
- **Panel Web Embebido (Cero dependencias):** Escanea códigos QR para vincular chips en vivo, monitorea métricas, progreso diario y elimina cuentas desde el navegador sin instalar Node.js ni Docker.

---

## Ejecutar en Windows

Requiere Go 1.26 o posterior y conexión a internet.

```powershell
$env:CGO_ENABLED = "0"
go run .
```

Una vez iniciada, abre tu navegador en:
* **Panel de Control y Monitoreo:** `http://127.0.0.1:8080/` (o `/admin`)
* **Endpoint de Consultas:** `http://127.0.0.1:8080/check?number=NUMERO`

---

## Variables de Entorno y Configuración

Puedes configurar todos los parámetros mediante variables de entorno o flags al iniciar:

| Variable de Entorno | Flag CLI | Valor por Defecto | Descripción |
| :--- | :--- | :---: | :--- |
| `MAX_DAILY_QUERIES_PER_NUMBER` | `-max-daily` | `1000` | Límite máximo de consultas que un chip puede hacer por día antes de pausarse hasta la medianoche (0 = ilimitado). |
| `TARGET_DAILY_TOTAL` | `-target-daily` | `10000` | Meta de consultas globales para el medidor visual del dashboard. |
| `SESSIONS_DIR` | `-sessions-dir` | `data/sessions` | Carpeta donde se guardan las sesiones SQLite independientes (`.db`). |
| `HTTP_ADDR` o `PORT` | `-addr` | `127.0.0.1:8080` | Dirección y puerto en el que escucha el servidor. |
| `REQUEST_DELAY_MS` | `-delay-ms` | `1000` | Milisegundos de espera/descanso forzado por chip entre consultas. |

### Ejemplo con variables personalizadas en PowerShell:

```powershell
$env:MAX_DAILY_QUERIES_PER_NUMBER = "800"
$env:TARGET_DAILY_TOTAL = "10000"
$env:HTTP_ADDR = "0.0.0.0:8080"
go run .
```

---

## Cómo Vincular Múltiples Números

1. Inicia el servidor y entra en tu navegador a `http://127.0.0.1:8080/`.
2. Haz clic en el botón verde **"➕ Vincular Nuevo Chip"**.
3. Escribe un alias (ej. `chip_01`, `linea_ventas`).
4. Se generará un código QR en pantalla.
5. Abre WhatsApp en tu celular: **Ajustes → Dispositivos vinculados → Vincular un dispositivo** y escanea el QR.
6. El panel confirmará la vinculación con el número detectado (ej. `+591XXXXXXXX`) y el chip entrará inmediatamente al Pool de rotación.
7. Repite el proceso para vincular los 10 chips.

*Nota:* Las sesiones se guardan en `data/sessions/*.db`. Al reiniciar la API, se conectan automáticamente sin pedir QR nuevamente. Si tenías una sesión anterior en `data/session.db`, se migra automáticamente a `data/sessions/default.db`.

---

## Endpoint de Consulta

En tu cliente HTTP, backend o terminal:

```powershell
curl.exe -i "http://127.0.0.1:8080/check?number=59170000000"
```

### Respuesta Exitosa (`200 OK`)

```json
{
  "number": "+59170000000",
  "active": true,
  "worker": "chip_01"
}
```

* `active`: `true` si el número tiene WhatsApp, `false` si WhatsApp confirmó que no está registrado.
* `worker`: ID del chip en el pool que atendió la consulta.

### Códigos de Error

Todos los errores devuelven JSON (ej. `{"error":"invalid_number"}`).

| HTTP | Código | Causa / Acción |
| :---: | :--- | :--- |
| **400** | `invalid_number` | Número vacío o formato incorrecto (debe incluir prefijo de país). |
| **404** | `not_found` | Ruta desconocida. |
| **405** | `method_not_allowed` | Solo se permite GET para consultas. |
| **429** | `rate_limited` | Todos los chips están ocupados o en pausa; revisa `Retry-After`. |
| **429** | `all_quotas_exceeded` | Todos los chips alcanzaron su límite diario (`MAX_DAILY_QUERIES_PER_NUMBER`). |
| **502** | `upstream_error` | Fallo o respuesta ambigua de los servidores de WhatsApp. |
| **503** | `whatsapp_unavailable` | No hay ningún chip conectado en el pool. |
| **504** | `timeout` | La consulta tardó más de 10 segundos. |

---

## Compilación y Pruebas

Para compilar un único binario ejecutable optimizado para Windows:

```powershell
$env:CGO_ENABLED = "0"
go test ./...
go vet ./...
go build -o bin/whatsapp-checker.exe .
.\bin\whatsapp-checker.exe
```
