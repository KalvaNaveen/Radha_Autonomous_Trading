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
| 15:50 | **Evening run** | Once today's daily candle is final: raises stops (and modifies their GTTs), flags exits for the morning, checks the market regime, and scans the universe. The candidates are queued for the next trading day. |

The book (positions, pending candidates, cash, cooldowns) is saved in
`data/portfolio.json`. You can stop and restart the engine at any time. Open
positions stay protected by their GTTs at Zerodha while it's off.

---

## Strategy

**Trend filter** (every candidate must pass):
- close above the 50-day EMA, the 20-day EMA above the 50-day EMA, and the 50-day EMA rising over 5 sessions;
- price at least ₹50;
- average daily turnover at least ₹10 crore.

**Market regime:** new entries are allowed only while NIFTY 50 closes above a rising 50-day EMA. Open positions are always managed.

**Setups** (on the completed daily candle):
- **Breakout:** close above the prior 20-day high, on at least 1.5× average volume.
- **Pullback:** the low touched the 20-day EMA (within 1%) in the last 3 sessions, and today closed above the 20-day EMA and above yesterday's high, on at least average volume.

**Ranking:** 60-day return relative to NIFTY 50. Up to 5 positions in total, at most 2 new ones per day.

**Position size:** the smallest of:
- 1% of equity at risk ÷ (entry − stop);
- 20% of equity ÷ price;
- available cash.

**Stops** (they only ever move up; R = entry − initial stop):

| Stage | When | Stop |
|---|---|---|
| Initial | On entry | Entry − 2 × ATR(14), kept between 3% and 8% below entry |
| Breakeven | Close ≥ entry + 1R | Entry + round-trip costs |
| Locked | Close ≥ entry + 2R | Entry + 1R |
| Trailing | After breakeven | Highest close − 3 × ATR (only if higher than the current stop) |

**Exits:**
- the stop is hit (GTT);
- after breakeven, a close below the 20-day EMA → sell at the next morning run;
- time stop: 40 sessions without reaching +1R;
- cooldown: no re-entry in the same stock for 5 sessions after an exit.

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

- **Control panel:** **Backtest** tab → choose the number of years → **Run backtest**. It uses today's Kite login to download daily candles and caches them in `data/candles/`, so later runs only fetch the new days.
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
- **Strategy:** breakout detection, no longs in downtrends, the liquidity filter, the breakeven → lock → trail ratchet, the stop never moving down, sizing limits, delivery charges to the paisa.
- **Backtester:** invariants (cash never negative, position cap respected, no overlapping trades, every trade pays charges) and gap-through-stop fills at the open.
- **Order manager:** limiter caps, priority order, retry budget, buy/sell round trip.
- **Candle cache:** incremental downloads, chunking.
- **Control panel API**, config validation, token expiry.
