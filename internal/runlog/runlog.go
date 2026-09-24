// ═══ 更新日志 ═══
// 2026-09-25：保留有界本地运行日志供内置管理台读取，热交接期间跨进程互斥轮换。
package runlog

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

const MaxBytes int64 = 8 << 20

type Writer struct {
	path string
	mu   sync.Mutex
}

var active *Writer

// Configure is called once during startup, before serving requests or starting
// background workers. Console logging remains available when file IO fails.
func Configure(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	// #nosec G304 -- path is derived from the administrator's state_file at startup, never from requests.
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	active = &Writer{path: path}
	return nil
}

func Output(console io.Writer) io.Writer {
	if active == nil {
		return console
	}
	return tee{console: console, file: active}
}

type tee struct {
	console io.Writer
	file    *Writer
}

func (t tee) Write(raw []byte) (int, error) {
	n, err := t.console.Write(raw)
	if _, fileErr := t.file.Write(raw); fileErr != nil {
		_, _ = fmt.Fprintf(os.Stderr, "[runlog] cannot persist log: %v\n", fileErr)
	}
	return n, err
}

func (w *Writer) Write(raw []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	unlock, err := lock(w.path)
	if err != nil {
		return 0, err
	}
	defer unlock()
	info, err := os.Stat(w.path)
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	if info != nil && info.Size()+int64(len(raw)) > MaxBytes {
		if err := os.Remove(w.path + ".2"); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		if err := os.Rename(w.path+".1", w.path+".2"); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
	}
	file, err := os.OpenFile(w.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	// A single oversized diagnostic line cannot defeat the retention bound.
	data := raw
	if int64(len(data)) > MaxBytes {
		data = data[len(data)-int(MaxBytes):]
	}
	_, err = file.Write(data)
	err = errors.Join(err, file.Close())
	if err != nil {
		return 0, err
	}
	return len(raw), nil
}
