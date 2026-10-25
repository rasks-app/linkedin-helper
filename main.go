// rasks-linkedin-helper - reads one person's LinkedIn inbox on their own Mac.
// Copyright (C) 2026 Rasks
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <https://www.gnu.org/licenses/>.

// The helper hosts mautrix-linkedin's LinkedIn client (linkedingo, unmodified)
// with no Matrix server, behind a small read-only API on 127.0.0.1:
//
//	GET /status                         who is signed in, and the realtime state
//	GET /conversations?before=<ms>      the primary inbox, 20 at a time, with unread state
//	GET /conversations/{urn}/messages   a thread's messages, newest last (?before=<ms>&count=)
//	GET /events                         Server-Sent Events: each new message as it lands
//
// It never sends, reacts, marks read, or looks up a profile: there is no route
// that could, and the only LinkedIn calls it makes are the reads above plus the
// realtime connection the web app itself keeps open (with its one heartbeat a
// minute). Every request needs the bearer token printed on start.
//
// HTTP/1.1 only: over Go's HTTP/2, LinkedIn left the inbox reads hanging for
// minutes and the realtime connect for 30 s; over HTTP/1.1 each answers in ~150 ms.

//go:debug http2client=0

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-linkedin/pkg/linkedingo"
)

// Session is what the sign-in window saves: the browser's cookies for
// www.linkedin.com, the X-LI headers its own requests carried, and its exact
// browser headers, so every request looks like the browser that signed in.
type Session struct {
	Cookies      json.RawMessage   `json:"cookies"`
	XLITrack     string            `json:"xLiTrack"`
	PageInstance string            `json:"pageInstance"`
	Headers      map[string]string `json:"headers"`
	SyncToken    string            `json:"syncToken,omitempty"`
}

type helper struct {
	log     zerolog.Logger
	path    string
	session Session
	jar     *linkedingo.StringCookieJar
	cli     *linkedingo.Client
	me      linkedingo.URN
	name    string
	token   string

	mu        sync.Mutex
	subs      map[chan []byte]struct{}
	state     string
	stateErr  string
	lastEvent time.Time
	counts    map[string]int

	peopleMu sync.Mutex
	people   map[string]person
}

func main() {
	sessionPath := flag.String("session", "session.json", "the session the sign-in window saved")
	addr := flag.String("addr", "127.0.0.1:0", "where to listen; port 0 picks a free one")
	verbose := flag.Bool("v", false, "log LinkedIn requests")
	flag.Parse()

	level := zerolog.InfoLevel
	if *verbose {
		level = zerolog.DebugLevel
	}
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.TimeOnly}).
		Level(level).With().Timestamp().Logger()

	h := &helper{log: log, path: *sessionPath, subs: map[chan []byte]struct{}{}, counts: map[string]int{}, people: map[string]person{}, state: "starting"}
	if err := h.load(); err != nil {
		log.Fatal().Err(err).Msg("Couldn't read the session")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = log.WithContext(ctx)

	if err := h.start(ctx); err != nil {
		log.Fatal().Err(err).Msg("Couldn't start")
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal().Err(err).Msg("Couldn't listen")
	}
	h.token = randomToken()
	// The first line on stdout is how the Mac app finds the helper.
	ready, _ := json.Marshal(map[string]any{"ready": true, "url": "http://" + ln.Addr().String(), "token": h.token, "me": h.name})
	fmt.Println(string(ready))

	srv := &http.Server{Handler: h.routes(), BaseContext: func(net.Listener) context.Context { return ctx }}
	go func() {
		<-ctx.Done()
		h.cli.RealtimeDisconnect()
		h.save()
		shut, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shut)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal().Err(err).Msg("Server stopped")
	}
}

func (h *helper) load() error {
	data, err := os.ReadFile(h.path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, &h.session); err != nil {
		return err
	}
	h.jar = linkedingo.NewEmptyStringCookieJar()
	if err := h.jar.UnmarshalJSON(h.session.Cookies); err != nil {
		return fmt.Errorf("cookies: %w", err)
	}
	if h.jar.GetCookie(linkedingo.LinkedInCookieJSESSIONID) == "" {
		return errors.New("the session has no JSESSIONID cookie: sign in again")
	}
	return nil
}

// save keeps the cookies LinkedIn rotated and the sync token for the next start.
func (h *helper) save() {
	h.mu.Lock()
	defer h.mu.Unlock()
	cookies, err := h.jar.MarshalJSON()
	if err != nil {
		return
	}
	h.session.Cookies = cookies
	data, _ := json.MarshalIndent(h.session, "", "  ")
	if err := os.WriteFile(h.path, data, 0o600); err != nil {
		h.log.Warn().Err(err).Msg("Couldn't save the session")
	}
}

func (h *helper) headers() http.Header {
	out := http.Header{}
	for k, v := range h.session.Headers {
		out.Set(k, v)
	}
	return out
}

