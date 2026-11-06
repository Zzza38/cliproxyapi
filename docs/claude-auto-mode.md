# Claude Code auto mode

Claude Code can request server-side action review through the `safeguards`
Messages field. The proxy preserves that field and other unknown request fields,
returns `safeguard_results` in JSON and SSE responses, and keeps tool-use IDs
unchanged when safeguards are present.

For these requests, caller-provided Anthropic feature headers and version take
precedence over the proxy's client fingerprint. The proxy still uses the selected
account's authentication and keeps the credential and feature betas required by the prepared request, including
the beta paired with `thinking.display: "updates"`. Configured payload rules remain the
final authority over the request body.

After updating the server, start a new Claude Code session in auto mode. Run
`/status` and check that **Auto mode server** says **Enabled**. A successful
Messages request alone does not prove the upstream account supports server-side
review. The proxy never manufactures a safety verdict or disables the classifier.

See Anthropic's [classifier billing notice](https://code.claude.com/docs/en/auto-mode-classifier-billing)
and [gateway compatibility guide](https://code.claude.com/docs/en/llm-gateway-protocol).
