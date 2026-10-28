// Package httpapi is the server's HTTP surface: health, the dev join, the
// meeting WebSocket and the media server's signalling. Stage 3 adds signing
// in, meetings and invites; until then the server only runs in dev mode.
package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/anthonyrego/huddle/internal/server/lktoken"
	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/wire"
)

// Rooms is what the API needs from the meetings store.
type Rooms interface {
	// EnsureRoom makes the room's meeting exist.
	EnsureRoom(ctx context.Context, room string) error
}

// Server answers the app.
type Server struct {
	Rooms Rooms
	// Media is the media server the dev join sends people to:
	// wire.MediaLiveKit (when empty) or wire.MediaBuiltin.
	Media string
	// Minter and LiveKitURL are LiveKit's: tokens signed with its API key,
	// and its URL as apps reach it (ws://host:7880).
	Minter     lktoken.Minter
	LiveKitURL string
	// RTCMinter signs tokens for huddled's own SFU (builtin), whose
	// signalling RTC serves at GET /rtc when set.
	RTCMinter rtctoken.Minter
	RTC       http.Handler
	// WS serves the meeting WebSocket.
	WS http.Handler
	// Dev allows /dev/join: anyone who can reach the server joins any room
	// under any name. Never on a public server.
	Dev bool
}

// Handler routes the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok\n"))
	})
	if s.Dev {
		mux.HandleFunc("POST /dev/join", s.devJoin)
	}
	if s.WS != nil {
		mux.Handle("GET /ws", s.WS)
	}
	if s.RTC != nil {
		mux.Handle("GET /rtc", s.RTC)
	}
	return mux
}

const maxBody = 16 << 10

func (s *Server) devJoin(w http.ResponseWriter, r *http.Request) {
	var req wire.DevJoin
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "body isn't a join: "+err.Error())
		return
	}
	room := slug(req.Room)
	name := strings.TrimSpace(req.Name)
	if room == "" || name == "" {
		httpError(w, http.StatusBadRequest, "a join needs a room and a name")
		return
	}
	if err := s.Rooms.EnsureRoom(r.Context(), room); err != nil {
		slog.Error("api: ensure room", "room", room, "err", err)
		httpError(w, http.StatusInternalServerError, "couldn't open the room")
		return
	}
	identity := slug(name) + "-" + randomHex(2)
	if req.Bot {
		identity += wire.BotSuffix
	}
	j := wire.Joined{Room: room, Identity: identity, WSURL: wsURL(r, "/ws")}
	var err error
	switch s.Media {
	case wire.MediaLiveKit, "":
		j.Media, j.MediaURL, j.LiveKitURL = wire.MediaLiveKit, s.LiveKitURL, s.LiveKitURL
		j.Token, err = s.Minter.Mint(lktoken.Grant{Room: room, Identity: identity, Name: name})
	case wire.MediaBuiltin:
		j.Media, j.MediaURL = wire.MediaBuiltin, wsURL(r, "/rtc")
		j.Token, err = s.RTCMinter.Mint(rtctoken.Grant{Room: room, Identity: identity, Name: name})
	default:
		err = fmt.Errorf("no media server %q", s.Media)
	}
	if err != nil {
		slog.Error("api: mint", "media", s.Media, "err", err)
		httpError(w, http.StatusInternalServerError, "couldn't make a media token")
		return
	}
	slog.Info("api: dev join", "room", room, "identity", identity, "media", j.Media)
	writeJSON(w, j)
}

// wsURL is a WebSocket at path on the host the app reached.
func wsURL(r *http.Request, path string) string {
	scheme := "ws"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "wss"
	}
	return scheme + "://" + r.Host + path
}

// slug lowercases s and keeps letters and digits, joining words with
// dashes: "Design review!" → "design-review".
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		default:
			dash = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	return b.String()
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(wire.Error{Message: msg})
}
