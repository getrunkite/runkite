# Agent skills

Tracked skill definitions for Cursor (and similar) agents working in this repo.

| Skill | Purpose |
|-------|---------|
| [runkite-new-agent](runkite-new-agent/SKILL.md) | Scaffold a new agent on the Runkite control plane |

`.cursor/` is gitignored. To load a skill in Cursor locally, symlink or copy:

```bash
mkdir -p .cursor/skills
ln -s ../../skills/runkite-new-agent .cursor/skills/runkite-new-agent
```
