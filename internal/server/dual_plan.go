package server

// Scheduling policy adapted from t508708/icloud-mail, commit 886eb52:
// internal/domain/apple_creation_policy.go and internal/autocreate/manager.go.
// Attribution and original license scope: internal/icloudmail/{LICENSE,NOTICE.md}.
import (
	"math/rand/v2"
	"time"
)

const dualSlots = 23
const dualMinimumInterval = 2 * time.Minute
const dualCycle = time.Hour
const dualDailyLimit = 24 * dualSlots
const dualOverdueGrace = 2 * time.Minute

// Exactly the upstream distribution: zero-based Web slots 5,11,17,22.
func dualSlotChannel(slot int) string {
	if (slot+1)*4/dualSlots > slot*4/dualSlots {
		return channelWeb
	}
	return channelApple
}
func dualWebOnlySlot(slot int) bool { return (slot+1)*5/dualSlots > slot*5/dualSlots }

// Each gap starts at 120s; randomly distribute the remaining 840 seconds.
// First execution is delayed too; the last deadline is exactly anchor + 1h.
func makeDualPlan(anchor time.Time, random func(int) int) []time.Time {
	if random == nil {
		random = rand.IntN
	}
	gaps := make([]time.Duration, dualSlots)
	for i := range gaps {
		gaps[i] = dualMinimumInterval
	}
	for i := 0; i < int((dualCycle-dualMinimumInterval*dualSlots)/time.Second); i++ {
		gaps[random(dualSlots)] += time.Second
	}
	plan := make([]time.Time, dualSlots)
	next := anchor.UTC()
	for i, gap := range gaps {
		next = next.Add(gap)
		plan[i] = next
	}
	return plan
}

type dualAttempt struct {
	At      time.Time `json:"at"`
	Channel string    `json:"channel"`
}
type dualScheduleRecord struct {
	Request        aliasBatchRequest    `json:"request"`
	State          aliasBatchStatus     `json:"state"`
	Plan           []time.Time          `json:"plan"`
	Slot           int                  `json:"slot"`
	Attempts       []dualAttempt        `json:"attempts,omitempty"`
	Paused         map[string]time.Time `json:"paused,omitempty"`
	LastAttempt    time.Time            `json:"last_attempt,omitempty"`
	InFlight       bool                 `json:"in_flight"`
	ChannelCreated map[string]int       `json:"channel_created"`
	WebRejected    bool                 `json:"web_rejected"`
	WebRevision    string               `json:"web_revision,omitempty"`
	AppleRejected  bool                 `json:"apple_rejected"`
	AppleRevision  string               `json:"apple_revision,omitempty"`
}

// Rolling limits count attempts, including rejected attempts, not successes.
// This ledger survives task replacement and restarts.
func (r *dualScheduleRecord) budgetUntil(now time.Time, channel string, webOnly bool) (global, scoped time.Time) {
	global = r.LastAttempt.Add(dualMinimumInterval)
	if r.Paused["all"].After(global) {
		global = r.Paused["all"]
	}
	scoped = r.Paused[channel]
	var hourly, daily, perChannel []time.Time
	for _, attempt := range r.Attempts {
		if attempt.At.After(now.Add(-24 * time.Hour)) {
			daily = append(daily, attempt.At)
		}
		if attempt.At.After(now.Add(-time.Hour)) {
			hourly = append(hourly, attempt.At)
			if attempt.Channel == channel {
				perChannel = append(perChannel, attempt.At)
			}
		}
	}
	if len(hourly) >= dualSlots {
		global = laterDualTime(global, hourly[len(hourly)-dualSlots].Add(time.Hour))
	}
	if len(daily) >= dualDailyLimit {
		global = laterDualTime(global, daily[len(daily)-dualDailyLimit].Add(24*time.Hour))
	}
	limit := 19
	if channel == channelWeb {
		limit = 4
		if webOnly {
			limit = 5
		}
	}
	if len(perChannel) >= limit {
		scoped = laterDualTime(scoped, perChannel[len(perChannel)-limit].Add(time.Hour))
	}
	return
}
func laterDualTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
func (r *dualScheduleRecord) replan(anchor time.Time, exactFirst bool) {
	r.Plan = makeDualPlan(anchor, nil)
	r.Slot = 0
	if exactFirst {
		r.Plan[0] = anchor.UTC()
	}
}
func (r *dualScheduleRecord) advance(now time.Time) {
	expected := r.Plan[r.Slot]
	r.Slot++
	if r.Slot == len(r.Plan) {
		r.replan(laterDualTime(expected, now), false)
		return
	}
	if earliest := now.Add(dualMinimumInterval); r.Plan[r.Slot].Before(earliest) {
		shift := earliest.Sub(r.Plan[r.Slot])
		for i := r.Slot; i < len(r.Plan); i++ {
			r.Plan[i] = r.Plan[i].Add(shift)
		}
	}
}
