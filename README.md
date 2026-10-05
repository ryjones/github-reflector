# github-reflector

A small self-hosted service that receives GitHub webhooks and posts a one-line
summary of each to a Discord channel.

Discord can already accept GitHub webhooks directly, by adding `/github` to a
channel webhook URL. That forwards everything GitHub sends, with no filtering
and no record of what happened to a delivery. On a repository with busy CI that
is hundreds of messages an hour, nearly all of them jobs that passed.
github-reflector sits in between and adds three things:

- **Filtering.** Only results that need attention are forwarded: a failed job,
  not the two hundred that passed. Jobs, checks and whole workflows can be
  ignored by name, or the policy can be turned round so that a single
  workflow's runs are the only thing forwarded.
- **A delivery queue.** Each message is written to disk before GitHub is
  answered and retried for up to 24 hours if Discord is unavailable, so a
  Discord outage does not turn into failed deliveries on the GitHub side.
- **An audit trail.** Every delivery is logged with what was done with it and,
  if it was not forwarded, why.

It is a single Go program with no dependencies outside the standard library. It
never calls the GitHub API and holds no GitHub credentials.
[DESIGN.md](DESIGN.md) explains how it works inside, with diagrams of each
path a delivery can take.

```
GitHub ──HTTPS──> Caddy (TLS) ──> reflector ──> queue/waiting ──> Discord webhook
                                      │
                                      └──> logs/  and  queue/skipped
```

A forwarded message is a Discord embed: a title that links to the thing on
GitHub, a short body, a colour for the outcome and the repository in the footer.

```
unit / test — failure
CI · main · 4m12s · ubuntu-1
octo/demo
```

## Contents

