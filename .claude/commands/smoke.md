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

Export environment variables from the UAT env file (stored in Claude Code's project
memory directory), then run the smoke script. Pass through any flags from $ARGUMENTS
(--full, --roundtrip).

Locate the env file. The path is derived from the project directory by Claude Code
(dashes replace path separators):

```bash
env_file=$(ls ~/.claude/projects/*/memory/.env.uat 2>/dev/null | head -1)
```

If `$env_file` is empty, stop: "UAT credentials not found. Create `.env.uat` in your
Claude Code project memory directory with SW_USER, SW_PASS, SW_BASE, STILLWATER_API_KEY."

Load and run:

```bash
export $(grep -v '^#' "$env_file" | xargs)
bash scripts/smoke.sh $ARGUMENTS
```

If `scripts/smoke.sh` is not found (running from a worktree), find the main repo
and fall back:

```bash
main_repo=$(git worktree list | head -1 | awk '{print $1}')
bash "$main_repo/scripts/smoke.sh" $ARGUMENTS
```

---

## Step 3 -- Report

Summarize the output:
- Total tests run (count lines matching `PASS` or `FAIL`)
- Pass / fail breakdown
- Any failed test names and details

If all pass: "All smoke tests passed."
If failures: List each failure, then suggest next steps.
