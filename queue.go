package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A queue decouples Discord delivery from the inbound webhook. When a post
// fails we no longer answer GitHub 502 and rely on its redelivery: the work
// item is written to queue/waiting and we answer 200, because we have taken
// durable responsibility for it. A worker retries with backoff; an item that
// succeeds moves to queue/delivered, and one still failing after the retry
// window moves to queue/failed.
type queue struct {
	waiting   string
	delivered string
	failed    string
	skipped   string
	window    time.Duration // how long to keep retrying before giving up
	log       *slog.Logger
	dcLog     *auditLog // outbound audit trail; the queue item has the detail,
	// this keeps logs/discord a continuous per-attempt record alongside
	// logs/github rather than going quiet once the queue took over delivery
}

// item is one pending Discord post, written as JSON. The Discord payload is
// stored verbatim so a retry re-sends exactly what was built originally.
type item struct {
	Delivery    string          `json:"delivery"`
	Event       string          `json:"event"`
	Repo        string          `json:"repo"`
	Title       string          `json:"title,omitempty"`
	Reason      string          `json:"skip_reason,omitempty"`
	Headers     map[string]any  `json:"inbound_headers,omitempty"`
	Inbound     json.RawMessage `json:"inbound_body,omitempty"`
	Payload     json.RawMessage `json:"discord_body,omitempty"`
	Response    string          `json:"discord_response,omitempty"`
	FirstSeen   time.Time       `json:"first_seen"`
	Attempts    int             `json:"attempts"`
	LastError   string          `json:"last_error,omitempty"`
	NextAttempt time.Time       `json:"next_attempt"`

	path string // where this item currently lives; not serialised
}

func newQueue(dir string, window time.Duration, log *slog.Logger) (*queue, error) {
	q := &queue{
		waiting:   filepath.Join(dir, "waiting"),
		delivered: filepath.Join(dir, "delivered"),
		failed:    filepath.Join(dir, "failed"),
		skipped:   filepath.Join(dir, "skipped"),
		window:    window,
		log:       log,
	}
	for _, d := range []string{q.waiting, q.delivered, q.failed, q.skipped} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return q, nil
}

// backoff grows quickly then flattens, so a long outage is retried steadily
// rather than hammering or going silent.
func backoff(attempts int) time.Duration {
	d := time.Duration(1<<min(attempts, 6)) * 30 * time.Second // 30s … 32m
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return d
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// name is stable across retries so an item keeps its identity as it moves.
func (it *item) name() string {
	ev := unsafeName.ReplaceAllString(it.Event, "_")
	d := unsafeName.ReplaceAllString(it.Delivery, "_")
	if d == "" {
		d = "nodelivery"
	}
	return fmt.Sprintf("%s-%s-%s.json", it.FirstSeen.UTC().Format("20060102T150405.000000000Z"), ev, d)
}

// enqueue writes a new work item into waiting, due immediately. The write goes
// to a temp file and is renamed in, so the worker never reads a half-written
// item. EVERY forwardable delivery goes through here, so queue/ is both the
// retry mechanism and the record of both legs of each exchange -- which is why
// there is no longer a separate debug capture.
func (q *queue) enqueue(it *item) error {
	it.FirstSeen = time.Now().UTC()
	it.NextAttempt = it.FirstSeen
	return q.write(filepath.Join(q.waiting, it.name()), it)
}

func (q *queue) write(path string, it *item) error {
	buf, err := json.MarshalIndent(it, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(buf, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// due returns the waiting items whose next attempt has arrived.
func (q *queue) due() ([]*item, error) {
	entries, err := os.ReadDir(q.waiting)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	var out []*item
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p := filepath.Join(q.waiting, e.Name())
		buf, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var it item
		if err := json.Unmarshal(buf, &it); err != nil {
			q.log.Error("unreadable queue item", "file", e.Name(), "err", err)
			continue
		}
		it.path = p
		if !it.NextAttempt.After(now) {
			out = append(out, &it)
		}
	}
	return out, nil
}

func (q *queue) move(it *item, dir string) error {
	return os.Rename(it.path, filepath.Join(dir, filepath.Base(it.path)))
}

// result records one delivery attempt in logs/discord.
func (q *queue) result(it *item, status int, err error) {
	if q.dcLog == nil {
		return
	}
	rec := map[string]any{
		"delivery": it.Delivery,
		"attempt":  it.Attempts,
		"repo":     it.Repo,
		"title":    it.Title,
		"queued_s": int(time.Since(it.FirstSeen).Seconds()),
	}
	if status != 0 {
		rec["status"] = status
	}
	if err != nil {
		rec["error"] = err.Error()
	}
	if wErr := q.dcLog.write(rec); wErr != nil {
		q.log.Error("discord audit write failed", "err", wErr)
	}
}

// retry attempts one delivery and files the item according to the result.
func (q *queue) retry(it *item, send func(json.RawMessage) (int, error)) {
	status, err := send(it.Payload)
	if err == nil {
		it.Attempts++
		it.Response = fmt.Sprintf("accepted (%d)", status)
		it.LastError = ""
		q.result(it, status, nil)
		if wErr := q.write(it.path, it); wErr != nil {
			q.log.Error("could not finalise queue item", "item", filepath.Base(it.path), "err", wErr)
		}
		if mErr := q.move(it, q.delivered); mErr != nil {
			q.log.Error("could not file delivered item", "item", filepath.Base(it.path), "err", mErr)
		} else {
			q.log.Info("delivered", "delivery", it.Delivery, "attempts", it.Attempts,
				"waited", time.Since(it.FirstSeen).Round(time.Second).String())
		}
		return
	}

	it.Attempts++
	it.LastError = err.Error()
	q.result(it, status, err)
	if time.Since(it.FirstSeen) > q.window {
		if mErr := q.move(it, q.failed); mErr != nil {
			q.log.Error("could not file failed item", "item", filepath.Base(it.path), "err", mErr)
			return
		}
		// record the final error alongside the item
		_ = q.write(filepath.Join(q.failed, filepath.Base(it.path)), it)
		q.log.Error("giving up on queued item", "delivery", it.Delivery,
			"attempts", it.Attempts, "window", q.window.String(), "err", err)
		return
	}
	it.NextAttempt = time.Now().UTC().Add(backoff(it.Attempts))
	if wErr := q.write(it.path, it); wErr != nil {
		q.log.Error("could not update queue item", "item", filepath.Base(it.path), "err", wErr)
	}
}

// skip records a delivery that policy declined to forward, so what was
// dropped and why is inspectable without turning anything on. These are the
// bulk of the traffic and nothing is pending on them, hence the short window.
func (q *queue) skip(it *item, reason string) error {
	it.FirstSeen = time.Now().UTC()
	it.Reason = reason
	return q.write(filepath.Join(q.skipped, it.name()), it)
}

// sweep enforces the retention of each directory.
func (q *queue) sweep(deliveredFor, keepFor, skippedFor time.Duration) {
	for dir, age := range map[string]time.Duration{
		q.delivered: deliveredFor,
		q.waiting:   keepFor,
		q.failed:    keepFor,
		q.skipped:   skippedFor,
	} {
		n, err := pruneByAge(dir, age)
		if err != nil {
			q.log.Error("queue sweep failed", "dir", dir, "err", err)
		} else if n > 0 {
			q.log.Info("pruned queue", "dir", filepath.Base(dir), "keep", age.String(), "removed", n)
		}
	}
}
