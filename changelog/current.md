# Changelog (Unreleased)

Record image-affecting changes to `manager/`, `worker/`, `copaw/`, `hermes/`, `openclaw-base/`, and `agentteams-controller/` here before the next release.

---

- fix(manager): make attached Worker Skill installation safe, deterministic, and independent of `assign_when` for explicit assignments ([04b6b9b](https://github.com/agentscope-ai/AgentTeams/commit/04b6b9bc10879cd5d9bea5df815a69828f67ec9f))
- fix(manager): restore canonical and distributed Worker Skill files after failed replacement and remove stale files ([71b6833](https://github.com/agentscope-ai/AgentTeams/commit/71b6833350a12f45cba88300c4dc104aebba61fc), [1ec5995](https://github.com/agentscope-ai/AgentTeams/commit/1ec5995892712491af704e1951e1164da2f9ef9f))
