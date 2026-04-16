package adapterproxy

import (
	"bufio"
	"fmt"
	"os"
	"sync"
	"time"
)

type wireLogger struct {
	mu       sync.Mutex
	file     *os.File
	writer   *bufio.Writer
	path     string
	maxSize  int64 // PX14/PX48: 0 = no rotation
	written  int64
	rotateN  int // CR4-P2a: disambiguate same-second rotations
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

	// PX14/PX48: Check rotation before writing.
	if logger.maxSize > 0 && logger.written > logger.maxSize {
		logger.rotateLocked()
	}

	// CR-P2: Guard against nil writer after failed rotation.
	if logger.writer == nil {
		return
	}

	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	n1, _ := fmt.Fprintf(logger.writer, "%s ", timestamp)
	n2, _ := fmt.Fprintf(logger.writer, format, args...)
	n3, _ := fmt.Fprintln(logger.writer)
	_ = logger.writer.Flush()
	logger.written += int64(n1 + n2 + n3)
}

// PX14/PX48: rotateLocked renames the current file and opens a new one.
func (logger *wireLogger) rotateLocked() {
	if logger.file == nil || logger.path == "" {
		return
	}
	_ = logger.writer.Flush()
	_ = logger.file.Close()

	// AT-09/CR4-P2a: Use timestamp+counter suffix for uniqueness across
	// restarts and same-second rollovers.
	logger.rotateN++
	rotatedPath := fmt.Sprintf("%s.%s.%d", logger.path, time.Now().UTC().Format("20060102-150405"), logger.rotateN)
	_ = os.Rename(logger.path, rotatedPath)

	newFile, err := os.OpenFile(logger.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		logger.file = nil
		logger.writer = nil
		return
	}
	logger.file = newFile
	logger.writer = bufio.NewWriterSize(newFile, 16*1024)
	logger.written = 0
}
