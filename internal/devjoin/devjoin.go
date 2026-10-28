// Package devjoin is the dev join's client side: the app, the tools and the
// bots ask a dev server (HUDDLE_DEV=1) to let a name into a room, and join
// the meeting and its media with what it answers. Stage 3's signing in
// replaces it. Pure.
package devjoin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/anthonyrego/huddle/internal/rtc"
	"github.com/anthonyrego/huddle/internal/wire"
)

// Join asks server (http://host:port) to let req.Name into req.Room. A
// server that can't be reached gives http.Client's *url.Error; one that
// refuses gives "join: <status>: <its message>".
func Join(ctx context.Context, server string, req wire.DevJoin) (wire.Joined, error) {
	var j wire.Joined
	body, _ := json.Marshal(req)
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(server, "/")+"/dev/join", bytes.NewReader(body))
	if err != nil {
		return j, err
	}
	r.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		return j, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4<<10))
		var e wire.Error
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
		}
		return j, fmt.Errorf("join: %s: %s", res.Status, e.Message)
	}
	if err := json.NewDecoder(res.Body).Decode(&j); err != nil {
		return j, fmt.Errorf("join: %w", err)
	}
	return j, nil
}

// RTC is the media side of a join's answer: which media server (an answer
// without Media is from a server older than the field, which means
// LiveKit), its URL (MediaURL, or LiveKitURL from such a server), the token
// and the identity.
func RTC(j wire.Joined) rtc.Join {
	r := rtc.Join{Kind: j.Media, URL: j.MediaURL, Token: j.Token, Identity: j.Identity}
	if r.Kind == "" {
		r.Kind = wire.MediaLiveKit
	}
	if r.URL == "" && r.Kind == wire.MediaLiveKit {
		r.URL = j.LiveKitURL
	}
	return r
}
