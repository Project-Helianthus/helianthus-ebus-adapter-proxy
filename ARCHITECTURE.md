# Architecture

## Goal

`helianthus-ebus-adapter-proxy` bridges downstream eBUS frames to an upstream adapter stream.

## Current package layout

- `cmd/helianthus-ebus-adapter-proxy`: executable entry point.
- `internal/domain/downstream`: downstream frame contracts and client interface.
- `internal/domain/upstream`: upstream message contracts and client interface.
- `internal/domain/routing`: routing contract between downstream and upstream domains.
- `internal/proxy`: service orchestration for routing and publication.

## Runtime flow

1. Receive a downstream frame.
2. Route the frame into an upstream message.
3. Publish the message using the upstream client.

This bootstrap keeps implementations minimal while establishing stable domain boundaries.
