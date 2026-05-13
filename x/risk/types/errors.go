package types

// Mark-price errors live on the market module: x/market/types
// {ErrZeroMarkPrice, ErrStaleMarkPrice, ErrMissingPrice}. Risk
// consumers of MarketKeeper.GetMarkPrice / GetMarkPriceAndDetails
// propagate those errors verbatim.
