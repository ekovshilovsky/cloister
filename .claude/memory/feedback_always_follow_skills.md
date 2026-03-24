---
name: always-follow-skill-workflow
description: Always follow the full skill workflow (brainstorm → spec → plan → subagent execution) even for scope-creep features that seem simple
type: feedback
---

Always follow the superpowers skill workflow for any non-trivial code change, even when it's "just one more thing" or "scope creep that seems small." The user values the process (brainstorming → spec → reviewed plan → subagent-driven execution with review gates) and expects it to be followed consistently.

**Why:** The user caught me skipping the workflow for the configurable workspace mount feature. Even though the code worked and tests passed, the process exists to catch design issues early and ensure quality through review gates.

**How to apply:** When a new feature surfaces during implementation (scope creep), pause and invoke the brainstorming skill before writing code. Don't rationalize skipping the process because "it's small" or "we're already in the flow."
