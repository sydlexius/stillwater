---
description: "Create a GitHub issue from the correct template with all required sections filled"
argument-hint: "<type> <title> (type: feature | bug | task)"
allowed-tools: ["Bash", "Read", "Write"]
---

# Create GitHub Issue

Create a new GitHub issue using the project's issue templates, following the
CLAUDE.md protocol for issue creation.

**Arguments:** $ARGUMENTS

---

## Step 1 -- Parse arguments

Extract the issue type (first word) and title (remainder) from $ARGUMENTS.

Valid types: `feature`, `bug`, `task`

If the type is missing or invalid, ask: "What type of issue? (feature / bug / task)"
If the title is missing, ask: "What is the issue title?"

---

## Step 2 -- Read template

Read the corresponding template:
- feature: `.github/ISSUE_TEMPLATE/feature.md`
- bug: `.github/ISSUE_TEMPLATE/bug.md`
- task: `.github/ISSUE_TEMPLATE/task.md`

---

## Step 3 -- Fill sections interactively

Present the agent hint defaults for the issue type and ask if they are OK:
- feature: `[mode: plan] [model: sonnet] [effort: medium]`
- bug: `[mode: direct] [model: sonnet] [effort: medium]`
- task: `[mode: direct] [model: haiku] [effort: low]`

Then for each content section in the template, ask the user to provide input.
If the user gives a brief phrase, expand it into a well-structured section.

---

## Step 4 -- Write body file

Write the fully populated template to `/tmp/gh-issue-body.md`.

---

## Step 5 -- Create the issue

Map the type to its label:
- feature: `enhancement`
- bug: `bug`
- task: `chore`

```bash
gh issue create --title "<title>" --body-file /tmp/gh-issue-body.md --label <label>
```

After creation, ask: "Assign to a milestone? (enter milestone title or skip)"

If yes:
```bash
gh issue edit <number> --milestone "<title>"
```

---

## Step 6 -- Cleanup

```bash
rm /tmp/gh-issue-body.md
```

Report the issue number and URL.
