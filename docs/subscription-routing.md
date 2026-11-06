# Subscription routing

Select this strategy in `config.yaml`:

```yaml
routing:
  strategy: soonest-reset-first
```

The strategy enables session affinity. Existing sessions stay on their account
while it remains available, including when another account's allowance is closer
to expiry. New sessions and failovers use the following order within the highest
available credential priority:

1. Earliest future weekly quota reset or available manual-reset credit expiry.
2. Most remaining weekly allowance, converted to base-plan units.
3. Credential ID for deterministic ties.

For example, a Codex account with 25% of a 20-unit allowance has 5 units remaining.
It wins a reset-time tie against an account with 40% of a 10-unit allowance, which
has 4 units remaining. A sooner deadline takes precedence over either quantity.

The configured accounting policy uses these multipliers:

| Provider | Plan | Base-plan units |
| --- | --- | ---: |
| Codex | Plus | 1 |
| Codex | Pro 100 / ProLite | 10 |
| Codex | Pro 200 / Pro | 20 |
| Codex | Pro 500 / ProMax | 50 |
| Claude | Pro | 1 |
| Claude | Max 5x | 5 |
| Claude | Ultra | 10 |

These are routing weights for comparing remaining allowance, not guarantees of
a fixed number of tokens. Unrecognized plans use one unit.

The server probes native Codex and Claude OAuth accounts once a minute, with at
most four probes running concurrently. It reads Codex usage and reset-credit
details, plus Claude usage and profile tier information, through each account's
normal authenticated transport. It never redeems a manual-reset credit.

Known exhausted weekly and short-term windows are unavailable until their reset.
Expired reset credits are ignored. Observations older than five minutes stop
affecting ranking and availability. Accounts without a known deadline follow
accounts with known deadlines; when none are known, selection uses round robin.
API-key credentials and providers without subscription probes retain this fallback.

This strategy applies to local routing. When CLIProxyAPIHome owns dispatch,
configure its routing separately.

Codex endpoint and reset-credit shapes follow the upstream
[backend client](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/client/rate_limit_resets.rs)
and [reset-credit types](https://github.com/openai/codex/blob/main/codex-rs/backend-client/src/types.rs).
