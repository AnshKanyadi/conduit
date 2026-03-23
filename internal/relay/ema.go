package relay

// EMA is an exponential moving average with a configurable smoothing factor.
// It tracks a sliding estimate of a value, giving more weight to recent samples.
//
// Why EMA instead of a simple counter?
// A counter treats the 1000th slow frame identically to the 1st. EMA
// naturally forgets old history — a client that was slow but then catches
// up will see its rate estimate recover without any explicit reset logic.
// This prevents over-penalising bursty clients (e.g. a browser pausing JS).
type EMA struct {
	alpha float64 // smoothing factor: 0 < alpha ≤ 1; higher = more reactive to changes
	val   float64 // current estimate
}

// newEMA creates an EMA with the given smoothing factor.
// alpha=0.1: each new sample contributes 10%, the running history 90%.
// Starts at 1.0 (optimistic) so a freshly connected client is not penalised.
func newEMA(alpha float64) EMA {
	return EMA{alpha: alpha, val: 1.0}
}

// Update incorporates a new observation into the moving average.
// Pass 1.0 when a frame was delivered successfully; 0.0 when it was dropped.
func (e *EMA) Update(sample float64) {
	e.val = e.alpha*sample + (1-e.alpha)*e.val
}

// Current returns the latest estimate.
// Values near 1.0 → client is keeping up. Values near 0.0 → client is stalled.
func (e *EMA) Current() float64 {
	return e.val
}
