# AgentTeams Use Cases

English | [中文](../zh-cn/usage/use-cases.md)

This guide provides reusable examples for giving a goal to the Manager, having multiple Workers or a Team complete it collaboratively, and retaining human oversight and acceptance throughout the process. Complete the [Quickstart](../quickstart.md) first and confirm that you can talk to the Manager and create a Worker.

Models, Skills, MCP Servers, and access to external systems in these examples are not granted automatically. Workers can use source repositories, search, monitoring, ticketing, and other external services only after the corresponding capabilities have been configured and authorized in the instance.

## 1. Choose the right collaboration pattern

| Task characteristics | Recommended pattern | When to use it |
|---|---|---|
| One goal, one specialty, short execution time | One standalone Worker | Lowest setup cost; both the Human and Manager can observe and intervene directly |
| Multiple independent subtasks that can run in parallel | Multiple standalone Workers | The Manager delegates separately and consolidates the results; suitable for temporary collaboration |
| Stable members and responsibilities that repeatedly handle similar projects | Team | The Team Leader decomposes, delegates, and consolidates within the Team; the Manager collaborates only with the Leader |
| Publishing, deletion, payment, external communication, or production changes | Worker or Team with human approval | Define approval gates in the task and prohibit irreversible operations without confirmation |

Do not create multiple Workers for a simple task merely to make it “multi-agent.” Decomposition is valuable when subtasks can make independent progress, require different tool permissions, or gain a clear benefit from parallel execution.

Standalone Workers and Teams can coexist in one instance. The Manager can directly coordinate temporary specialists as standalone Workers, while a Team Leader coordinates a stable team internally. See [Declarative Resource Management](resource-management.md) for resource fields and creation methods.

## 2. General execution method

An acceptance-oriented AgentTeams task normally follows these steps:

1. **Define the outcome**: state what must be delivered instead of asking only to “research” or “handle” something.
2. **Separate responsibilities**: give every Worker a clear, non-overlapping scope.
3. **Constrain permissions**: list the allowed repositories, directories, MCP Servers, and external systems; add human approval gates for sensitive operations.
4. **Define collaboration artifacts**: require plans, progress records, and final artifacts in the shared task directory so that critical information does not exist only in chat context.
5. **Set acceptance criteria**: specify tests, citations, formats, risk notes, and completion conditions.
6. **Intervene during execution**: the Human reviews progress in Matrix, adds requirements, or stops an incorrect direction.
7. **Consolidate the result**: the Manager checks every subtask and reports the results, evidence, known limitations, and open decisions to the Human.

Follow these collaboration rules during execution:

- **The Manager orchestrates only**: the Manager decomposes, registers, and delegates tasks, tracks their state, and consolidates results. It should not use file or command tools to implement work already assigned to a Worker.
- **Verify the real assignment state**: after the Manager says that a task is assigned, verify `assigned_to`, `status`, the acknowledgement time, and the result path in the task record. A sent message, synchronized files, or task text appearing in a room does not mean that the Worker acknowledged and started the task. If no acknowledgement or artifact appears within the agreed time, mark the task blocked, record the reason, and reassign it to an available Worker.
- **Use a separate task for each responsibility**: when multiple Workers must confirm the same artifact, create separate tasks and result paths for the primary author and each reviewer. Do not let multiple Workers share one task state or `result.md`; one submission could otherwise hide the fact that another Worker did not execute.
- **Use one canonical shared directory**: define one exact shared directory for each task and include its full path in assignment and handoff messages. A Worker synchronizes its artifacts before the next Worker pulls from the same path, preventing a review of an outdated duplicate.
- **Use real Matrix mentions**: when the next role must be triggered automatically, actually @mention the recipient in the correct room. Writing `@name` only in the message body may not include Matrix mention metadata and is not proof that automatic handoff occurred.

Run a new use case at a small scope first. After confirming that the roles, tool permissions, and acceptance method work, expand the task or organize the members into a Team.

## 3. Use case 1: Software feature delivery

### Goal

Start from a requirement and complete design, implementation, testing, and code review. This works well when multiple technical roles need to collaborate in parallel and a Human must be able to inspect the code or change requirements at any time.

### Recommended roles

| Role | Primary responsibilities |
|---|---|
| Backend Worker | APIs, data models, server implementation, and unit tests |
| Frontend Worker | Pages, interactions, API integration, and frontend tests |
| Test Worker | Acceptance cases, integration tests, regression checks, and defect reports |
| Review Worker | Design consistency, security risks, compatibility, and missing cases |

