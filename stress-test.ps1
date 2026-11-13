param (
    [int]$MaxRequests = 5000,
    [int]$DelayMs = 500,
    [string]$CountryPrefix = "5917",
    [string]$ApiUrl = "http://127.0.0.1:8080/check"
)

Clear-Host
Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "   WHATSAPP STRESS TEST - PRUEBA DE RESISTENCIA Y LÍMITES" -ForegroundColor Cyan
Write-Host "==========================================================" -ForegroundColor Cyan
Write-Host "URL API:       $ApiUrl"
Write-Host "Máx consultas: $MaxRequests"
Write-Host "Espera (delay): ${DelayMs}ms entre peticiones"
Write-Host "Detención:     Presiona Ctrl+C en cualquier momento"
Write-Host "==========================================================" -ForegroundColor Gray
Write-Host ""

$count = 0
$success = 0
$inactive = 0
$rateLimits = 0
$serverErrors = 0
$bannedOrDown = 0
$startTime = [System.Diagnostics.Stopwatch]::StartNew()

function Get-RandomNumber {
    # Genera un número móvil válido de 8 dígitos para Bolivia
    $suffix = (Get-Random -Minimum 1000000 -Maximum 9999999).ToString()
    return "${CountryPrefix}${suffix}"
}

try {
    for ($i = 1; $i -le $MaxRequests; $i++) {
        $count++
        $targetNumber = Get-RandomNumber
        $url = "$($ApiUrl)?number=$targetNumber"

        $reqStart = [System.Diagnostics.Stopwatch]::StartNew()
        try {
            $response = Invoke-RestMethod -Uri $url -Method Get -TimeoutSec 10 -ErrorAction Stop
            $reqStart.Stop()
            $ms = $reqStart.ElapsedMilliseconds

            if ($response.active -eq $true) {
                $success++
                Write-Host "[$count] $targetNumber -> " -NoNewline -ForegroundColor White
                Write-Host "TIENE WHATSAPP" -ForegroundColor Green -NoNewline
                Write-Host " (${ms}ms) | Chip: $($response.worker)" -ForegroundColor DarkGray
            } else {
                $inactive++
                Write-Host "[$count] $targetNumber -> " -NoNewline -ForegroundColor White
                Write-Host "NO REGISTRADO" -ForegroundColor DarkYellow -NoNewline
                Write-Host " (${ms}ms) | Chip: $($response.worker)" -ForegroundColor DarkGray
            }
        }
        catch {
            $reqStart.Stop()
            $ms = $reqStart.ElapsedMilliseconds
            $statusCode = 0
            if ($_.Exception.Response) {
                $statusCode = [int]$_.Exception.Response.StatusCode
            }

            if ($statusCode -eq 429) {
                $rateLimits++
                Write-Host "[$count] $targetNumber -> " -NoNewline -ForegroundColor White
                Write-Host "429 RATE LIMIT RECIBIDO (WhatsApp pidió pausa)" -ForegroundColor Yellow
                Write-Host "    -> Alerta: WhatsApp empezó a frenar las consultas en la cuenta." -ForegroundColor Yellow
            }
            elseif ($statusCode -eq 503) {
                $bannedOrDown++
                Write-Host "[$count] $targetNumber -> " -NoNewline -ForegroundColor White
                Write-Host "503 WHATSAPP DESCONECTADO O SESIÓN REVOCADA" -ForegroundColor Red
                Write-Host "    -> ALERTA: La cuenta se desconectó o Meta revocó la vinculación." -ForegroundColor Red
                
                # Si recibimos 3 errores 503 seguidos, detenemos la prueba
                if ($bannedOrDown -ge 2) {
                    Write-Host ""
                    Write-Host "Deteniendo prueba: La cuenta de WhatsApp ya no responde (posible baneo o desvinculación)." -ForegroundColor Red
                    break
                }
            }
            else {
                $serverErrors++
                Write-Host "[$count] $targetNumber -> Error HTTP $statusCode / $($_.Exception.Message) (${ms}ms)" -ForegroundColor DarkRed
            }
        }

        if ($DelayMs -gt 0) {
            Start-Sleep -Milliseconds $DelayMs
        }
    }
}
finally {
    $startTime.Stop()
    $totalSeconds = [Math]::Round($startTime.Elapsed.TotalSeconds, 1)
    $reqPerSec = if ($totalSeconds -gt 0) { [Math]::Round($count / $totalSeconds, 2) } else { 0 }

    Write-Host ""
    Write-Host "==========================================================" -ForegroundColor Cyan
    Write-Host "               RESULTADOS DE LA PRUEBA                    " -ForegroundColor Cyan
    Write-Host "==========================================================" -ForegroundColor Cyan
    Write-Host "Total Consultas enviadas:    $count"
    Write-Host "Tiempo total transcurrido:   ${totalSeconds}s ($reqPerSec req/seg)"
    Write-Host "Números Activos (True):      $success" -ForegroundColor Green
    Write-Host "Números Inactivos (False):   $inactive" -ForegroundColor DarkYellow
    Write-Host "Límites 429 (WhatsApp Rate): $rateLimits" -ForegroundColor Yellow
    Write-Host "Desconexiones 503 (Ban/Drop):$bannedOrDown" -ForegroundColor Red
    Write-Host "Otros errores:               $serverErrors" -ForegroundColor DarkRed
    Write-Host "==========================================================" -ForegroundColor Cyan

    if ($bannedOrDown -gt 0) {
        Write-Host "CONCLUSIÓN: El chip aguantó $count consultas antes de que WhatsApp desconectara la sesión." -ForegroundColor Red
    } elseif ($rateLimits -gt 0) {
        Write-Host "CONCLUSIÓN: El chip aguantó $count consultas. WhatsApp aplicó un límite temporal (429)." -ForegroundColor Yellow
    } else {
        Write-Host "CONCLUSIÓN: El chip procesó $count consultas exitosamente sin bloqueos ni límites." -ForegroundColor Green
    }
    Write-Host ""
}
