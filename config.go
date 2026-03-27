package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds all gateway configuration, populated from environment variables.
type Config struct {
	// Server
	Port         int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration

	// Database
	DBPath string

	// Auth
	AdminKey string

	// Logging
	LogLevel LogLevel

	// Upstream
	UpstreamURL string

	// Pool tuning
	PoolReloadInterval   time.Duration
	RPMResetInterval     time.Duration
	TokenRefreshLeadTime time.Duration
	DefaultRPM           int
	DefaultMaxConcur     int
	MaxRetryAttempts     int

	// Async log writer
	LogChannelSize  int
	LogFlushSize    int
	LogFlushInterval time.Duration
}

// LogLevel represents the severity level for logging.
type LogLevel int

const (
	LogLevelDebug LogLevel = iota
	LogLevelInfo
	LogLevelWarn
	LogLevelError
)

func parseLogLevel(s string) LogLevel {
	switch strings.ToLower(s) {
	case "debug":
		return LogLevelDebug
	case "warn", "warning":
		return LogLevelWarn
	case "error":
		return LogLevelError
	default:
		return LogLevelInfo
	}
}

func (l LogLevel) String() string {
	switch l {
	case LogLevelDebug:
		return "DEBUG"
	case LogLevelInfo:
		return "INFO"
	case LogLevelWarn:
		return "WARN"
	case LogLevelError:
		return "ERROR"
	default:
		return "INFO"
	}
}

// LoadConfig reads configuration from environment variables with sensible defaults.
func LoadConfig() (*Config, error) {
	cfg := &Config{
		Port:         getEnvInt("PORT", 8080),
		ReadTimeout:  getEnvDuration("READ_TIMEOUT", 30*time.Second),
		WriteTimeout: getEnvDuration("WRITE_TIMEOUT", 10*time.Minute),
		IdleTimeout:  getEnvDuration("IDLE_TIMEOUT", 120*time.Second),

		DBPath: getEnv("DB_PATH", "gateway.db"),

		AdminKey: getEnv("ADMIN_KEY", "admin281100"),

		LogLevel: parseLogLevel(getEnv("LOG_LEVEL", "info")),

		UpstreamURL: getEnv("UPSTREAM_URL", "https://api.anthropic.com"),

		PoolReloadInterval:   getEnvDuration("POOL_RELOAD_INTERVAL", 2*time.Minute),
		RPMResetInterval:     getEnvDuration("RPM_RESET_INTERVAL", 60*time.Second),
		TokenRefreshLeadTime: getEnvDuration("TOKEN_REFRESH_LEAD", 30*time.Minute),
		DefaultRPM:           getEnvInt("DEFAULT_RPM", 20),
		DefaultMaxConcur:     getEnvInt("DEFAULT_MAX_CONCUR", 5),
		MaxRetryAttempts:     getEnvInt("MAX_RETRY_ATTEMPTS", 3),

		LogChannelSize:   getEnvInt("LOG_CHANNEL_SIZE", 8192),
		LogFlushSize:     getEnvInt("LOG_FLUSH_SIZE", 200),
		LogFlushInterval: getEnvDuration("LOG_FLUSH_INTERVAL", 2*time.Second),
	}

	// Validate upstream URL
	if _, err := url.Parse(cfg.UpstreamURL); err != nil {
		return nil, fmt.Errorf("invalid UPSTREAM_URL %q: %w", cfg.UpstreamURL, err)
	}
	cfg.UpstreamURL = strings.TrimRight(cfg.UpstreamURL, "/")

	// Validate port
	if cfg.Port < 1 || cfg.Port > 65535 {
		return nil, fmt.Errorf("invalid PORT %d: must be 1-65535", cfg.Port)
	}

	// Show admin key info
	if cfg.AdminKey == "admin281100" {
		fmt.Println("\033[1;33m  [!] Using default admin key. Set ADMIN_KEY env var for production.\033[0m")
	}

	return cfg, nil
}

// PrintStartupBanner prints a formatted startup banner with config summary.
func (cfg *Config) PrintStartupBanner() {
	fmt.Println("\033[1;36m")
	fmt.Println("   ╔═══════════════════════════════════════════╗")
	fmt.Println("   ║        Claude API Gateway  v1.0.0         ║")
	fmt.Println("   ╚═══════════════════════════════════════════╝")
	fmt.Println("\033[0m")
	fmt.Printf("   \033[90m%-22s\033[0m %s\n", "Upstream:", cfg.UpstreamURL)
	fmt.Printf("   \033[90m%-22s\033[0m :%d\n", "Listen:", cfg.Port)
	fmt.Printf("   \033[90m%-22s\033[0m %s\n", "Database:", cfg.DBPath)
	fmt.Printf("   \033[90m%-22s\033[0m %s\n", "Log Level:", cfg.LogLevel)
	fmt.Printf("   \033[90m%-22s\033[0m %d\n", "Default RPM/account:", cfg.DefaultRPM)
	fmt.Printf("   \033[90m%-22s\033[0m %d\n", "Default Concurrency:", cfg.DefaultMaxConcur)
	fmt.Printf("   \033[90m%-22s\033[0m %d\n", "Max Retry Attempts:", cfg.MaxRetryAttempts)
	fmt.Println()
	fmt.Printf("   \033[1;32m▸ Admin UI:\033[0m  http://localhost:%d/\n", cfg.Port)
	fmt.Printf("   \033[1;32m▸ API Base:\033[0m  http://localhost:%d/v1/messages\n", cfg.Port)
	fmt.Printf("   \033[1;32m▸ Health:\033[0m    http://localhost:%d/health\n", cfg.Port)
	fmt.Println()
}

// --- helpers ---

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}

func generateRandomKey(length int) string {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