Use multiple standalone Workers for a one-time feature. For ongoing maintenance of the same product, organize these members into an engineering Team and let the Team Leader manage decomposition and consolidation.

### Example request to the Manager

```text
Implement account login for the sample application.

Deliverables:
1. A backend login API, one minimal protected endpoint that requires the login token, input validation, and unit tests;
2. A frontend login page with error handling and API integration;
3. Integration tests for successful login, incorrect password, missing fields, and access to the protected endpoint without a token;
4. A change summary with run instructions, test results, known limitations, and security risks.

Ask one Worker to draft the API contract, then create separate confirmation tasks for the frontend and test Workers. After the contract is confirmed, run backend and frontend implementation in parallel while the Test Worker prepares acceptance cases, followed by an independent review.
All changes must remain in the repository copy under the shared task directory. Do not publish, merge, or modify production.
Ask me before proceeding if the API contract conflicts. Completion requires passing relevant tests and reporting the file list and test evidence.
```

### Suggested workflow

1. The Manager asks one Worker to draft the API contract in the shared directory, then creates separate confirmation tasks for the frontend and test Workers. The contract must define both a successful login response and at least one protected endpoint that requires the token.
2. Backend and frontend implementation proceed in parallel; the Test Worker prepares acceptance cases from the contract.
3. The Human reviews implementation progress in Matrix and adds boundary conditions when needed.
4. After the code is ready, the Test Worker runs integration checks and records failed cases with reproduction steps.
5. The Review Worker performs a read-only review and does not overwrite another Worker's implementation.
6. The Manager consolidates test results, review comments, and remaining issues.

### Acceptance checklist

- [ ] The implementation matches the requested scope without unauthorized extra features.
- [ ] The API contract, implementation, and tests agree.
- [ ] Important success and failure paths have automated coverage.
- [ ] Test commands, results, and failures are reproducible.
- [ ] No real secrets, tokens, or user data were committed.
- [ ] Publishing, merging, and production changes still require explicit Human approval.

### Required capabilities

If the task needs a remote repository, configure the GitHub, GitLab, or corresponding code-platform MCP Server/Skill first, and grant only the required repository and operation permissions. Without remote write access, Workers can still prepare code and patches in the shared directory for the Human to review and submit.

## 4. Use case 2: Research and analysis report

### Goal

Collect sources, analyze data, and verify facts in parallel, then produce a report that includes sources and explains uncertainty.

### Recommended roles

| Role | Primary responsibilities |
|---|---|
| Research Worker | Collect primary sources and record titles, links, dates, and main conclusions |
| Data Analysis Worker | Clean and analyze supplied data; document definitions, assumptions, and calculations |
| Fact-check Worker | Check source quality, time ranges, conflicting information, and unsupported conclusions |
| Report Editor Worker | Unify structure, terminology, and style without adding unverified facts |

### Example request to the Manager

```text
Research the most common barriers enterprises encountered when adopting multi-agent systems during the last 12 months, and produce a report for technical leaders.

Requirements:
- Prefer official documentation, research papers, and public first-party case studies;
- Attach a source and publication date to every key conclusion;
- Distinguish sourced facts, analytical inferences, and recommendations;
- Preserve conflicting sources and explain their differences;
- Do not invent content from inaccessible paid reports.

Use Research, Fact-check, and Report Editor Workers. Submit the research plan and source scope for my confirmation before writing the complete report.
Deliver a Markdown report, source list, key-conclusion summary, and unresolved questions.
```

### Suggested workflow

1. The Research Worker proposes keywords, the time range, and source priorities.
2. The Human confirms the scope before large-scale research begins on the wrong question.
3. Multiple Workers can collect sources by source type or subtopic, using one shared source-record format.
4. The Fact-check Worker maps every key conclusion to its supporting evidence.
5. The Report Editor writes only from confirmed material and explicitly marks content with insufficient evidence.
6. The Manager consolidates the report and unresolved conflicts without presenting inference as fact.

### Acceptance checklist

- [ ] The report states the research question, time range, and method.
- [ ] Key facts can be traced to accessible sources.
- [ ] Source dates are appropriate for the time sensitivity of each conclusion.
- [ ] Inferences, recommendations, and sourced facts are clearly separated.
- [ ] Data calculations can be reproduced from the original data and documented steps.
- [ ] Uncertainty, missing data, and conflicting information are not hidden.

### Required capabilities

Workers can retrieve material directly only when search, browser, database, or enterprise knowledge-base tools have been configured. Without external retrieval, the Human should place the source material in the shared directory and require Workers to analyze only that material.

## 5. Use case 3: Content production and localization

### Goal

