package types

// PreRiskSnapshot captures the pre-state risk envelope of an account
// at the moment a state-mutating handler starts running, so the same
// handler can later prove its post-state did not regress against the
// pre-state via IsValidRiskChangeFrom.
//
// Snapshots are values: they live for the duration of one Msg / fill,
// are passed by parameter, and are discarded when the function
// returns. There is no chain-level KV cache — pre-state must NEVER
// outlive the handler that produced it, otherwise a later tx would
// compare its post-state against an unrelated previous tx's
// pre-state and silently accept regressions.
//
// Field semantics:
//
//   - Cross: nil when the account has no cross aggregate at the
//     moment of the snapshot (e.g. a fresh account). Treated as
//     "missing pre-state" by IsValidRiskChangeFrom, which falls back
//     to "post must be HEALTHY" per the Lighter spec.
//   - Isolated: keyed by marketIdx, populated only for non-zero
//     isolated positions held by the account at snapshot time. A
//     per-market entry MUST be missing for any isolated market that
//     was opened during the handler so the same fall-back kicks in.
type PreRiskSnapshot struct {
	Cross    *RiskParameters
	Isolated map[uint32]RiskParameters
}

// IsolatedFor returns the pre-state isolated RP for `marketIdx` and a
// flag indicating whether one was recorded. Callers use the flag to
// drive the "missing pre" fall-back.
func (s PreRiskSnapshot) IsolatedFor(marketIdx uint32) (RiskParameters, bool) {
	if s.Isolated == nil {
		return RiskParameters{}, false
	}
	rp, ok := s.Isolated[marketIdx]
	return rp, ok
}
