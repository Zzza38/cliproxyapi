# Client quota reporting

CLIProxyAPI reports pooled quota to Codex and Claude Code through their native response headers. Codex WebSocket sessions also receive a `codex.rate_limits` event for each request. General upstream header passthrough does not need to be enabled.

The pool contains enabled accounts for the request's provider and model, subject to its credential policy. Session pinning does not narrow the displayed pool. Codex and Claude have separate pools. Duplicate account identities count once, using their newest observation.

For each usage window, the reported percentage is the sum of observed usage divided by the number of accounts. For example, accounts with 20% and 80% remaining report 50% remaining across their combined allowance. Each account has equal weight because upstream percentage signals do not provide absolute plan capacities. This is a normalized estimate, not a token-weighted total across different subscription plans.

Each window must have a valid observation for every account in its pool. Unknown quota, expired reset times, and incompatible Codex window durations suppress that window instead of presenting an invented balance. Observations come from ordinary upstream responses; this feature does not poll accounts or spend requests to discover their quota. The displayed reset is the earliest observed reset in the pool, when the pool's balance next changes.

Supported windows are Codex primary and secondary, and Claude 5-hour and 7-day windows, including model-specific Claude windows when provided. Upstream single-account quota headers and Codex WebSocket quota events are replaced so they cannot overwrite the combined result. Other response headers retain their existing passthrough policy.

Home-managed execution retains its existing reporting behavior. The local account manager cannot infer a complete pool managed by a separate Home server.

Codex's header and WebSocket event formats are defined in its [rate limit parser](https://github.com/openai/codex/blob/main/codex-rs/codex-api/src/rate_limits.rs).

## Codex account usage panel

Codex app-server reads account usage separately from model responses. Its `account/rateLimits/read` call uses `chatgpt_base_url`, even when the model provider points at CLIProxyAPI. Configure both URLs to avoid sending the proxy credential to ChatGPT's account API:

```toml
chatgpt_base_url = "https://your-proxy.example/backend-api"

[model_providers.cli_proxy]
base_url = "https://your-proxy.example/v1"
```

The authenticated `GET /backend-api/wham/usage` endpoint reports the same normalized local Codex pool. Restart the Codex app-server after changing its configuration. This endpoint does not report or consume manual reset credits; unknown credit counts are not reported as zero.

## Claude account usage

The authenticated `GET /api/oauth/usage` endpoint returns the local Claude pool in the OAuth usage format. `five_hour` and `seven_day` contain `utilization` on a 0–100 scale and an ISO `resets_at` timestamp. Unknown windows are null. Manual reset credits and spending limits are omitted.

Claude's inference gateway setting does not establish that its separate account-usage reader uses the gateway. Verify the reader's destination before provisioning it. Integrations with a separate account-usage client, such as T3, can read this endpoint with their configured proxy credential. Do not send proxy credentials to Anthropic's account API or use them to replace a direct-Anthropic login.
