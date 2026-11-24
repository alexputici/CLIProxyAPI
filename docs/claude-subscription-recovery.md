# Claude subscription recovery

To use eligible Claude subscriptions before API credits and return after recovery:

```yaml
routing:
  strategy: soonest-reset-first
  session-affinity: true
  session-affinity-rebind-on: any
  session-affinity-prefer-claude-subscription: true
```

Give Claude OAuth credentials a higher `priority` than Claude API keys. Normal
priority, model support, cooldown, caller eligibility and retry exclusions still
apply. `soonest-reset-first` chooses among eligible subscriptions using observed
weekly reset times; the recovery option also works with other built-in selectors.

The option defaults to false. When enabled, a portable conversation bound to a
Claude API key returns to an eligible Claude OAuth credential on its next request,
including explicit session IDs, inherited child bindings and LCP matching. It
retains healthy OAuth bindings and does not switch between other providers or
arbitrary priority tiers. Recovery only considers the highest eligible priority
tier, so an intentionally higher-priority API key still takes precedence.

Selection does not rewrite the model, effort or conversation history. Changing
credentials can rebuild the upstream prompt cache. `session-affinity-rebind-on:
never` retains its no-failover contract and disables this option. An explicit
credential pin also takes precedence.

## Hosted resources

Optional recovery is suppressed when the original request contains native Claude
Thread continuation IDs, a reused container ID, workspace custom Skills, file references in image,
document or container-upload blocks, or encrypted advisor result blocks. Those
conversations need their owning credential's funding route. The selector does not
copy, strip or rewrite hosted resources. Claude Thread owner pins are also applied
by the manager before normal selection.

Anthropic's [Files API documentation](https://platform.claude.com/docs/en/build-with-claude/files)
scopes files to a workspace. The proxy's existing Thread owner bindings and
advisor ownership tracking provide the corresponding local constraints. This
option does not establish ownership for arbitrary external references, and does
not change existing unavailable-credential failover or executor sanitization.
Clients must retain explicit owner pins when using account-owned resources through
other request formats or fields.

## Spending and deployment limits

A transient subscription error can still cause API usage under the existing
retry policy, even if subscription quota is not exhausted. `rebind-on: quota`
retains the original binding for transient failures but can still use API credits
for the failing request. This option is a routing preference, not a spend cap.

The policy runs in the local session-affinity selector. A Home-controlled or
plugin-owned scheduling decision must implement its own corresponding policy.
Changing the option through configuration reload recreates the selector, like
other affinity-policy changes, and clears its local bindings.