Use one set of facts and brand requirements to complete planning, drafting, review, and multilingual versions without allowing separate Workers to introduce inconsistent facts or terminology.

### Recommended roles

| Role | Primary responsibilities |
|---|---|
| Content Planning Worker | Audience, structure, information hierarchy, and channel requirements |
| Writing Worker | Draft from the approved outline and fact material |
| Review Worker | Verify facts, terminology, links, style, and compliance requirements |
| Localization Worker | Adapt meaning and expression for the target language and region |

### Example request to the Manager

```text
Using release-notes.md and product-facts.md in the shared directory, produce a product release article and an English version.

Audience: developers with basic technical knowledge.
The Chinese article should be 1,200–1,600 Chinese characters. The English version does not need to be literal, but facts, versions, commands, and links must match.
Use only the supplied fact files. List missing information as open questions instead of filling it in.

Use Content Planning, Chinese Writing, Fact Review, and English Localization Workers.
Ask me to confirm the outline before drafting. Deliver Chinese and English Markdown, a fact-check table, and open questions.
```

### Suggested workflow

1. The Planning Worker builds a fact table and terminology table from the source material.
2. The Human confirms the audience, outline, tone, and prohibited claims.
3. The Writing Worker uses only information in the fact table.
4. The Review Worker checks versions, commands, links, and capability boundaries item by item.
5. The Localization Worker creates the target-language version from the same facts and terminology.
6. The Manager compares headings, sections, code blocks, links, and key numbers across both languages.

### Acceptance checklist

- [ ] Both languages cover the same key facts and limitations.
- [ ] Versions, commands, links, and product names match exactly.
- [ ] Marketing language does not expand capabilities beyond available evidence.
- [ ] The target-language text reads naturally without changing the original meaning.
- [ ] Open items remain clearly marked instead of being filled automatically.

## 6. Use case 4: Incident analysis and remediation proposal

### Goal

Have separate Workers analyze logs, configuration, and recent changes, then produce testable root-cause hypotheses and remediation proposals. This use case is read-only by default and must not automatically modify production.

### Recommended roles

| Role | Primary responsibilities |
|---|---|
| Log Analysis Worker | Timeline, error patterns, and affected scope |
| Configuration Analysis Worker | Compare configuration with a known-good state and inspect dependencies |
| Change Analysis Worker | Review recent releases, commits, and infrastructure changes |
| Verification Worker | Design reproduction, rollback, and post-fix verification steps |

### Example request to the Manager

```text
Analyze the logs, configuration snapshot, and change records under incident-2026-08-06/ in the shared directory, and identify possible causes of intermittent 502 responses.

Constraints:
- Read only the supplied material and query read-only monitoring endpoints;
- Do not restart services, modify configuration, scale, roll back, or run production commands;
- For every root-cause hypothesis, list supporting evidence, counter-evidence, confidence, and a verification method;
- If evidence is insufficient, list the additional data required.

Use Log, Configuration, and Change Analysis Workers in parallel, then have a Verification Worker organize the lowest-risk validation plan.
Deliver an incident timeline, root-cause candidates, recommended response order, rollback conditions, and operations awaiting approval.
```

### Suggested workflow

1. The Manager confirms the time range, system boundary, and permitted data sources.
2. Workers form hypotheses independently to reduce early anchoring on one explanation.
3. The Verification Worker compares hypotheses and prioritizes read-only, reversible checks.
4. The Human decides whether to authorize any restart, rollback, or production modification.
5. After approval, Workers verify only within the approved scope and do not expand the change.
6. The Manager consolidates the final conclusion, evidence, and preventive follow-up work.

### Acceptance checklist

- [ ] Logs or monitoring evidence supports the timeline and affected scope.
- [ ] Root-cause conclusions are distinguished from correlation observations.
- [ ] Every proposal describes risk, rollback, and verification.
- [ ] No production write operation occurred without Human approval.
- [ ] The report contains no unredacted tokens, passwords, or user data.

## 7. Use case 5: Long-running project collaboration

### Goal

Use a stable set of roles across a multi-stage project and keep task state and artifacts in shared storage so that collaboration does not depend on a single model session.

### When to use a Team

Prefer a Team when these conditions occur together:

- The same roles collaborate repeatedly instead of completing only one task.
- The project contains multiple stages and dependent subtasks.
- A Team Leader should manage internal decomposition while the Manager focuses on project-level results.
- Shared task directories, progress records, and acceptance states are needed to restore context.

### Example request to the Manager

