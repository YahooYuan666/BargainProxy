package pool

import "time"

// Wall-clock deadlines (BP-327, magpie 8769a58 / plugins#1126).
//
// # WHY
//
// time.Now().Add(d) keeps the runtime's MONOTONIC reading alongside the wall
// clock, and monotonic readings STOP while the machine is suspended. Every
// rest/hold deadline in this package was built that way, so a laptop that
// slept through a cooldown woke up with the wall still in the future by
// however long it slept:
//
//	00:10  account rested 30 minutes  →  wall = 00:40
//	00:15  machine suspends
//	09:00  machine resumes            →  00:40 - now reads as still ahead by
//	                                      the sleep duration (hours), so the
//	                                      account stays out of routing
//
// The upstream hit exactly this (a quota wall shown as expiring at 00:40 was
// still in force at 09:57). Our case is worse on paper: this gateway runs as a
// desktop service on a laptop, and one of its walls is SEVEN DAYS long (the
// 11140 restriction rest). A single overnight suspend pushes that wall out by
// another eight hours, every night.
//
// # WHAT
//
// onWall strips the monotonic reading by round-tripping through time.Date,
// which constructs a pure wall-clock instant. Deadlines are then compared
// against a clock that also advances while suspended, which is the only
// definition a user can verify against the panel's "until" column.
//
// Deadlines that came from the UPSTREAM (a parsed reset timestamp, a
// Retry-After instant) are already wall-only and pass through unchanged.
func onWall(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	y, mo, d := t.Date()
	h, mi, s := t.Clock()
	return time.Date(y, mo, d, h, mi, s, t.Nanosecond(), t.Location())
}
