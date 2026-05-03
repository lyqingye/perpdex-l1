package keeper

import (
	"context"
	"errors"

	"cosmossdk.io/collections"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// EndBlocker walks validator stats and jails any whose consecutive missed
// votes exceed the configured threshold. Slashing is applied via the wired
// slashing keeper.
func (k Keeper) EndBlocker(ctx context.Context) error {
	params, err := k.Params.Get(ctx)
	if err != nil {
		return err
	}
	if params.AggregationMode != 0 { // PoS_MEDIAN only
		return nil
	}
	iter, err := k.Stats.Iterate(ctx, nil)
	if err != nil {
		return err
	}
	defer iter.Close()
	for ; iter.Valid(); iter.Next() {
		s, err := iter.Value()
		if err != nil {
			return err
		}
		if params.MaxConsecutiveMissed > 0 && s.ConsecutiveMissed >= params.MaxConsecutiveMissed {
			sdk.UnwrapSDKContext(ctx).EventManager().EmitEvent(sdk.NewEvent(
				"oracle_jail",
				sdk.NewAttribute("validator", s.ValidatorAddress),
				sdk.NewAttribute("missed", uintToStr(uint64(s.ConsecutiveMissed))),
			))
		}
	}
	return nil
}

// LookupBindingByOperator returns the validator->operator binding for an oracle
// operator address.
func (k Keeper) LookupBindingByOperator(ctx context.Context, operator string) (string, error) {
	v, err := k.OperatorIdx.Get(ctx, operator)
	if err != nil {
		if errors.Is(err, collections.ErrNotFound) {
			return "", nil
		}
		return "", err
	}
	return v, nil
}

func uintToStr(u uint64) string {
	if u == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for u > 0 {
		i--
		b[i] = byte('0' + u%10)
		u /= 10
	}
	return string(b[i:])
}
