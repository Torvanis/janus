# Load testing

This optional [k6](https://k6.io/) harness exercises chat, streaming,
embeddings, and model-list requests through Janus. Run it only against a
separate test installation and upstreams you control. It can generate
substantial load and upstream charges.

The default thresholds are test targets, **not published benchmark results**:
200 preallocated virtual users, 10,000 requests per minute, less than 1%
failed requests, and less than 100 ms p95 paired gateway overhead. Capacity
and latency depend on hardware, policies, workload, and upstream behavior.

## Setup

1. Install k6 and prepare an isolated Janus instance and test upstream.
2. Enable the desired chat and embeddings models and grant access to a test user.
3. Create a client token through the user interface and supply it through the
   `JANUS_TOKEN` environment variable. Do not commit tokens or include them in
   command-line history.
4. Set `BASELINE_URL` to the upstream URL for paired latency measurements.
   Use `BASELINE_TOKEN` if that upstream requires a different credential.
5. Configure quotas and limits intentionally; rate-limited requests count as
   failed requests in this harness.

Example, after supplying credentials in the environment:

```sh
k6 run test/load/k6.js \
  -e JANUS_URL=http://127.0.0.1:8080 \
  -e JANUS_MODEL=example-model \
  -e JANUS_EMBED_MODEL=example-embedding-model \
  -e BASELINE_URL=http://127.0.0.1:9099 \
  -e USERS=200 -e RATE_PER_MINUTE=10000 -e DURATION=5m
```

Run the load generator on a different machine for meaningful capacity tests.
A co-located generator competes with Janus for CPU and network resources.

## Variables

- `JANUS_URL`: gateway base URL; default `http://127.0.0.1:8080`.
- `JANUS_TOKEN`: required client token.
- `JANUS_MODEL`: enabled and granted chat model; default `example-model`.
- `JANUS_EMBED_MODEL`: embeddings model; defaults to `JANUS_MODEL`.
- `USERS`: preallocated virtual users; default `200`.
- `RATE_PER_MINUTE`: sustained arrival rate; default `10000`.
- `DURATION`: run duration; default `5m`.
- `BASELINE_URL`: required direct upstream URL for overhead measurement.
- `BASELINE_TOKEN`: direct upstream token; defaults to `JANUS_TOKEN`.
- `SKIP_OVERHEAD`: setting `1` explicitly waives overhead measurement for
  harness smoke tests. Such a run does not verify the overhead threshold.

The traffic mix is 60% buffered chat, 15% streaming chat, 20% embeddings, and
5% model listing. The script exits nonzero when thresholds are missed. A
missing baseline aborts by default rather than reporting unmeasured overhead
as a success. The paired measurement is an estimate: direct and proxied
requests are separate requests, so upstream variance still affects it.
