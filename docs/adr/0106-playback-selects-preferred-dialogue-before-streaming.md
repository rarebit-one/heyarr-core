# 0106. Playback selects preferred dialogue before streaming

**Status:** Accepted
**Date:** 2026-10-10
**Builds on:** ADR-0069

## Context

ADR-0069 streams only one audio track to avoid encoding unused tracks. Selecting
that track by position can send a dub when preferred dialogue exists later in
the file. Once discarded, the preferred track cannot be selected by the player.
A codec decision made against a different track can also wrongly copy audio
that the client cannot decode.

## Decision

Select main audio from the source metadata before planning codec compatibility.
Clients declare ordered audio language preferences; an omitted preference uses
optional node configuration. No matching main track keeps original main audio.
Commentary does not substitute for dialogue. Existing probes must be refreshed
to gain commentary disposition metadata; unknown language tags remain unknown. Direct-play clients apply their
own audio preference because the immutable blob route retains all tracks.

The stream token signs the selected audio ordinal alongside codec decisions.
Seeks reuse that token. Only one audio track is processed, preserving the cost
bound from ADR-0069. Unexpired earlier tokens retain their original selection.

Captions retain source language and SDH metadata. Extraction canonicalizes ISO
language aliases and labels SDH sidecars so clients can prefer full preferred-
language captions without selecting quarantined assets.

Revisit the one-track output when streaming clients need live track changes
without re-planning, or when users need separate per-work language preferences.
