package main

import (
	"fmt"
	"strings"
	"time"
)

// message is the normalised shape we hand to Discord, independent of which
// GitHub event produced it.
type message struct {
	Title string
	URL   string
	Body  string
	Repo  string
	Color int
}

// Discord embed colours.
const (
	colGreen  = 0x2ea043 // opened, created, published
	colPurple = 0x8957e5 // merged
	colRed    = 0xd1242f // closed without merging, failure
	colGrey   = 0x6e7681 // everything else
	colBlue   = 0x1f6feb // pushes and comments
)

// ignoredJobNames holds job/check names that are never forwarded, lower-cased.
// Populated from IGNORE_JOB_NAMES at startup.
var ignoredJobNames = map[string]bool{}

// jobNameIgnored matches the whole name and also the trailing segment, so both
// "Spotless" and "acceptanceTests / Spotless" are caught by one entry.
func jobNameIgnored(name string) bool {
	if len(ignoredJobNames) == 0 || name == "" {
		return false
	}
	n := strings.ToLower(strings.TrimSpace(name))
	if ignoredJobNames[n] {
		return true
	}
	if i := strings.LastIndex(n, "/"); i >= 0 {
		if ignoredJobNames[strings.TrimSpace(n[i+1:])] {
			return true
		}
	}
	return false
}

// ignoredCheckApps holds GitHub App names whose check_run / check_suite events
// are never forwarded, lower-cased. Populated from IGNORE_CHECK_APPS.
//
// Setting this to "GitHub Actions" removes the duplication between the two
// event families: an Actions job raises both a workflow_job and a check_run, so
// dropping the Actions-owned checks leaves workflow_job as the single source
// for Actions while external apps (DCO and friends), which raise no
// workflow_job at all, still come through.
var ignoredCheckApps = map[string]bool{}

// forwardCheckSuite is off unless FORWARD_CHECK_SUITE is set. A check_suite is
// only a container for an app's check_runs on one commit: it carries no
// outcome those runs do not already report, so forwarding both duplicates
// every result. With two DCO apps installed, one commit produced four messages
// -- a check_run and a check_suite from each.
var forwardCheckSuite = false

// forwardSuccess is off unless FORWARD_SUCCESS is set. A green CI job is not
// news: only results that need attention are forwarded. workflow_run already
// behaved this way; check_run and workflow_job now match it.
var forwardSuccess = false

// ignoredConclusions are outcomes that need no attention. Defaults to success,
// cancelled and skipped: on a fail-fast matrix a sibling failure cancels the
// rest, and a skipped job did not run at all, so neither is a result of its own.
var ignoredConclusions = map[string]bool{"success": true, "cancelled": true, "skipped": true}

// ignoredWorkflows are workflow names or file paths never forwarded.
var ignoredWorkflows = map[string]bool{}

// onlyWorkflows turns the policy round when it is non-empty: nothing is
// forwarded except the completed workflow_run of a workflow listed here, by
// name or file path. Populated from ONLY_WORKFLOWS.
//
// workflow_run is the single source on purpose. It is the only event that
// carries the workflow's file path -- workflow_job has just the display name,
// which besu renamed from "bft-soak-test" to "BFT Soak Test" on 2026-10-02 --
// and it arrives once per run, where forwarding the jobs as well would repeat
// the same outcome.
var onlyWorkflows = map[string]bool{}

func workflowIgnored(nameOrPath string) bool {
	return workflowListed(ignoredWorkflows, nameOrPath)
}

func workflowListed(list map[string]bool, nameOrPath string) bool {
	if len(list) == 0 || nameOrPath == "" {
		return false
	}
	n := strings.ToLower(strings.TrimSpace(nameOrPath))
	if list[n] {
		return true
	}
	// also match on the file's basename, so ".github/workflows/old_dco.yml"
	// is caught by an "old_dco.yml" entry
	if i := strings.LastIndex(n, "/"); i >= 0 && list[n[i+1:]] {
		return true
	}
	return false
}

func conclusionIgnored(c string) bool {
	return c != "" && !forwardSuccess && ignoredConclusions[c]
}

func checkAppIgnored(app string) bool {
	return len(ignoredCheckApps) > 0 && ignoredCheckApps[strings.ToLower(strings.TrimSpace(app))]
}

// duration renders the gap between two RFC3339 stamps, e.g. "4m12s".
func duration(start, end string) string {
	if start == "" || end == "" {
		return ""
	}
	t0, err0 := time.Parse(time.RFC3339, start)
	t1, err1 := time.Parse(time.RFC3339, end)
	if err0 != nil || err1 != nil || !t1.After(t0) {
		return ""
	}
	return t1.Sub(t0).Round(time.Second).String()
}

