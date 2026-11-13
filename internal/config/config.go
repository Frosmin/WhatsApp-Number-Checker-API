package config

import (
	"bufio"
	"bytes"
	"flag"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr                     string
	SessionsDir              string
	MaxDailyQueriesPerNumber int
	TargetDailyTotal         int
	RequestDelay             time.Duration
}

func Load() *Config {
	// Cargar automáticamente el archivo .env si existe
	loadDotEnv(".env")

	addrDefault := getEnv("HTTP_ADDR", "127.0.0.1:8080")
	if port := os.Getenv("PORT"); port != "" {
		addrDefault = ":" + port
	}

	sessionsDefault := getEnv("SESSIONS_DIR", "data/sessions")
	maxDailyDefault := getEnvInt("MAX_DAILY_QUERIES_PER_NUMBER", 1000)
	targetDailyDefault := getEnvInt("TARGET_DAILY_TOTAL", 10000)
	delayMsDefault := getEnvInt("REQUEST_DELAY_MS", 1000)

	addr := flag.String("addr", addrDefault, "Dirección HTTP para escuchar (ej. 127.0.0.1:8080)")
	sessionsDir := flag.String("sessions-dir", sessionsDefault, "Carpeta donde se guardan las sesiones SQLite")
	maxDaily := flag.Int("max-daily", maxDailyDefault, "Límite máximo de consultas por número al día (0 = sin límite)")
	targetDaily := flag.Int("target-daily", targetDailyDefault, "Meta diaria de consultas totales")
	delayMs := flag.Int("delay-ms", delayMsDefault, "Milisegundos mínimos de espera entre consultas por chip")

	flag.Parse()

	return &Config{
		Addr:                     *addr,
		SessionsDir:              *sessionsDir,
		MaxDailyQueriesPerNumber: *maxDaily,
		TargetDailyTotal:         *targetDaily,
		RequestDelay:             time.Duration(*delayMs) * time.Millisecond,
	}
}

func loadDotEnv(filenames ...string) {
	for _, filename := range filenames {
		data, err := os.ReadFile(filename)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(bytes.NewReader(data))
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			line = strings.TrimPrefix(line, "export ")
			idx := strings.Index(line, "=")
			if idx <= 0 {
				continue
			}
			key := strings.TrimSpace(line[:idx])
			val := strings.TrimSpace(line[idx+1:])

			// Quitar comillas simples o dobles
			if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
				val = val[1 : len(val)-1]
			}

			_ = os.Setenv(key, val)
		}
	}
}

func getEnv(key, fallback string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return fallback
}
