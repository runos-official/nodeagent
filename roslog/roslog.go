package roslog

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const logFilePath = "/var/log/runos.log"

var (
	programLevel = new(slog.LevelVar)
	L            *slog.Logger
	logFile      *os.File
	logFileMu    sync.Mutex
)

type contextKey string

const (
	logFieldsKey contextKey = "log_fields"
)

func init() {
	initLogger()
}

func initLogger() {
	level := getLogLevel()
	programLevel.Set(level)

	opts := &slog.HandlerOptions{
		Level:     programLevel,
		AddSource: false, // We'll add source manually with proper formatting
	}

	// Open or create the log file
	logFileMu.Lock()
	if logFile == nil {
		// Ensure directory exists
		logDir := filepath.Dir(logFilePath)
		if err := os.MkdirAll(logDir, 0755); err != nil {
			// Fall back to stderr if we can't create log directory
			fmt.Fprintf(os.Stderr, "Failed to create log directory %s: %v\n", logDir, err)
			logFile = os.Stderr
		} else {
			var err error
			logFile, err = os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				// Fall back to stderr if we can't open log file
				fmt.Fprintf(os.Stderr, "Failed to open log file %s: %v\n", logFilePath, err)
				logFile = os.Stderr
			}
		}
	}
	logFileMu.Unlock()

	// Always use JSON handler writing to log file
	handler := slog.NewJSONHandler(logFile, opts)
	L = slog.New(handler)
}

func getLogLevel() slog.Level {
	// Environment variable override
	if level := os.Getenv("RUNOS_LOG_LEVEL"); level != "" {
		switch strings.ToLower(level) {
		case "debug":
			return slog.LevelDebug
		case "info":
			return slog.LevelInfo
		case "warn":
			return slog.LevelWarn
		case "error":
			return slog.LevelError
		}
	}

	// Default to info level
	return slog.LevelInfo
}

// Runtime level control
func SetLogLevel(level slog.Level) {
	programLevel.Set(level)
}

// EnableDebug sets the global log level to debug.
func EnableDebug() {
	programLevel.Set(slog.LevelDebug)
}

// DisableDebug resets the global log level to info.
func DisableDebug() {
	programLevel.Set(slog.LevelInfo)
}

// ContextWithLogFields creates or modifies a context with log fields (metadata)
// If the context already has log fields, the new fields are merged with existing ones
func ContextWithLogFields(ctx context.Context, fields map[string]any) context.Context {
	// Get existing fields if present
	existingFields, ok := ctx.Value(logFieldsKey).(map[string]any)
	if !ok || existingFields == nil {
		// No existing fields, just store the new ones
		return context.WithValue(ctx, logFieldsKey, fields)
	}

	// Merge new fields with existing ones
	merged := make(map[string]any, len(existingFields)+len(fields))
	for k, v := range existingFields {
		merged[k] = v
	}
	for k, v := range fields {
		merged[k] = v
	}

	return context.WithValue(ctx, logFieldsKey, merged)
}

// CreateInstallContext creates a context tagged with installation phase
func CreateInstallContext(ctx context.Context) context.Context {
	return ContextWithLogFields(ctx, map[string]any{
		"phase": "NODE_INSTALLATION",
	})
}

// extractContextValues pulls context metadata if present
func extractContextValues(ctx context.Context) []any {
	fields, ok := ctx.Value(logFieldsKey).(map[string]any)
	if !ok || fields == nil {
		return nil
	}

	// Convert map to slog attributes
	attrs := make([]any, 0, len(fields)*2)
	for key, value := range fields {
		attrs = append(attrs, slog.Any(key, value))
	}

	return attrs
}

// cleanPath removes common prefixes from file paths
func cleanPath(file string) string {
	// Remove common prefixes
	prefixes := []string{
		"/root/runos/dev/nodeagent/",
		"/home/runner/work/nodeagent/nodeagent/",
		// Add other common prefixes as needed
	}

	for _, prefix := range prefixes {
		if strings.HasPrefix(file, prefix) {
			return strings.TrimPrefix(file, prefix)
		}
	}

	// If no prefix matched, just return the filename
	return filepath.Base(file)
}

