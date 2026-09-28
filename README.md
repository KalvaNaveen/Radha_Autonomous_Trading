# Radha — automated swing trading engine for NSE (Zerodha Kite Connect)

Radha trades **long-only equity delivery (CNC)**, holding positions for days to
weeks. Each evening it scans a universe of stocks on daily candles and queues
entry candidates. It buys them the next morning. Every position is protected at
the broker by a **GTT stop**, which the engine ratchets upward as the trade
works. A built-in **backtester** runs the exact same rules on years of
history, so you can see how the strategy would have performed before you risk
money.

> **Risk notice.** This is software, not financial advice. Backtest first, then
> paper-trade for several weeks. Past results — real or simulated — don't
> guarantee future ones. You are responsible for your account and for SEBI
> compliance.

---

## Quick start

1. Run the binary for your OS from `dist/`:
   - Windows: `radha-engine-windows-amd64.exe`
   - Mac: `radha-engine-darwin-arm64` (Apple Silicon) or `radha-engine-darwin-amd64` (Intel)
   - Linux: `radha-engine-linux-amd64`

   Put it in an empty folder and run it. On the first run it creates `config.yaml` (paper mode) and `universe.csv`, and opens the control panel at `http://127.0.0.1:8080`.
2. In the panel, paste your Kite **API key and secret**.
3. On developers.kite.trade, set your app's **Redirect URL** to `http://127.0.0.1:8080/kite/callback`.
4. Click **Log in with Zerodha**. Do this once per trading morning.
5. Open the **Backtest** tab, choose 5 years and click **Run backtest**. Read the results before anything else.
6. Leave the engine running on trading days. The daily schedule is below.

Building from source (Go 1.22+): `make deps && make test && make build`.

---

## A day in the engine's life (IST)

| Time | Step | What happens |
|---|---|---|
| 08:45 | **Prepare** | Waits for your Kite login. Reconciles the book with your Kite holdings and GTTs: a position sold while the engine was off gets booked as closed, and a missing stop is re-armed. If the previous evening's run was missed, it's run now to catch up. |
| 09:20 | **Morning run** | 1) Sells positions flagged the evening before. 2) Sells anything that **gapped below its stop** (a GTT places a *limit* order, which a gap can skip past). 3) Buys the queued candidates, highest relative strength first, with a LIMIT order 0.5% above the ask. It skips a candidate if it opened > 2% above the signal close, opened below its stop, or is at the upper circuit. Every buy gets a GTT stop straight away. |
| 09:20–15:30 | **Monitor** | Refreshes prices every minute. Detects when a GTT stop has filled and books the trade. Re-arms any stop that's missing, and sells at market if the price is below the stop with no GTT armed. |
| 15:50 | **Evening run** | Once today's daily candle is final: raises stops if ratcheting is on (and modifies their GTTs), flags exits (e.g. EMA10 below EMA20) for the morning, checks the market regime, and scans the universe. The candidates are queued for the next trading day. |

The book (positions, pending candidates, cash, cooldowns) is saved in
`data/portfolio.json`. You can stop and restart the engine at any time. Open
positions stay protected by their GTTs at Zerodha while it's off.

---

## Strategy

**Filters** (every candidate must pass):
- price at least ₹50;
- average daily turnover at least ₹10 crore;
- for `breakout`/`pullback` (and for `ema_cross` with `cross_trend_filter: true`): close above the 50-day EMA, the 20-day EMA above the 50-day EMA, and the 50-day EMA rising over 5 sessions.

**Market regime:** new entries are allowed only while NIFTY 50 closes above a rising 50-day EMA. Open positions are always managed.

**Entry (default `setups: ema_cross`), on the completed daily candle:** EMA10 crosses above EMA20 while Supertrend(10, 3) is green. The entry is taken only on the day the combined condition turns true (whichever of the two happens last), and bought at the next morning run.

Other setups are still available: `breakout` (close above the prior 20-day high on ≥ 1.5× volume), `pullback`, or `both`.

**Ranking:** 60-day return relative to NIFTY 50. Up to 5 positions in total, at most 2 new ones per day.

**Position size:** the smallest of:
- 1% of equity at risk ÷ (entry − stop);
- 20% of equity ÷ price;
- available cash.

