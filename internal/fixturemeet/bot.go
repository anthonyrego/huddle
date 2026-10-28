package fixturemeet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/anthonyrego/huddle/internal/devjoin"
	"github.com/anthonyrego/huddle/internal/wire"
)

// Bot is a fixture participant: a name and the track it plays.
type Bot struct {
	Name string
	Ogg  string

	mic Mic
	t   *Track
}

// Connect joins the bot to the room through the dev server, as a bot (the
// echo bot leaves it alone), and publishes its (still quiet) microphone
// on the media server the join names.
func (b *Bot) Connect(ctx context.Context, server, room string) error {
	t, err := ReadTrack(b.Ogg)
	if err != nil {
		return fmt.Errorf("%w (make fixtures renders it)", err)
	}
	b.t = t
	j, err := devjoin.Join(ctx, server, wire.DevJoin{Room: room, Name: b.Name, Bot: true})
	if err != nil {
		return err
	}
	if b.mic, err = JoinMic(ctx, devjoin.RTC(j), "microphone"); err != nil {
		return fmt.Errorf("%s: %w", b.Name, err)
	}
	return nil
}

// Play sends the track in real time from t0 until it ends or ctx does.
func (b *Bot) Play(ctx context.Context, t0 time.Time) error {
	return b.t.Play(ctx, b.mic, t0, false)
}

// Close leaves the room.
func (b *Bot) Close() {
	if b.mic != nil {
		b.mic.Close()
	}
}

// PlayAll joins every bot, then plays them together from a moment just
// after the last has joined, which it reports with started.
func PlayAll(ctx context.Context, server, room string, bots []*Bot, started func(t0 time.Time)) error {
	for _, b := range bots {
		if err := b.Connect(ctx, server, room); err != nil {
			for _, o := range bots {
				o.Close()
			}
			return err
		}
	}
	defer func() {
		for _, b := range bots {
			b.Close()
		}
	}()
	// Let subscriptions settle before the first word.
	t0 := time.Now().Add(1500 * time.Millisecond)
	if started != nil {
		started(t0)
	}
	var wg sync.WaitGroup
	errs := make([]error, len(bots))
	for i, b := range bots {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = b.Play(ctx, t0) }()
	}
	wg.Wait()
	return errors.Join(errs...)
}
