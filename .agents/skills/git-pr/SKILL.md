---
name: git-pr
description: "Use this skill for git operations (staging, committing, branching) and creating GitHub pull requests. Invoke when the user asks to commit changes, create a branch, push code, or open a PR/MR."
argument-hint: "[branch-name] [optional: base-branch]"
allowed-tools: Bash(git *), Bash(gh *)
---

# Git Operations & Pull Request Creation

Handle git workflow operations: staging changes, creating commits, branching, pushing, and opening GitHub pull requests via the `gh` CLI.

## Workflow

### 1. Assess the current state

Run these commands in parallel to understand what needs to be done:

```bash
git status
git diff --stat
git log --oneline -5
git branch --show-current
```

### 2. Create a feature branch (if needed)

If the user is on `main` or the base branch, create a new feature branch:

```bash
git checkout -b <branch-name>
```

Branch naming convention: `feature/<short-description>` or `fix/<short-description>`.
If `$ARGUMENTS` provides a branch name, use that instead.

### 3. Stage and commit changes

- Stage specific files by name — avoid `git add -A` or `git add .` to prevent accidentally committing secrets (`.env`, credentials) or large binaries.
- Write a concise commit message that explains the **why**, not just the **what**.
- Always use a HEREDOC for the commit message to ensure proper formatting:

```bash
git add <file1> <file2> ...
git commit -m "$(cat <<'EOF'
<type>: <short summary>

<optional body explaining why>

Co-Authored-By: Claude Opus 4.6 (1M context) <noreply@anthropic.com>
EOF
)"
```

Commit message types: `feat`, `fix`, `refactor`, `test`, `docs`, `chore`.

### 4. Push to remote

```bash
git push -u origin <branch-name>
```

If the branch already tracks a remote, a plain `git push` suffices.

### 5. Create a pull request

Use the GitHub CLI to create the PR. Always use a HEREDOC for the body:

```bash
gh pr create --title "<short title, under 70 chars>" --body "$(cat <<'EOF'
## Summary
- <bullet point 1>
- <bullet point 2>

## Test plan
- [ ] <testing step 1>
- [ ] <testing step 2>

🤖 Generated with [Claude Code](https://claude.com/claude-code)
EOF
)"
```

Options:
- To target a specific base branch: `--base <branch>`
- To add reviewers: `--reviewer <user1>,<user2>`
- To add labels: `--label <label1>,<label2>`
- For draft PRs: `--draft`

### 6. Return the PR URL

After creating the PR, output the URL so the user can access it directly.

## Safety rules

- **Never force push** (`--force`, `--force-with-lease`) without explicit user confirmation.
- **Never push to main/master** directly — always use a feature branch + PR.
- **Never skip hooks** (`--no-verify`) unless the user explicitly asks.
- **Never amend** a published commit without explicit user confirmation.
- **Never commit** files that look like secrets (`.env`, `credentials.json`, `*.pem`, `*.key`). Warn the user instead.
- **Never run** `git reset --hard`, `git checkout .`, `git clean -f`, or `git branch -D` without explicit confirmation.
- If a pre-commit hook fails, fix the underlying issue and create a **new** commit — do not amend.

## Handling common scenarios

### User says "commit and push"
1. Stage changed files
2. Create commit
3. Push to current branch (create branch first if on main)

### User says "create a PR" (changes already committed)
1. Check if branch tracks remote; push if needed
2. Determine base branch (default: `main`)
3. Analyze all commits on the branch vs base to write the PR description
4. Create PR via `gh pr create`

### User says "commit, push, and create a PR"
1. Full workflow: stage → commit → push → create PR

### Merge conflicts
- Prefer resolving conflicts over discarding changes
- Show the user the conflicting files before taking action
