# Liquidation Improvement Fee

This note explains how the per-fill **liquidation improvement fee** is
charged during a partial-liquidation IOC and where the money ends up.
It is targeted at integrators, market makers and operators who need to
reconcile fills that involve a liquidated account.

## Where the fee comes from

When a perp account drops into `PARTIAL_LIQUIDATION` health, a keeper
bot dispatches `MsgLiquidate`, which:

1. Cancels every resting order owned by the victim (so the victim
   cannot front-run the close-out).
2. Submits a system-issued
   `LIQUIDATION_ORDER + IOC + reduce_only` taker on the victim's
   behalf at the victim's `zero_price`. See
   [`x/matching/keeper/match_liquidation.go`](../x/matching/keeper/match_liquidation.go).
3. Matches against open makers at maker prices that are **not worse
   than `zero_price`**. Any improvement above `zero_price` is taxed
   per-fill.

The taker on this synthetic order is always the victim. The maker is
whichever resting order the matching loop crosses with.

## The fee formula

The fee is computed in
[`liquidationImprovementFee`](../x/trade/keeper/perp/engine.go) and is
applied per individual fill:

```text
improvement     = sign(takerSide) * (price - zero_price)
price_diff_rate = (|improvement| * FeeTick) / price
effective_rate  = min(market.LiquidationFee, price_diff_rate)
fee             = notional * effective_rate / FeeTick
notional        = base_amount * price
```

Notes on the inputs:

- `price` is the **actual fill price** (the maker's resting price), not
  the zero-price floor.
- `improvement` is non-positive when the maker price is at or worse
  than `zero_price`; the fee collapses to zero in that case.
- `market.LiquidationFee` is a governance-configured cap, expressed in
  the same `FeeTick` precision used everywhere else in the chain.
- The fee is paid in cross collateral on the maker side.

## Who pays the fee

**The maker pays.** Concretely, the fee is debited from the maker's
cross collateral inside
[`applyCrossAccount`](../x/trade/keeper/perp/cross.go) right after the
realized PnL is settled. The taker (the victim) is never charged the
improvement fee; the victim's only obligation is to accept fills at or
better than its `zero_price`.

This is the opposite direction from the maker rebate that traders are
used to on a normal book. Two design points explain it:

1. **The taker is forced.** The victim does not choose to enter the
   book — it is a system-issued liquidation order. The maker is the
   only party in this fill that voluntarily placed an order.
2. **The maker is already getting a price improvement.** The fill
   prints at the maker's resting price, which is by construction
   strictly better than `zero_price` (otherwise the
   `price_diff_rate` is zero and no fee is charged). The improvement
   fee is a cap on that windfall: the maker keeps `price - zero_price`
   minus `min(market.LiquidationFee, price_diff_rate)` of the
   improvement, scaled by notional.

In other words: the maker pays a fraction of the improvement they
captured by being inside the spread when the liquidation hit, and that
fraction is at most `market.LiquidationFee`.

## Where the fee ends up

`f.LiquidationFeeRecipient` is set by `MatchLiquidationOrder` and
defaults to the **Insurance Fund operator account**
(`perptypes.InsuranceFundOperatorAccountIdx`). The accumulated fees
back the IF's ability to absorb future deleverage / ADL events and so
indirectly insulate every other trader from residual debt.

## Worked example

```text
zero_price            = 39_000
maker price (fill)    = 39_200
base_amount           = 5
market.LiquidationFee = 50          (i.e. 5 bps of price)
FeeTick               = 1_000_000

improvement           = 200
notional              = 5 * 39_200          = 196_000
price_diff_rate       = (200 * 1_000_000) / 39_200
                     ≈ 5_102
effective_rate        = min(50, 5_102)      = 50
fee                   = 196_000 * 50 / 1_000_000
                     ≈ 9.8                  → 9 (integer floor)
```

The fee scales linearly with `effective_rate`. In a calm market the
maker's price will land close to `zero_price`, the `price_diff_rate`
will be small, the `min` will pick `price_diff_rate`, and the
effective fee rate collapses to that natural spread — so makers do not
get charged the configured cap on tight fills.

## FAQ

**Q: I'm a maker. Why was a fee debited from my account on a fill
labelled `LiquidationOrder`?**

Because the matching loop crossed your resting order with a
system-issued liquidation taker, and your resting price was strictly
better than the victim's `zero_price`. The fee is a cap on the
improvement you captured; it is at most
`min(market.LiquidationFee, price_diff_rate)` of notional.

**Q: Can I opt out?**

No. The fee is per-market and configured by governance; there is no
per-account override. The improvement-fee cap is enforced inside the
trade engine for every liquidation fill that crosses the configured
threshold.

**Q: Will I always pay the full `market.LiquidationFee`?**

No. The effective rate is `min(market.LiquidationFee, price_diff_rate)`.
On a fill where your maker price is only marginally better than
`zero_price`, `price_diff_rate` is small and wins the `min`; the
configured cap only kicks in when the improvement is large enough that
`price_diff_rate >= market.LiquidationFee`.

**Q: I'm a regular taker on a non-liquidation order. Does this fee
apply to me?**

No. The improvement fee path is gated on `LiquidationFeeBps > 0` AND
on the trade being initiated by `MatchLiquidationOrder`. Normal
user-driven `OpenOrder` fills route through the standard `TakerFee` /
`MakerFee` pipeline and never touch `liquidationImprovementFee`.

## Pointers

- Fee computation: [`x/trade/keeper/perp/engine.go`](../x/trade/keeper/perp/engine.go) — `liquidationImprovementFee` and the surrounding `Apply` flow.
- Maker debit: [`x/trade/keeper/perp/cross.go`](../x/trade/keeper/perp/cross.go) — `applyCrossAccount`.
- Liquidation IOC dispatch: [`x/matching/keeper/match_liquidation.go`](../x/matching/keeper/match_liquidation.go) — `MatchLiquidationOrder` and `applyLiquidationFill`.
- Liquidation entry point: [`x/liquidation/keeper/liquidate.go`](../x/liquidation/keeper/liquidate.go) — `Liquidate`.