- [Quick start](#quick-start)
- [GitHub webhook settings](#github-webhook-settings)
- [Try it locally](#try-it-locally)
- [Running without Docker](#running-without-docker)
- [What gets forwarded](#what-gets-forwarded)
- [Cutting CI noise](#cutting-ci-noise)
- [Configuration](#configuration)
- [Responses](#responses)
- [Logs](#logs)
- [Delivery queue](#delivery-queue)
- [Security](#security)
- [License](#license)

## Quick start

You need a host with Docker and the compose plugin, a DNS name that resolves to
it, and ports 80 and 443 reachable from the internet. Caddy terminates TLS and
gets its certificate from Let's Encrypt over port 80, so there is no certificate
to manage and no DNS credential on the host.

```sh
git clone https://github.com/ryjones/github-reflector.git
cd github-reflector
cp .env.example .env
```

Fill in three values in `.env`:

| Variable | Value |
|---|---|
| `GITHUB_WEBHOOK_SECRET` | a secret you generate: `openssl rand -hex 32` |
| `DISCORD_WEBHOOK_URL` | from Discord: channel → Edit Channel → Integrations → Webhooks → New Webhook → Copy Webhook URL |
| `WEBHOOK_DOMAIN` | the DNS name of this host, for example `hooks.example.org` |

Then start it:

```sh
docker compose up -d --build
docker compose logs -f
```

The reflector container runs as uid and gid `1000` so that the files it writes
to `./logs` and `./queue` belong to an ordinary user on the host. If the user
that cloned the repository has a different id (`id -u`), change `user:` in
`compose.yaml` to match, or the container will fail to start with
`permission denied`.

Finally, point a GitHub webhook at `https://<WEBHOOK_DOMAIN>/webhook` as
described in the next section. One instance serves any number of repositories
and organisations, as long as their webhooks share the secret; everything goes
to the one Discord channel.

## GitHub webhook settings

Repository or organisation → Settings → Webhooks → Add webhook:

| Field | Value |
|---|---|
| Payload URL | `https://<WEBHOOK_DOMAIN>/webhook` |
| Content type | `application/json` |
| Secret | the same string as `GITHUB_WEBHOOK_SECRET` |
| SSL verification | enabled |
| Events | whichever of the events [below](#what-gets-forwarded) you want |

The content type matters: the service parses the body as JSON, and a webhook
left on `application/x-www-form-urlencoded` is answered `400`.

GitHub sends a `ping` when the webhook is saved, and it should come back green.
The webhook's **Recent Deliveries** tab shows the request and response for
anything that does not. Subscribing to an event the service does not handle is
harmless; it is answered `204` and recorded as ignored.

## Try it locally

The development stack has no Caddy and no TLS. It publishes the service on
`127.0.0.1:8080` so that you can drive it with `curl`:

```sh
cp .env.example .env    # fill the two secrets; a made-up signing secret is fine
docker compose -f compose.dev.yaml up --build
```

Send yourself a signed delivery:

```sh
SECRET=$(grep ^GITHUB_WEBHOOK_SECRET .env | cut -d= -f2-)
BODY='{"ref":"refs/heads/main","compare":"https://example.invalid/c","repository":{"full_name":"octo/demo"},"sender":{"login":"octocat"},"commits":[{"id":"abcdef1234567","message":"Test commit"}]}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $2}')"
curl -i -X POST http://127.0.0.1:8080/webhook \
  -H "X-GitHub-Event: push" -H "X-GitHub-Delivery: test-1" \
  -H "X-Hub-Signature-256: $SIG" -d "$BODY"
```

`200` means it was accepted and queued, and "octocat pushed 1 commit to main"
should appear in the Discord channel within a second. `401` means the signature
did not match. To see what the service would send without posting anything, set
`DISCORD_ENABLED=0`; `DISCORD_WEBHOOK_URL` must still be set to something.

## Running without Docker

Go 1.23 or later is the only requirement:

```sh
go build -o github-reflector .

GITHUB_WEBHOOK_SECRET=... DISCORD_WEBHOOK_URL=... \
LOG_DIR=./logs QUEUE_DIR=./queue LISTEN_ADDR=127.0.0.1:8080 \
  ./github-reflector
```

`LOG_DIR` and `QUEUE_DIR` default to `/var/log/reflector` and `/var/queue`, so
set them unless the process may write there. The service speaks plain HTTP and
expects something in front of it to terminate TLS: any reverse proxy that
passes `POST /webhook` through unmodified will do. `GET /healthz` answers `ok`
for health checks.

## What gets forwarded

| Event | Forwarded when |
|---|---|
| `push` | it carries commits; a branch or tag deletion arrives as an empty push and is skipped |
| `pull_request` | opened, reopened, ready_for_review, closed; a merge is coloured differently from a plain close |
| `issues` | opened, closed, reopened |
| `issue_comment` | created |
| `release` | published |
| `workflow_run` | completed, and the conclusion is not on the ignore list |
| `workflow_job` | completed, and the conclusion is not on the ignore list |
| `check_run` | completed, and the conclusion is not on the ignore list |
| `check_suite` | never, unless `FORWARD_CHECK_SUITE=1`; it duplicates its own check runs |
| `create`, `delete` | always: a branch or tag was created or deleted |
| `star` | created |
| `fork` | always |
| `ping` | answered `200` so the webhook goes green; not forwarded |

Anything else is answered `204` and ignored. Noisy sub-actions such as
`synchronize`, `labeled` and `assigned` are deliberately dropped.

The conclusions ignored by default are `success`, `cancelled` and `skipped`, so
out of the box the CI events only surface failures, timeouts and the like.
`IGNORE_CONCLUSIONS` replaces that list, and `FORWARD_SUCCESS=1` switches it
off.

Colours follow the outcome: green for success and for things opened or
published, red for failure, `timed_out`, `action_required` and pull requests
closed without merging, purple for a merge, blue for pushes and comments, grey
for the rest.

## Cutting CI noise

CI events dominate the traffic from any repository with a real test matrix. On
[besu-eth/besu](https://github.com/besu-eth/besu), the repository this was
built to watch, 98% of the first 212 deliveries were `check_run`, `check_suite`
and `workflow_job`. The defaults and the settings below exist to get that down
to the handful of messages somebody needs to read.

**Only finished work.** `check_run` and `workflow_job` are forwarded only for
the `completed` action. The `queued` and `in_progress` deliveries carry no
conclusion and are about two thirds of the volume.

**`cancelled` and `skipped` are not results.** A skipped job never ran. On a
fail-fast matrix one real failure cancels every sibling, so cancelled jobs are
mostly collateral: over 30 days of besu, 150 of the 274 runs containing a
failed job concluded `cancelled` rather than `failure`.

**`check_run` and `workflow_job` overlap.** `workflow_job` is native to GitHub
Actions and describes execution: the workflow, the branch, the runner, the
duration. `check_run` is the generic Checks API and describes a commit's
status. Every Actions job raises both, with the same name, so a single job
produces two messages. `check_run` is the wider of the two, though: external
apps such as DCO raise checks with no `workflow_job` at all.

`IGNORE_CHECK_APPS=GitHub Actions`, which `.env.example` sets, resolves this.
Actions jobs arrive once, as `workflow_job` with its richer payload, and checks
from other apps still arrive as `check_run`. The same filter applies to
`check_suite`.

**Individual jobs.** `IGNORE_JOB_NAMES` drops jobs and checks by name,
whichever of the two events carries them. Matching is case-insensitive against
the whole name and against the segment after the last `/`, so one `Spotless`
entry also catches `acceptanceTests / Spotless`.

**Whole workflows.** `IGNORE_WORKFLOWS` drops a workflow's runs and jobs.
Entries are matched against the workflow's name and, for `workflow_run`, its
file path; the file's basename is enough.

### Forwarding one workflow only

`ONLY_WORKFLOWS` turns the policy round. With it set, the only thing forwarded
is the completed `workflow_run` of a listed workflow. Every other delivery,
including jobs, checks, pushes and pull requests, is declined with a reason
that names the setting. Entries match the workflow's name or its file path, and
the basename is enough:

```
ONLY_WORKFLOWS=nightly.yml
FORWARD_SUCCESS=1
```

That forwards one message per run of `nightly.yml`, pass or fail, and nothing
else. Without `FORWARD_SUCCESS=1` only the runs that did not succeed are
forwarded, because the ignore lists still apply to what the allowlist lets
through.

`workflow_run` is the single source on purpose. It is the only event that
carries the workflow's file path, where `workflow_job` has just the display
name, which anybody editing the workflow can change. It also arrives once per
run, where forwarding the jobs as well would report the same outcome twice. The
message carries what triggered the run, how long it took and the attempt
number, and links to the run page. **The GitHub webhook must be subscribed to
"Workflow runs"** for any of this to arrive.

## Configuration

Everything is set through environment variables, which the compose files read
from `.env`. [`.env.example`](.env.example) documents each one at more length.

| Variable | Default | Meaning |
|---|---|---|
| `GITHUB_WEBHOOK_SECRET` | required | shared signing secret; the same string goes in the webhook's **Secret** field |
| `DISCORD_WEBHOOK_URL` | required | Discord channel webhook; anyone holding it can post to the channel |
| `WEBHOOK_DOMAIN` | required by `compose.yaml` | DNS name Caddy serves and gets a certificate for |
| `DISCORD_ENABLED` | `1` | `0` pauses posting: deliveries are still verified, summarised and logged, and answered `200` |
| `IGNORE_CHECK_APPS` | empty | comma-separated GitHub App names whose checks are never forwarded |
| `IGNORE_JOB_NAMES` | empty | comma-separated job and check names never forwarded |
| `IGNORE_WORKFLOWS` | empty | comma-separated workflow names or file paths never forwarded |
| `ONLY_WORKFLOWS` | empty | comma-separated workflow names or file paths; when set, only their completed runs are forwarded |
| `IGNORE_CONCLUSIONS` | `success,cancelled,skipped` | conclusions never forwarded; setting it replaces the list |
| `FORWARD_SUCCESS` | `0` | `1` forwards every conclusion, overriding `IGNORE_CONCLUSIONS` |
| `FORWARD_CHECK_SUITE` | `0` | `1` forwards `check_suite` |
| `RETRY_WINDOW_HOURS` | `24` | how long a failing post is retried before it is filed under `queue/failed` |
| `DELIVERED_RETENTION_HOURS` | `6` | how long `queue/delivered` is kept |
| `QUEUE_RETENTION_DAYS` | `7` | how long `queue/waiting` and `queue/failed` are kept |
| `LOG_SKIPPED` | `1` | `0` stops recording declined deliveries in `queue/skipped` |
| `SKIPPED_RETENTION_MINUTES` | `60` | how long `queue/skipped` is kept |
| `LOG_RETENTION_DAYS` | `30` | how long the daily audit logs are kept |
| `LISTEN_ADDR` | `:8080` | listen address; fixed by the compose files |
| `LOG_DIR` | `/var/log/reflector` | audit log directory; the compose files mount `./logs` there |
| `QUEUE_DIR` | `/var/queue` | queue directory; the compose files mount `./queue` there |

The service refuses to start if either secret is missing, rather than running
unauthenticated. A changed `.env` takes effect on `docker compose up -d`.

Values in `.env` may contain spaces and are not quoted. Docker Compose reads
that correctly; a shell does not, so do not `source` the file.

## Responses

| Status | Meaning |
|---|---|
| `200` | accepted and queued for Discord; also a `ping`, and anything forwardable while paused |
| `204` | authentic, but not something this forwards |
| `400` | the body is not JSON |
| `401` | the signature is missing or does not match |
| `413` | the body is larger than 5 MB |
| `502` | the delivery could not be written to the queue |

`200` is returned once the delivery is on disk, not once Discord has it. From
then on getting it to Discord is the queue's job.

## Logs

Beyond `docker compose logs`, which is ephemeral, the service writes a durable
JSON-lines audit trail, one file per UTC day per direction:

```
logs/github/YYYY-MM-DD.log    every inbound delivery, accepted or rejected
logs/discord/YYYY-MM-DD.log   every outbound post, one line per attempt
```

Both carry the `delivery` id, GitHub's `X-GitHub-Delivery`, so a delivery can
be followed from arrival to Discord:

```sh
grep <delivery-id> logs/*/$(date -u +%F).log
```

Inbound records carry `event`, `action`, `repo`, `sender`, `bytes`, `status`
and an `outcome`:

| Outcome | Meaning |
|---|---|
| `queued` | accepted and handed to the delivery queue |
| `ignored` | declined by policy; `reason` says why |
| `paused` | would have been forwarded, but `DISCORD_ENABLED=0` |
| `ping` | GitHub's webhook test |
| `bad_signature` | rejected; `remote` holds the peer address |
| `malformed_json` | signed correctly, but the body did not parse |
| `too_large` | body over 5 MB |
| `queue_failed` | could not be written to the queue; `error` says why |

An `ignored` line carries a `reason` such as `event type watch is not handled`,
`job concluded success` or `push carried no commits (branch or tag delete)`, so
a quiet Discord channel can be told apart from a broken one. Push records also
carry `ref`, `commits` and `deleted`. Behind Caddy the `remote` on a rejected
delivery is the proxy's address on the compose network, not the sender's.

To see the shape of what is arriving and what is being done with it:

```sh
jq -r '[.event, .outcome, .reason // ""] | @tsv' logs/github/$(date -u +%F).log \
  | sort | uniq -c | sort -rn
```

Outbound records carry the attempt number, Discord's HTTP status, how long the
item had been queued and the error if the post did not complete.

Files older than 30 days are deleted, at startup and once a day after that.
`LOG_RETENTION_DAYS` changes the window. Pruning only touches files named
`YYYY-MM-DD.log`, so anything else in those directories is left alone.

## Delivery queue

Discord delivery is decoupled from the inbound webhook. Every forwardable
delivery is written to `queue/waiting`, answered `200` immediately, and posted
by a worker:

```
queue/waiting/<ts>-<event>-<delivery>.json     accepted, not yet delivered
queue/delivered/…                              posted successfully
queue/failed/…                                 still failing after the retry window
queue/skipped/…                                declined by policy, with the reason
```

The worker is nudged the moment something is enqueued, so normal delivery takes
well under a second; a 30-second tick is only a backstop.

**A failed post does not become GitHub's problem.** Answering GitHub with an
error during a Discord outage would only mark the delivery failed there, and
GitHub does not retry a failed delivery by itself. Instead the item stays in
`waiting` and is retried with backoff, one minute doubling to a 30-minute
ceiling, for `RETRY_WINDOW_HOURS`. After that it moves to `failed` with its last error. Any
answer from Discord other than a 2xx counts as a failure, including a `429`
rate limit.

**Each item carries both legs of the exchange**: the GitHub headers and body as
received, and the exact Discord payload and response.

```json
{ "delivery": "...", "event": "workflow_job", "repo": "...", "title": "...",
  "inbound_headers": {...}, "inbound_body": {...},
  "discord_body": {...},   "discord_response": "accepted (204)",
  "first_seen": "...", "attempts": 1, "next_attempt": "...", "last_error": "" }
```

Retention differs by state because the states mean different things:

| Directory | Kept | Why |
|---|---|---|
| `queue/delivered` | 6 hours | it worked; only useful for recent spot-checks |
| `queue/waiting` | 7 days | in flight, or evidence of an outage |
| `queue/failed` | 7 days | the ones that need looking at |
| `queue/skipped` | 60 minutes | the bulk of traffic; nothing is pending on it |

`queue/skipped` holds everything the filters declined, each with its
`skip_reason` and the inbound body, so "why did nothing appear in Discord?" is
answerable by looking, without turning anything on. On a busy repository it is
most of the traffic, which is why it ages out in an hour. `LOG_SKIPPED=0` stops
recording it.

Items are written to a temporary file and renamed in, so the worker never reads
a half-written item, and moves between states are renames within one
filesystem.

## Security

**Authentication.** Deliveries are authenticated the way GitHub intends. Every
request carries an `X-Hub-Signature-256` header, an HMAC-SHA256 of the raw body
keyed with the secret you generated and pasted into the webhook's settings. The
service computes the same HMAC and compares in constant time, before parsing
the body. Anything that fails is answered `401` and dropped.

That secret is yours, not GitHub's. It grants nothing on GitHub; it only lets
the service tell a real delivery from a forged one.

**What is on disk.** `logs/` and `queue/` hold webhook payloads: commit
messages, issue and comment text, branch names. For a private repository,
treat those directories as you would the repository. Neither the signing secret
nor the Discord webhook URL is written to a log or a queue item, and
`Authorization` and `Cookie` headers are redacted from stored requests.

**Network.** In the production stack Caddy is the only container bound to the
host, and it routes nothing but `/webhook`. The signature check is the control
that matters, but you can also close port 443 to everything except GitHub.
GitHub publishes the ranges its webhooks come from, with no authentication
needed:

```sh
curl -s https://api.github.com/meta | jq -r '.hooks[]'
```

Port 80 has to stay open to the internet for Let's Encrypt validation. The
ranges change occasionally; if deliveries start failing after you have
restricted 443, read `meta` again before looking anywhere else.

## License

Apache License 2.0. See [LICENSE](LICENSE).
