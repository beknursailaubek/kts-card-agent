package logfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotate(t *testing.T) {
	dir := t.TempDir()
	w, err := Open(dir, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 1023) + "\n"
	for i := 0; i < 1536; i++ {
		if _, err := w.Write([]byte(line)); err != nil {
			t.Fatal(err)
		}
	}
	w.Close()
	st, err := os.Stat(filepath.Join(dir, "agent.log"))
	if err != nil || st.Size() > 1<<20 {
		t.Fatal(st, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.log.1")); err != nil {
		t.Fatal(err)
	}
}
