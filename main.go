// github-reflector receives GitHub webhooks and posts a summary of each one to
// a Discord channel webhook.
//
// It never talks to the GitHub API, so it holds no GitHub credentials. Requests
// are authenticated the way GitHub intends: each delivery carries an
// X-Hub-Signature-256 header, an HMAC-SHA256 of the raw body keyed with a
// shared secret that we also hold. Anything that fails that check is dropped
// before the body is parsed.
package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GitHub's own limit is 25 MB; anything near it is not something we summarise.
const maxBody = 5 << 20

type reflector struct {
	secret     []byte
	discordURL string
	client     *http.Client
	log        *slog.Logger
	ghLog      *auditLog     // every inbound delivery, accepted or not
	dcLog      *auditLog     // every outbound post to Discord
	paused     bool          // accept and log, but do not post to Discord
	logSkipped bool          // record declined deliveries in queue/skipped
	q          *queue        // durable queue: the record and the retry mechanism
	wake       chan struct{} // nudges the worker when something is enqueued
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	secret := os.Getenv("GITHUB_WEBHOOK_SECRET")
	if secret == "" {
		log.Error("GITHUB_WEBHOOK_SECRET is not set; refusing to start unauthenticated")
		os.Exit(1)
	}
	discordURL := os.Getenv("DISCORD_WEBHOOK_URL")
	if discordURL == "" {
		log.Error("DISCORD_WEBHOOK_URL is not set")
		os.Exit(1)
	}
	addr := os.Getenv("LISTEN_ADDR")
	if addr == "" {
		addr = ":8080"
	}

	// Job and check names that are never forwarded, however they conclude.
	for _, n := range strings.Split(os.Getenv("IGNORE_JOB_NAMES"), ",") {
		if n = strings.ToLower(strings.TrimSpace(n)); n != "" {
			ignoredJobNames[n] = true
		}
	}
	if len(ignoredJobNames) > 0 {
		names := make([]string, 0, len(ignoredJobNames))
		for n := range ignoredJobNames {
			names = append(names, n)
		}
		sort.Strings(names)
		log.Info("ignoring job names", "names", names)
	}

	for _, a := range strings.Split(os.Getenv("IGNORE_CHECK_APPS"), ",") {
		if a = strings.ToLower(strings.TrimSpace(a)); a != "" {
			ignoredCheckApps[a] = true
		}
	}
	if len(ignoredCheckApps) > 0 {
		apps := make([]string, 0, len(ignoredCheckApps))
		for a := range ignoredCheckApps {
			apps = append(apps, a)
		}
		sort.Strings(apps)
		log.Info("ignoring check apps", "apps", apps)
	}

	// DISCORD_ENABLED=0 keeps everything running -- signature checks, audit log,
	// captures -- but posts nothing. Deliveries are still answered 200, so
	// GitHub does not queue redeliveries while forwarding is paused.
	paused := os.Getenv("DISCORD_ENABLED") == "0" ||
		strings.EqualFold(os.Getenv("DISCORD_ENABLED"), "false")
	if paused {
		log.Warn("forwarding to Discord is PAUSED; deliveries are logged only")
	}

	forwardSuccess = os.Getenv("FORWARD_SUCCESS") == "1" ||
		strings.EqualFold(os.Getenv("FORWARD_SUCCESS"), "true")
	log.Info("forwarding policy", "successes", forwardSuccess)

	forwardCheckSuite = os.Getenv("FORWARD_CHECK_SUITE") == "1" ||
		strings.EqualFold(os.Getenv("FORWARD_CHECK_SUITE"), "true")

	if v := os.Getenv("IGNORE_CONCLUSIONS"); v != "" {
		ignoredConclusions = map[string]bool{}
		for _, c := range strings.Split(v, ",") {
			if c = strings.ToLower(strings.TrimSpace(c)); c != "" {
				ignoredConclusions[c] = true
			}
		}
	}
	for _, w := range strings.Split(os.Getenv("IGNORE_WORKFLOWS"), ",") {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
			ignoredWorkflows[w] = true
		}
	}
	// ONLY_WORKFLOWS inverts the policy: with it set, the completed run of a
	// listed workflow is the only thing forwarded.
	for _, w := range strings.Split(os.Getenv("ONLY_WORKFLOWS"), ",") {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" {
			onlyWorkflows[w] = true
		}
	}
	if len(onlyWorkflows) > 0 {
		ws := make([]string, 0, len(onlyWorkflows))
		for w := range onlyWorkflows {
			ws = append(ws, w)
		}
		sort.Strings(ws)
		log.Info("forwarding ONLY these workflows' runs", "workflows", ws)
	}
	{
		cs := make([]string, 0, len(ignoredConclusions))
		for c := range ignoredConclusions {
			cs = append(cs, c)
		}
		ws := make([]string, 0, len(ignoredWorkflows))
		for w := range ignoredWorkflows {
			ws = append(ws, w)
		}
		sort.Strings(cs)
		sort.Strings(ws)
		log.Info("ignoring", "conclusions", cs, "workflows", ws)
	}

	queueDir := os.Getenv("QUEUE_DIR")
	if queueDir == "" {
		queueDir = "/var/queue"
	}
	retryWindow := 24 * time.Hour
	if v := os.Getenv("RETRY_WINDOW_HOURS"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			retryWindow = time.Duration(n * float64(time.Hour))
		}
	}
	deliveredKeep := 6 * time.Hour
	if v := os.Getenv("DELIVERED_RETENTION_HOURS"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			deliveredKeep = time.Duration(n * float64(time.Hour))
		}
	}
	// Recording declined deliveries is on by default; it is what makes "why did
	// nothing appear?" answerable. On a very noisy repo it can be turned off.
	logSkipped := os.Getenv("LOG_SKIPPED") != "0" &&
		!strings.EqualFold(os.Getenv("LOG_SKIPPED"), "false")
	skippedKeep := 60 * time.Minute
	if v := os.Getenv("SKIPPED_RETENTION_MINUTES"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			skippedKeep = time.Duration(n * float64(time.Minute))
		}
	}
	queueKeep := 7 * 24 * time.Hour
	if v := os.Getenv("QUEUE_RETENTION_DAYS"); v != "" {
		if n, err := strconv.ParseFloat(v, 64); err == nil && n > 0 {
			queueKeep = time.Duration(n * 24 * float64(time.Hour))
		}
	}

	logDir := os.Getenv("LOG_DIR")
	if logDir == "" {
		logDir = "/var/log/reflector"
	}
	keepDays := 30
	if v := os.Getenv("LOG_RETENTION_DAYS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			keepDays = n
		}
	}
	ghLog, err := newAuditLog(filepath.Join(logDir, "github"))
	if err != nil {
		log.Error("cannot open github log dir", "dir", logDir, "err", err)
		os.Exit(1)
	}
	dcLog, err := newAuditLog(filepath.Join(logDir, "discord"))
	if err != nil {
		log.Error("cannot open discord log dir", "dir", logDir, "err", err)
		os.Exit(1)
	}

	q, err := newQueue(queueDir, retryWindow, log)
	if err == nil {
		q.dcLog = dcLog
	}
	if err != nil {
		log.Error("cannot open queue dir", "dir", queueDir, "err", err)
		os.Exit(1)
	}

	r := &reflector{
		secret:     []byte(secret),
		discordURL: discordURL,
		client:     &http.Client{Timeout: 15 * time.Second},
		log:        log,
		ghLog:      ghLog,
		dcLog:      dcLog,
		paused:     paused,
		logSkipped: logSkipped,
		q:          q,
		wake:       make(chan struct{}, 1),
	}

	// Retry worker. Items land in queue/waiting when a live post fails; this
	// drains them, moving each to queue/delivered or, past the retry window,
	// queue/failed.
	go func() {
		const tick = 30 * time.Second // backstop; enqueue nudges us directly
		for {
			due, err := q.due()
			if err != nil {
				log.Error("cannot read queue", "err", err)
			}
			for _, it := range due {
				q.retry(it, func(b json.RawMessage) (int, error) {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					return r.postRaw(ctx, b)
				})
			}
			select {
			case <-r.wake:
			case <-time.After(tick):
			}
		}
	}()

	// Queue retention: delivered is short, waiting and failed are the record.
	go func() {
		for {
			q.sweep(deliveredKeep, queueKeep, skippedKeep)
			time.Sleep(15 * time.Minute)
		}
	}()
	log.Info("delivery queue", "dir", queueDir, "retry_window", retryWindow.String(),
		"delivered_keep", deliveredKeep.String(), "queue_keep", queueKeep.String(),
		"skipped_keep", skippedKeep.String(), "log_skipped", logSkipped)

	// Prune on start and once a day after that, so a long-running container
	// does not accumulate files indefinitely.
	go func() {
		for {
			for _, d := range []string{filepath.Join(logDir, "github"), filepath.Join(logDir, "discord")} {
				if gone, err := prune(d, keepDays); err != nil {
					log.Error("prune failed", "dir", d, "err", err)
				} else if len(gone) > 0 {
					log.Info("pruned old logs", "dir", d, "keep_days", keepDays, "removed", gone)
				}
			}
			time.Sleep(24 * time.Hour)
		}
	}()
	log.Info("audit logs", "dir", logDir, "keep_days", keepDays)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("POST /webhook", r.handle)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}
	log.Info("listening", "addr", addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server stopped", "err", err)
		os.Exit(1)
	}
}