// summarise turns an event into a message. The second return is false for
// events we deliberately do not forward, and the third says why, so an
// "ignored" line in the audit log can be told apart from a malfunction.
func summarise(event string, p map[string]any) (message, bool, string) {
	repo := str(sub(p, "repository"), "full_name")
	actor := str(sub(p, "sender"), "login")
	m := message{Repo: repo, Color: colGrey}

	if len(onlyWorkflows) > 0 {
		if event != "workflow_run" {
			return m, false, "ONLY_WORKFLOWS is set: only workflow_run is forwarded, not " + event
		}
		run := sub(p, "workflow_run")
		if !workflowListed(onlyWorkflows, str(run, "name")) && !workflowListed(onlyWorkflows, str(run, "path")) {
			return m, false, "workflow " + str(run, "name") + " is not in ONLY_WORKFLOWS"
		}
	}

	switch event {
	case "push":
		commits, _ := p["commits"].([]any)
		ref := strings.TrimPrefix(str(p, "ref"), "refs/heads/")
		if len(commits) == 0 { // branch create/delete arrive as pushes too
			return m, false, "push carried no commits (branch or tag delete)"
		}
		m.Title = fmt.Sprintf("%s pushed %s to %s", actor, plural(len(commits), "commit"), ref)
		m.URL = str(p, "compare")
		m.Color = colBlue
		var b strings.Builder
		for i, c := range commits {
			if i == 5 {
				fmt.Fprintf(&b, "…and %d more\n", len(commits)-5)
				break
			}
			cm, _ := c.(map[string]any)
			fmt.Fprintf(&b, "`%s` %s\n", short(str(cm, "id")), firstLine(str(cm, "message")))
		}
		m.Body = b.String()

	case "pull_request":
		pr := sub(p, "pull_request")
		action := str(p, "action")
		num := num(pr, "number")
		switch action {
		case "opened", "reopened", "ready_for_review":
			m.Color = colGreen
		case "closed":
			if b, _ := pr["merged"].(bool); b {
				action, m.Color = "merged", colPurple
			} else {
				m.Color = colRed
			}
		default:
			return m, false, "pr action " + action + " is not interesting"
		}
		m.Title = fmt.Sprintf("%s %s PR #%d: %s", actor, action, num, str(pr, "title"))
		m.URL = str(pr, "html_url")

	case "issues":
		action := str(p, "action")
		if action != "opened" && action != "closed" && action != "reopened" {
			return m, false, "issue action " + action + " is not interesting"
		}
		iss := sub(p, "issue")
		if action == "opened" {
			m.Color = colGreen
		}
		m.Title = fmt.Sprintf("%s %s issue #%d: %s", actor, action, num(iss, "number"), str(iss, "title"))
		m.URL = str(iss, "html_url")

	case "issue_comment":
		if str(p, "action") != "created" {
			return m, false, "comment action is not create"
		}
		iss, c := sub(p, "issue"), sub(p, "comment")
		m.Title = fmt.Sprintf("%s commented on #%d: %s", actor, num(iss, "number"), str(iss, "title"))
		m.URL = str(c, "html_url")
		m.Body = truncate(str(c, "body"), 400)
		m.Color = colBlue

	case "release":
		if str(p, "action") != "published" {
			return m, false, "release action is not publish"
		}
		rel := sub(p, "release")
		m.Title = fmt.Sprintf("%s published release %s", actor, str(rel, "tag_name"))
		m.URL = str(rel, "html_url")
		m.Color = colGreen

	case "workflow_run":
		run := sub(p, "workflow_run")
		if str(p, "action") != "completed" {
			return m, false, "workflow run not finished yet"
		}
		if workflowIgnored(str(run, "name")) || workflowIgnored(str(run, "path")) {
			return m, false, "workflow " + str(run, "name") + " is on the ignore list"
		}
		concl := str(run, "conclusion")
		if concl == "skipped" || conclusionIgnored(concl) {
			// only surface what needs attention
			return m, false, "workflow run concluded " + concl
		}
		m.Title = fmt.Sprintf("workflow %s %s on %s", str(run, "name"), concl, str(run, "head_branch"))
		m.URL = str(run, "html_url")
		var parts []string
		if e := str(run, "event"); e != "" {
			parts = append(parts, e)
		}
		if d := duration(str(run, "run_started_at"), str(run, "updated_at")); d != "" {
			parts = append(parts, d)
		}
		if a := num(run, "run_attempt"); a > 1 {
			parts = append(parts, fmt.Sprintf("attempt %d", a))
		}
		m.Body = strings.Join(parts, " · ")
		m.Color = conclusionColour(concl)

	// CI plumbing. These are high volume -- besu-eth/besu emits hundreds an
	// hour -- so check_run and workflow_job are gated to the completed action:
	// the queued and in_progress ones carry no outcome and are roughly two
	// thirds of the traffic.
	case "check_run":
		if a := str(p, "action"); a != "completed" {
			return m, false, "check_run action " + a + " (only completed is forwarded)"
		}
		cr := sub(p, "check_run")
		if app := str(sub(cr, "app"), "name"); checkAppIgnored(app) {
			return m, false, "check_run from app " + app + " is on the ignore list"
		}
		if n := str(cr, "name"); jobNameIgnored(n) {
			return m, false, "check name " + n + " is on the ignore list"
		}
		if c := str(cr, "conclusion"); conclusionIgnored(c) {
			return m, false, "check concluded " + c
		}
		state := str(cr, "conclusion")
		if state == "" {
			state = str(cr, "status")
		}
		m.Title = fmt.Sprintf("check %s: %s", str(cr, "name"), state)
		m.URL = str(cr, "html_url")
		m.Body = fmt.Sprintf("%s · `%s`", str(sub(cr, "app"), "name"), short(str(cr, "head_sha")))
		m.Color = conclusionColour(str(cr, "conclusion"))

	case "check_suite":
		if c := str(sub(p, "check_suite"), "conclusion"); conclusionIgnored(c) {
			return m, false, "check suite concluded " + c
		}
		if !forwardCheckSuite {
			return m, false, "check_suite duplicates its check_runs (set FORWARD_CHECK_SUITE=1 to keep)"
		}
		cs := sub(p, "check_suite")
		if app := str(sub(cs, "app"), "name"); checkAppIgnored(app) {
			return m, false, "check_suite from app " + app + " is on the ignore list"
		}
		state := str(cs, "conclusion")
		if state == "" {
			state = str(cs, "status")
		}
		m.Title = fmt.Sprintf("check suite %s", state)
		if b := str(cs, "head_branch"); b != "" {
			m.Title += " on " + b
		}
		m.URL = str(cs, "html_url") // often absent on this event
		m.Body = fmt.Sprintf("%s · `%s`", str(sub(cs, "app"), "name"), short(str(cs, "head_sha")))
		m.Color = conclusionColour(str(cs, "conclusion"))

	case "workflow_job":
		if a := str(p, "action"); a != "completed" {
			return m, false, "workflow_job action " + a + " (only completed is forwarded)"
		}
		wj := sub(p, "workflow_job")
		if n := str(wj, "name"); jobNameIgnored(n) {
			return m, false, "job name " + n + " is on the ignore list"
		}
		if w := str(wj, "workflow_name"); workflowIgnored(w) {
			return m, false, "workflow " + w + " is on the ignore list"
		}
		if c := str(wj, "conclusion"); conclusionIgnored(c) {
			return m, false, "job concluded " + c
		}
		state := str(wj, "conclusion")
		if state == "" {
			state = str(wj, "status")
		}
		m.Title = fmt.Sprintf("%s — %s", str(wj, "name"), state)
		m.URL = str(wj, "html_url")
		var parts []string
		if w := str(wj, "workflow_name"); w != "" {
			parts = append(parts, w)
		}
		if b := str(wj, "head_branch"); b != "" {
			parts = append(parts, b)
		}
		if d := duration(str(wj, "started_at"), str(wj, "completed_at")); d != "" {
			parts = append(parts, d)
		}
		if a := num(wj, "run_attempt"); a > 1 {
			parts = append(parts, fmt.Sprintf("attempt %d", a))
		}
		if rn := str(wj, "runner_name"); rn != "" {
			parts = append(parts, rn)
		}
		m.Body = strings.Join(parts, " · ")
		m.Color = conclusionColour(str(wj, "conclusion"))

	case "create", "delete":
		m.Title = fmt.Sprintf("%s %sd %s %s", actor, event, str(p, "ref_type"), str(p, "ref"))
		m.URL = str(sub(p, "repository"), "html_url")

	case "star":
		if str(p, "action") != "created" {
			return m, false, "star action is not create"
		}
		m.Title = fmt.Sprintf("%s starred %s", actor, repo)
		m.URL = str(sub(p, "repository"), "html_url")

	case "fork":
		m.Title = fmt.Sprintf("%s forked %s", actor, repo)
		m.URL = str(sub(p, "forkee"), "html_url")

	default:
		return m, false, "event type " + event + " is not handled"
	}
	return m, true, ""
}

// conclusionColour maps a GitHub conclusion to an embed colour. An empty
// conclusion means the thing is still running.
func conclusionColour(c string) int {
	switch c {
	case "success":
		return colGreen
	case "failure", "timed_out", "action_required", "startup_failure":
		return colRed
	case "cancelled", "skipped", "neutral", "stale":
		return colGrey
	case "":
		return colBlue // queued or in progress
	default:
		return colGrey
	}
}

// --- helpers that tolerate anything GitHub sends -------------------------

func sub(p map[string]any, k string) map[string]any {
	if p == nil {
		return nil
	}
	v, _ := p[k].(map[string]any)
	return v
}

func str(p map[string]any, k string) string {
	if p == nil {
		return ""
	}
	s, _ := p[k].(string)
	return s
}

func num(p map[string]any, k string) int {
	if p == nil {
		return 0
	}
	f, _ := p[k].(float64) // encoding/json gives every number as float64
	return int(f)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return truncate(s, 120)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}
