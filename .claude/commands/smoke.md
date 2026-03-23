---
description: "Run API smoke tests against a running Stillwater instance"
argument-hint: "[--full] [--roundtrip]"
allowed-tools: ["Bash", "Read"]
---

# Smoke Test

Run the API smoke test suite against a running Stillwater instance.

**Arguments:** $ARGUMENTS

---

## Step 1 -- Check the app is running

```bash
curl -sf http://localhost:1973/api/v1/health 2>/dev/null && echo "RUNNING" || echo "NOT_RUNNING"
```

If NOT_RUNNING, stop and say:
"The app is not running at localhost:1973. Start it first:
- Native: `make build && SW_LOG_LEVEL=debug ./stillwater`
- Docker: `./setupdocker.sh`

Then re-run `/smoke`."

---

## Step 2 -- Load credentials and run

Export environment variables from the UAT env file, then run the smoke script.
Pass through any flags from $ARGUMENTS (--full, --roundtrip).

```bash
export $(grep -v '^#' ~/.claude/projects/-root-Dev-stillwater/memory/.env.uat | xargs)
bash scripts/smoke.sh $ARGUMENTS
```

If `scripts/smoke.sh` is not found (running from a worktree), fall back:

```bash
export $(grep -v '^#' ~/.claude/projects/-root-Dev-stillwater/memory/.env.uat | xargs)
bash /root/Dev/stillwater/scripts/smoke.sh $ARGUMENTS
```

If the env file is missing, stop: "UAT credentials not found at
`~/.claude/projects/-root-Dev-stillwater/memory/.env.uat`. Create it with
SW_USER, SW_PASS, SW_BASE, STILLWATER_API_KEY."

---

## Step 3 -- Report

Summarize the output:
- Total tests run (count lines matching `PASS` or `FAIL`)
- Pass / fail breakdown
- Any failed test names and details

If all pass: "All smoke tests passed."
If failures: List each failure, then suggest next steps.