**Protective stop:** entry − 3 × ATR(14), kept between 3% and 8% below entry, placed as a GTT at Zerodha so it works even when this PC is off. Once a close reaches +1.5R, the stop moves to entry + charges (`breakeven_at_r`), so a trade that was well in profit can no longer turn into a loss. It never moves down.

Also available (all tested, none adopted — see below): `stop_mode: supertrend | swing_low`, `trail_mode: atr | supertrend`, `target_r` + `partial_pct` (backtest only), and the legacy ratchet (`fixed_stop: false`).

**Exits:**
- EMA10 closes below EMA20 → sell at the next morning run (`exit_on_ema_cross`);
- the protective stop is hit (GTT), or the stock opens below it;
- cooldown: no re-entry in the same stock for 5 sessions after an exit.

**Drawdown breaker:** if equity falls 15% below its peak, new entries pause for 20 trading days; then the peak resets to current equity and trading resumes.

**Why these defaults:** `tools/study` replays variants on cached Kite candles over 5 years and each half separately. On the default 25-stock list (2021-09 → 2026-09):

| Rules | 5-yr return | Max DD | Profit factor | 1st half | 2nd half |
|---|---|---|---|---|---|
| Original (breakout + pullback, ratcheting stop) | −15.5% | 27.7% | 0.81 | +7.0% | −25.9% |
| Breakout + Heikin-Ashi | +2.8% | 14.7% | 1.05 | +13.8% | −6.1% |
| **EMA10/20 cross + Supertrend, fixed 3×ATR stop** | **+15.0%** | **11.7%** | **1.37** | **+6.9%** | **+2.3%** |

**Stops, trailing and targets** (same study, on top of the EMA-cross rules): every trailing stop made results worse — the EMA10/20 cross-down already works as a trailing exit, and a tighter trail only cut winners short. Profit targets were unstable (+4R: +5.7%, +5R: +15.3%, +6R: +17.6%, +8R: +13.3%), which is noise, not an edge. Breakeven at +1.5R was neutral over 5 years (+14.9% vs +15.0%) and better in 2024–26 (+4.3% vs +2.3%), so it is on for capital protection.

NIFTY 50 returned +28% over the same period. The EMA-cross rules are the first set that made money in both halves, but ~110 trades on 25 stocks is a small sample: widen the universe and re-run the study before trading real capital.

**Costs modelled:** Zerodha delivery charges.
- brokerage ₹0;
- STT 0.1% on both the buy and the sell;
- stamp duty 0.015% on the buy;
- NSE exchange charge 0.00307%, SEBI fee ₹10/crore, GST 18%;
- DP charge ₹15.34 per sell;
- plus 0.1% slippage per side.

Every number above can be changed in `config.yaml`.

---

## Backtesting

- **Control panel:** **Backtest** tab → choose the number of years → **Run backtest**. **Reset…** clears the saved result (tick the box to also delete the downloaded candles and re-download them). A result made with different rules than the engine now uses is marked **Out of date**. It uses today's Kite login to download daily candles and caches them in `data/candles/`, so later runs only fetch the new days.
- **Command line:** `radha-backtest -years 5` downloads from Kite. `radha-backtest -csv ./history -years 5` works offline from CSV files (`date,open,high,low,close,volume`, one file per stock plus `NIFTY50.csv`).

The simulation, day by day:
1. **At the open:** sell positions flagged the evening before, then buy candidates (with the same gap checks as live).
2. **During the day:** a stop hit fills at the stop, or at the open if the stock gapped below it.
3. **At the close:** raise stops, flag exits, scan for the next day.

Output: headline figures (CAGR vs NIFTY, max drawdown, win rate, average win and loss, profit factor, expectancy in R, time in market, total charges), an equity curve plotted against the index, and every trade with the reason it exited. Files are written to `data/backtest/latest/` (or `-out`): `report.html`, `trades.csv`, `equity.csv`.

---

## Control panel

