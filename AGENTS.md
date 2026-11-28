# Agent Workspace

- Backend: Go. Keep one local process and SQLite; do not introduce microservices.
- Product source of truth: `PRD.md`. Target architecture contracts and external design provenance: `docs/architecture.md` (orchestration in `docs/orchestration.md`, provider compatibility in `docs/provider-compatibility.md`, frozen convergence ADR in `docs/adr/0001-architecture-convergence.md`, and active Phase 0-5 execution roadmap in `docs/plans/architecture-convergence-roadmap.md`).
- A local Pi Agent implements changes. Codex coordinates, inspects, validates, and reviews.
- Use an isolated branch/worktree for each slice. Do not edit another worker's owned paths or revert its changes.
- Never commit `.local/`, runtime databases, agent sessions, credentials, or personal configuration. Local progress is `.local/progress/development.md`.
- Keep message visibility separate from model wakeups, message persistence separate from delivery acknowledgements, and task submission separate from acceptance.
- An unknown external execution must remain unknown until reconciled. A cancellation request does not release resources before a terminal receipt.
- Bind tool identity to the server's execution session; model arguments cannot choose the caller, project/channel (or legacy group), or grants.
- Provider credentials remain with the original CLI. Discovery must not run paid model probes or mutate authentication.
- Tests for persistence, retry, cross-project/cross-channel (and legacy cross-group) isolation, resource locking, and unknown outcomes are release gates. Record benchmarks before changing proven performance behavior.
- Product UI and errors should be Chinese; code, identifiers, and code comments should be English.