```text
Create a long-running engineering Team for the developer portal redesign, with a Team Leader and frontend, backend, and test members.

The project has five stages: requirement clarification, technical design, implementation, integration testing, and release preparation.
Before each stage, submit a plan. At the end of each stage, submit artifacts, test evidence, risks, and dependencies for the next stage.
Any production release, data migration, or external notification requires my explicit approval.

First return the Team structure, stage plan, shared-directory convention, and acceptance gates. Do not start implementation yet.
```

### Suggested workflow

Within a Team Worker, the relative `shared/` path resolves to that Team's dedicated shared space rather than the global shared directory used by standalone Workers. The Manager should receive stage results through the Team Leader. When directly inspecting object storage, use the Team-specific storage prefix; absence from the global shared directory does not mean that a Team member failed to submit the artifact.

1. The Manager creates or selects a Team and delegates the project goal to the Team Leader.
2. The Team Leader establishes stages and task dependencies, advancing work only after prerequisites are complete.
3. Workers save plans, progress, and results in the shared task directory and @mention the Leader when a decision is required.
4. The Leader consolidates stage results and notifies the Manager on completion, blockers, or approval needs.
5. The Manager reports project-level status to the Human without bypassing the Leader to direct Team Workers.
6. The Human approves, adjusts, or stops the project at stage acceptance gates.

### Acceptance checklist

- [ ] Team member responsibilities and communication paths are clear.
- [ ] Stages, task dependencies, and completion criteria have persistent records.
- [ ] Progress can be restored from shared artifacts after a session reset or Worker replacement.
- [ ] Blockers, approvals, and scope changes are escalated to the Human promptly.
- [ ] The Manager, Leader, and Workers do not delegate the same task more than once.

See [Declarative Resource Management](resource-management.md) for Teams, Team Leaders, Human permissions, and task flow.

## 8. Use case 6: Add and use a custom Skill

### Goal

Add a `bilingual-doc-review` Skill that does not depend on an external service to an existing Worker, then have that Worker use it to inspect a pair of Chinese and English documents. This case verifies three separate outcomes: the Skill files were distributed, the Worker runtime discovered the Skill, and the Worker actually followed the Skill while completing the task.

Before starting, prepare an existing idle Worker and refer to it as `doc-reviewer` throughout this example. If its actual name differs, replace the name consistently. See the [Worker Guide](worker-guide.md) for Worker creation methods.

### 1. Prepare the Skill package

Create this directory:

```text
bilingual-doc-review/
├── SKILL.md
└── references/
    └── checklist.md
```

Use the following content for `SKILL.md`:

```markdown
---
name: bilingual-doc-review
description: Compare paired Chinese and English Markdown documents for structural and factual consistency.
---

# Bilingual documentation review

Use this Skill when you need to compare paired Chinese and English Markdown documents.

1. Read `references/checklist.md` before reviewing any files.
2. Report the ruleset ID from that file at the beginning of your result.
3. Compare heading order, code blocks, links, commands, configuration names, numbers, defaults, and limitations.
4. Distinguish factual mismatches from acceptable translation differences.
5. Do not edit source files unless the task explicitly requests changes.
6. Save a Markdown report containing a summary and a table with severity, location, Chinese value, English value, and recommended resolution.
```

Use the following content for `references/checklist.md`:

```markdown
# Review checklist

Ruleset ID: agentteams-bilingual-doc/v1

- Heading order and section coverage match.
- Code blocks, commands, paths, links, and configuration names match.
- Versions, ports, timeouts, limits, defaults, and other numeric facts match.
- Warnings, prerequisites, unsupported cases, and fallback behavior exist in both languages.
- Stylistic differences are not reported as factual mismatches.
```

From the directory that contains `bilingual-doc-review/`, run:

```bash
zip -r bilingual-doc-review.zip bilingual-doc-review/
```

### 2. Distribute the Skill

