# ADR NNNN — Title

**Status:** Proposed | Accepted | Superseded by ADR-NNNN | Deprecated
**Date:** YYYY-MM-DD
**Deciders:** roles, not names
**Consulted:** who was asked
**Affects:** components and documents this decision constrains

---

## Context

What situation forces a decision. Constraints that are given, not chosen. What breaks if no decision
is made. Keep it factual; the argument belongs under Decision Drivers.

## Decision Drivers

Ordered by weight. The first driver should be the one that would decide it alone if the others were
neutral.

| # | Driver | Weight |
|---|---|---|

## Options Considered

### Option A — name

Description, then:

| Pros | Cons |
|---|---|

Repeat per option. Every option that was seriously considered appears, including the ones rejected
quickly, with the reason they were rejected quickly.

## Decision

The chosen option, stated in one sentence, followed by the specifics that make it implementable.

## Consequences

### Positive

### Negative

### Neutral

### What this forecloses

Options this decision removes from the table, and what it would cost to get them back.

## Revisit when

The concrete signal that would reopen this decision. "When it stops working" is not a signal.

---

## Notes on using this template

- One decision per ADR. If it needs two Decision sections, it is two ADRs.
- ADRs are immutable once Accepted. A changed mind is a new ADR that supersedes this one.
- The Negative consequences section is mandatory and must not be empty. A decision with no downside
  was not a decision.
- Asynchronous review window: 48 hours minimum before an ADR moves to Accepted, so every time zone
  gets a full working day to object. See `11-delivery-plan.md` §11.4.
