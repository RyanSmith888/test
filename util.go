package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// Leveled Logger
// ============================================================================

type Logger struct {
	level LogLevel
	mu    sync.Mutex
}

var gLogger *Logger

func initLogger(level LogLevel) {
	gLogger = &Logger{level: level}
}

func (l *Logger) log(level LogLevel, format string, args ...any) {
	if level < l.level {
		return
	}
	now := time.Now().Format("2006-01-02 15:04:05.000")

	// Get caller info (skip 2: log -> logXxx -> caller)
	_, file, line, ok := runtime.Caller(2)
	caller := "???"
	if ok {
		parts := strings.Split(file, "/")
		if len(parts) > 0 {
			caller = fmt.Sprintf("%s:%d", parts[len(parts)-1], line)
		}
	}

	msg := fmt.Sprintf(format, args...)

	var color string
	switch level {
	case LogLevelDebug:
		color = "\033[90m"
	case LogLevelInfo:
		color = "\033[36m"
	case LogLevelWarn:
		color = "\033[33m"
	case LogLevelError:
		color = "\033[31m"
	}

	l.mu.Lock()
	fmt.Fprintf(os.Stdout, "%s %s%-5s\033[0m \033[90m%-20s\033[0m %s\n",
		now, color, level.String(), caller, msg)
	l.mu.Unlock()
}

func logDebug(format string, args ...any) { gLogger.log(LogLevelDebug, format, args...) }
func logInfo(format string, args ...any)  { gLogger.log(LogLevelInfo, format, args...) }
func logWarn(format string, args ...any)  { gLogger.log(LogLevelWarn, format, args...) }
func logError(format string, args ...any) { gLogger.log(LogLevelError, format, args...) }

// ============================================================================
// HTTP Response Helpers
// ============================================================================

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.Encode(data)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"type":    "error",
		"message": msg,
	})
}

// ============================================================================
// String / Token Helpers
// ============================================================================

func maskToken(token string) string {
	if len(token) <= 12 {
		return strings.Repeat("*", len(token))
	}
	return token[:6] + strings.Repeat("*", len(token)-12) + token[len(token)-6:]
}

func splitAndTrim(s, sep string) []string {
	parts := strings.Split(s, sep)
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

func fmtInt64(n int64) string {
	return strconv.FormatInt(n, 10)
}

func parseInt64(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

// ============================================================================
// Request Context Keys (type-safe, no header hacks)
// ============================================================================

type ctxKey int

const (
	ctxKeyAPIKeyID ctxKey = iota
	ctxKeyAPIKeyName
	ctxKeyAccountState
	ctxKeyStartTime
)
