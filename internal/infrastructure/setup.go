package infrastructure

import (
	"log"
	"log/slog"
	"os"
	"strings"
)

func init() {
	opts := PrettyHandlerOptions{
		Multiline: true,
	}
	env := os.Getenv("ENV")
	if env == "" {
		env = "local"
	}
	serviceName := os.Getenv("SERVICE_NAME")
	if serviceName == "" {
		serviceName = "ndocs-ratelimit-demo"
	}
	logLevel := parseLogLevel(os.Getenv("LOG_LEVEL"))

	// This init function is executed as soon as this package is imported.
	if env == "local" {
		opts.SlogOpts = slog.HandlerOptions{
			Level: logLevel,
		}
		logger := slog.New(NewPrettyHandler(os.Stdout, opts)).With(
			slog.String("service", serviceName),
			slog.String("env", env),
		)
		slog.SetDefault(logger)
		return
	}
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level: logLevel,
	})
	logger := slog.New(jsonHandler).With(
		slog.String("service", serviceName),
		slog.String("env", env),
	)
	slog.SetDefault(logger)
}

func parseLogLevel(rawLevel string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(rawLevel)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	case "", "info":
		return slog.LevelInfo
	default:
		log.Printf("invalid LOG_LEVEL %q, defaulting to info", rawLevel)
		return slog.LevelInfo
	}
}