func (r *reflector) handle(w http.ResponseWriter, req *http.Request) {
	event := req.Header.Get("X-GitHub-Event")
	delivery := req.Header.Get("X-GitHub-Delivery")

	// One audit line per delivery, whatever happens to it — including the ones
	// we reject, which are the interesting ones.
	rec := map[string]any{"event": event, "delivery": delivery}
	status, outcome := http.StatusOK, "forwarded"
	defer func() {
		rec["status"] = status
		rec["outcome"] = outcome
		if err := r.ghLog.write(rec); err != nil {
			r.log.Error("github audit write failed", "err", err)
		}
	}()

	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBody))
	if err != nil {
		status, outcome = http.StatusRequestEntityTooLarge, "too_large"
		http.Error(w, "payload too large", status)
		return
	}
	rec["bytes"] = len(body)

	// The exact delivery as received, before any validation, so a rejected or
	// malformed one is captured too.

	// Authenticate before looking at the content at all.
	if !r.validSignature(req.Header.Get("X-Hub-Signature-256"), body) {
		status, outcome = http.StatusUnauthorized, "bad_signature"
		rec["remote"] = req.RemoteAddr
		r.log.Warn("rejected delivery with bad signature", "delivery", delivery, "event", event)
		http.Error(w, "signature mismatch", status)
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		status, outcome = http.StatusBadRequest, "malformed_json"
		http.Error(w, "malformed json", status)
		return
	}
	rec["repo"] = str(sub(payload, "repository"), "full_name")
	if event == "push" {
		rec["ref"] = str(payload, "ref")
		if c, isList := payload["commits"].([]any); isList {
			rec["commits"] = len(c)
		}
		rec["deleted"], _ = payload["deleted"].(bool)
	}
	rec["sender"] = str(sub(payload, "sender"), "login")
	if a := str(payload, "action"); a != "" {
		rec["action"] = a
	}

	// GitHub sends this when a webhook is first saved; answering 200 is what
	// turns the tick green in the repo settings.
	if event == "ping" {
		outcome = "ping"
		r.log.Info("ping", "delivery", delivery, "zen", str(payload, "zen"))
		w.WriteHeader(status)
		return
	}

	msg, ok, why := summarise(event, payload)
	if !ok {
		status, outcome = http.StatusNoContent, "ignored"
		rec["reason"] = why
		if r.logSkipped {
			if sErr := r.q.skip(&item{
				Delivery: delivery, Event: event, Repo: str(sub(payload, "repository"), "full_name"),
				Headers: headerMap(req.Header), Inbound: json.RawMessage(body),
			}, why); sErr != nil {
				r.log.Error("could not record skipped delivery", "delivery", delivery, "err", sErr)
			}
		}
		r.log.Info("ignored event", "event", event, "delivery", delivery, "reason", why)
		w.WriteHeader(status)
		return
	}
	rec["title"] = msg.Title

	if r.paused {
		// Record what would have gone out, so the window is auditable after the
		// fact, then answer 200 so GitHub does not retry.
		outcome = "paused"
		if err := r.dcLog.write(map[string]any{
			"delivery": delivery, "paused": true,
			"repo": msg.Repo, "title": msg.Title, "body": msg.Body, "url": msg.URL,
		}); err != nil {
			r.log.Error("discord audit write failed", "err", err)
		}
		r.log.Info("not forwarded (paused)", "event", event, "delivery", delivery)
		w.WriteHeader(status)
		return
	}

	if err := r.q.enqueue(&item{
		Delivery: delivery, Event: event, Repo: msg.Repo, Title: msg.Title,
		Headers: headerMap(req.Header), Inbound: json.RawMessage(body),
		Payload: discordBody(msg),
	}); err != nil {
		status, outcome = http.StatusBadGateway, "queue_failed"
		rec["error"] = err.Error()
		r.log.Error("could not queue delivery", "delivery", delivery, "err", err)
		http.Error(w, "could not queue", status)
		return
	}
	outcome = "queued"
	select {
	case r.wake <- struct{}{}: // nudge the worker so delivery is prompt
	default:
	}
	r.log.Info("queued for delivery", "event", event, "delivery", delivery, "repo", msg.Repo)
	w.WriteHeader(status)
}

