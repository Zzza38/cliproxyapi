# management-ui

Source for the CLIProxyAPI management panel served at `/management.html`.

It is [Cli-Proxy-API-Management-Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center)
v1.27.3 (MIT) with these changes:

- **Subscription pools on the Quota page.** Claude and Codex accounts are pooled into
  one weekly-allowance chart per provider, with pool empty, first account empty, next
  reset and burn rate. A remaining/drain-rate toggle, burn window, future usage and week
  navigation sit above the charts. The chart code is adapted from
  [yazinsai/prox](https://github.com/yazinsai/prox) commit `f6a0125628` (MIT).
- **Percentages relative to the largest plan.** Each provider pool uses its highest
  configured subscription as 100%. For example, with Pro 500 and Pro 100 accounts,
  total capacity is 120%. With 99% and 90% of their own allowances remaining, the
  pool displays 117% / 120%. Plan weights still use Codex 1/10/20/50 and Claude
  1/5/10 internally, so routing and quota comparisons retain the same behavior.
- **Compact panels.** Account-order rows and explanatory paragraphs are omitted.
  Charts retain short recorded/projected labels and a reference-plan label.
- **soonest-reset-first in the routing and config editors.** Session affinity stays
  enabled for this strategy.
- **Auto-read.** Visible quota pages can refresh every 2, 5 or 15 minutes, or never.
  Reset credits are redeemed only through the account card's reset button.

### Where the history comes from

The server samples every Claude and Codex credential's weekly window once a minute and
keeps 28 days of real readings (`GET /v8/management/credentials/quota/history`). The
charts therefore show history from while the browser was closed and across server
restarts. Successful quota reads this browser makes are still logged in `localStorage`
per management URL, and both are merged in memory:

- On the first visit per connection, the browser's weekly readings for credentials that
  still exist are imported into the server store (`POST` on the same path, up to 100,000
  rows). The server skips duplicates. The local log is kept, not cleared.
- The quota page re-reads the server history once a minute while it is open and visible,
  whether or not auto-read is on. Server readings win on equal timestamps. Server history
  is not copied to `localStorage`.
- If the server store is disabled or the endpoint is missing, the charts fall back to the
  browser log alone, as before.
- Nothing is back-filled. The recorded pool line starts only once every account in the
  pool has a reading, and straight lines join consecutive real readings.
- The current pool value uses whichever reading is newer, the server's sample or this
  browser's last read. The short-window account cards still come from normal quota reads.
- Burn rate needs two real readings at least a minute apart. It spreads the spend between
  readings evenly and ends at the latest reading, never at "now".
- Everything right of "now" is a dashed projection: measured burn × the future usage
  factor, with each account refilling to its full allowance at its weekly reset.
- "Clear local history" deletes this browser's log for the current server. The server's
  history stays.

Each provider panel also has one row of server token counts for the last 24 hours
(`GET /v8/management/observability/usage/tokens`): total, input, output, cache read and
cache write. Input already includes the cache tokens, and the total is the server's own
figure. The page re-reads them with the history, once a minute. The row is hidden when
the server has no token store or no such endpoint.

## Commands

Requires Node 24 and pnpm 12. Tests run on Bun, installed as a dev dependency.

```bash
pnpm install
pnpm dev          # Vite dev server; enter your CLIProxyAPI URL on the login screen
pnpm build        # type-checks, then writes dist/management.html (single file)
pnpm test         # bun test (tests/)
pnpm lint
pnpm typecheck
pnpm check        # typecheck + lint + test + build
```

To keep requests same-origin in development, start Vite with a proxy target:

```bash
CLIPROXY_URL=http://127.0.0.1:8317 pnpm dev
```

## Using the build with CLIProxyAPI

The server serves one file named `management.html`. It looks for it at
`MANAGEMENT_STATIC_PATH` (a directory, or the file itself), then `$WRITABLE_PATH/static/`,
then `static/` next to `config.yaml`.

1. Build:

   ```bash
   cd management-ui && pnpm install && pnpm build
   ```

2. Turn off the panel auto-updater. If you skip this, the server replaces your file with
   the upstream release at startup and every 3 hours, because the hashes differ.

   ```yaml
   management:
     secret-key: "your-management-key" # required; an empty key disables the management API
     disable-control-panel: false
     disable-auto-update-panel: true
   ```

   Legacy-layout configs use the same keys under `remote-management:`.

3. Point the server at the file, using either option:

   ```bash
   # a) environment variable (file or directory)
   export MANAGEMENT_STATIC_PATH=/abs/path/to/management-ui/dist/management.html

   # b) or copy it to the default location next to config.yaml
   mkdir -p /path/to/config-dir/static
   cp dist/management.html /path/to/config-dir/static/management.html
   ```

   In Docker, mount the file and set `MANAGEMENT_STATIC_PATH` to its path inside the
   container.

4. Restart CLIProxyAPI and open `http://127.0.0.1:8317/management.html`. Log in with
   the management key.

5. Check that the server is serving this build and not the upstream release:

   ```bash
   sha256sum dist/management.html
   curl -s http://127.0.0.1:8317/management.html | sha256sum
   ```

Home (cluster) mode and `disable-control-panel: true` both turn the `/management.html`
route off.

The panel uses the `/v8/management` API: credentials, config, `requests/api-call` for
upstream quota reads, `routing/cooldown/reset`, `credentials/quota/history` for the
persisted weekly history, and `observability/usage/tokens` for token counts. Against a
server without those endpoints, it stops asking for the session, uses the browser log
only, and hides the token row.

## Layout

- `src/features/quota/pool/` holds the pool model, plan units, local and server history,
  the server sync loop, the routing-order estimate, and the glue between them. Everything
  except `useQuotaHistory.ts` is React-free and covered by `tests/quotaPool.test.ts` and
  `tests/quotaServerHistory.test.ts`.
- `src/features/quota/components/QuotaPool*.tsx` and `QuotaControls.tsx` are the
  charts and controls.
- Everything else is upstream v1.27.3. Upstream's Bun lockfile, CI workflows and its
  Bun-version test were left out. Imports that Bun's flat `node_modules` resolved
  implicitly (`motion-dom`, `react-router`, `@codemirror/*`) are now explicit
  dependencies for pnpm.

Other edits to upstream files:

- `soonest-reset-first` support in `types/routing.ts`, `types/visualConfig.ts`,
  `services/api/routing.ts`, `hooks/useVisualConfig.ts`, the routing `PolicyPanel` and
  `logic.ts`, `SectionNetwork.tsx`, `DashboardPage.tsx` and `config/searchIndex.ts`.
- `QuotaPage.tsx`, `QuotaPage.module.scss` and `uiState.ts` for the pools section.
- New `quota_pool` strings and the strategy labels in all seven locales.
- One upstream test assertion in `tests/authFileCooldowns.test.ts` compared raw text
  against HTML-escaped markup. It now compares against the escaped form.

To pull in a newer upstream release, diff its `src/` against v1.27.3 and apply that
diff here, then re-check the files above.
