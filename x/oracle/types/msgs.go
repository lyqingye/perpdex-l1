package types

import (
	sdk "github.com/cosmos/cosmos-sdk/types"
	sdkerrors "github.com/cosmos/cosmos-sdk/types/errors"
)

var (
	_ sdk.Msg = (*MsgAggregateOracleVotes)(nil)
	_ sdk.Msg = (*MsgUpdateParams)(nil)
)

func validAddr(s string) error {
	if _, err := sdk.AccAddressFromBech32(s); err != nil {
		return sdkerrors.ErrInvalidAddress.Wrap(err.Error())
	}
	return nil
}

func (m *MsgAggregateOracleVotes) ValidateBasic() error {
	if err := validAddr(m.Authority); err != nil {
		return err
	}
	if m.Height <= 0 {
		return ErrInvalidVote.Wrap("height must be > 0")
	}
	for _, agg := range m.Aggregations {
		if agg.IndexPrice == 0 || agg.MarkPrice == 0 {
			return ErrInvalidPrice.Wrapf(
				"market_index=%d zero price (index=%d mark=%d)",
				agg.MarketIndex, agg.IndexPrice, agg.MarkPrice,
			)
		}
	}
	return nil
}

func (m *MsgUpdateParams) ValidateBasic() error {
	if err := validAddr(m.Authority); err != nil {
		return err
	}
	return m.Params.Validate()
}
