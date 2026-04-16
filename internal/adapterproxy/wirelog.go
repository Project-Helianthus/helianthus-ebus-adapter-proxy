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

	// CR-P2: Guard against nil writer.
	if logger.writer == nil {
		return
	}

	// Pre-format the line to know its exact size before writing.
	timestamp := time.Now().UTC().Format(time.RFC3339Nano)
	line := fmt.Sprintf("%s ", timestamp) + fmt.Sprintf(format, args...) + "\n"
	lineLen := int64(len(line))

	// Rotate BEFORE writing if this line would exceed the cap.
	if logger.maxSize > 0 && logger.written+lineLen > logger.maxSize {
		logger.rotateLocked()
		if logger.writer == nil {
			return
		}
	}

	n, _ := logger.writer.WriteString(line)
	_ = logger.writer.Flush()
	logger.written += int64(n)
}

// PX14/PX48: rotateLocked renames the current file and opens a new one.
func (logger *wireLogger) rotateLocked() {
	if logger.file == nil || logger.path == "" {
		return
	}
	_ = logger.writer.Flush()
	_ = logger.file.Close()

	// Use nanosecond timestamp suffix for uniqueness across restarts.
	// Use a nanosecond timestamp suffix to reduce rotation name collisions across rapid restarts.
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
