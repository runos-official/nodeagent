package roslog

import (
	"os"
	"sync"
)

// reopenWriter is an io.Writer for a log file that can be reopened by path.
// logrotate renames the log file; Reopen then makes new lines go to a new file
// at the same path, with no restart of the process.
type reopenWriter struct {
	mu   sync.Mutex
	path string
	f    *os.File
	// owned is false when f is a fallback (stderr) that must not be closed.
	owned bool
}

func openLogFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	// O_CREATE does not change the mode of an existing file. Keep the log root-only.
	if err := f.Chmod(0600); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

func newReopenWriter(path string) (*reopenWriter, error) {
	f, err := openLogFile(path)
	if err != nil {
		return nil, err
	}
	return &reopenWriter{path: path, f: f, owned: true}, nil
}

// Write implements io.Writer.
func (w *reopenWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Write(p)
}

// Reopen opens the log path again and swaps it in. On error the current file
// stays in use, so no log line is lost.
func (w *reopenWriter) Reopen() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.owned {
		return nil
	}
	f, err := openLogFile(w.path)
	if err != nil {
		return err
	}
	old := w.f
	w.f = f
	return old.Close()
}

// Close closes the file if the writer owns it.
func (w *reopenWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.owned {
		return nil
	}
	return w.f.Close()
}

// ReopenLogFile reopens the agent log file by path. Call it after logrotate has
// renamed the file. It is safe to call at any time. If the agent logs to stderr
// because the log file could not be opened, it does nothing.
func ReopenLogFile() error {
	logFileMu.Lock()
	w := logWriter
	logFileMu.Unlock()
	return w.Reopen()
}
