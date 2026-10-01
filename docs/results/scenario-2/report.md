# Scenario 2: different models and changing limits: PASS

Run from 2026-10-02T00:08:51+05:30 to 2026-10-02T00:17:16+05:30. Everything below was measured in this run; nothing is projected.

## Checks

| Result | Check | Evidence |
|---|---|---|
| PASS | Every request is accounted for | submitted 480000 = succeeded 334159 + failed 0 + expired 12912 + rejected 132929 + waiting 0; generator sent 480000 (rejected 132929, send errors 0); unknown ids 0 |
| PASS | No 60-second period exceeds a configured limit | model-a: peak 29,453 requests and 29,453,000 tokens in any 60 s, 0 windows over limit, 0 calls refused by the provider; model-b: peak 19,613 requests and 19,613,000 tokens in any 60 s, 0 windows over limit, 0 calls refused by the provider;  |
| PASS | Model A adjusts to the lower limit without a restart | sent 0 requests in the 2 to 40 seconds after the cut (the last minute was already over the new limit, so it must wait); then 4,900 in the final constrained minute against a limit of 5,000 |
| PASS | Model A adjusts to the restored limit without a restart | sent 4,900 requests in seconds 2 to 12 after the restore (full pace is 5,000 per 10 s); 29,400 in the final minute against a limit of 30,000 |
| PASS | Model B continues processing while Model A is constrained | model-b's lowest completed count in any 60-second window after warm-up was 19,587 (limit 20,000, required 18,000) |
| PASS | The service was not restarted | service start time 00:08:51 is unchanged and earlier than the run |
| PASS | The report shows configured limits and observed throughput over time | see the per-model tables under "Throughput over time": one row per 10 seconds with the limit in force and the observed rate |

## Requests

| Submitted | Completed | Successful | Failed | Expired | Rejected | Still waiting |
|---:|---:|---:|---:|---:|---:|---:|
| 480,000 | 334,159 | 334,159 | 0 | 12,912 | 132,929 | 0 |

- Achieved completion rate: **660.4 per second** (334,159 completed in the 506 seconds from first submit to last completion; sending lasted 480 seconds).
- After the 60-second warm-up: **631.9 per second**, 37915 per minute.
- Highest completions in a single second: 898. Provider calls per second including retries: 661.8.
- Submitted at 1000.0 per second. Submit acknowledgement latency: p50 0.39 ms, p99 1.03 ms.
- Accepted request ids looked up one by one after the run: expired 12,912, succeeded 334,159.

## Limits and observed throughput per model

Observed values are the largest total in any 60 consecutive seconds. The provider columns come from the simulator's own counters.

| Model | RPM limit | Sent, peak 60 s | Provider accepted, peak 60 s | TPM limit | Tokens, peak 60 s | Provider tokens, peak 60 s | Refused by provider (429) | Windows over limit |
|---|---:|---:|---:|---:|---:|---:|---:|---:|
| model-a | 30,000 | 29,453 | 29,453 | 60,000,000 | 29,453,000 | 29,453,000 | 0 | 0 |
| model-b | 20,000 | 19,613 | 19,613 | 40,000,000 | 19,613,000 | 19,613,000 | 0 | 0 |

Limit changes during the run:

- +120 s: model-a set to 5,000 RPM, 60,000,000 TPM
- +120 s: model-a set to 5,000 RPM, 60,000,000 TPM
- +300 s: model-a set to 5,000 RPM, 60,000,000 TPM
- +300 s: model-a set to 30,000 RPM, 60,000,000 TPM

## Latency (milliseconds)

Queue is submit to first send. Provider is one provider call. Total is submit to final state, including waiting and retries.

| Model | Stage | p50 | p95 | p99 | Max | Samples |
|---|---|---:|---:|---:|---:|---:|
| model-a | queue | 17344.8 | 24687.1 | 24687.1 | 30000.0 | 169,457 |
| model-a | provider | 83.7 | 250.9 | 401.8 | 1356.5 | 169,457 |
| model-a | total | 18760.1 | 30035.6 | 30035.6 | 30640.6 | 182,369 |
| model-b | queue | 24057.6 | 24057.6 | 24057.6 | 24057.6 | 164,702 |
| model-b | provider | 80.5 | 250.9 | 401.8 | 1425.0 | 164,702 |
| model-b | total | 24687.1 | 24687.1 | 24687.1 | 25422.7 | 164,702 |

## Per-model outcomes

| Model | Submitted | Successful | Failed | Expired | Rejected | Provider calls | Lowest completed in any 60 s after warm-up |
|---|---:|---:|---:|---:|---:|---:|---:|
| model-a | 287,244 | 169,457 | 0 | 12,912 | 104,875 | 169,457 | 840 |
| model-b | 192,756 | 164,702 | 0 | 0 | 28,054 | 164,702 | 19,587 |