| Area | What it shows |
|---|---|
| Checklist | Credentials, today's Kite login, universe, engine stage (with a **Retry** button after an error), the next three scheduled runs |
| KPIs | Equity, cash, invested, realized P&L, positions / maximum, drawdown from peak, market regime |
| Open positions | Entry, LTP, P&L, current R, stop and its stage, GTT status, next action (hold, or "SELL next morning: reason") |
| Candidates | Tomorrow's queue with the reasons, plus the verdict for every stock in the last scan |
| Backtest | Run a backtest; view figures, the equity curve vs NIFTY, the trades, and the full HTML report |
| Trades | The journal, with monthly P&L and a cumulative total |
| Universe | Edit the scanned stocks (up to 300 NSE symbols) |
| Logs | Live log, filterable to warnings and errors |

---

## Going live — checklist

1. The backtest over 5+ years is acceptable **after costs**, and you're comfortable with its maximum drawdown.
2. At least 4–6 weeks of paper trading show behaviour consistent with the backtest.
3. **DDPI is active** on your Zerodha account (or you authorise CDSL TPIN each day). Without it, API sells of holdings are rejected, which includes GTT stops.
4. A static IPv4 address is registered in the Kite developer console, and `network.bind_ip` is set to it.
5. `mode: live`. Start with small `risk.capital`.

The engine only ever sells quantities **it bought itself**. Any other shares
you hold, including extra shares of the same stock, are left alone.

---

## Rehearsal against a simulated Kite

`tools/kitemock` simulates Kite's login, historical data and quotes, and plays a scripted day.
- **SWUP** and **SWDN** break out and get bought in the morning run.
- **SWDN** then falls 7% and its GTT stop fires.
- The evening run manages the rest and scans for the next day.

```bash
go run ./tools/kitemock -setup reh          # terminal 1 (writes reh/config.yaml with a compressed ~6-minute schedule)
./bin/radha-engine -config reh/config.yaml  # terminal 2, then open http://127.0.0.1:8080/login
```

Observed run:

```
16:53:18 previous evening run was missed — catching up now
16:53:23 evening run complete  candidates=3 regime=RISK-ON
16:54:09 BOUGHT SWUP  21 @ 940.10  stop 902.30  risk ₹794   → stop GTT armed 902.25
16:54:09 BOUGHT SWX02 37 @ 529.76  stop 510.48  risk ₹713   → stop GTT armed 510.45
16:54:09 BOUGHT SWDN   5 @ 3431.72 stop 3319.22 risk ₹563   → stop GTT armed 3319.20
16:56:09 stop GTT filled SWDN @ 3300.90 → TRADE CLOSED net −₹706.95 (−1.26R, gap between minute polls)
16:58:13 evening run complete  positions=2 candidates=0
```

---

## Architecture

```
cmd/engine            daemon + control panel (first run creates config.yaml / universe.csv)
cmd/backtest          CLI backtester (Kite or CSV)
internal/swing        strategy rules, sizing, costs, portfolio state, journal — shared by live and backtest
internal/backtest     day-loop simulator + HTML/CSV report
internal/engine       schedule: prepare → morning → monitor → evening; reconciliation; backtest runner
internal/data         universe, instruments, cached daily candles (chunked, paced under Kite limits)
internal/broker       Kite (CNC orders, GTT, holdings, quotes, history) · Paper (simulated account) · static-IP client
internal/ordermanager priority queue (emergency > stop > entry), sliding-window limiter, retries, GTT de-duplication
internal/indicators   EMA, Wilder ATR, SMA, prior N-day high
internal/auth         daily Kite login (redirect flow), token + credential storage
internal/web          control panel (embedded HTML/JS)
tools/kitemock        Kite simulator for rehearsals
```

## Tests

`make test` runs:
- **Indicators:** EMA, ATR and highs.
- **Strategy:** EMA10/20 cross + Supertrend entry and cross-down exit, Supertrend and Heikin-Ashi maths, breakout detection, no longs in downtrends, the liquidity filter, the breakeven → lock → trail ratchet, the stop never moving down, sizing limits, delivery charges to the paisa.
- **Backtester:** invariants (cash never negative, position cap respected, no overlapping trades, every trade pays charges) and gap-through-stop fills at the open.
- **Order manager:** limiter caps, priority order, retry budget, buy/sell round trip.
- **Candle cache:** incremental downloads, chunking.
- **Control panel API**, config validation, token expiry.
