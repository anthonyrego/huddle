package opus

import "sync"

// The transpiled libopus hands the addresses of Go locals to its C-style
// callees as uintptrs (hundreds of places, e.g. CELT concealment's
// autocorrelation buffer). Go moves a goroutine's stack when it grows, and
// the garbage collector shrinks idle stacks, so a call that grows the stack
// leaves those addresses pointing at the old copy: libopus then reads
// garbage (seen as a SILK resampler assertion after concealment, only with
// the GC running; GODEBUG=gcshrinkstackoff=1 makes it go away).
//
// So every call into the transpiled code runs on one worker goroutine
// whose stack never moves: it grows its stack once, far beyond what a call
// needs, and waits for work from a frame deep enough (over a quarter of the
// stack in use) that the GC never shrinks it.

const (
	pinDepth    = 640 << 10 // stack in use while the worker waits
	pinHeadroom = 512 << 10 // grown beyond that once, for the calls
	pinFrame    = 4 << 10   // stack each descent step takes
)

var pin struct {
	once sync.Once
	jobs chan pinJob
}

type pinJob struct {
	f    func()
	done chan struct{}
}

// onPinned runs f on the worker and waits for it.
func onPinned(f func()) {
	pin.once.Do(func() {
		pin.jobs = make(chan pinJob)
		go descend(pinDepth/pinFrame, func() {
			descend(pinHeadroom/pinFrame, func() {}) // grow, once
			for j := range pin.jobs {
				j.f()
				close(j.done)
			}
		})
	})
	j := pinJob{f: f, done: make(chan struct{})}
	pin.jobs <- j
	<-j.done
}

// descend takes about n*pinFrame bytes of stack, then calls at.
//
//go:noinline
func descend(n int, at func()) byte {
	var pad [pinFrame - 128]byte
	pad[n%len(pad)] = byte(n)
	if n <= 0 {
		at()
		return pad[0]
	}
	return descend(n-1, at) + pad[(n*7)%len(pad)]
}
