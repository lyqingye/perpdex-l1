package daemon

import (
	oracletypes "github.com/perpdex/perpdex-l1/x/oracle/types"
)

// oracleMarketPrice is an internal alias used to break the import cycle
// between this daemon package and `x/oracle/keeper`. The keeper imports the
// daemon to construct a PriceFetcher; if the daemon also tried to import the
// keeper to reference its types, we'd cycle. Defining the alias against the
// types package (which the keeper also imports) keeps everyone happy.
type oracleMarketPrice = oracletypes.MarketPrice
