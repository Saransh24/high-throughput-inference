# High-throughput inference under provider limits

A service that sends inference requests to a provider as fast as each model's
requests-per-minute (RPM) and tokens-per-minute (TPM) limits allow, never
faster, and accounts for every request it was given.

The repository contains three programs in one binary, `hti`:

| Program | Command | What it does |
|---|---|---|
| Inference service | `hti serve` | Accepts single requests and batches, queues them per model, sends them under the model's limits, retries transient failures, calls back when a batch finishes |
| Provider simulator | `hti simulator` | Stands in for the provider: per-model limits, latency and failure mix, with its own independent counters |
| Load generator | `hti loadgen` | Sends traffic at a chosen rate and writes a report |

`hti scenario` runs the assignment's validation scenarios end to end and prints
pass or fail for each criterion.

## Status

| Item | State |
|---|---|
| Scenario 1, 2 and 3 | Implemented; results in [docs/BENCHMARKS.md](docs/BENCHMARKS.md) |
| 300,000 and 1,000,000 completed requests per second | Not built yet. The current provider path makes one HTTP call per request, which is the right shape for the scenarios but not for these rates |
| Design for 10 billion requests per minute | Outlined in the PRD; full write-up pending |

## Requirements

Go 1.22 or newer. No other dependencies: the code uses only the standard
library, and nothing else needs to be running.

## Quick start

```sh
go build -o bin/hti ./cmd/hti      # on Windows: -o bin/hti.exe
go test ./...

bin/hti scenario 3                 # about 25 seconds
bin/hti scenario 1                 # about 7 minutes
bin/hti scenario 2                 # about 9 minutes
bin/hti scenario all
```

Each scenario starts its own simulator and service on local ports
(19090 to 19092 by default, change with `--base-port`), drives them over HTTP,
prints each check, and writes `reports/scenario-N-<timestamp>/` containing
`report.md`, `report.json` and `raw.json` (the per-second series the checks
were computed from). The exit code is non-zero if any check fails.
`--short` runs shorter phases for a quick look; the full run is the evidence.

## Running the three programs yourself

Open three terminals.

```sh
bin/hti simulator --config configs/simulator.json     # http://127.0.0.1:9090
bin/hti serve     --config configs/service.json       # http://127.0.0.1:8080
bin/hti loadgen   --rate 1000 --duration 2m --models model-a=0.6,model-b=0.4
```

Load generator flags:

| Flag | Meaning | Default |
|---|---|---|
| `--rate` | Requests per second | 1000 |
| `--duration` | How long to send | 60s |
| `--models` | Model mix, for example `model-a=0.6,model-b=0.4` | `model-a` |
| `--batch-size` | 1 sends single requests; larger sends batches of that size | 1 |
| `--tokens` | Approximate tokens per request | 1000 |
| `--token-jitter` | Vary tokens by up to this fraction | 0 |
| `--ramp-up` | Time to climb from zero to the full rate | 0 |
| `--warmup` | Period excluded from the after-warm-up figures | 0 |
| `--callback` | Start a callback receiver and attach it to every batch | off |
| `--callback-fail-first` | Reject this many deliveries per batch before accepting | 0 |
| `--drain` | How long to wait for outstanding work after sending stops | 60s |
| `--out` | Report directory | `reports` |

The generator is open-loop: it sends at the requested rate whether or not the
service keeps up, so queueing delay is visible instead of hidden.

At the end of a run the report lists submitted, completed, successful, failed,
expired and rejected requests; the achieved completion rate; p50, p95 and p99
latency split into queue wait, provider time and total; configured and
observed RPM and TPM per model, as seen by both the service and the
simulator; and batch completion and callback timing.

### Sending requests by hand

```sh
# one request
curl -X POST http://127.0.0.1:8080/v1/requests -H "Content-Type: application/json" \
  -d '{"request_id":"req-1","model":"model-a","estimated_tokens":1000,"payload":{"prompt":"hi"}}'
curl http://127.0.0.1:8080/v1/requests/req-1

# a batch with a callback
curl -X POST http://127.0.0.1:8080/v1/batches -H "Content-Type: application/json" \
  -d '{"callback_url":"http://127.0.0.1:9191/callback","requests":[
        {"request_id":"a","model":"model-a","estimated_tokens":1000},
        {"request_id":"b","model":"model-b","estimated_tokens":500}]}'
curl http://127.0.0.1:8080/v1/batches/<batch_id>
curl http://127.0.0.1:8080/v1/batches/<batch_id>/results
```

## Configuring models and changing limits

Models are configuration, not code. `configs/service.json` lists each model's
limits as the service understands them, and `configs/simulator.json` lists
what the simulated provider enforces, plus its latency and failure mix:

```json
"model-a": {
  "rpm": 50000,
  "tpm": 100000000,
  "latency": { "kind": "lognormal", "p50_ms": 80, "p99_ms": 400 },
  "transient_rate": 0.05,
  "permanent_rate": 0.02
}
```

Latency `kind` is `fixed` (`ms`), `uniform` (`min_ms`, `max_ms`) or
`lognormal` (`p50_ms`, `p99_ms`).

To change limits while everything is running:

```sh
bin/hti limits --model model-a --rpm 5000      # lower
bin/hti limits --model model-a --rpm 30000     # restore
```

This calls the two HTTP endpoints below in a safe order (the service first
takes the lower of old and new, then the provider changes, then the service
takes the new value), so the service never sends against a limit the provider
has already lowered. You can also call them directly:

