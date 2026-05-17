package types

// PreRiskSnapshot captures the pre-state risk envelope at the start
// of a state-mutating handler so IsValidRiskChangeFrom can later
// prove the post-state did not regress.
//
// Snapshots are values that live for exactly one handler; pre-state
// MUST NOT outlive its producer or a later tx would compare against
// an unrelated previous snapshot.
//
//   - Cross: nil → treat as "no pre-state"; post must be HEALTHY.
//   - Isolated: keyed by marketIdx, populated only for non-zero
//     isolated positions at snapshot time. Markets opened during the
//     handler are intentionally absent so the same fallback kicks in.
type PreRiskSnapshot struct {
	Cross    *RiskParameters
	Isolated map[uint32]RiskParameters
}

// IsolatedFor returns the pre-state isolated RP for marketIdx and a
// flag — false drives the "missing pre" fallback.
func (s PreRiskSnapshot) IsolatedFor(marketIdx uint32) (RiskParameters, bool) {
	if s.Isolated == nil {
		return RiskParameters{}, false
	}
	rp, ok := s.Isolated[marketIdx]
	return rp, ok
}