## Throughput over time

One row per 10 seconds. "Draining" marks the period after a limit was lowered: the last 60 seconds still hold sends made under the old limit, so the service sends nothing until they age out.

### model-a

| Time | RPM limit | Sent in 10 s | Sent, last 60 s | Share of limit | Tokens, last 60 s | Completed in 10 s | Queued |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 10 s | 30,000 | 4,753 | 4,753 | 15.8% | 4,753,000 | 4,702 | 869 |
| 20 s | 30,000 | 4,899 | 9,652 | 32.2% | 9,652,000 | 4,905 | 1,956 |
| 30 s | 30,000 | 4,900 | 14,552 | 48.5% | 14,552,000 | 4,896 | 3,099 |
| 40 s | 30,000 | 4,901 | 19,453 | 64.8% | 19,453,000 | 4,897 | 4,198 |
| 50 s | 30,000 | 4,900 | 24,353 | 81.2% | 24,353,000 | 4,908 | 5,238 |
| 60 s | 30,000 | 4,900 | 29,253 | 97.5% | 29,253,000 | 4,891 | 6,320 |
| 70 s | 30,000 | 4,899 | 29,399 | 98.0% | 29,399,000 | 4,899 | 7,413 |
| 80 s | 30,000 | 4,901 | 29,401 | 98.0% | 29,401,000 | 4,903 | 8,493 |
| 90 s | 30,000 | 4,900 | 29,401 | 98.0% | 29,401,000 | 4,904 | 9,516 |
| 100 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,901 | 10,609 |
| 110 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,903 | 11,706 |
| 120 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,891 | 11,757 |
| 130 s | 5,000 | 240 | 24,741 | draining | 24,741,000 | 293 | 11,439 |
| 140 s | 5,000 | 0 | 19,840 | draining | 19,840,000 | 0 | 5,446 |
| 150 s | 5,000 | 0 | 14,940 | draining | 14,940,000 | 0 | 1,958 |
| 160 s | 5,000 | 0 | 10,040 | draining | 10,040,000 | 0 | 1,960 |
| 170 s | 5,000 | 0 | 5,140 | draining | 5,140,000 | 0 | 1,960 |
| 180 s | 5,000 | 766 | 1,006 | 20.1% | 1,006,000 | 753 | 1,957 |
| 190 s | 5,000 | 817 | 1,583 | 31.7% | 1,583,000 | 823 | 1,960 |
| 200 s | 5,000 | 816 | 2,399 | 48.0% | 2,399,000 | 816 | 1,959 |
| 210 s | 5,000 | 817 | 3,216 | 64.3% | 3,216,000 | 816 | 1,960 |
| 220 s | 5,000 | 817 | 4,033 | 80.7% | 4,033,000 | 818 | 1,960 |
| 230 s | 5,000 | 816 | 4,849 | 97.0% | 4,849,000 | 817 | 1,959 |
| 240 s | 5,000 | 817 | 4,900 | 98.0% | 4,900,000 | 810 | 1,960 |
| 250 s | 5,000 | 817 | 4,900 | 98.0% | 4,900,000 | 819 | 1,959 |
| 260 s | 5,000 | 816 | 4,900 | 98.0% | 4,900,000 | 819 | 1,959 |
| 270 s | 5,000 | 817 | 4,900 | 98.0% | 4,900,000 | 815 | 1,960 |
| 280 s | 5,000 | 817 | 4,900 | 98.0% | 4,900,000 | 817 | 1,960 |
| 290 s | 5,000 | 816 | 4,900 | 98.0% | 4,900,000 | 820 | 1,959 |
| 300 s | 5,000 | 817 | 4,900 | 98.0% | 4,900,000 | 813 | 1,960 |
| 310 s | 30,000 | 4,698 | 8,781 | 29.3% | 8,781,000 | 4,656 | 2,963 |
| 320 s | 30,000 | 4,900 | 12,865 | 42.9% | 12,865,000 | 4,906 | 4,047 |
| 330 s | 30,000 | 4,900 | 16,948 | 56.5% | 16,948,000 | 4,888 | 5,169 |
| 340 s | 30,000 | 4,900 | 21,031 | 70.1% | 21,031,000 | 4,909 | 6,219 |
| 350 s | 30,000 | 4,900 | 25,115 | 83.7% | 25,115,000 | 4,903 | 7,362 |
| 360 s | 30,000 | 4,900 | 29,198 | 97.3% | 29,198,000 | 4,903 | 8,398 |
| 370 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,889 | 9,413 |
| 380 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,900 | 10,534 |
| 390 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,903 | 11,725 |
| 400 s | 30,000 | 4,901 | 29,401 | 98.0% | 29,401,000 | 4,900 | 11,759 |
| 410 s | 30,000 | 4,899 | 29,400 | 98.0% | 29,400,000 | 4,905 | 11,759 |
| 420 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,884 | 11,759 |
| 430 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,917 | 11,758 |
| 440 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,901 | 11,758 |
| 450 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,897 | 11,760 |
| 460 s | 30,000 | 4,900 | 29,399 | 98.0% | 29,399,000 | 4,896 | 11,759 |
| 470 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,901 | 11,759 |
| 480 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,898 | 11,759 |
| 490 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,897 | 7,350 |
| 500 s | 30,000 | 4,900 | 29,400 | 98.0% | 29,400,000 | 4,899 | 2,450 |

