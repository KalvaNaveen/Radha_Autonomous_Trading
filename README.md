# kitealgo — intraday MIS momentum engine for NSE (Zerodha Kite Connect)

A Go daemon that trades up to 50 NSE stocks per day using a VWAP + RVOL + 10/20 EMA
pullback strategy. It has a stepped profit lock, 10-EMA trailing stops and a hard
square-off. Every order-changing call goes through one rate-limited, prioritised
order manager.

> **Risk notice.** This is software, not financial advice. Run it in `paper` mode
> for several weeks before going live. Check every journal against your Kite
> contract notes. You are responsible for your broker account and for complying
> with SEBI's rules.

---

## Your daily routine

| When | You | Engine |
|---|---|---|
| Any evening after 8 PM | Drop `watchlists/<next-trading-date>.csv` | Nothing yet |
| ~08:30–09:10 | Open `http://127.0.0.1:8080/login` (through an SSH tunnel) and log in to Zerodha | Catches the redirect and stores today's token |
| 08:30 | — | Loads the watchlist and instruments, then 10 days of 1-minute history (for RVOL and the EMA seed) |
| 09:15–09:45 | — | Warmup: builds candles, VWAP and RVOL. No orders |
| 09:45–14:45 | — | Trades |
| 14:45–15:08 | — | Manages exits only |
| 15:08 | — | Kill switch: flattens everything |
| 15:09:30 | — | Sweep: asks the **broker** for any open MIS position or live tagged order and fixes it |
| Evening | Read `journal/<date>.csv` | Sleeps until the next trading day |

Watchlist format (header optional, up to 50 rows, `#` comments allowed):

```csv
symbol,benchmark
RELIANCE,NIFTY ENERGY
HDFCBANK,NIFTY BANK
TCS,NIFTY IT
INFY
```

`benchmark` is optional. It names an NSE sector index the radar also checks.
Run `./engine -config config.yaml -check` after dropping a file to validate it.

If the file is missing at 08:30, the engine keeps checking every minute until 14:45.
If you haven't logged in, it logs the login URL every 5 minutes.

---

## Strategy, as implemented

**Entry** is evaluated on each closed 5-minute bar, and only between 09:45 and 14:45. Long rules:

1. The 10 EMA crossed above the 20 EMA on this bar or the previous one (`pullback_candles: 2`).
2. The bar pulled back into the EMA band: `low ≤ max(EMA10, EMA20) × (1 + 0.15%)`.
3. The bar closed back above the 10 EMA.
4. `close > VWAP × 1.002`.
5. `RVOL ≥ 1.5`. RVOL is today's cumulative volume divided by the 10-day average cumulative volume at the same minute, interpolated within the minute.
6. The radar doesn't disagree: NIFTY 50, and the stock's benchmark if given, isn't bearish.
7. Risk allows it: a free slot, enough margin, the daily loss limit not hit, and fewer than 3 trades today in this symbol.

Short is the mirror image. The EMAs are seeded from the previous sessions' 5-minute
closes, so they're valid at 09:45 instead of after 20 bars (~11:00).

**Order used:** an IOC limit order 0.5% beyond the best ask or bid. It fills at the
touch and never rests. The price cap satisfies SEBI's market-protection requirement.

**Position size:** `min(capital × 0.5% / stop_distance, capital × leverage / max_positions / price)`.

**Stops.** One SL-M order sits at the broker, so it survives an engine crash or a
network loss. It only ever tightens:

| Stage | Trigger | Stop | Net result if hit |
|---|---|---|---|
| Initial | fill | 20 EMA, clamped to 0.25%–0.80% from entry | loss ≤ 0.80% + costs |
| Breakeven | +0.40% gross | entry ± 0.15% | ≈ 0 (costs covered) |
| Profit lock | +0.65% gross | entry ± 0.40% | **+0.25% net** |
| Trailing | each 5-minute close after breakeven | max(lock, 10 EMA ∓ 0.25%) | ≥ locked level |

You'll also see an exit **on a 5-minute close** on the wrong side of the 10 EMA,
or at the 15:08 square-off.

Why two stages instead of one: at +0.40% gross, a stop that locks 0.25% net would
have to sit at +0.40%, which is the current price, so it would trigger at once.

---

## Where this differs from the original spec, and why

