package adapterproxy

import (
	"bufio"
	"fmt"
	"os"
	"sync"
	"time"
)

type wireLogger struct {
	mu     sync.Mutex
	file   *os.File
	writer *bufio.Writer
}

func (logger *wireLogger) Close() error {
	if logger == nil {
		return nil
	}

	logger.mu.Lock()
	defer logger.mu.Unlock()

	if logger.writer != nil {
		_ = logger.writer.Flush()
	}
	if logger.file != nil {
		return logger.file.Close()
	}
	return nil
}

func (logger *wireLogger) LogLine(format string, args ...any) {
	if logger == nil {
		return
	}

	logger.mu.Lock()
	defer logger.mu.Unlock()

	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	_, _ = fmt.Fprintf(logger.writer, "%s ", timestamp)
	_, _ = fmt.Fprintf(logger.writer, format, args...)
	_, _ = fmt.Fprintln(logger.writer)
	_ = logger.writer.Flush()
}
