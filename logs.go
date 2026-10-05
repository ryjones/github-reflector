package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// auditLog appends JSON lines to <dir>/<YYYY-MM-DD>.log, rolling at UTC
// midnight. Each direction (inbound from GitHub, outbound to Discord) gets its
// own instance so the two streams stay separable on disk.
type auditLog struct {
	dir  string
	mu   sync.Mutex
	day  string
	file *os.File
}

func newAuditLog(dir string) (*auditLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &auditLog{dir: dir}, nil
}

// write appends one record. A failure here must never drop a webhook, so the
// error is returned for the caller to log rather than being fatal.
func (a *auditLog) write(rec map[string]any) error {
	if a == nil {
		return nil
	}
	now := time.Now().UTC()
	rec["ts"] = now.Format(time.RFC3339Nano)
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	day := now.Format("2006-01-02")
	if a.file == nil || a.day != day {
		if a.file != nil {
			a.file.Close()
		}
		f, err := os.OpenFile(filepath.Join(a.dir, day+".log"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		a.file, a.day = f, day
	}
	_, err = fmt.Fprintf(a.file, "%s\n", line)
	return err
}

// prune deletes day files older than keep days. Only files matching the
// YYYY-MM-DD.log shape are considered, so nothing else in the directory is at
// risk if someone drops a file there.
func prune(dir string, keep int) (removed []string, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -keep)
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".log" {
			continue
		}
		stamp := e.Name()[:len(e.Name())-len(".log")]
		day, perr := time.Parse("2006-01-02", stamp)
		if perr != nil {
			continue // not one of ours
		}
		if day.Before(cutoff) {
			if rerr := os.Remove(filepath.Join(dir, e.Name())); rerr == nil {
				removed = append(removed, e.Name())
			}
		}
	}
	return removed, nil
}