// cleanFunction removes module prefix from function name
func cleanFunction(fn string) string {
	// Remove module prefix (everything before the last /)
	if idx := strings.LastIndex(fn, "/"); idx >= 0 {
		return fn[idx+1:]
	}
	return fn
}

// logWithCaller logs with the correct caller information
func logWithCaller(level slog.Level, msg string, args ...any) {
	logWithCallerAndContext(context.Background(), level, msg, 2, args...)
}

// logWithCallerAndContext logs with caller info and context values
func logWithCallerAndContext(ctx context.Context, level slog.Level, msg string, callerDepth int, args ...any) {
	if !L.Enabled(ctx, level) {
		return
	}

	// Add context values if present
	if ctx != nil {
		ctxAttrs := extractContextValues(ctx)
		if len(ctxAttrs) > 0 {
			args = append(args, ctxAttrs...)
		}
	}

	// Always include caller info in log file
	pc, file, line, ok := runtime.Caller(callerDepth + 1) // Skip this function and the wrapper
	if ok {
		fn := runtime.FuncForPC(pc)
		args = append(args, slog.Group("source",
			slog.String("function", cleanFunction(fn.Name())),
			slog.String("file", cleanPath(file)),
			slog.Int("line", line),
		))
	}

	// Create record without PC (since we're adding source manually)
	r := slog.NewRecord(time.Now(), level, msg, 0)
	r.Add(args...)

	_ = L.Handler().Handle(ctx, r)
}

// Convenience functions (non-context)
func E(msg string, err error, args ...any) {
	if err != nil {
		args = append(args, slog.Any("error", err))
	}
	logWithCaller(slog.LevelError, msg, args...)
}

// W logs msg at warn level, attaching err as an "error" field when non-nil.
func W(msg string, err error, args ...any) {
	if err != nil {
		args = append(args, slog.Any("error", err))
	}
	logWithCaller(slog.LevelWarn, msg, args...)
}

// I logs msg at info level with the given key/value args.
func I(msg string, args ...any) {
	logWithCaller(slog.LevelInfo, msg, args...)
}

// D logs msg at debug level with the given key/value args.
func D(msg string, args ...any) {
	logWithCaller(slog.LevelDebug, msg, args...)
}

// Context-aware convenience functions
func Ectx(ctx context.Context, msg string, err error, args ...any) {
	if err != nil {
		args = append(args, slog.Any("error", err))
	}
	logWithCallerAndContext(ctx, slog.LevelError, msg, 2, args...)
}

// Wctx logs msg at warn level with fields carried on ctx, attaching err when non-nil.
func Wctx(ctx context.Context, msg string, err error, args ...any) {
	if err != nil {
		args = append(args, slog.Any("error", err))
	}
	logWithCallerAndContext(ctx, slog.LevelWarn, msg, 2, args...)
}

// Ictx logs msg at info level with fields carried on ctx.
func Ictx(ctx context.Context, msg string, args ...any) {
	logWithCallerAndContext(ctx, slog.LevelInfo, msg, 2, args...)
}

// Dctx logs msg at debug level with fields carried on ctx.
func Dctx(ctx context.Context, msg string, args ...any) {
	logWithCallerAndContext(ctx, slog.LevelDebug, msg, 2, args...)
}

// User output functions - these write to stdout for the user to see
// Use these when you want to explicitly show something to the user

// Print outputs a message to stdout (for user-facing output)
func Print(msg string) {
	fmt.Print(msg)
}

// Println outputs a message to stdout with a newline (for user-facing output)
func Println(msg string) {
	fmt.Println(msg)
}

// Printf outputs a formatted message to stdout (for user-facing output)
func Printf(format string, args ...any) {
	fmt.Printf(format, args...)
}
