---
paths: ["backend/**"]
---
- Stdlib only; a new module requires justification in the PR description.
- Wrap errors with %w and operation context ("homebox create entity: %w").
- Table-driven tests; placement scoring changes always need a test.
- JSON contracts here mirror app/src/types.ts; change both in one commit.
- Never log photo bytes, API keys, or the Homebox pepper.
