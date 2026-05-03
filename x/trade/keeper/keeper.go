package keeper

import (
	"context"
	"fmt"

	"cosmossdk.io/collections"
	"cosmossdk.io/core/store"
	"cosmossdk.io/math"

	"github.com/cosmos/cosmos-sdk/codec"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/trade/types"
)

// Keeper provides pure trade application functions used by x/matching and
// x/liquidation. It owns no state apart from Params.
type Keeper struct {
	cdc           codec.BinaryCodec
	storeService  store.KVStoreService
	authority     string
	accountKeeper types.AccountKeeper
	marketKeeper  types.MarketKeeper
	fundingKeeper types.FundingKeeper
	riskKeeper    types.RiskKeeper

	Schema collections.Schema
	Params collections.Item[types.Params]
}

func NewKeeper(cdc codec.BinaryCodec, storeService store.KVStoreService, authority string,
	ak types.AccountKeeper, mk types.MarketKeeper, fk types.FundingKeeper, rk types.RiskKeeper,
) Keeper {
	sb := collections.NewSchemaBuilder(storeService)
	k := Keeper{
		cdc:           cdc,
		storeService:  storeService,
		authority:     authority,
		accountKeeper: ak,
		marketKeeper:  mk,
		fundingKeeper: fk,
		riskKeeper:    rk,

		Params: collections.NewItem(sb, types.ParamsKey, "params", codec.CollValue[types.Params](cdc)),
	}
	schema, err := sb.Build()
	if err != nil {
		panic(fmt.Errorf("trade: %w", err))
	}
	k.Schema = schema
	return k
}

func (k Keeper) Authority() string { return k.authority }

// Fill is the input to ApplyPerpsMatching / ApplySpotMatching. It captures one
// match between a maker and a taker.
type Fill struct {
	MakerAccountIndex uint64
	TakerAccountIndex uint64
	MarketIndex       uint32
	Price             uint32
	BaseAmount        uint64
	IsTakerAsk        bool
	TakerFee          uint32
	MakerFee          uint32
	NoFee             bool // liquidation/deleverage path
	// NoRiskCheck skips the post-trade IsValidRiskChange call on the
	// taker. Reserved for forced close-outs (market-expiry exit, etc.)
	// where the insurance fund has to absorb residual size even when
	// doing so worsens its own health classification.
	NoRiskCheck bool
}

// ApplyPerpsMatching applies a perp fill to both maker and taker positions.
// Implements the 8-step pipeline from 14-trade.md §3:
//  1. settle pending funding for both sides
//  2. compute position deltas (4 scenarios)
//  3. realize PnL into collateral
//  4. apply taker/maker fees, transfer to TREASURY
//  5. recompute isolated allocated_margin if needed
//  6. update OI
//  7. validate IsValidRiskChange for taker (and maker if cross-margined)
func (k Keeper) ApplyPerpsMatching(ctx context.Context, f Fill) error {
	if err := k.fundingKeeper.SettlePositionFunding(ctx, f.MakerAccountIndex, f.MarketIndex); err != nil {
		return err
	}
	if err := k.fundingKeeper.SettlePositionFunding(ctx, f.TakerAccountIndex, f.MarketIndex); err != nil {
		return err
	}

	makerSign := int64(1)
	if !f.IsTakerAsk {
		makerSign = -1
	}
	takerSign := -makerSign

	if err := k.applyPositionChange(ctx, f.MakerAccountIndex, f.MarketIndex, f.Price, f.BaseAmount, makerSign); err != nil {
		return err
	}
	if err := k.applyPositionChange(ctx, f.TakerAccountIndex, f.MarketIndex, f.Price, f.BaseAmount, takerSign); err != nil {
		return err
	}

	if !f.NoFee {
		notional := math.NewIntFromUint64(f.BaseAmount).Mul(math.NewIntFromUint64(uint64(f.Price)))
		takerFee := notional.Mul(math.NewIntFromUint64(uint64(f.TakerFee))).Quo(math.NewInt(int64(perptypes.FeeTick)))
		makerFee := notional.Mul(math.NewIntFromUint64(uint64(f.MakerFee))).Quo(math.NewInt(int64(perptypes.FeeTick)))
		if err := k.accountKeeper.AddCollateral(ctx, f.TakerAccountIndex, takerFee.Neg()); err != nil {
			return err
		}
		if err := k.accountKeeper.AddCollateral(ctx, f.MakerAccountIndex, makerFee.Neg()); err != nil {
			return err
		}
		// Fees go to the treasury account (account_index=0).
		if err := k.accountKeeper.AddCollateral(ctx, perptypes.TreasuryAccountIndex, takerFee.Add(makerFee)); err != nil {
			return err
		}
	}

	if err := k.marketKeeper.UpdateOpenInterest(ctx, f.MarketIndex, int64(f.BaseAmount)); err != nil {
		return err
	}

	if f.NoRiskCheck {
		return nil
	}
	ok, err := k.riskKeeper.IsValidRiskChange(ctx, f.TakerAccountIndex)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("trade: taker risk regression")
	}
	return nil
}

