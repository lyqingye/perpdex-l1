package keeper

import (
	"testing"

	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

// TestValidateUpdateMarket_Ok accepts a sensibly populated Msg.
func TestValidateUpdateMarket_Ok(t *testing.T) {
	m := &types.MsgUpdateMarket{
		NewStatus:           perptypes.MarketStatusActive,
		NewTakerFee:         100,
		NewMakerFee:         10,
		NewMinBaseAmount:    1,
		NewMinQuoteAmount:   1,
		NewOrderQuoteLimit:  0,
		NewExpiryTimestamp:  0,
	}
	require.NoError(t, validateUpdateMarket(m))
}

// TestValidateUpdateMarket_BadStatus rejects arbitrary enum values.
func TestValidateUpdateMarket_BadStatus(t *testing.T) {
	m := &types.MsgUpdateMarket{NewStatus: 99, NewTakerFee: 1, NewMakerFee: 1, NewMinBaseAmount: 1, NewMinQuoteAmount: 1}
	err := validateUpdateMarket(m)
	require.ErrorIs(t, err, types.ErrInvalidMarket)
}

// TestValidateUpdateMarket_FeeTooHigh rejects fees at or above FeeTick.
func TestValidateUpdateMarket_FeeTooHigh(t *testing.T) {
	m := &types.MsgUpdateMarket{
		NewStatus:         perptypes.MarketStatusActive,
		NewTakerFee:       uint32(perptypes.FeeTick),
		NewMakerFee:       1,
		NewMinBaseAmount:  1,
		NewMinQuoteAmount: 1,
	}
	err := validateUpdateMarket(m)
	require.ErrorIs(t, err, types.ErrInvalidParams)
}

// TestValidateUpdateMarket_ZeroMinAmount rejects zero minimums which
// would otherwise silently disable the protection.
func TestValidateUpdateMarket_ZeroMinAmount(t *testing.T) {
	m := &types.MsgUpdateMarket{
		NewStatus:         perptypes.MarketStatusActive,
		NewTakerFee:       1,
		NewMakerFee:       1,
		NewMinBaseAmount:  0,
		NewMinQuoteAmount: 1,
	}
	err := validateUpdateMarket(m)
	require.ErrorIs(t, err, types.ErrInvalidParams)
}
