# Keeping the wiki true

The wiki is a view of the code, not a second source of truth. Every claim on
it is also in the repository, and a behaviour change updates both in the same
commit.

| Page | Source of truth |
|---|---|
| [Home](Home) | `README.md` |
| [Getting started](Getting-started) | `README.md`, `internal/cli` |
| [Harness setup](Harness-setup) | `docs/harness-setup.md` |
| [Protocol](Protocol) | `docs/protocol.md` |
| [Autonomy](Autonomy) | `docs/protocol.md` |
| [Troubleshooting](Troubleshooting) | the fixes that shipped, each with its version |
| [Design notes](Design-notes) | `docs/CONTEXT.md` |

The pages live in `docs/wiki/` in the repository, so a change to them can be
reviewed in a pull request like any other. Publishing is one push to the wiki
repository; `docs/wiki/README.md` has the commands.

## Why this page exists

A wiki that drifts is worse than no wiki: it is a second answer, and the
wrong one. The rule that keeps it honest is the same one the code follows —
every claim must be true of the binary.
