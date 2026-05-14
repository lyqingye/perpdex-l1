package types

import "errors"

// ErrStopIteration is the sentinel a callback returns to terminate an
// orderbook iteration without surfacing an error to the caller. The
// keeper-side iterators (IterateAccountOpenOrders, IterateTriggers)
// translate it into a nil return; any *other* non-nil error returned
// from a callback aborts iteration and is propagated verbatim.
//
// Plain `errors.New` is intentional: this is a control-flow sentinel,
// not a user-facing chain error, so it should not be tagged with the
// module codespace or shown in CLI responses.
var ErrStopIteration = errors.New("orderbook: stop iteration")
