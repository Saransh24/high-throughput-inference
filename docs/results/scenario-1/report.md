# Scenario 1: reach the available provider capacity: PASS

Run from 2026-10-02T00:02:25+05:30 to 2026-10-02T00:08:50+05:30. Everything below was measured in this run; nothing is projected.

## Checks

| Result | Check | Evidence |
|---|---|---|
| PASS | Every request is accounted for | submitted 360000 = succeeded 313756 + failed 0 + expired 0 + rejected 46244 + waiting 0; generator sent 360000 (rejected 46244, send errors 0); unknown ids 0 |
| PASS | At least 90% of allowed request capacity is completed after warm-up | average 49001 completed per minute after warm-up (98.0% of 50,000); lowest 60-second window 48,976; required 45,000 |
| PASS | No 60-second period exceeds a configured limit | model-a: peak 49,105 requests and 49,105,000 tokens in any 60 s, 0 windows over limit, 0 calls refused by the provider;  |

## Requests

| Submitted | Completed | Successful | Failed | Expired | Rejected | Still waiting |
|---:|---:|---:|---:|---:|---:|---:|
| 360,000 | 313,756 | 313,756 | 0 | 0 | 46,244 | 0 |

- Achieved completion rate: **815.0 per second** (313,756 completed in the 385 seconds from first submit to last completion; sending lasted 360 seconds).
- After the 60-second warm-up: **816.7 per second**, 49001 per minute.
- Highest completions in a single second: 847. Provider calls per second including retries: 814.1.
- Submitted at 1000.0 per second. Submit acknowledgement latency: p50 0.55 ms, p99 1.16 ms.
- Accepted request ids looked up one by one after the run: succeeded 313,756.

## Limits and observed throughput per model

Observed values are the largest total in any 60 consecutive seconds. The provider columns come from the simulator's own counters.

| Model | RPM limit | Sent, peak 60 s | Provider accepted, peak 60 s | TPM limit | Tokens, peak 60 s | Provider tokens, peak 60 s | Refused by provider (429) | Windows over limit |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| model-a | 50,000 | 49,105 | 49,105 | 100,000,000 | 49,105,000 | 49,105,000 | 0 | 0 |

## Latency (milliseconds)

Queue is submit to first send. Provider is one provider call. Total is submit to final state, including waiting and retries.

| Model | Stage | p50 | p95 | p99 | Max | Samples |
|---|---|---:|---:|---:|---:|---:|
| model-a | queue | 24057.5 | 24057.5 | 24057.5 | 24057.5 | 313,756 |
| model-a | provider | 83.7 | 261.0 | 417.8 | 1489.3 | 313,756 |
| model-a | total | 24687.1 | 24687.1 | 24687.1 | 25487.0 | 313,756 |

## Per-model outcomes

| Model | Submitted | Successful | Failed | Expired | Rejected | Provider calls | Lowest completed in any 60 s after warm-up |
|---|---:|---:|---:|---:|---:|---:|---:|
| model-a | 360,000 | 313,756 | 0 | 0 | 46,244 | 313,756 | 48,976 |

## Throughput over time

One row per 10 seconds. "Draining" marks the period after a limit was lowered: the last 60 seconds still hold sends made under the old limit, so the service sends nothing until they age out.

### model-a

