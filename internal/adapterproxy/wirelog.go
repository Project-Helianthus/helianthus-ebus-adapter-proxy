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

	// PX14/PX48/CR5: Rotate at or above limit (>= not >) to prevent
	// exceeding the configured cap by one line.
	if logger.maxSize > 0 && logger.written >= logger.maxSize {
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

	// AT-09/CR4-P2a: Use timestamp+counter suffix for uniqueness.
	// Copilot: Use nanosecond timestamp to avoid collision after restart.
	rotatedPath := fmt.Sprintf("%s.%d", logger.path, time.Now().UnixNano())
	if err := os.Rename(logger.path, rotatedPath); err != nil {
		// CR6-P2b: Rename failed — reopen in append mode and seed written
		// from the actual file size so rotation stays aligned.
		newFile, openErr := os.OpenFile(logger.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if openErr != nil {
			logger.file = nil
			logger.writer = nil
			return
		}
		logger.file = newFile
		logger.writer = bufio.NewWriterSize(newFile, 16*1024)
		if stat, statErr := newFile.Stat(); statErr == nil {
			logger.written = stat.Size()
		}
		return
	}

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
