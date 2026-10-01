# Benchmark report

Every number in the "Measured" sections comes from a run on the machine
below. Full reports, with per-10-second tables, are in
[results/](results/); the console log of the run is
[results/full-run.log](results/full-run.log). Reproduce with
`bin/hti scenario all`.

## Test environment

| | |
|---|---|
| Machine | Laptop, Intel Core i5-9300H @ 2.40 GHz, 4 cores / 8 threads, 7.8 GB RAM |
| OS | Windows 11 (windows/amd64) |
| Go | 1.27.1 |
| Topology | Simulator, service and load generator on the same machine, talking over loopback HTTP. One HTTP call per request from service to simulator |
| Date | 2 October 2026 |

## Simulation assumptions

- The provider is simulated; no real model is called.
- Simulated response time is log-normal with a median of 80 ms and a 99th
  percentile of 400 ms.
- Token usage equals each request's `estimated_tokens`.
- A request counts as completed only when its provider call ended in success
  or final failure. Expired and rejected requests are not completed.
- The simulator enforces limits over a sliding 60-second window in 100 ms
  slots and refuses anything over with a 429. It keeps its own counters; the
  "provider" figures below are from those, not from the service.

## Configuration

Service defaults ([configs/service.json](../configs/service.json)): 2%
headroom, 200 ms pacer burst, 30-second deadline for single requests, admit
while the expected wait is within 80% of the deadline, up to 4 provider calls
per request with 200 ms to 10 s backoff.

## Measured: scenario 1, reach the available capacity

One model at 50,000 RPM and 100M TPM. 1,000-token requests submitted at 1,000
per second (120% of capacity) for 6 minutes: 1 minute warm-up, 5 measured.
[Full report](results/scenario-1/report.md).

| Measure | Result | Required |
|---|---:|---:|
| Completed per minute after warm-up | 49,001 (98.0% of the limit) | at least 45,000 |
| Lowest completed in any 60 s after warm-up | 48,976 | at least 45,000 |
| Provider: peak requests in any 60 s | 49,105 | at most 50,000 |
| Provider: peak tokens in any 60 s | 49,105,000 | at most 100,000,000 |
| Provider calls refused for exceeding a limit | 0 | 0 |
| Submitted | 360,000 | |
| Completed (all successful) | 313,756 | |
| Rejected at submission | 46,244 | |
| Expired | 0 | |
| Unaccounted for | 0 | 0 |

Latency: provider call p50 83.7 ms, p95 261 ms, p99 418 ms. Queue wait
p50, p95 and p99 are all about 24.1 s. That is expected under sustained
overload: the queue stays full, and "full" is defined as 80% of the
30-second deadline, so every admitted request waits about 24 seconds. Requests
beyond that are refused immediately instead of being left to expire, which is
why 46,244 were rejected and none expired.

## Measured: scenario 2, different models and changing limits

Model A at 30,000 RPM / 60M TPM and Model B at 20,000 RPM / 40M TPM, both
overloaded (600 and 400 requests per second) for 8 minutes. Model A was cut
to 5,000 RPM at 2:00 and restored at 5:00 through the HTTP API.
[Full report](results/scenario-2/report.md).

| Measure | Result | Required |
|---|---:|---:|
| Model A sent in seconds 2 to 40 after the cut | 0 | |
| Model A sent in the last constrained minute | 4,900 | at most 5,000 |
| Model A sent in seconds 2 to 12 after the restore | 4,900 (full pace is 5,000 per 10 s) | |
| Model A sent in the final minute | 29,400 | at most 30,000 |
| Model B: lowest completed in any 60 s after warm-up | 19,587 | at least 18,000 |
| Provider: peak requests in any 60 s, A / B | 29,453 / 19,613 | 30,000 / 20,000 |
| Provider calls refused for exceeding a limit | 0 | 0 |
| Service restarted | no | no |
| Submitted | 480,000 | |
| Completed | 334,159 | |
| Expired | 12,912 | |
| Rejected | 132,929 | |
| Unaccounted for | 0 | 0 |

When the limit was lowered, Model A had sent about 29,400 requests in the
previous 60 seconds, far above the new 5,000. The service sent nothing for
about 50 seconds, until that trailing total dropped below the new limit, then
continued at 4,900 per minute. The 12,912 expired requests are the ones that
were already queued for Model A when the cut happened and could not be sent
within their 30-second deadline. Model B was unaffected throughout.

## Measured: scenario 3, asynchronous batch completion

One batch of 10,000 requests across two models, with 5% transient and 2%
permanent provider failures. The callback receiver refused the first two
deliveries. [Full report](results/scenario-3/report.md).

| Measure | Result | Required |
|---|---:|---:|
| Acknowledgement time for 10,000 requests | 14.2 ms | under 1,000 ms |
| First callback attempt relative to the last request's final state | 1 ms after | after |
| Callback attempts and receiver answers | 3: 503, 503, 200 | succeeds after recovery |
| Callback body equals `GET /v1/batches/{id}` | yes | yes |
| Result rows / unique ids / submitted ids | 10,000 / 10,000 / 10,000 | each id exactly once |
| Outcome | 9,900 succeeded, 100 failed (all permanent) | |
| Provider failures injected | 487 transient, 100 permanent | |
| Retry calls made | 487 | |
| Time to complete the batch | about 17 s | |
| Time from completion to accepted callback | about 3 s (retries after 1 s and 2 s) | |

## Failures observed

- No request was lost or counted twice in any run.
- No provider call was refused for exceeding a limit in any run.
- No send errors between the load generator and the service.

## Bottlenecks

- In all three scenarios the limiter is the bottleneck by design: the service
  completes work at 98% of the configured limit and the machine is mostly idle.
- The current provider path makes one HTTP call per request. That is
  appropriate up to a few thousand requests per second and is the reason the
  300,000 per second benchmark has not been run on this path.

## Not yet measured

| Target | State |
|---|---|
| 300,000 completed requests per second | Needs the grouped provider transport, which is not built yet. No figure is claimed |
| 1,000,000 completed requests per second | Same |

## Projections

None in this document yet. The design for 10 billion requests per minute
will be written separately and labelled as a projection.
