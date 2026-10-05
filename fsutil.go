package main

// Filesystem helpers shared by the audit log and the delivery queue.
//
// These began life supporting a separate DEBUG_CAPTURE mode that wrote the
// exact request and response of every delivery to requests/{github,discord}.
// The queue now carries both legs of each exchange and ages them through
// waiting -> delivered/failed, so that mode was removed and only the helpers
// it introduced remain.

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Headers whose value must never reach disk.
var redactHeaders = map[string]bool{
	"authorization":       true,
	"cookie":              true,
	"set-cookie":          true,
	"proxy-authorization": true,
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]`)

// stamp is the filename prefix shared by a request/response pair, so the two
// halves of one exchange sort together. Nanoseconds keep it unique.

// headerMap flattens headers, redacting anything sensitive.
func headerMap(h http.Header) map[string]any {
	out := map[string]any{}
	for k, v := range h {
		if redactHeaders[strings.ToLower(k)] {
			out[k] = "<redacted>"
			continue
		}
		if len(v) == 1 {
			out[k] = v[0]
		} else {
			out[k] = v
		}
	}
	return out
}

// bodyValue embeds the body as JSON when it parses, so the capture file is one
// readable document rather than a string of escaped JSON.
func bodyValue(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err == nil {
		return v
	}
	return string(b)
}

// pruneByAge removes capture files older than age. They are not named by day,
// so this goes on modification time.
func pruneByAge(dir string, age time.Duration) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().Add(-age)
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(dir, e.Name())) == nil {
			removed++
		}
	}
	return removed, nil
}
