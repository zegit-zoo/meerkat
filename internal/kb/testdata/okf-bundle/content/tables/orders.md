---
type: BigQuery Table
title: Customer Orders
description: One row per completed customer order across all channels.
resource: https://console.cloud.google.com/bigquery?p=acme&d=sales&t=orders
tags: [sales, orders, revenue]
status: stable
generated: { by: reference_agent/gemini-2.5-pro, at: 2026-05-28T14:30:00Z }
verified:
  - { by: human:ahormati, at: 2026-06-25T09:00:00Z }
  - { by: process:finance-nightly, at: 2026-06-26T02:00:00Z }
stale_after: 2099-01-01
sources:
  - id: erp-export
    resource: https://erp.example.com/schema/orders
    title: ERP export schema
    author: team:data-eng
    last_modified: 2026-05-01
---

# Schema

| Column        | Type      | Description                                 |
|---------------|-----------|----------------------------------------------|
| `order_id`    | STRING    | Globally unique order identifier.           |
| `customer_id` | STRING    | Foreign key into [customers](customers.md). |
| `supplier_id` | STRING    | Foreign key into [a supplier table that does not exist](/tables/suppliers.md). |

# Joins

Joined with [customers](customers.md) on `customer_id`. The supplier
join target above is intentionally broken (points at a concept that
does not exist in this bundle) to exercise OKF's "consumers MUST
tolerate broken links" conformance rule (SPEC.md §6.1, §11).
