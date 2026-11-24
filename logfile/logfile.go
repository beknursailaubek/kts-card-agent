// Package logfile — журнал программы: agent.log до max байт, одна ротация в agent.log.1.
package logfile

import (
	"io"
	"os"
	"path/filepath"
	"sync"
)

type rotator struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	size int64
}

func Open(dir string, max int64) (io.WriteCloser, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	r := &rotator{path: filepath.Join(dir, "agent.log"), max: max}
	return r, r.open()
}

func (r *rotator) open() error {
	f, err := os.OpenFile(r.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	r.f, r.size = f, st.Size()
	return nil
}

func (r *rotator) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.size+int64(len(p)) > r.max && r.size > 0 {
		r.f.Close()
		if err := os.Rename(r.path, r.path+".1"); err != nil {
			_ = os.Remove(r.path) // Windows не заменяет существующий agent.log.1 при сбое — не растём без предела
		}
		if err := r.open(); err != nil {
			return 0, err
		}
	}
	n, err := r.f.Write(p)
	r.size += int64(n)
	return n, err
}

func (r *rotator) Close() error { return r.f.Close() }
