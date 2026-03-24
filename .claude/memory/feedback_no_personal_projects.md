---
name: no-personal-project-names
description: Never reference the user's actual project names, paths, or client names in code, tests, docs, or examples for open-source projects
type: feedback
---

Do not use the user's real project names, directory paths, or client names in examples, tests, documentation, or commit messages for open-source projects. Use generic placeholder names instead (e.g., ~/Projects/my-app, ~/work/my-project).

**Why:** This is an open-source project. Personal project names leak private information into public repositories.

**How to apply:** When writing examples, tests, config samples, or documentation, always use generic names. When the user shares a real path for testing, use it only in ephemeral terminal commands, never in committed code.
