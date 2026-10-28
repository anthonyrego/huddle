package sfu

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/pion/webrtc/v4"

	"github.com/anthonyrego/huddle/internal/server/rtctoken"
	"github.com/anthonyrego/huddle/internal/wire/signal"
)

// Signalling on /rtc.
const (
	joinWait   = 10 * time.Second // for the first frame, the join
	writeWait  = 10 * time.Second
	pingEvery  = 15 * time.Second // a client that doesn't answer a ping in writeWait is gone
	sendBuffer = 256              // messages queued for a client before it's dropped as too slow
	maxFrame   = 1 << 20          // what a client may send in one frame: an SDP with every track in a room
	closeWait  = 5 * time.Second  // for sessions to end when the server closes
)

// Config is what the SFU needs.
type Config struct {
	// Network is where media goes: one UDP port (the HTTP port's number)
	// and the address offered on it.
	Network Network
	// Tokens checks the token each join carries; the dev join mints them
	// with the same secret.
	Tokens rtctoken.Minter
	// Log takes the SFU's lines (sfu: …); nil is slog.Default().
	Log *slog.Logger
}

// Server is the SFU: rooms of people, each with a connection to publish on
// and one to receive on, all on one UDP port. ServeHTTP is its signalling,
// the /rtc WebSocket.
type Server struct {
	cfg      Config
	log      *slog.Logger
	net      *network
	upAPI    *webrtc.API
	downAPI  *webrtc.API
	now      func() time.Time // the clock tokens are checked against (tests move it)
	closedCh chan struct{}

	mu     sync.Mutex
	rooms  map[string]*room
	closed bool
}

// New opens the media port and makes the server.
func New(cfg Config) (*Server, error) {
	if len(cfg.Tokens.Secret) == 0 {
		return nil, errors.New("sfu: no token secret")
	}
	log := cfg.Log
	if log == nil {
		log = slog.Default()
	}
	n, err := listen(cfg.Network)
	if err != nil {
		return nil, fmt.Errorf("sfu: %w", err)
	}
	upAPI, err := newAPI(n, up)
	if err != nil {
		n.close()
		return nil, fmt.Errorf("sfu: %w", err)
	}
	downAPI, err := newAPI(n, down)
	if err != nil {
		n.close()
		return nil, fmt.Errorf("sfu: %w", err)
	}
	s := &Server{cfg: cfg, log: log, net: n, upAPI: upAPI, downAPI: downAPI, now: time.Now,
		closedCh: make(chan struct{}), rooms: map[string]*room{}}
	log.Info("sfu: media", "addrs", fmt.Sprint(n.addrs()), "ip", cfg.Network.IP, "external", cfg.Network.External)
	return s, nil
}

// Addrs are the addresses the media port listens on.
func (s *Server) Addrs() []net.Addr { return s.net.addrs() }

// Close ends every session (each hears bye) and closes the media port.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	close(s.closedCh)
	rooms := make([]*room, 0, len(s.rooms))
	for _, r := range s.rooms {
		rooms = append(rooms, r)
	}
	s.rooms = map[string]*room{}
	s.mu.Unlock()
	var ps []*participant
	for _, r := range rooms {
		ps = append(ps, r.close()...)
	}
	for _, p := range ps {
		p.kick("the server is closing")
	}
	deadline := time.After(closeWait)
	for _, p := range ps {
		select {
		case <-p.done:
		case <-deadline:
			s.log.Warn("sfu: sessions still closing", "room", p.grant.Room, "identity", p.grant.Identity)
			return s.net.close()
		}
	}
	return s.net.close()
}

// ServeHTTP is /rtc: it upgrades the request, takes the join, and runs the
// session until the client leaves or is dropped.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return // Accept has answered
	}
	conn.SetReadLimit(maxFrame)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	g, all, err := s.readJoin(ctx, conn)
	if err != nil {
		s.log.Info("sfu: refused", "err", err)
		s.refuse(ctx, conn, err)
		return
	}
	p, err := newParticipant(s, conn, g, all, cancel)
	if err != nil {
		s.log.Error("sfu: a session's connections", "room", g.Room, "identity", g.Identity, "err", err)
		s.refuse(ctx, conn, errors.New("the server couldn't make the connections"))
		return
	}
	go p.writeLoop(ctx)
	go p.pingLoop(ctx)
	if !s.enter(p) {
		p.kick("the server is closing")
		p.readLoop(ctx)
		p.finish()
		return
	}
	p.readLoop(ctx)
	p.finish()
}

// readJoin reads the first frame and checks it's a join this server takes.
func (s *Server) readJoin(ctx context.Context, conn *websocket.Conn) (rtctoken.Grant, bool, error) {
	jctx, cancel := context.WithTimeout(ctx, joinWait)
	defer cancel()
	_, data, err := conn.Read(jctx)
	if err != nil {
		return rtctoken.Grant{}, false, fmt.Errorf("no join: %w", err)
	}
	e, err := decode(data)
	if err != nil {
		return rtctoken.Grant{}, false, err
	}
	if e.Kind != signal.KindJoin {
		return rtctoken.Grant{}, false, fmt.Errorf("first frame is %q, want join", e.Kind)
	}
	var j signal.Join
	if err := e.Decode(&j); err != nil {
		return rtctoken.Grant{}, false, err
	}
	if j.Protocol != signal.Protocol {
		return rtctoken.Grant{}, false, fmt.Errorf("this server speaks protocol %d and the client %d: update the app", signal.Protocol, j.Protocol)
	}
	var all bool
	switch j.Subscribe {
	case "", signal.SubscribeAll:
		all = true
	case signal.SubscribeNone:
	default:
		return rtctoken.Grant{}, false, fmt.Errorf("subscribe %q: want %q or %q", j.Subscribe, signal.SubscribeAll, signal.SubscribeNone)
	}
	g, err := s.cfg.Tokens.Verify(j.Token, s.now())
	if err != nil {
		return rtctoken.Grant{}, false, fmt.Errorf("the token: %w", err)
	}
	return g, all, nil
}

// refuse tells the client why, then closes.
func (s *Server) refuse(ctx context.Context, conn *websocket.Conn, why error) {
	if frame, err := signal.Marshal(signal.KindError, signal.Error{Message: why.Error()}); err == nil {
		wctx, cancel := context.WithTimeout(ctx, writeWait)
		conn.Write(wctx, websocket.MessageText, frame)
		cancel()
	}
	conn.Close(websocket.StatusPolicyViolation, "refused")
}

// enter puts p in its room, made if it's the first. False when the server
// is closing.
func (s *Server) enter(p *participant) bool {
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return false
		}
		r := s.rooms[p.grant.Room]
		if r == nil {
			r = newRoom(s, p.grant.Room)
			s.rooms[r.name] = r
		}
		s.mu.Unlock()
		if r.enter(p) {
			return true
		}
		// The room emptied and closed between finding it and entering:
		// make another.
	}
}

// dropRoom forgets r, which has emptied.
func (s *Server) dropRoom(r *room) {
	s.mu.Lock()
	if s.rooms[r.name] == r {
		delete(s.rooms, r.name)
	}
	s.mu.Unlock()
}

// room is the named room, or nil (tests).
func (s *Server) room(name string) *room {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rooms[name]
}
