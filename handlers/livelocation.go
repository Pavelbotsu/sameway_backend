package handlers

import (
	"sync"
	"time"

	"github.com/Pavelbotsu/sameway_backend/config"
)

// Live-location relay throttle. Both parties stream their position over the
// existing WebSocket once a ride is active; we cap the relay rate per ride so a
// chatty client can't spam its counterpart. In-process only (single instance) —
// move to Redis if we scale out. Teardown is implicit: the relay query filters
// on status, so once a ride leaves accepted/in_progress no events are sent.
var (
	liveLocMu   sync.Mutex
	liveLocLast = map[string]time.Time{} // key: ride_request id
)

// shouldRelayLiveLoc reports whether enough time has passed since the last relay
// for this ride to send another. Throttle window = config.C.MatchLiveLocS.
func shouldRelayLiveLoc(requestID string) bool {
	window := time.Duration(config.C.MatchLiveLocS * float64(time.Second))
	liveLocMu.Lock()
	defer liveLocMu.Unlock()
	now := time.Now()
	if last, ok := liveLocLast[requestID]; ok && now.Sub(last) < window {
		return false
	}
	liveLocLast[requestID] = now
	return true
}
