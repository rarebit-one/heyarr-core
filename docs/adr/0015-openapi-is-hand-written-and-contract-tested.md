# 0015. OpenAPI is hand-written and contract-tested

**Status:** Accepted
**Date:** 2026-08-19

## Context

Spec §10 lists OpenAPI. The usual choices are to generate handlers from a spec,
generate a spec from handlers, or write both and hope.

## Decision

`api/openapi.yaml` is written by hand. Handlers are written by hand. A test
asserts that every registered route appears in the spec and every documented
path is registered.

## Consequences

Generated handlers push the shape of the API toward what the generator finds
easy, and the generated layer is exactly where the range-serving and SSE
endpoints do not fit. Generating the spec from code produces documentation that
is accurate and useless.

The parity test is the actual mechanism. Without it this decision is just "write
both and hope", which is the option that always rots.
