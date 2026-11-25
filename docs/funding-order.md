# Funding order

`routing.funding-order` ranks what pays for a request, most preferred first.

```yaml
routing:
  strategy: soonest-reset-first
  session-affinity: true
  session-affinity-rebind-on: quota
  funding-order: ["allowance", "api-credit", "usage-credit"]
```

| Class | Meaning |
| --- | --- |
| `allowance` | A subscription sign-in with plan allowance left. |
| `api-credit` | An API key, charged to that organization's credit. |
| `usage-credit` | A subscription sign-in that is past its plan allowance and still answers by drawing paid credits. |

The order applies ahead of credential `priority`. Priority then orders
credentials inside the chosen class, and the routing strategy picks among
those. Model support, cooldowns, caller eligibility and retry exclusions apply
as usual. Classes left out of the list rank last, in the order shown above. An
empty list disables ranking. An unknown or repeated class name disables it too
and logs an error.

## How a subscription becomes usage-credit

A Codex account at its plan limit does not fail. It answers from its credit
balance and reports `x-codex-limit-reached: true` with a window at 100%. No
error ever removes it from selection, so the selector reads those headers. The
account is `usage-credit` from the response that reports the limit until the
exhausted window's reset time. After that it is `allowance` again.

Only Codex is recognised. A Claude subscription without extra usage is
rejected at its limit and cools down through the normal quota handling, so it
is never a candidate while exhausted. Recognising Claude extra usage needs its
response headers observed first.

The classification comes from the account's latest response only. A limit
flag that arrives without a window, which happens when Codex reports a window
above 100%, is trusted for 30 minutes and then checked again with one request.
A subscription with no quota headers yet counts as `allowance`. After a
restart the first request to an exhausted account therefore draws credits once
and corrects the view.

## Bound sessions

With session affinity, a bound session keeps its account while healthy. The
funding order is the one exception. When a credential of the same provider is
available in a better class, the session moves to it on its next request. This
covers explicit session IDs, inherited child bindings and LCP matching.

A request keeps its account when it refers to state that only that account
holds:

- an explicit credential pin, including Claude Thread owner pins set by the
  manager
- Claude Thread continuation IDs, a reused container, workspace custom Skills,
  file references in image, document or container-upload blocks, and encrypted
  advisor results
- a Responses request that names `previous_response_id` or a conversation, or
  refers to an uploaded file, a container or a vector store. The next request
  that carries its full input is portable.

A downstream Codex WebSocket connection is pinned to its account for as long
as it stays open, so such a session moves when it next connects.

The selector never copies, strips or rewrites hosted resources. Moving a
session does not change its model, effort or history. It can rebuild the
upstream prompt cache once. `session-affinity-rebind-on: never` keeps every
existing binding and still applies the order to new sessions.

## Limits

This is a routing preference, not a spending cap. A lower class is used when
nothing better is available. A transient failure on a preferred credential can
send that request to a lower class under the normal retry policy.

A move assumes the conversation's history is accepted by the destination
account. That holds between two sign-ins of the same kind, which is also what
quota failover relies on. The binding changes when the destination is picked,
before it has answered. A destination that rejects the conversation keeps the
session until the binding expires.

Any sign-in that is not an API key counts as `allowance` unless it is
recognised as drawing credits. That includes service accounts on mixed routes.

The policy runs in the local session-affinity selector, so it requires
`session-affinity: true`. A Home-controlled or plugin-owned scheduling decision
must implement its own policy. Changing the order through a configuration
reload recreates the selector, like other affinity settings, and clears its
local bindings.