func (h *helper) start(ctx context.Context) error {
	// Who is signed in: the one call the web app makes on every page load too.
	probe := linkedingo.NewClient(ctx, linkedingo.NewURN(""), h.jar, h.session.PageInstance, h.session.XLITrack, "", linkedingo.Handlers{})
	probe.SetBrowserHeaders(h.headers())
	profile, err := probe.GetCurrentUserProfile(ctx)
	if err != nil {
		return fmt.Errorf("signed-in check: %w", err)
	}
	if profile.MiniProfile.EntityURN.ID() == "" {
		return errors.New("LinkedIn didn't say who is signed in: the cookies are bad")
	}
	h.me = linkedingo.NewURN(profile.MiniProfile.EntityURN.ID())
	h.name = strings.TrimSpace(profile.MiniProfile.FirstName + " " + profile.MiniProfile.LastName)
	h.log.Info().Str("me", h.name).Msg("Signed in")

	h.cli = linkedingo.NewClient(ctx, h.me, h.jar, h.session.PageInstance, h.session.XLITrack, h.session.SyncToken, linkedingo.Handlers{
		Heartbeat: func(context.Context) { h.setState("connected", "") },
		ClientConnection: func(ctx context.Context, _ *linkedingo.ClientConnection) {
			h.setState("connected", "")
			// After a reconnect, catch up on what changed while the stream was down:
			// driven by the connection, never by a clock.
			go h.catchUp(ctx)
		},
		TransientDisconnect: func(_ context.Context, err error) { h.setState("reconnecting", err.Error()) },
		BadCredentials: func(_ context.Context, err error) {
			// LinkedIn signed the session out (or sent it to a checkpoint). Stop and say so.
			h.setState("signed_out", err.Error())
			h.publish("signed_out", map[string]any{"error": err.Error()})
		},
		UnknownError: func(_ context.Context, err error) {
			h.setState("failed", err.Error())
			h.publish("failed", map[string]any{"error": err.Error()})
		},
		DecoratedEvent: h.onEvent,
		ConversationsSyncToken: func(_ context.Context, token string) {
			h.mu.Lock()
			h.session.SyncToken = token
			h.mu.Unlock()
			go h.save()
		},
	})
	h.cli.SetBrowserHeaders(h.headers())
	return h.cli.RealtimeConnect(ctx)
}

func (h *helper) catchUp(ctx context.Context) {
	resp, err := h.cli.GetConversationsBySyncToken(ctx)
	if err != nil {
		h.log.Warn().Err(err).Msg("Couldn't catch up")
		return
	}
	for _, conv := range resp.Elements {
		h.publish("conversation", h.conversation(conv))
	}
}

func (h *helper) setState(state, why string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state != state {
		h.log.Info().Str("state", state).Str("why", why).Msg("Realtime")
	}
	h.state, h.stateErr = state, why
}

func (h *helper) onEvent(_ context.Context, ev *linkedingo.DecoratedEvent) {
	data := ev.Payload.Data
	h.mu.Lock()
	h.lastEvent = time.Now()
	h.counts[data.Type]++
	h.mu.Unlock()
	switch {
	case data.DecoratedMessage != nil:
		msg := data.DecoratedMessage.Result
		h.publish("message", map[string]any{
			"conversation": msg.Conversation.EntityURN.String(),
			"message":      h.message(msg),
			"leftServerAt": ev.LeftServerAt.UnixMilli(),
		})
	case data.DecoratedConversation != nil:
		h.publish("conversation", h.conversation(data.DecoratedConversation.Result))
	case data.DecoratedSeenReceipt != nil:
		h.publish("seen", map[string]any{"topic": ev.Topic.String()})
	}
}

func (h *helper) publish(kind string, body any) {
	line, _ := json.Marshal(map[string]any{"type": kind, "at": time.Now().UnixMilli(), "data": body})
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- line:
		default: // a slow reader misses events rather than holding the stream
		}
	}
}

// The shapes the API returns: what a Rasks chat source needs, nothing more.

type person struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Headline string `json:"headline,omitempty"`
	Photo    string `json:"photo,omitempty"`
	URL      string `json:"url,omitempty"`
	Me       bool   `json:"me,omitempty"`
}

type message struct {
	ID       string   `json:"id"`
	From     person   `json:"from"`
	Text     string   `json:"text"`
	At       int64    `json:"at"`
	Format   string   `json:"format,omitempty"`
	Attached []string `json:"attached,omitempty"`
}

type conversation struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Group        bool     `json:"group"`
	Unread       bool     `json:"unread"`
	LastActivity int64    `json:"lastActivity"`
	LastRead     int64    `json:"lastRead,omitempty"`
	People       []person `json:"people"`
	Last         *message `json:"last,omitempty"`
	Categories   []string `json:"categories,omitempty"`
}

