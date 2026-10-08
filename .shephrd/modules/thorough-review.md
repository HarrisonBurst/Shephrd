# Thorough review

An optional playbook for changes needing independent scrutiny. It applies only when selected by the task request or applicable repository guidance, and can be adapted by explicit task instructions.

## Approach

1. Establish the scope, acceptance criteria, risks, and validation approach. Keep the plan proportionate to the change.
2. Implement within that scope and run the repository's required checks plus relevant behavior-level validation. For a bug, reproduce the user-visible failure before changing code.
3. Have a reviewer other than the implementer inspect the exact candidate revision and base. Make the review read-only and provide the acceptance criteria, relevant context, and check results. Review correctness, regressions, security implications, and missing tests; separate material findings from optional suggestions.
4. Address concrete findings within the approved scope. Run checks affected by the fixes and obtain a targeted follow-up review of the changes and their interactions.
5. Report the final artifact identity, checks, review coverage, and any unresolved findings. Earlier review evidence does not cover a later revision automatically.

## Stop conditions

Use one initial independent review and, when fixes are needed, one targeted follow-up by default. Report remaining material findings as blockers rather than starting an open-ended review cycle. Resolve missing reviewer access, disagreement, or out-of-scope findings with the user instead of treating them as approval or silently expanding the task.

Do not create unrelated cleanup work from optional suggestions. A user can explicitly request a different review scope or additional rounds.

## Boundaries

Review evidence is not landing, release, discard, or deployment authorization. This playbook does not require a PR or a particular model family and does not change task states or worker artifact contracts.

When using Shephrd workers, arrange feedback while an implementation worker is waiting if same-attempt continuation is wanted. A terminal task cannot receive more work through `worker send`; further implementation needs an explicitly authorized lifecycle path, not an automatic retry. Independent review remains ordinary requested work, not a new approval state in Shephrd.
