package agnt2utils

import (
	"encoding/csv"
	"fmt"
	"os"
	"sync"
)

// CSVWriter scaffolds a concurrent-safe CSV writer for AGNT2 benchmark metrics
type CSVWriter struct {
	file   *os.File
	writer *csv.Writer
	mu     sync.Mutex
}

// NewCSVWriter initializes a new CSV metrics logger
func NewCSVWriter(path string, headers []string) (*CSVWriter, error) {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, fmt.Errorf("failed to open csv file %s: %w", path, err)
	}

	w := csv.NewWriter(f)

	// If file is new/empty, write headers
	stat, err := f.Stat()
	if err == nil && stat.Size() == 0 && len(headers) > 0 {
		if err := w.Write(headers); err != nil {
			f.Close()
			return nil, fmt.Errorf("failed to write csv headers: %w", err)
		}
		w.Flush()
	}

	return &CSVWriter{
		file:   f,
		writer: w,
	}, nil
}

// WriteRow safely writes a single row of metrics
func (c *CSVWriter) WriteRow(row []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.writer.Write(row); err != nil {
		return fmt.Errorf("failed to write csv row: %w", err)
	}
	c.writer.Flush()
	return c.writer.Error()
}

// Close flushes any pending writes and closes the file
func (c *CSVWriter) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writer.Flush()
	return c.file.Close()
}
