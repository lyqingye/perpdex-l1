package types_test

import (
	"os"
	"testing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

	perptypes "github.com/perpdex/perpdex-l1/types"
	"github.com/perpdex/perpdex-l1/x/market/types"
)

const testAuthority = "px1qv9pzxqlyckngw6zf9g9whn9d3eh4qvgsxc8cx"

// TestMain configures the chain-wide `px` bech32 prefix once per
// process so validAuth accepts the canonical test address.
func TestMain(m *testing.M) {
	cfg := sdk.GetConfig()
	cfg.SetBech32PrefixForAccount("px", "pxpub")
	cfg.SetBech32PrefixForValidator("pxvaloper", "pxvaloperpub")
	cfg.SetBech32PrefixForConsensusNode("pxvalcons", "pxvalconspub")
	os.Exit(m.Run())
}

// TestMsgUpdateMarket_ValidateBasic_Ok accepts a sensibly populated Msg.
func TestMsgUpdateMarket_ValidateBasic_Ok(t *testing.T) {
	m := &types.MsgUpdateMarket{
		Authority:          testAuthority,
		NewStatus:          perptypes.MarketStatusActive,
		NewTakerFee:        100,
		NewMakerFee:        10,
		NewMinBaseAmount:   1,
		NewMinQuoteAmount:  1,
		NewOrderQuoteLimit: 0,
		NewExpiryTimestamp: 0,
	}
	require.NoError(t, m.ValidateBasic())
}

// TestMsgUpdateMarket_ValidateBasic_BadStatus rejects arbitrary enum values.
func TestMsgUpdateMarket_ValidateBasic_BadStatus(t *testing.T) {
	m := &types.MsgUpdateMarket{
		Authority:         testAuthority,
		NewStatus:         99,
		NewTakerFee:       1,
		NewMakerFee:       1,
		NewMinBaseAmount:  1,
		NewMinQuoteAmount: 1,
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidMarket)
}

// TestMsgUpdateMarket_ValidateBasic_FeeTooHigh rejects fees at or above FeeTick.
func TestMsgUpdateMarket_ValidateBasic_FeeTooHigh(t *testing.T) {
	m := &types.MsgUpdateMarket{
		Authority:         testAuthority,
		NewStatus:         perptypes.MarketStatusActive,
		NewTakerFee:       uint32(perptypes.FeeTick),
		NewMakerFee:       1,
		NewMinBaseAmount:  1,
		NewMinQuoteAmount: 1,
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}

// TestMsgUpdateMarket_ValidateBasic_ZeroMinAmount rejects zero minimums
// which would otherwise silently disable the protection.
func TestMsgUpdateMarket_ValidateBasic_ZeroMinAmount(t *testing.T) {
	m := &types.MsgUpdateMarket{
		Authority:         testAuthority,
		NewStatus:         perptypes.MarketStatusActive,
		NewTakerFee:       1,
		NewMakerFee:       1,
		NewMinBaseAmount:  0,
		NewMinQuoteAmount: 1,
	}
	require.ErrorIs(t, m.ValidateBasic(), types.ErrInvalidParams)
}
