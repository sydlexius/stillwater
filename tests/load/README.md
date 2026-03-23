# Stillwater Load Testing

Load tests for the Stillwater API using [vegeta](https://github.com/tsenart/vegeta).

## Prerequisites

Install vegeta:

```bash
go install github.com/tsenart/vegeta@latest
```

## Configuration

Set these environment variables before running:

```bash
export SW_BASE="http://localhost:1973"
export SW_API_KEY="sw_your_api_key_here"
```

## Running

### Quick smoke test (low rate, short duration)

```bash
bash tests/load/run.sh smoke
```

### Read-heavy load test (artist listing endpoints)

```bash
bash tests/load/run.sh read
```

### Search load test

```bash
bash tests/load/run.sh search
```

### Full load test (all endpoint categories)

```bash
bash tests/load/run.sh all
```

### Custom parameters

```bash
SW_RATE=100 SW_DURATION=60s bash tests/load/run.sh read
```

## Profiling during load tests

To capture CPU and memory profiles during a load test:

1. Start Stillwater with pprof enabled:
   ```bash
   SW_PPROF=1 ./stillwater
   ```

2. In another terminal, run the load test:
   ```bash
   bash tests/load/run.sh read
   ```

3. In a third terminal, capture profiles:
   ```bash
   # CPU profile (30 seconds)
   go tool pprof -http=:8080 http://localhost:6060/debug/pprof/profile?seconds=30

   # Heap profile (snapshot)
   go tool pprof -http=:8080 http://localhost:6060/debug/pprof/heap

   # Goroutine dump
   go tool pprof -http=:8080 http://localhost:6060/debug/pprof/goroutine
   ```

## Output

Results are written to `tests/load/results/` (gitignored). Each run produces:
- A text report with latency percentiles, throughput, and error rates
- A binary Vegeta result file (`.bin`) for later re-reporting (including JSON via `vegeta report -type=json`)
