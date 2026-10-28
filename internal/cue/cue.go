// Package cue holds board ops until they should be seen. Each op carries
// the playback position of the audio that arrived before it, and is
// released once the listener has (nearly) heard that far. Releases are
// spaced out, so a batch of ops, which the model often sends before it
// speaks, draws one box at a time instead of all at once.
//
// Copied from an earlier project of the author's.
package cue

import "time"

// Queue releases values in the order they were pushed.
type Queue[T any] struct {
	Lead int64         // release this many bytes before the position is heard
	Gap  time.Duration // at least this long between releases

	pending []item[T]
	last    time.Time
	hold    time.Time
}

type item[T any] struct {
	at int64
	v  T
}

// Push queues v to be seen once playback reaches at.
func (q *Queue[T]) Push(at int64, v T) { q.pending = append(q.pending, item[T]{at, v}) }

// Len is how many values are waiting.
func (q *Queue[T]) Len() int { return len(q.pending) }

// Next returns the next value whose position has been reached, if the gap
// since the last release has passed. played is how far the listener has
// heard; drained says playback has nothing queued, so everything already
// received has been heard (or was never audio).
func (q *Queue[T]) Next(now time.Time, played int64, drained bool) (T, bool) {
	var zero T
	if len(q.pending) == 0 || now.Before(q.hold) || now.Sub(q.last) < q.Gap {
		return zero, false
	}
	it := q.pending[0]
	if !drained && played+q.Lead < it.at {
		return zero, false
	}
	q.pending = q.pending[1:]
	q.last = now
	return it.v, true
}

// HoldUntil releases nothing before t: after a barge-in, a cancellation for
// ops already queued may still be on its way.
func (q *Queue[T]) HoldUntil(t time.Time) { q.hold = t }

// Drop removes and returns the waiting values f matches.
func (q *Queue[T]) Drop(f func(T) bool) []T {
	var out []T
	keep := q.pending[:0]
	for _, it := range q.pending {
		if f(it.v) {
			out = append(out, it.v)
		} else {
			keep = append(keep, it)
		}
	}
	q.pending = keep
	return out
}