| Spec | Implementation | Reason |
|---|---|---|
| Token bucket at 8 OPS via `time.Ticker` | Sliding-window log: ≤8 per 1.15 s, ≤350/min, ≤4,500/day | A token bucket with rate 8 and burst 8 lets **16** requests through in one second. The 150 ms guard absorbs dispatch jitter: the 50-agent stress test saw 10 calls in one second without it and ≤8 with it. |
| Exceeding 10 OPS "suspends the account" | 8 OPS cap kept | Under SEBI's April 2026 retail-algo framework, going above 10 OPS means registering the strategy. Kite rejects the extra requests with HTTP 429. |
| Unbuffered fan-out channels | Buffered mailboxes that drop the oldest tick | With unbuffered channels, one slow agent either stalls the WebSocket or loses nearly every tick. Volume and VWAP are cumulative in every tick, so dropping an old tick loses nothing. |
| "Priority channel" | Explicit mutex-protected lanes | Go's `select` chooses randomly among ready channels, so it doesn't actually prioritise. |
| Retry the stop update once, then flatten | Stop **placement** failure → flatten. Stop **modify** failure → check the resting stop; flatten only if it can't be confirmed live | A failed modify leaves the old stop in place, so the position isn't unprotected. |
| Exit = cancel the stop, then send a market order | Exit = **modify the SL-M into a MARKET order** | Done as one operation, the stop and the exit can never both fill. Cancel-then-place is only the fallback. |
| Trail on every 5-minute bar | Moves smaller than 0.05% are skipped; cancel-and-replace after 24 modifications | Kite allows 25 modifications per order. |
| Kill switch at 15:10 | 15:08 (configurable) | Zerodha auto-squares stocks it classifies as CAS at 15:12. |
| Tick-by-tick VWAP | Exchange ATP by default; own calculation available | Kite ticks are roughly 1-second snapshots, not individual trades. |
| — | Entries queued longer than 10 s are dropped | When 50 stocks signal together, stops go first. An entry filled 10 s late is chasing. |
| — | Ambiguous PLACE failures (timeout/5xx) are checked against the order book before any retry | The order may already exist at the broker. Retrying blindly can double an exit and leave you in a reversed position. |
| — | The fill ledger decides the position | Any fill the engine didn't expect, or a restart mid-day, results in an *orphan*. The engine adopts it and flattens it. |

---

## Rehearsal: the full flow against a simulated Kite (no market, no money)

`tools/kitemock` pretends to be Kite: the login redirect, the session exchange (it
verifies the SHA-256 checksum), profile, margins, the instrument master,
historical minute candles, and a binary KiteTicker WebSocket. It plays a scripted
~19-minute session with 1-minute candles:

- **RADHAUP:** a long setup.
- **RADHADN:** a short setup.
- **RADHAFLAT:** must never trade.
- **NOTAREALSTOCK:** must be skipped.

Run it on a weekday (the engine skips weekends):

```bash
make build
go run ./tools/kitemock -setup rehearsal     # writes rehearsal/config.yaml + today's watchlist, starts the mock
./bin/engine -config rehearsal/config.yaml   # second terminal
# open http://127.0.0.1:8080/login in your browser, then watch http://127.0.0.1:8080/status
```

Expected output, from a real run:

```
00:16:57 Kite session established            user=RK1234
00:16:57 watchlist  NOTAREALSTOCK: not an NSE EQ instrument — skipped
00:16:59 session prepared                    agents=3 indices=2 radar=permissive
00:16:59 ticker connected                    instruments=5
00:21:44 ENTRY signal  RADHAUP  LONG  qty=497  "cross+pullback, close 100.50 vwap 100.00 rvol 3.01"
00:21:44 ENTRY signal  RADHADN  SHORT qty=502
00:21:44 FILLED / stop live                  RADHAUP trigger=100.10, RADHADN trigger=99.90
00:23:39 lock stage BREAKEVEN                stop → 100.75 / 99.25
00:25:01 lock stage PROFIT_LOCK              stop → 101.00 / 99.00
00:30:44 – 00:31:44 trailing                 stop → 101.15 / 98.85
00:32:37 trade closed  RADHAUP net ₹178.14   stop hit (PROFIT_LOCK @ 101.15)
00:32:37 trade closed  RADHADN net ₹181.51   stop hit (PROFIT_LOCK @ 98.85)
00:34:44 SQUARE-OFF: kill switch → all agents HALTED
00:35:14 sweep complete                      (broker flat, no live orders)
00:36:15 session closed                      trades=2 realized=359.65 orders=11, 0 errors
```

The `dev:` overrides that point the engine at the mock are rejected when `mode: live` is set.

---

## Architecture

