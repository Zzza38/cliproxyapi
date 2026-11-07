# Fork maintenance

This repository contains the Go server at the root and the custom dashboard in
`management-ui/`. The original frontend repository's history is preserved as a
parent of the consolidation merge; its source remains in `management-ui/`.

Remotes:

- `origin`: `git@github.com:Zzza38/cliproxyapi.git`
- `upstream`: `https://github.com/router-for-me/CLIProxyAPI.git`

To update, fetch upstream tags and merge the selected release into a branch.
Keep the custom subscription selector, Claude quota backoff, durable quota/token
history, Claude safeguard pass-through, and Codex Ultrafast eligibility tests.
Run `go test ./...`, build `./cmd/server`, and run `pnpm run check` inside
`management-ui/` before merging the pull request.

Keep management panel auto-updates disabled when serving the custom dashboard.
Set `management.disable-auto-update-panel: true` and install the built
`management-ui/dist/management.html` at the configured static path.

CI checks both the backend and dashboard. Upstream-only contribution guards and
Docker publishing are restricted to the upstream repository; they do not close,
retarget, or block pull requests in this fork.
