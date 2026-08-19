# 0016. Licensing: AGPL-3.0-or-later, DCO, no CLA

**Status:** Accepted
**Date:** 2026-08-19

## Context

Heyarr is a self-hostable server. The licence choice determines whether a hosted
proprietary fork is possible.

## Decision

AGPL-3.0-or-later for the server. Contributions under Developer Certificate of
Origin sign-off. No CLA and no copyright assignment.

## Consequences

Network copyleft is the point: someone running a modified Heyarr as a service
must share those modifications. That matches a project whose whole thesis is
sovereign, self-hosted infrastructure.

No CLA means the project cannot unilaterally relicense later — deliberately.
Contributors keep their copyright, and the licence cannot be changed out from
under them.

If the Storage Fabric is extracted as a library (§18, ADR-0007), it may be
released under Apache-2.0 instead, so it can be reused by projects that are not
themselves AGPL. That is a separate decision for a separate module.
