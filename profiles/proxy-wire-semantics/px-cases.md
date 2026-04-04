# Proxy Wire-Semantics PX Case Set

This file defines the adjunct proxy semantics matrix that must be validated alongside the transport matrix (`T01..T88`).

Primary gate:
- `T01..T88` (transport/protocol matrix)

Required adjunct:
- `PX01..PX12` (proxy wire-semantics matrix)

Case inventory:

| Case | Focus |
|---|---|
| `PX01` | stale `STARTED` absorb succeeds when matching result arrives in absorb window |
| `PX02` | stale `STARTED` absorb expires via bounded fail path |
| `PX03` | `SYN` while waiting for command `ACK` reopens arbitration immediately |
| `PX04` | `SYN` while waiting for target response bytes reopens arbitration immediately |
| `PX05` | lower initiator wins same-boundary competition |
| `PX06` | queued higher initiator loses when lower initiator arrives before next round closes |
| `PX07` | requeue-after-timeout by former owner still wins by lower initiator priority |
| `PX08` | equal-initiator FIFO ordering is preserved |
| `PX09` | local target observes request only from echoed `RECEIVED` path, never from owner `SEND` intent |
| `PX10` | local emulated target response inside responder window remains coherent |
| `PX11` | late responder bytes are rejected and counted |
| `PX12` | non-owner/non-responder sends are rejected during active transaction |

Outcome policy:
- Allowed outcomes: `pass`, `xfail`, `blocked-infra`
- `xpass` is not allowed for `PX` inventory.
- `blocked-infra` is allowed only with `infra_reason=adapter_no_signal`.
