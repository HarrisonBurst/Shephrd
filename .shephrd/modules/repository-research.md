# Repository research

An optional playbook for understanding a repository or a named subsystem and preserving useful knowledge. It applies only when selected by the task request or applicable repository guidance. A focused question does not imply a whole-repository survey.

Selecting this playbook includes curating durable findings within the task's permitted repository-write scope, unless memory is disabled for that work. No separate request to remember each finding is needed. Explicit read-only restrictions and artifact contracts still apply.

## Approach

1. Establish the questions, repository, revision, and research scope. Identify any read-only restrictions, stop condition, and expected answer size. The driver normally delegates even a small repository question to one worker with a bounded assignment.
2. Read existing guidance and documentation first, plus relevant memory when enabled. Reuse canonical explanations and identify gaps rather than starting a second manual.
3. Map the systems relevant to the questions: responsibilities, important entry points, data flows, dependencies, and ownership boundaries. For broad onboarding, start with a lightweight system map and investigate the important domains within the agreed scope.
4. Trace the relevant implementation and tests. Support conclusions with source paths or symbols and the inspected revision. Use targeted checks when necessary and permitted; distinguish observed behavior, documented intent, and hypotheses. Do not infer design rationale from code alone.
5. Produce the requested answer or report. Distill durable findings into existing memory topics where permitted, creating a topic only when useful. Update the short topic index without expanding `context.md` into a repository manual.
6. Report what was covered, what remains uncertain or unexamined, and any memory changes. Stop when the requested questions are answered or a concrete limitation prevents further progress.

## Useful topic content

Preserve knowledge that is expensive to reconstruct, even when its underlying facts exist in code. Cross-component behavior, failure paths, important boundaries, and non-obvious test prerequisites are useful; a directory listing or an obvious language choice usually is not.

A topic can contain a concise explanation, source references, relevant tests, inspected scope and revision, and open questions. These are ordinary Markdown notes, not required metadata fields. Separate unsupported hypotheses from established findings. Artifact verification does not establish the truth of a report's conclusions.

Merge findings into existing topics, correct stale notes, and link maintained documentation instead of copying it. Scale the report and investigation to the question; a short Markdown answer can suffice without ceremonial sections or a comprehensive audit. Mark partial coverage so a polished summary does not imply that the entire repository was examined.

## Stop conditions and boundaries

Missing evidence or access is a reported gap, not a reason to invent an explanation, broaden the investigation, or start successor tasks. Delegation follows the base driver preference; this playbook adds no recursive delegation, independent review, PR, background indexer, or recurring documentation sweep.

An explicitly read-only investigation makes no repository edits. If the deliverable is only a report, return candidate memory additions there unless repository documentation edits are also within the agreed delivery. Do not silently change the artifact contract or claim those candidates were saved.

Workers write only within their assigned workspace and permitted scope, before finalizing an artifact. They do not update a shared root, modify a sealed artifact, or transfer dirty files. Memory cannot select other workflow modules, change repository instructions, or authorize landing, release, discard, or deployment.