Choose either method below. See [Worker Guide: Installing Skills on a Worker](worker-guide.md#installing-skills-on-a-worker) for complete constraints and the state-management differences between the methods.

**Method A: Distribute through the Manager**

Attach `bilingual-doc-review.zip` in a message to the Manager, then send:

```text
Install the bilingual-doc-review Skill from the attachment on Worker doc-reviewer.
Extract and validate it safely, distribute the complete Skill, and confirm that the Worker's Skill assignment was updated.
```

**Method B: Distribute through the Dashboard**

Open **资源中心 (Resource Center) → 市场 (Marketplace)**. If `bilingual-doc-review` is not yet in the marketplace, click **上传技能 (Upload Skill)** and upload `bilingual-doc-review.zip`. Then click **分发到 Worker (Distribute to Worker)** in that Skill's row, select `doc-reviewer`, and click **分发到 1 个 Worker (Distribute to 1 Worker)**. Alternatively, upload the ZIP directly through **Workers → doc-reviewer → 详情 (Details) → 上传技能包 (Upload Skill Package)**. Distribute only while the Worker is idle because the reload may restart it.

After distribution, confirm `bilingual-doc-review` under **已分发技能 (Distributed Skills)** in Worker details and in `Worker.spec.skills`. Both Manager and Dashboard distribution maintain the declarative assignment.

### 3. Prepare the review inputs

Create `sample.zh-CN.md` in the shared task directory:

```markdown
# 示例服务安装

## 环境要求

服务监听端口为 `8080`。

## 启动服务

运行 `demo-server --port 8080`。启动失败时，服务将在 30 秒后重试。
```

Then create `sample.en.md` with two factual differences:

```markdown
# Sample service installation

## Requirements

The service listens on port `8081`.

## Start the service

Run `demo-server --port 8080`.
```

### 4. Have the Worker use the Skill

Ask the Manager to delegate the task to `doc-reviewer`:

```text
Ask Worker doc-reviewer to use the bilingual-doc-review Skill to compare
sample.zh-CN.md and sample.en.md in the shared task directory.

Requirements:
1. First confirm that the runtime can discover the Skill;
2. Follow the Skill to read any required reference files and put the ruleset ID at the beginning of the report;
3. Do not modify the source files;
4. Save the result as bilingual-review-report.md;
5. Reply with the report path, number of differences, and a short conclusion.
```

Delegate this step through the Manager and confirm that it actually @mentions the Worker in the correct room. Plain text entered directly in a Worker room may not carry Matrix mention metadata, so it can be added only to room history without triggering runtime execution.

Do not include the ruleset ID or expected differences in the task request. Otherwise, the Worker could repeat the prompt without reading the Skill.

### 5. Accept the result

The report should meet these expectations:

- It starts with ruleset ID `agentteams-bilingual-doc/v1`, demonstrating that the Worker read the Skill reference file.
- It identifies the `8080` versus `8081` mismatch in the port description.
- It identifies that the Chinese document specifies a retry after 30 seconds while the English document omits that behavior.
- It does not report natural heading translations as factual mismatches.
- It leaves both input files unchanged and creates `bilingual-review-report.md`.

If the Skill appears in the Dashboard but the Worker cannot report the ruleset ID, first confirm that the Worker reload completed or wait for periodic synchronization, then retry the task. If it still fails, use [Worker Guide: Troubleshooting](worker-guide.md#troubleshooting) to check the ZIP structure, `SKILL.md` metadata, and runtime logs.

## 9. Reusable task template

Use this template as a starting point when giving a complex goal to the Manager:

```text
Goal:
<the problem to solve>

Deliverables:
1. <artifact one>
2. <artifact two>

Roles and responsibilities:
- <Worker/Team role>: <responsibility>

Inputs and allowed access:
- <shared directory, repository, data source, MCP Server>

Constraints:
- Prohibited: <publishing, deletion, payment, production changes, and so on>
- Approval required: <operations requiring Human confirmation>

Collaboration requirements:
- Save the plan, progress, and results under <shared directory>
- Stop and ask me when <condition> occurs

Acceptance criteria:
- <testing, citation, format, performance, or security requirements>

First return the task decomposition, role assignment, dependencies, and open questions. Wait for my confirmation before executing.
```

The constraints and acceptance criteria in this template matter more than the number of roles. Without explicit completion conditions, the Manager cannot reliably decide when to continue, request revision, or finish.

## 10. Usage boundaries

These situations are usually not suitable for direct autonomous execution by multiple Agents:

- The requirement is still ambiguous, and different interpretations would produce fundamentally different outcomes.
- The operation is irreversible, but there is no approval, backup, or rollback mechanism.
- The data contains sensitive information that cannot be provided to the model or Workers.
- The task depends on external systems, credentials, or specialized tools that have not been configured.
- The workload is small enough that one Agent or an ordinary script is sufficient.

AgentTeams provides collaboration, isolation, visibility, and human-intervention mechanisms. It does not replace business authorization, data governance, professional review, or production change processes.

## 11. Next steps

- Complete the smallest Human → Manager → Worker workflow in the [Quickstart](../quickstart.md).
- Read the [Manager Guide](manager-guide.md) and [Worker Guide](worker-guide.md) for operation and maintenance.
- Use [Declarative Resource Management](resource-management.md) to create reusable Worker, Team, and Human resources.
- See [Local Deployment](deployment/local.md) for a local instance and [Kubernetes Deployment](deployment/kubernetes.md) for a shared instance.
