package pool

// BP-146: escalation bookkeeping for upstream account restrictions.
//
// A single 403/11140 disables that account and nothing else. A SECOND one on
// the same channel inside restrictedChannelWindow means the pattern — not the
// account — is what upstream is reacting to, so the channel is parked briefly
// instead of letting the next request walk the remaining accounts into the
// same wall (Coding2API f428e33: three accounts hit per request, the third
// banned three minutes later).

import "time"

const (
	// restrictedChannelWindow is how close two restrictions must be to count
	// as one pattern. Coding2API's two rounds were 17 minutes apart; an hour
	// apart is two independent accidents.
	restrictedChannelWindow = 30 * time.Minute
	// restrictedChannelPause is how long the channel is parked once the
	// second restriction lands. Shorter than Coding2API's 24h: this gateway's
	// pools are small and the user is present, so a park that outlives the
	// session is worse than the risk it prevents.
	restrictedChannelPause = 2 * time.Hour
)

// noteRestrictedLocked records one restriction and applies the escalation.
// Callers hold p.mu.
func (p *Pool) noteRestrictedLocked(channel string, now time.Time, modelName string) {
	prev, seen := p.lastRestricted[channel]
	p.lastRestricted[channel] = now
	if !seen || now.Sub(prev) > restrictedChannelWindow {
		return // first one in the window: the account alone is the answer
	}
	p.markChannelBusyLocked(channel, modelName, now.Add(restrictedChannelPause))
	delete(p.lastRestricted, channel) // the pause speaks for itself now
}
