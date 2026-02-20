# k8s-vectordb-sync

Kubernetes controller (Go/Kubebuilder) that watches cluster resources via dynamic informers and syncs instance metadata to cluster-whisperer's REST API. Follows Viktor Farcic's [dot-ai-controller](https://github.com/vfarcic/dot-ai-controller) architecture.

## YOLO Workflow Mode (ACTIVE)

This repo uses YOLO workflow mode. Proceed without trivial confirmations. Never ask "Shall I continue?", "Do you want to proceed?", or "Ready to start?" — just do the work. The user will interrupt if needed.

**Do ask** when something is ambiguous, when a decision has major implications, or when you need to deviate from what the PRD explicitly defines. This follows the same principle as Getting Help: ask for clarification rather than making assumptions.

**CodeRabbit reviews are REQUIRED before merging any PR.** Autonomously resolve non-architectural comments, summarize changes for the user, and await explicit human approval before merging. This is non-negotiable.

## Autonomous PRD Workflow

This repo uses autonomous workflow chaining. After completing a PRD task, proceed directly: implement the work, run `/prd-update-progress`, then `/prd-next` for the next task. Continue the cycle without pausing for confirmation. Only pause for ambiguous decisions, architectural choices, or PRD deviations.

## CodeRabbit Reviews (MANDATORY)

Every PR must go through CodeRabbit review before merge. This is a hard requirement, not optional.

**Timing:** CodeRabbit reviews take ~5 minutes to complete. After creating a PR, wait at least 5 minutes before checking for the review. Do NOT poll every 30 seconds.

**Process:**
1. Create the PR and push to remote
2. Wait 5 minutes, then check for CodeRabbit review using `mcp__coderabbitai__get_coderabbit_reviews`
3. If review not ready, wait another 2-3 minutes before checking again
4. For each CodeRabbit comment: explain the issue, give a recommendation, then **follow your own recommendation** (YOLO mode)
5. After addressing each issue, use `mcp__coderabbitai__resolve_comment` to mark resolved
6. Only stop for user input if something is truly ambiguous or has major architectural implications
7. After ALL comments are addressed, summarize changes and await explicit human approval before merging

## Tech Stack

- **Language**: Go 1.22+
- **Framework**: Kubebuilder
- **Build**: Make (Kubebuilder-generated Makefile)
- **Deployment**: Helm chart, Dockerfile (multi-arch: amd64 + arm64)
- **CI**: GitHub Actions

## Git Conventions

- Don't squash git commits
- Make a new branch for each new feature
- Never reference task management systems in code files or documentation
- Create a new PR to merge to main anytime there are codebase additions
- Autonomously resolve non-architectural CodeRabbit comments; await explicit human approval before merging

## Testing Strategy

This is a K8s/Infrastructure project. Follow the integration-first testing approach:

- **Integration tests against real Kind clusters** for controller behavior (informer watching, event handling, reconciliation)
- **Unit tests** for pure logic: metadata extraction, resource filtering, debounce/batch assembly, payload formatting
- **Contract tests** for REST API client: request shapes, response handling, retry logic
- **Lifecycle tests** over atomic tests: CREATE → verify → UPDATE → verify → DELETE → verify

<!-- @import: Claude Code rule import, not a filesystem path for contributors -->
See testing decision guide for full K8s/Infrastructure strategy: @~/Documents/Repositories/claude-config/guides/testing-decision-guide.md

## Infrastructure Safety

- When dealing with Kubernetes clusters, always make a backup of any files you edit
- Never render a system unbootable or overwrite any database or datastore without explicit permission
- List planned infrastructure commands before executing so the user can review scope
- Only apply Kubernetes resource manifests directly. Do not run host-level setup scripts unless explicitly asked
