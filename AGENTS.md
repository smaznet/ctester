# AGENTS.md

Guidance for AI agents working on **x-tester**.

## What this project is

Go orchestrator around **Xray**:

- Fetch subscription links (`vmess` / `vless` / `trojan` / `ss`)
- Probe via a dedicated Xray instance
- Mount healthy outbounds onto a main Xray via `xray api` (`ado` / `adi` / `rmo` / `rmi`)
- Accept client traffic (socks5 / http / direct) and sticky-balance to mounted nodes
- Persist probe/ignore state in SQLite; fixed Bubble Tea TUI

Target runtime: **Linux** for production (`direct` needs `SO_ORIGINAL_DST`). macOS is OK for build/dev except transparent redirect.

## Hard constraints (do not regress)

1. **Xray API flag order**: `xray api <cmd> --server=host:port <file.json>` — `--server` must come *before* file args.
2. **API JSON shape**: wrap as `{"outbounds":[...]}` / `{"inbounds":[...]}` — bare outbound objects fail with `no valid outbound found`.
3. **Serialize HandlerService calls** per Xray instance (`apiMu`). Concurrent `ado`/`adi` races cause spurious `add outbound` failures.
4. **Routing slots**: base config pre-seeds `slot-in-N → slot-out-N` rules; dynamic AddRule is not relied on.
5. **Auth**: password-only; any username accepted (used for `hash_username` sticky).
6. **Country filter**: filtered nodes are `ignored` — never retest; persist in SQLite.
7. **TUI**: keep output height-clamped (alt-screen); do not print probe logs to stdout (they go to log ring).
8. **SQLite**: `MaxOpenConns(1)` — never `Query` + nested `Exec` on the same connection without finishing/closing rows first (deadlock).

## Layout

| Path | Role |
|------|------|
| `cmd/x-tester` | CLI `-c config.yaml` |
| `internal/app` | lifecycle, sub refresh, probe rounds, remount |
| `internal/config` | YAML schema + validation |
| `internal/sub` | subscription fetch/parse → outbound maps |
| `internal/xray` | start processes, slot alloc, API mount |
| `internal/probe` | health + geo (`ifconfig.io/country_code` plain text) |
| `internal/balancer` | sticky + latency weights |
| `internal/listen` | client protocols; Linux `direct_*` build tags |
| `internal/store` | SQLite `node_states` (+ legacy `ignored_nodes`) |
| `internal/tui` | fixed dashboard |
| `internal/stats` | HTTP JSON |
| `config.example.yaml` | canonical config example |

## Config knobs agents should know

- `listen.accept_proxy_protocol`: when true, require PROXY v1/v2; `hash_ip` uses header source IP
- `probe.max_active`: cap mounted healthy nodes (`0` = unlimited); at cap only recheck actives; below cap fill/replace again
- `probe.standby`: warm pool size (probed OK, not mounted); promotes instantly when active drops; needs `max_active > 0`
- `probe.latency_tolerance`: evict active if slower than best active by more than this (not standby; retest later can return)
- `probe.mount_batch`: mount to main every N healthy results (multiples of N), flush remainder at end; `0` = end-only
- `probe.interval_active` / `interval_failed`: due scheduling after restart uses DB `last_check`
- `grouping.url` default: `https://ifconfig.io/country_code` (plain country code body)
- `database`: path relative to config file dir unless absolute
- On restart: restore ignored/failed/active from DB; **remount** previous actives without re-probe; skip due until intervals elapse

## Coding conventions

- Prefer small focused packages; keep `app` as wiring only.
- Match existing style: no drive-by refactors, no unsolicited README/docs unless asked.
- Errors shown in TUI should include protocol + address + compact xray API message.
- When changing outbound generation (`internal/sub`), keep Xray JSON compatible with current xray-core CLI.
- Tests: `go test ./internal/store/ ./internal/sub/ ./internal/xray/` (xray tests skip/require `xray` in PATH).

## Common failure modes

| Symptom | Likely cause |
|---------|----------------|
| `add outbound` / `ado:` | bad link JSON, stale tag, or API race (should be mitigated by `apiMu` + retry) |
| `no free xray slots` | concurrency too high vs `slotCount` or removes stuck |
| Always `waiting` / no nodes | empty/bad `sub_urls`, all ignored by `filter_country`, or fetch errors in TUI log |
| Scroll in terminal | TUI view exceeding height — clamp lines to `WindowSizeMsg` height |
| SQLite hang on Open | nested query/exec with single conn — read then write |

## Debug artifacts

- Work dir: temp `x-tester-*` with `main/` and `probe/`
- `probe/last-api-fail.json` — last failed ado/adi payload path + error
- `probe/probe.log`, `main/main.log` — xray process logs

## Do not

- Import full `github.com/xtls/xray-core` as a library unless explicitly requested (project shells out to the binary).
- Break sticky restore-on-primary-up behavior.
- Retest `ignored` nodes after country filter reject.
- Write markdown files the user did not ask for.