// postRaw sends an already-rendered payload. The queue worker uses this; it
// deliberately does no retrying of its own, because the queue owns that.
func (r *reflector) postRaw(ctx context.Context, body []byte) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.discordURL, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		// A *url.Error quotes the request URL, and the Discord webhook URL is a
		// credential. This error is written to the queue item and to
		// logs/discord, so keep only the underlying cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return 0, err
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("discord returned %s", resp.Status)
	}
	return resp.StatusCode, nil
}

// validSignature compares the delivery's HMAC in constant time.
func (r *reflector) validSignature(header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, r.secret)
	mac.Write(body)
	return hmac.Equal(want, mac.Sum(nil))
}

type discordPayload struct {
	Username string         `json:"username,omitempty"`
	Embeds   []discordEmbed `json:"embeds"`
}

type discordEmbed struct {
	Title       string `json:"title"`
	URL         string `json:"url,omitempty"`
	Description string `json:"description,omitempty"`
	Color       int    `json:"color,omitempty"`
	Footer      *struct {
		Text string `json:"text"`
	} `json:"footer,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
}

// discordBody renders the message as the exact JSON Discord will receive, so a
// queued retry re-sends byte-for-byte what the first attempt tried.
func discordBody(m message) json.RawMessage {
	embed := discordEmbed{
		Title:       m.Title,
		URL:         m.URL,
		Description: m.Body,
		Color:       m.Color,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}
	if m.Repo != "" {
		embed.Footer = &struct {
			Text string `json:"text"`
		}{Text: m.Repo}
	}
	buf, err := json.Marshal(discordPayload{Username: "github", Embeds: []discordEmbed{embed}})
	if err != nil {
		return json.RawMessage(`{"content":"(payload could not be rendered)"}`)
	}
	return buf
}
