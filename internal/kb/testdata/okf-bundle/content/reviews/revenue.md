---
type: Attested Computation
title: Revenue for fiscal year
description: Recognized revenue for a fiscal year, per Finance's definition.
status: stable
generated: { by: reference_agent/gemini-2.5-pro, at: 2026-06-20T22:53:05Z }
verified: { by: process:finance-nightly, at: 2026-06-26T02:00:00Z }
---

# Computation

    SELECT SUM(amount) AS revenue
    FROM finance.recognized_revenue
    WHERE fiscal_year = @year

`verified` here is written as a bare mapping (no list dash) rather than
a one-element list, to exercise SPEC.md §5.2/§11's "consumers MUST
treat a bare mapping as a one-element list." The verifier is a
`process:` actor (not `human:`), so this concept's trust tier should
derive to machine-confirmed.