| Time | RPM limit | Sent in 10 s | Sent, last 60 s | Share of limit | Tokens, last 60 s | Completed in 10 s | Queued |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 10 s | 50,000 | 8,274 | 8,274 | 16.5% | 8,274,000 | 8,192 | 1,492 |
| 20 s | 50,000 | 8,165 | 16,439 | 32.9% | 16,439,000 | 8,158 | 3,324 |
| 30 s | 50,000 | 8,167 | 24,606 | 49.2% | 24,606,000 | 8,176 | 5,157 |
| 40 s | 50,000 | 8,168 | 32,774 | 65.5% | 32,774,000 | 8,166 | 6,992 |
| 50 s | 50,000 | 8,165 | 40,939 | 81.9% | 40,939,000 | 8,168 | 8,826 |
| 60 s | 50,000 | 8,166 | 49,105 | 98.2% | 49,105,000 | 8,163 | 10,659 |
| 70 s | 50,000 | 8,169 | 49,000 | 98.0% | 49,000,000 | 8,163 | 12,492 |
| 80 s | 50,000 | 8,165 | 49,000 | 98.0% | 49,000,000 | 8,166 | 14,325 |
| 90 s | 50,000 | 8,167 | 49,000 | 98.0% | 49,000,000 | 8,171 | 16,159 |
| 100 s | 50,000 | 8,166 | 48,998 | 98.0% | 48,998,000 | 8,176 | 17,992 |
| 110 s | 50,000 | 8,167 | 49,000 | 98.0% | 49,000,000 | 8,151 | 19,598 |
| 120 s | 50,000 | 8,166 | 49,000 | 98.0% | 49,000,000 | 8,162 | 19,597 |
| 130 s | 50,000 | 8,167 | 48,998 | 98.0% | 48,998,000 | 8,190 | 19,598 |
| 140 s | 50,000 | 8,167 | 49,000 | 98.0% | 49,000,000 | 8,143 | 19,600 |
| 150 s | 50,000 | 8,166 | 48,999 | 98.0% | 48,999,000 | 8,178 | 19,597 |
| 160 s | 50,000 | 8,169 | 49,002 | 98.0% | 49,002,000 | 8,166 | 19,597 |
| 170 s | 50,000 | 8,165 | 49,000 | 98.0% | 49,000,000 | 8,166 | 19,598 |
| 180 s | 50,000 | 8,167 | 49,001 | 98.0% | 49,001,000 | 8,163 | 19,600 |
| 190 s | 50,000 | 8,166 | 49,000 | 98.0% | 49,000,000 | 8,168 | 19,597 |
| 200 s | 50,000 | 8,167 | 49,000 | 98.0% | 49,000,000 | 8,182 | 19,598 |
| 210 s | 50,000 | 8,167 | 49,001 | 98.0% | 49,001,000 | 8,152 | 19,597 |
| 220 s | 50,000 | 8,166 | 48,998 | 98.0% | 48,998,000 | 8,174 | 19,596 |
| 230 s | 50,000 | 8,167 | 49,000 | 98.0% | 49,000,000 | 8,170 | 19,600 |
| 240 s | 50,000 | 8,168 | 49,001 | 98.0% | 49,001,000 | 8,156 | 19,599 |
| 250 s | 50,000 | 8,166 | 49,001 | 98.0% | 49,001,000 | 8,177 | 19,596 |
| 260 s | 50,000 | 8,166 | 49,000 | 98.0% | 49,000,000 | 8,166 | 19,600 |
| 270 s | 50,000 | 8,166 | 48,999 | 98.0% | 48,999,000 | 8,164 | 19,598 |
| 280 s | 50,000 | 8,168 | 49,001 | 98.0% | 49,001,000 | 8,169 | 19,598 |
| 290 s | 50,000 | 8,166 | 49,000 | 98.0% | 49,000,000 | 8,163 | 19,598 |
| 300 s | 50,000 | 8,166 | 48,998 | 98.0% | 48,998,000 | 8,163 | 19,597 |
| 310 s | 50,000 | 8,168 | 49,000 | 98.0% | 49,000,000 | 8,171 | 19,600 |
| 320 s | 50,000 | 8,166 | 49,000 | 98.0% | 49,000,000 | 8,156 | 19,597 |
| 330 s | 50,000 | 8,167 | 49,001 | 98.0% | 49,001,000 | 8,168 | 19,600 |
| 340 s | 50,000 | 8,167 | 49,000 | 98.0% | 49,000,000 | 8,169 | 19,600 |
| 350 s | 50,000 | 8,166 | 49,000 | 98.0% | 49,000,000 | 8,178 | 19,598 |
| 360 s | 50,000 | 8,167 | 49,001 | 98.0% | 49,001,000 | 8,162 | 19,597 |
| 370 s | 50,000 | 8,166 | 48,999 | 98.0% | 48,999,000 | 8,160 | 12,251 |
| 380 s | 50,000 | 8,167 | 49,000 | 98.0% | 49,000,000 | 8,165 | 4,086 |

## Run configuration

- Rate 1000 requests per second for 6m0s (ramp-up 0s), model mix model-a=1, batch size 1, about 1000 tokens per request (jitter 0%).
- Environment: windows/amd64, 8 logical CPUs (Intel(R) Core(TM) i5-9300H CPU @ 2.40GHz), go1.27.1.

## Simulation assumptions

- The provider is simulated; no real model is called. Latency and failures come from the simulator's configuration.
- Service, simulator and load generator ran on the same machine and share its CPU.
- Token usage equals each request's estimated_tokens.
- A request counts as completed only when the provider call ended in success or final failure. Expired and rejected requests are not completed.
