# ADR 0011 — Two-party approval for production promotion

**Status:** Accepted
**Date:** 2026-07-08
**Deciders:** Platform lead, client risk function, client change management
**Consulted:** Consuming teams, client audit
**Affects:** `controlplane` promotion gate, `05-identity.md` §8, audit retention, `10-network-security.md` §7

---

## Context

An agent's behaviour is defined by its prompts, its tool set and its model configuration. These change
between versions in ways that are not visible in a code review of the calling application, and that
can materially change what the agent does with the client's data and money.

`SPEC.md` §1.4 requires human approval for production promotion, as a two-party rule: one owning-team
approver and one platform approver, neither of whom may be the requester.

This ADR records why that specific shape, what it costs, and what it deliberately does not achieve.
The client's audit function will ask all three questions.

## Decision Drivers

| # | Driver | Weight |
|---|---|---|
| 1 | Segregation of duties: no individual may unilaterally place code in production | Decisive |
| 2 | An auditor must be able to reconstruct why a version was allowed into production | Decisive |
| 3 | The friction must be proportionate; a gate everyone routes around is worse than no gate | Very high |
| 4 | The approval must not be the thing that prevents an incident being fixed | Very high |
| 5 | The two approvers bring different knowledge, not the same knowledge twice | High |

## Options Considered

### Option A — Automated gates only, no human approval

| Pros | Cons |
|---|---|
| Zero friction; fully self-service | Automated gates cannot assess whether the agent's *behaviour change* is appropriate |
| Fast | No segregation of duties; fails a standard control expectation |
| | Nothing catches "this version now sends customer data to a new tool" |

Rejected on driver 1.

### Option B — Single approver

| Pros | Cons |
|---|---|
| Meaningfully less friction than two | One person can be wrong, rushed, or under pressure |
| Some segregation of duties, if the approver is not the requester | Does not satisfy a strict segregation-of-duties expectation |
| | Only one perspective; typically either domain or platform, not both |

Rejected on drivers 1 and 5.

### Option C — Two-party approval: one owning-team, one platform, requester excluded

| Pros | Cons |
|---|---|
| Two perspectives: domain risk and platform risk | Real friction; two people must act |
| Satisfies segregation of duties structurally | Approver availability becomes a delivery dependency |
| The approvers bring genuinely different knowledge | Approvals can become rubber-stamping if the evidence is poor |

### Option D — Change Advisory Board review per promotion

| Pros | Cons |
|---|---|
| Maximum scrutiny | Weekly cadence versus a daily deployment rhythm |
| Fits the client's existing process | Reviewers lack context on a specific agent's behaviour |
| | Would make agent deployment effectively monthly, which drives teams to avoid promotion entirely |

Rejected on driver 3.

### Option E — Two approvers, either role

| Pros | Cons |
|---|---|
| Simpler role model; easier to find two people | Two platform engineers reviewing an agent's domain behaviour add little |
| | Two team members reviewing their own team's agent is weak segregation |

Rejected on driver 5. The value is in the *difference* between the two approvers, not the count.

## Decision

**Two-party approval for production promotion: one owning-team approver and one platform approver,
both distinct from the requester and from each other.**

| Rule | Enforcement |
|---|---|
| One owning-team approver | Must hold the owning-team approver role for that agent's team |
| One platform approver | Must hold the platform approver role |
| Neither is the requester | Structural check; a self-approval attempt is rejected **and recorded** |
| The two are distinct | A person holding both roles cannot satisfy both |
| Approval is against a specific gate snapshot | Approving `snap_01J...` approves that evaluation, not the version in general |
| Approval window | 72 hours, then the request expires and gates are re-evaluated on re-request |
| Gates are never reused | A snapshot is evidence of a moment, not a reusable pass |

### What each approver is asked

They are asked different questions, and the approval UI asks them differently.

| Approver | Question |
|---|---|
| Owning team | Is this behaviour change appropriate for this agent's purpose and its data? Do you accept the projected cost? Are you on call for it? |
| Platform | Do the gate results support promotion? Is the quota within envelope? Does the pool entitlement match the data classification? Is anything about this version's telemetry or error profile concerning? |

### Evidence retained

| Artefact | Contents | Retention |
|---|---|---|
| Gate snapshot | Every gate result **with its input values**, evaluation timestamp, `evaluator_version` | 7 years |
| Approval records | Two records: actor, role, timestamp, `snapshot_id` | 7 years |
| Change reference | ServiceNow CHG or RITM, integrated or via the manual fallback | 7 years |
| Version transition | Previous and new production version, effective timestamp | 7 years |
| Rejected attempts | Self-approval attempts, expired requests, failed evaluations | 7 years |

`evaluator_version` is on the snapshot because gate logic changes over time; without it a historical
snapshot cannot be interpreted against the rules that were in force.

### Emergency revert

**Reverting production to a previously-active version may be approved by a single platform approver,
recorded as an emergency action and reviewed within 24 hours.**

The two-party rule protects against introducing an unreviewed version. Reverting to a version that
already passed the full gate and already ran in production introduces nothing new. The risk of a slow
revert during an incident exceeds the risk of a single-approver revert to a known-good state.
Promoting a *new* version always requires two parties, including during an incident.

## Consequences

### Positive

- Segregation of duties is enforced structurally, not by convention, and the enforcement is
  demonstrable to an auditor.
- The evidence set answers the auditor's actual question — "why was this allowed into production on
  this date, and who allowed it" — without depending on systems that may since have been
  decommissioned.
- Two different perspectives are applied, which catches classes of problem neither would catch alone.
- The emergency revert path means the control does not become the thing that prolongs an incident.

### Negative

- **Real friction.** Every production version bump needs two people to act within 72 hours. For a team
  shipping weekly this is a weekly coordination cost.
- Approver availability becomes a delivery dependency, particularly across time zones and during
  holidays. Mitigated by having several approvers per role.
- Approvals can degenerate into rubber-stamping. The mitigation is the quality of the evidence
  presented — the gate snapshot with its input values — not additional process.
- The rule cannot prevent collusion between two approvers. This is addressed by audit, not by the
  system, and is stated plainly in `10-network-security.md` §7.1.
- Structural enforcement requires a role model that must be kept current as people change teams. Stale
  roles are the most likely operational failure of this control.

### Neutral

- Staging promotion requires no human approval, only the automated gates. The friction is placed where
  the risk is.

### What this forecloses

Fully automated continuous deployment to production for agents. Recovering it would mean the client
accepting automated gates alone as sufficient segregation of duties, which is not a technical
decision.

## Revisit when

The measured approval latency — request to second approval — consistently exceeds 24 hours, indicating
the control has become a delivery bottleneck rather than a review. The response would be more
approvers or better evidence presentation, not fewer approvers.
