package types

import (
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseCurrencyPair(t *testing.T) {
	cases := []struct {
		in   string
		err  bool
		base string
		quo  string
	}{
		{"BTC/USD", false, "BTC", "USD"},
		{"  eth/usdt ", false, "ETH", "USDT"},
		{"sol/USD", false, "SOL", "USD"},
		{"BTCUSDT", true, "", ""},
		{"BTC/", true, "", ""},
		{"/USD", true, "", ""},
		{"", true, "", ""},
	}
	for _, tc := range cases {
		got, err := ParseCurrencyPair(tc.in)
		if tc.err {
			require.Error(t, err, tc.in)
			continue
		}
		require.NoError(t, err, tc.in)
		require.Equal(t, tc.base, got.Base)
		require.Equal(t, tc.quo, got.Quote)
	}
}

func TestPriceFromString(t *testing.T) {
	v, err := PriceFromString("60123.45", 6)
	require.NoError(t, err)
	require.Zero(t, v.Cmp(big.NewInt(60123450000)))
}

func TestPriceFromStringRejectsZero(t *testing.T) {
	_, err := PriceFromString("0", 6)
	require.Error(t, err)
}

func TestPriceFromFloat(t *testing.T) {
	v := PriceFromFloat(60123.45, 6)
	require.Zero(t, v.Cmp(big.NewInt(60123450000)))
}
