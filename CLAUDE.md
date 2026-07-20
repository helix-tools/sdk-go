# CLAUDE.md

Guidance for AI coding agents working in this repository.

## Customer-visible content policy (MANDATORY — Thales, 2026-07-20)

Applies to ALL customer-visible content produced from this repo (READMEs, docs pages, UI copy, code samples):

1. Hardcoded values ONLY inside the get-started samples for uploading/downloading datasets; everything else points by reference.
2. NO hardcoded Helix prices — link to https://helix.tools/#pricing. (A customer's OWN example price as an API parameter in a sample is acceptable.)
3. NO internals: no encryption algorithms/mechanics (say "encrypted in transit and at rest", never name algorithms or key-management services), no AWS resource names/patterns (queue names/URLs, bucket names, ARNs, account IDs, SSM paths), no infrastructure topology. SQS may be named as the consumer notification mechanism; queue names/patterns may not.
4. NO confidential how-it-works: describe what the customer does and gets, never how the platform implements it.

A violation is a review-blocking defect. When in doubt, use capability language and link out.

## Engineering rules (MANDATORY)

- Never weaken a CI check, test, or coverage gate to force green — fix the code. A gamed green is worse than an honest red.
- Work in isolated git worktrees; stage files by explicit path (never `git add -A`).
- No AI attribution in commits or PRs.
- Local-first: everything green locally before push; CI confirms, never discovers.
- Every changed line is tested (happy, bad, edge); negative-control new tests.