func (h *helper) person(p linkedingo.MessagingParticipant) person {
	out := person{ID: p.EntityURN.ID(), Me: p.EntityURN.ID() == h.me.ID()}
	switch {
	case p.ParticipantType.Member != nil:
		m := p.ParticipantType.Member
		out.Name = strings.TrimSpace(m.FirstName.Text + " " + m.LastName.Text)
		out.Headline = m.Headline.Text
		out.URL = m.ProfileURL
		if m.ProfilePicture != nil {
			out.Photo = m.ProfilePicture.GetLargestArtifactURL()
		}
	case p.ParticipantType.Organization != nil:
		o := p.ParticipantType.Organization
		out.Name = o.Name.Text
		out.URL = o.PageURL
		if o.Logo != nil {
			out.Photo = o.Logo.GetLargestArtifactURL()
		}
	}
	// A message names its sender by id alone; the conversation's participants
	// carry the name, so remember each one seen and fill the rest from it.
	h.peopleMu.Lock()
	defer h.peopleMu.Unlock()
	if out.Name != "" {
		h.people[out.ID] = out
	} else if known, ok := h.people[out.ID]; ok {
		known.Me = out.Me
		return known
	}
	return out
}

func (h *helper) message(m linkedingo.Message) message {
	out := message{
		ID:     m.EntityURN.String(),
		From:   h.person(m.Sender),
		Text:   m.Body.Text,
		At:     m.DeliveredAt.UnixMilli(),
		Format: string(m.MessageBodyRenderFormat),
	}
	for _, rc := range m.RenderContent {
		switch {
		case rc.File != nil:
			out.Attached = append(out.Attached, "file")
		case rc.VectorImage != nil:
			out.Attached = append(out.Attached, "image")
		case rc.Video != nil:
			out.Attached = append(out.Attached, "video")
		case rc.Audio != nil:
			out.Attached = append(out.Attached, "audio")
		case rc.ExternalMedia != nil:
			out.Attached = append(out.Attached, "gif")
		case rc.RepliedMessageContent != nil:
			out.Attached = append(out.Attached, "reply")
		case rc.ForwardedMessage != nil:
			out.Attached = append(out.Attached, "forward")
		case rc.HostURNData != nil:
			out.Attached = append(out.Attached, "post")
		}
	}
	return out
}

func (h *helper) conversation(c linkedingo.Conversation) conversation {
	out := conversation{
		ID:           c.EntityURN.String(),
		Title:        c.Title,
		Group:        c.GroupChat,
		Unread:       !c.Read,
		LastActivity: c.LastActivityAt.UnixMilli(),
		LastRead:     c.LastReadAt.UnixMilli(),
		Categories:   c.Categories,
	}
	var names []string
	for _, p := range c.ConversationParticipants {
		pp := h.person(p)
		out.People = append(out.People, pp)
		if !pp.Me {
			names = append(names, pp.Name)
		}
	}
	if out.Title == "" {
		out.Title = strings.Join(names, ", ")
	}
	if n := len(c.Messages.Elements); n > 0 {
		last := h.message(c.Messages.Elements[n-1])
		for _, m := range c.Messages.Elements {
			if m.DeliveredAt.UnixMilli() > last.At {
				last = h.message(m)
			}
		}
		out.Last = &last
	}
	return out
}

func (h *helper) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		writeJSON(w, map[string]any{
			"me": h.name, "state": h.state, "error": h.stateErr,
			"lastEvent": h.lastEvent.UnixMilli(), "events": h.counts, "listeners": len(h.subs),
		})
	})
	mux.HandleFunc("GET /conversations", func(w http.ResponseWriter, r *http.Request) {
		before := time.Now()
		if ms, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); err == nil {
			before = time.UnixMilli(ms)
		}
		resp, err := h.cli.GetConversationsUpdatedBefore(r.Context(), before)
		if err != nil {
			fail(w, err)
			return
		}
		out := []conversation{}
		if resp != nil {
			for _, c := range resp.Elements {
				out = append(out, h.conversation(c))
			}
		}
		writeJSON(w, map[string]any{"conversations": out})
	})
	mux.HandleFunc("GET /conversations/{urn}/messages", func(w http.ResponseWriter, r *http.Request) {
		before := time.Now()
		if ms, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); err == nil {
			before = time.UnixMilli(ms)
		}
		count := 20
		if n, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil && n > 0 && n <= 50 {
			count = n
		}
		resp, err := h.cli.GetMessagesBefore(r.Context(), linkedingo.NewURN(r.PathValue("urn")), before, count)
		if err != nil {
			fail(w, err)
			return
		}
		out := []message{}
		if resp != nil {
			for _, m := range resp.Elements {
				out = append(out, h.message(m))
			}
		}
		writeJSON(w, map[string]any{"messages": out})
	})
	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no streaming", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch := make(chan []byte, 64)
		h.mu.Lock()
		h.subs[ch] = struct{}{}
		h.mu.Unlock()
		defer func() {
			h.mu.Lock()
			delete(h.subs, ch)
			h.mu.Unlock()
		}()
		fmt.Fprint(w, ": connected\n\n")
		flusher.Flush()
		for {
			select {
			case <-r.Context().Done():
				return
			case line := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", line)
				flusher.Flush()
			}
		}
	})
	return h.auth(mux)
}

// auth lets in only the Mac app: the token, and no browser page on another origin.
func (h *helper) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+h.token || r.Header.Get("Origin") != "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var re *linkedingo.ResponseError
	if errors.As(err, &re) && re.StatusCode == http.StatusUnauthorized || errors.Is(err, linkedingo.ErrTokenInvalidated) {
		status = http.StatusUnauthorized
	}
	http.Error(w, err.Error(), status)
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
