package retryhttp

type Logger interface {
	Debug(msg string, args ...any)
	Error(msg string, args ...any)
}

type noopLogger struct{}

func (noopLogger) Debug(msg string, args ...any) {}

func (noopLogger) Error(msg string, args ...any) {}