### model-b

| Time | RPM limit | Sent in 10 s | Sent, last 60 s | Share of limit | Tokens, last 60 s | Completed in 10 s | Queued |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 10 s | 20,000 | 3,168 | 3,168 | 15.8% | 3,168,000 | 3,134 | 623 |
| 20 s | 20,000 | 3,267 | 6,435 | 32.2% | 6,435,000 | 3,261 | 1,370 |
| 30 s | 20,000 | 3,266 | 9,701 | 48.5% | 9,701,000 | 3,278 | 2,058 |
| 40 s | 20,000 | 3,267 | 12,968 | 64.8% | 12,968,000 | 3,263 | 2,792 |
| 50 s | 20,000 | 3,267 | 16,235 | 81.2% | 16,235,000 | 3,265 | 3,586 |
| 60 s | 20,000 | 3,266 | 19,501 | 97.5% | 19,501,000 | 3,269 | 4,338 |
| 70 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,274 | 5,078 |
| 80 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,258 | 5,832 |
| 90 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,261 | 6,641 |
| 100 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,266 | 7,382 |
| 110 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,281 | 7,835 |
| 120 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,255 | 7,840 |
| 130 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,268 | 7,838 |
| 140 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,264 | 7,840 |
| 150 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,270 | 7,838 |
| 160 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,270 | 7,838 |
| 170 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,262 | 7,838 |
| 180 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,273 | 7,838 |
| 190 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,263 | 7,838 |
| 200 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,265 | 7,834 |
| 210 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,270 | 7,837 |
| 220 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,261 | 7,839 |
| 230 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,268 | 7,839 |
| 240 s | 20,000 | 3,267 | 19,601 | 98.0% | 19,601,000 | 3,272 | 7,838 |
| 250 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,259 | 7,839 |
| 260 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,276 | 7,838 |
| 270 s | 20,000 | 3,267 | 19,601 | 98.0% | 19,601,000 | 3,264 | 7,837 |
| 280 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,269 | 7,837 |
| 290 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,260 | 7,837 |
| 300 s | 20,000 | 3,266 | 19,599 | 98.0% | 19,599,000 | 3,266 | 7,839 |
| 310 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,266 | 7,836 |
| 320 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,268 | 7,838 |
| 330 s | 20,000 | 3,266 | 19,599 | 98.0% | 19,599,000 | 3,273 | 7,839 |
| 340 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,262 | 7,838 |
| 350 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,268 | 7,838 |
| 360 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,269 | 7,839 |
| 370 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,270 | 7,839 |
| 380 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,261 | 7,838 |
| 390 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,261 | 7,836 |
| 400 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,273 | 7,839 |
| 410 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,265 | 7,837 |
| 420 s | 20,000 | 3,267 | 19,601 | 98.0% | 19,601,000 | 3,269 | 7,837 |
| 430 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,265 | 7,837 |
| 440 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,267 | 7,838 |
| 450 s | 20,000 | 3,267 | 19,601 | 98.0% | 19,601,000 | 3,268 | 7,836 |
| 460 s | 20,000 | 3,266 | 19,600 | 98.0% | 19,600,000 | 3,268 | 7,838 |
| 470 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,262 | 7,839 |
| 480 s | 20,000 | 3,266 | 19,599 | 98.0% | 19,599,000 | 3,270 | 7,838 |
| 490 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,264 | 4,900 |
| 500 s | 20,000 | 3,267 | 19,600 | 98.0% | 19,600,000 | 3,272 | 1,634 |

## Run configuration

- Rate 1000 requests per second for 8m0s (ramp-up 0s), model mix model-a=0.6, model-b=0.4, batch size 1, about 1000 tokens per request (jitter 0%).
- Environment: windows/amd64, 8 logical CPUs (Intel(R) Core(TM) i5-9300H CPU @ 2.40GHz), go1.27.1.

## Simulation assumptions

- The provider is simulated; no real model is called. Latency and failures come from the simulator's configuration.
- Service, simulator and load generator ran on the same machine and share its CPU.
- Token usage equals each request's estimated_tokens.
- A request counts as completed only when the provider call ended in success or final failure. Expired and rejected requests are not completed.