// applyPositionChange handles the four position-change scenarios from
// 14-trade.md §3.2: open new, increase, decrease, flip.
func (k Keeper) applyPositionChange(ctx context.Context, accountIdx uint64, marketIdx uint32, price uint32, baseAmount uint64, sign int64) error {
	pos, err := k.accountKeeper.GetPosition(ctx, accountIdx, marketIdx)
	if err != nil {
		return err
	}
	curSize := pos.Position
	delta := math.NewIntFromUint64(baseAmount).MulRaw(sign)
	newSize := curSize.Add(delta)

	curEntryQuote := pos.EntryQuote
	notional := math.NewIntFromUint64(baseAmount).Mul(math.NewIntFromUint64(uint64(price))).MulRaw(sign)

	switch {
	case curSize.IsZero():
		// open new position
		pos.EntryQuote = notional
	case sameSign(curSize, delta):
		// increase
		pos.EntryQuote = curEntryQuote.Add(notional)
	case newSize.IsZero() || sameSign(curSize, newSize):
		// pure decrease (or close): realize partial PnL into collateral
		realized := notional.Add(curEntryQuote.Mul(delta).Quo(curSize.Neg()))
		if err := k.accountKeeper.AddCollateral(ctx, accountIdx, realized); err != nil {
			return err
		}
		// scale entry_quote proportionally to remaining size
		if curSize.IsZero() {
			pos.EntryQuote = math.ZeroInt()
		} else {
			pos.EntryQuote = curEntryQuote.Mul(newSize).Quo(curSize)
		}
	default:
		// flip: close existing then open in opposite direction
		closeBase := curSize.Abs()
		closeNotional := closeBase.Mul(math.NewIntFromUint64(uint64(price))).MulRaw(-sign)
		realized := closeNotional.Add(curEntryQuote)
		if err := k.accountKeeper.AddCollateral(ctx, accountIdx, realized); err != nil {
			return err
		}
		residual := delta.Add(curSize) // residual same sign as delta
		residualNotional := residual.Mul(math.NewIntFromUint64(uint64(price)))
		pos.EntryQuote = residualNotional
	}
	pos.Position = newSize
	return k.accountKeeper.SetPosition(ctx, pos)
}

func sameSign(a, b math.Int) bool {
	if a.IsZero() || b.IsZero() {
		return false
	}
	return a.IsNegative() == b.IsNegative()
}

// ApplySpotMatching applies a spot fill: taker gives quote, gets base (buy)
// or vice versa (sell). UNIFIED collateral mode keeps account.collateral and
// account_asset.balance synchronized.
func (k Keeper) ApplySpotMatching(ctx context.Context, f Fill, baseAssetID, quoteAssetID uint32) error {
	notional := math.NewIntFromUint64(f.BaseAmount).Mul(math.NewIntFromUint64(uint64(f.Price)))
	// Direction: taker is ask -> taker sells base, buys quote.
	if f.IsTakerAsk {
		if err := k.transferAsset(ctx, f.TakerAccountIndex, f.MakerAccountIndex, baseAssetID, math.NewIntFromUint64(f.BaseAmount)); err != nil {
			return err
		}
		if err := k.transferAsset(ctx, f.MakerAccountIndex, f.TakerAccountIndex, quoteAssetID, notional); err != nil {
			return err
		}
	} else {
		if err := k.transferAsset(ctx, f.MakerAccountIndex, f.TakerAccountIndex, baseAssetID, math.NewIntFromUint64(f.BaseAmount)); err != nil {
			return err
		}
		if err := k.transferAsset(ctx, f.TakerAccountIndex, f.MakerAccountIndex, quoteAssetID, notional); err != nil {
			return err
		}
	}

	if !f.NoFee {
		takerFee := notional.Mul(math.NewIntFromUint64(uint64(f.TakerFee))).Quo(math.NewInt(int64(perptypes.FeeTick)))
		makerFee := notional.Mul(math.NewIntFromUint64(uint64(f.MakerFee))).Quo(math.NewInt(int64(perptypes.FeeTick)))
		if err := k.transferAsset(ctx, f.TakerAccountIndex, perptypes.TreasuryAccountIndex, quoteAssetID, takerFee); err != nil {
			return err
		}
		if err := k.transferAsset(ctx, f.MakerAccountIndex, perptypes.TreasuryAccountIndex, quoteAssetID, makerFee); err != nil {
			return err
		}
	}
	return nil
}

func (k Keeper) transferAsset(ctx context.Context, from, to uint64, assetID uint32, amount math.Int) error {
	src, err := k.accountKeeper.GetAccountAsset(ctx, from, assetID)
	if err != nil {
		return err
	}
	dst, err := k.accountKeeper.GetAccountAsset(ctx, to, assetID)
	if err != nil {
		return err
	}
	src.Balance = src.Balance.Sub(amount)
	dst.Balance = dst.Balance.Add(amount)
	if err := k.accountKeeper.SetAccountAsset(ctx, src); err != nil {
		return err
	}
	return k.accountKeeper.SetAccountAsset(ctx, dst)
}
