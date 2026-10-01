# Scenario 3: asynchronous batch completion: PASS

Run from 2026-10-02T00:17:17+05:30 to 2026-10-02T00:17:38+05:30. Everything below was measured in this run; nothing is projected.

## Checks

| Result | Check | Evidence |
|---|---|---|
| PASS | Every request is accounted for | submitted 10000 = succeeded 9900 + failed 100 + expired 0 + rejected 0 + waiting 0; generator sent 10000 (rejected 0, send errors 0); unknown ids 0 |
| PASS | The acknowledgement is returned within one second | POST /v1/batches with 10,000 requests answered 202 in 14.2 ms |
| PASS | The callback is sent only after all requests reach a final state | last request reached a final state at 00:17:34.742; first callback attempt arrived at 00:17:34.743 (1 ms later) |
| PASS | Callback delivery succeeds after the destination recovers | receiver answered 503 503 200 ; service records state "delivered" after 3 attempts; all signatures valid: true |
| PASS | The callback summary matches the batch status returned by the API | both report status "completed_with_failures", total 10000, succeeded 9900, failed 100 (permanent 100, retries exhausted 0, expired 0) |
| PASS | Every request id appears exactly once in the final batch result | 10000 result rows, 10000 unique ids, all 10000 submitted ids present once; per-request statuses add up to the summary: true |
| PASS | Transient and permanent failures were exercised | the provider returned 487 transient and 100 permanent failures; the service made 487 retry calls; 100 requests ended failed |
| PASS | No 60-second period exceeds a configured limit | model-a: peak 5,180 requests and 5,180,000 tokens in any 60 s, 0 windows over limit, 0 calls refused by the provider; model-b: peak 5,307 requests and 5,307,000 tokens in any 60 s, 0 windows over limit, 0 calls refused by the provider;  |

## Requests

| Submitted | Completed | Successful | Failed | Expired | Rejected | Still waiting |
|---:|---:|---:|---:|---:|---:|---:|
| 10,000 | 10,000 | 9,900 | 100 | 0 | 0 | 0 |

- Achieved completion rate: **555.6 per second** (10,000 completed in the 18 seconds from first submit to last completion; sending lasted 1 seconds).
- Highest completions in a single second: 793. Provider calls per second including retries: 508.9.
- Submitted at 9783.2 per second. Submit acknowledgement latency: p50 14.22 ms, p99 14.22 ms.

## Limits and observed throughput per model

Observed values are the largest total in any 60 consecutive seconds. The provider columns come from the simulator's own counters.

| Model | RPM limit | Sent, peak 60 s | Provider accepted, peak 60 s | TPM limit | Tokens, peak 60 s | Provider tokens, peak 60 s | Refused by provider (429) | Windows over limit |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| model-a | 30,000 | 5,180 | 5,180 | 60,000,000 | 5,180,000 | 5,180,000 | 0 | 0 |
| model-b | 20,000 | 5,307 | 5,307 | 40,000,000 | 5,307,000 | 5,307,000 | 0 | 0 |

## Latency (milliseconds)

Queue is submit to first send. Provider is one provider call. Total is submit to final state, including waiting and retries.

| Model | Stage | p50 | p95 | p99 | Max | Samples |
|---|---|---:|---:|---:|---:|---:|
| model-a | queue | 5142.0 | 10016.2 | 10370.1 | 10370.1 | 4,943 |
| model-a | provider | 80.5 | 241.3 | 417.8 | 1314.0 | 5,180 |
| model-a | total | 5347.7 | 10016.2 | 10416.8 | 10637.3 | 4,943 |
| model-b | queue | 8232.6 | 15419.5 | 16036.2 | 16042.6 | 5,057 |
| model-b | provider | 80.5 | 261.0 | 417.8 | 960.3 | 5,307 |
| model-b | total | 8232.6 | 15419.5 | 16036.2 | 16297.9 | 5,057 |

## Per-model outcomes

| Model | Submitted | Successful | Failed | Expired | Rejected | Provider calls | Lowest completed in any 60 s after warm-up |
|---|---:|---:|---:|---:|---:|---:|---:|
| model-a | 4,943 | 4,885 | 58 | 0 | 0 | 5,180 | n/a |
| model-b | 5,057 | 5,015 | 42 | 0 | 0 | 5,307 | n/a |

## Batches and callbacks

- Batches submitted: 1, accepted: 1.
- Acknowledgement time: p50 14.2 ms, max 14.2 ms.
- Time to complete a batch: min 16.3 s, median 16.3 s, max 16.3 s.
- Callbacks delivered: 1 of 1. Time from batch completion to the accepted delivery: min 3002 ms, median 3002 ms, max 3002 ms.
- 1 batch(es) needed 3 delivery attempt(s).
- Batches whose results contain every submitted id exactly once: 1 of 1.

## Throughput over time

One row per 10 seconds. "Draining" marks the period after a limit was lowered: the last 60 seconds still hold sends made under the old limit, so the service sends nothing until they age out.

### model-a

| Time | RPM limit | Sent in 10 s | Sent, last 60 s | Share of limit | Tokens, last 60 s | Completed in 10 s | Queued |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 10 s | 30,000 | 4,287 | 4,287 | 14.3% | 4,287,000 | 4,044 | 1,110 |
| 20 s | 30,000 | 893 | 5,180 | 17.3% | 5,180,000 | 899 | 0 |

### model-b

| Time | RPM limit | Sent in 10 s | Sent, last 60 s | Share of limit | Tokens, last 60 s | Completed in 10 s | Queued |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 10 s | 20,000 | 2,859 | 2,859 | 14.3% | 2,859,000 | 2,683 | 2,513 |
| 20 s | 20,000 | 2,448 | 5,307 | 26.5% | 5,307,000 | 2,374 | 0 |

## Run configuration

- Rate 10000 requests per second for 1s (ramp-up 0s), model mix model-a=0.5, model-b=0.5, batch size 10000, about 1000 tokens per request (jitter 0%).
- Environment: windows/amd64, 8 logical CPUs (Intel(R) Core(TM) i5-9300H CPU @ 2.40GHz), go1.27.1.

## Simulation assumptions

- The provider is simulated; no real model is called. Latency and failures come from the simulator's configuration.
- Service, simulator and load generator ran on the same machine and share its CPU.
- Token usage equals each request's estimated_tokens.
- A request counts as completed only when the provider call ended in success or final failure. Expired and rejected requests are not completed.