```
KiteTicker WS ──► ticker.Multiplexer ──(drop-oldest mailbox)──► agent.Agent ×N ─┐
      │ order postbacks                                                          │ OrderPayload
      ▼                                                                          ▼
ordermanager.UpdateRouter ◄── Reconciler (polls order book every 3s)   ordermanager.OrderManager
      │ (dedup, terminal-state guard)                                   ├─ PriorityQueue P1>P2>P3
      └──────────────► agent inbox (never dropped)                      ├─ RateLimiter 8/1.15s·350/min·4500/day
                                                                        ├─ retries + PLACE de-dup
radar.Radar (NIFTY 50 + sector indices) ──veto──► agent                 └─ broker.Kite (static-IP HTTP) | broker.Paper
risk.Manager (sizing, slots, margin, daily breaker, journal) ◄──► agent
engine.Engine: daily scheduler, login wait, 15:08 kill, 15:09:30 broker sweep, SIGTERM flatten
```

```
cmd/engine/main.go            flags, logging, signal handling
internal/engine               daily lifecycle, kill switch, safety sweep, /status
internal/agent                stock_agent.go (state machine), strategy.go (pure maths)
internal/indicators           EMA, VWAP, RVOL profile, candle builder
internal/ordermanager         rate_limiter.go, priority_queue.go, order_manager.go, router.go
internal/ticker               WebSocket multiplexer
internal/broker               Trader/MarketData interfaces, Kite, Paper, static-IP client
internal/{auth,risk,radar,history,watchlist,clock,config}
pkg/models                    shared types
```

Each agent's state (`FLAT → PENDING_ENTRY → IN_POSITION → TRAILING → PENDING_EXIT → FLAT/HALTED`)
is owned by its own goroutine, so the hot path has no locks. Status snapshots
use an `RWMutex`.

---

## Setup

1. **Kite Connect app** at developers.kite.trade:
   - Set the redirect URL to `http://127.0.0.1:8080/kite/callback`.
   - Whitelist your VPS's static IPv4 under IP Whitelist.
2. **VPS** with a static IPv4 (any Indian region). Go 1.22 or later.
3. Build:
   ```bash
   make deps     # go mod tidy — fetches gokiteconnect + yaml and writes go.sum
   make test     # unit + end-to-end simulation tests (race detector on)
   make build    # → bin/engine
   ```
4. `cp config.example.yaml config.yaml`, then set `api_key` and `bind_ip`. Fill in `holidays` from NSE's circular.
5. `export KITE_API_SECRET=...`. With systemd, put it in `/etc/kitealgo.env` instead.
6. `sudo cp deploy/kitealgo.service /etc/systemd/system/ && sudo systemctl enable --now kitealgo`
7. Each morning: `ssh -L 8080:127.0.0.1:8080 you@vps`, open `http://127.0.0.1:8080/login`, log in. Check progress at `/status`.

`/status` returns JSON: phase, risk (P&L, open slots, halt reason), order-manager
counters and remaining rate budget, ticker mailbox depth and drops, and each
agent's state, VWAP, EMAs, RVOL, position and last signal verdict.

---

## Tests

- **`internal/agent`**: runs full sessions against the paper broker with the real order manager and router.
  - Winning trade: breakeven → profit lock → trailing → stop exit.
  - Losing trade: initial stop hit.
  - Kill switch mid-trade.
  - Stop placement that fails twice: the position is flattened.
  - 50 agents at once: no 1-second window above 8 broker calls, and everything is flat after the kill.
- **`internal/ordermanager`**:
  - The limiter never exceeds its cap in any window; the token-bucket counter-example.
  - Priority ordering; kill switch drops entries.
  - Stop retry budget.
  - Ambiguous PLACE de-duplicated (the exit executes exactly once).
  - Router de-duplication and the terminal-state guard.
- **Other packages**: indicator maths, lock levels, stop clamps, entry rules, risk sizing and breaker, radar modes, watchlist parsing, token expiry at 06:00, config validation.

## Known limits

- Paper fills are pessimistic (far side of the book + 2 bps). They are not an exchange queue model.
- After a mid-day restart the engine **flattens** positions it finds from earlier orders; it doesn't resume managing them. Per-symbol trade counts reset on restart.
- The radar reads index trend from the day's open and the 5-minute EMAs; index ticks carry no volume, so there's no index VWAP.
- The 0.15% cost model is a flat percentage. Real brokerage is capped at ₹20 per order, so percentage costs fall as trade size grows.