```sh
curl -X PUT http://127.0.0.1:8080/v1/models/model-a/limits -d '{"rpm":5000}'
curl -X PUT http://127.0.0.1:9090/admin/models/model-a     -d '{"rpm":5000}'
```

A `PUT` for a model the service does not know creates it, given both `rpm`
and `tpm`. No restart is involved in any of this.

## API

| Method and path | Purpose |
|---|---|
| `POST /v1/requests` | Submit one request. `202` with its status, `429` if rejected, `200` with the existing request if the id was already submitted |
| `GET /v1/requests/{id}` | Status, attempts, output or error, timestamps |
| `POST /v1/requests/lookup` | Statuses for a list of ids |
| `POST /v1/batches` | Submit a batch, optionally with `callback_url`. `202` with a batch id. An `Idempotency-Key` header makes a repeated submit return the first batch |
| `GET /v1/batches/{id}` | Batch status, counts and callback delivery state |
| `GET /v1/batches/{id}/results` | Per-request results in submission order; `offset`, `limit`, `status` |
| `GET /v1/models` | Models with limits and counters |
| `PUT /v1/models/{model}/limits` | Change `rpm` and/or `tpm` |
| `GET /v1/stats` | Counters, latency and a per-second series for every model |

Simulator: `POST /v1/infer`, `GET /admin/models`, `PUT /admin/models/{model}`,
`GET /admin/stats`.

## How it works

```
clients ──> ingest API ──> lane per model ──> provider client ──> provider
            validate       queue + limiter     HTTP call          (simulator)
            admit/reject   (pacer + guard)          │
                                                    v
            callback  <── batch tracker <── completion handler
            worker        (pending count)   final state or retry
```

**One lane per model.** Each model has its own queue, limiter and dispatcher
loop. A throttled model cannot hold up another.

**Two-part limiter**, applied to both requests and tokens
([internal/limiter](internal/limiter/limiter.go)):

- A *pacer*: a token bucket that refills at 98% of the limit with a 200 ms
  burst. It keeps traffic smooth.
- A *guard*: a sliding window over the last 60.5 seconds in 100 ms slots. A
  request is sent only if the window plus that request fits under the limit
  in force. This is the hard guarantee.

For 50,000 RPM the pace is 816.7 requests per second and the worst case in
any 60 seconds is 49,163.

**Request lifecycle.** `queued → in_flight → succeeded | failed`, with
`retry_wait` between attempts and `expired` if the deadline passes while
waiting. A request refused at the door is `rejected`. The move to a final
state happens once, under the request's lock, so each request is counted once.

**Batches.** A batch holds its requests in submission order and a count of
those not yet final. When the count reaches zero the batch gets its final
status and the callback job is created; that is the only place a callback is
created, so it cannot be sent early. The callback body and the status
endpoint are built by the same function.

## Design decisions and trade-offs

| Decision | Why | Cost |
|---|---|---|
| Sliding 60-second guard instead of a per-minute bucket | "No observed 60-second period" means any window, not calendar minutes. A full-minute bucket can release two minutes of traffic inside one window | 2% of capacity is left unused as headroom |
| Guard window of 60.5 s, not 60 s | The provider counts a request when it arrives, a little after the service sent it. The extra half second absorbs that | Slightly more conservative than strictly needed |
| On a limit decrease, stop until the trailing window fits (strict) | It is the only behaviour that stays within the new limit under every reading of the rule | The model pauses: about 50 s when going from 30,000 to 5,000 RPM |
| Reject at the door when the queue is too long to be served before the deadline | A fast `429` with `Retry-After` is more useful to a client than a request that waits and then expires | Requests are refused while capacity exists a few seconds later |
| Deadline bounds when a request may be *sent*, not when it finishes | A call already in flight has consumed provider capacity; cancelling it wastes that | A request can finish after its deadline |
| Retries are served before new work and pay the limiter again | Every retry is a real provider call and must count against the limit; older work finishes first | Under heavy failure, new work waits |
| Permanent errors are not retried | Retrying a bad request burns capacity | Depends on classifying errors correctly (429, 5xx and timeouts are transient; other 4xx are permanent) |
| Batches are admitted whole or refused whole | A half-accepted batch is hard for a client to reason about | A large batch can be refused when a smaller one would fit |
| Callbacks are at-least-once, signed, with results by link | A receiver can verify the sender (HMAC), de-duplicate by `X-HTI-Delivery-Id`, and fetch results at its own pace | The receiver must be idempotent |
| The simulator keeps its own counters | The service should not be the judge of its own limits. A scenario passes on what the provider saw | Two implementations of window counting to keep correct |
| State is in memory | Keeps the hot path free of I/O | State is lost on restart; a write-behind journal is the planned fix |
| Standard library only | A reviewer needs nothing but Go | Some things (histograms, config) are hand-written |

Known limitations: no authentication; no persistence across restarts; within
a model the queue is first-in first-out, so a very large request at the head
delays the ones behind it; an expired request is noticed when it reaches the
head of the queue; finished requests are kept in memory indefinitely.

## Layout

```
cmd/hti/             entry point and subcommands
internal/limiter/    pacer and 60-second guard, tested on a fake clock
internal/window/     sliding-window counter
internal/service/    lanes, request state machine, batches, callbacks, HTTP API
internal/provider/   provider interface and HTTP client
internal/simulator/  provider simulator
internal/loadgen/    load generator, callback receiver, report
internal/scenario/   the three validation scenarios
internal/metrics/    per-second series and latency histogram
internal/api/        JSON types shared by all of the above
configs/             example configuration
docs/                benchmark report
```
