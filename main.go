// rasks-linkedin-helper - reads one person's LinkedIn inbox on their own Mac, and sends the replies they approve.
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
// with no Matrix server, behind a small API on 127.0.0.1:
//
//	GET  /status                         who is signed in, and the realtime state
//	GET  /conversations?before=<ms>      the primary inbox, 20 at a time, with unread state
//	GET  /threads/{thread}/messages      a thread's messages, newest last (?before=<ms>&count=)
//	POST /threads/{thread}/messages      send one text reply the person approved in Rasks
//	GET  /events                         Server-Sent Events: each new message as it lands
//
// Its one write is a text reply in a conversation it already listed: it never
// starts a conversation, sends a file, reacts, marks read, or looks up a
// profile, and there is no route that could. A reply carries the Rasks id it
// was queued under, and a second request with the same id gets the first one's
// answer without sending again. The only other LinkedIn calls are the reads
// above plus the realtime connection the web app itself keeps open (with its
// one heartbeat a minute). Every request needs the bearer token printed on
// start, and a request from a web page (any Origin header) is refused.
//
// The session (the sign-in window's cookies and browser headers) lives in the
// macOS Keychain, never in a file: `save` puts it there from stdin, `forget`
// deletes it, and the server saves the cookies LinkedIn rotates back to it.
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
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-linkedin/pkg/linkedingo"
)

// Exit codes the Mac app reads: signed out means sign in again, never retry.
const (
	exitFailed    = 1
	exitSignedOut = 3
)

// Session is what the sign-in window captured: the browser's cookies for
// www.linkedin.com, the X-LI headers its own requests carried, and its exact
// browser headers, so every request looks like the browser that signed in.
type Session struct {
	Cookies      json.RawMessage   `json:"cookies"`
	XLITrack     string            `json:"xLiTrack"`
	PageInstance string            `json:"pageInstance"`
	Headers      map[string]string `json:"headers"`
	SyncToken    string            `json:"syncToken,omitempty"`
}

// store keeps the session: the Keychain, or a fake in tests.
type store interface {
	Load() (Session, error)
	Save(Session) error
	Forget() error
}

func main() {
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr, TimeFormat: time.TimeOnly}).With().Timestamp().Logger()
	keys := keychainStore{service: keychainService, account: keychainAccount}

	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	switch cmd {
	case "save":
		// The Mac app pipes the sign-in window's session in; it never touches a file.
		if err := saveFrom(os.Stdin, keys); err != nil {
			log.Error().Err(err).Msg("Couldn't save the session")
			os.Exit(exitFailed)
		}
		fmt.Println(`{"saved":true}`)
	case "forget":
		if err := keys.Forget(); err != nil {
			log.Error().Err(err).Msg("Couldn't delete the session")
			os.Exit(exitFailed)
		}
		fmt.Println(`{"forgotten":true}`)
	case "serve":
		serve(log, keys, args)
	case "version":
		fmt.Println(version)
	default:
		fmt.Fprintf(os.Stderr, "usage: %s [serve [-addr 127.0.0.1:0] [-v] | save < session.json | forget | version]\n", os.Args[0])
		os.Exit(2)
	}
}

// version is set at build time (-ldflags "-X main.version=…").
var version = "dev"

// saveFrom reads one session from r and keeps it, once it has the cookies a
// LinkedIn session needs.
func saveFrom(r io.Reader, keys store) error {
	var s Session
	if err := json.NewDecoder(io.LimitReader(r, 1<<20)).Decode(&s); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	jar := linkedingo.NewEmptyStringCookieJar()
	if err := jar.UnmarshalJSON(s.Cookies); err != nil {
		return fmt.Errorf("cookies: %w", err)
	}
	if jar.GetCookie(linkedingo.LinkedInCookieJSESSIONID) == "" {
		return errors.New("the session has no JSESSIONID cookie")
	}
	return keys.Save(s)
}

func serve(log zerolog.Logger, keys store, args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := flags.String("addr", "127.0.0.1:0", "where to listen; port 0 picks a free one")
	verbose := flags.Bool("v", false, "log LinkedIn requests")
	_ = flags.Parse(args)
	if *verbose {
		log = log.Level(zerolog.DebugLevel)
	} else {
		log = log.Level(zerolog.InfoLevel)
	}
	if host, _, err := net.SplitHostPort(*addr); err != nil || (host != "127.0.0.1" && host != "::1") {
		log.Fatal().Str("addr", *addr).Msg("The helper listens on the loopback address only")
	}

	h := newHelper(log, keys)
	if err := h.load(); err != nil {
		h.exit(err, "Couldn't read the session")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx = log.WithContext(ctx)

	if err := h.start(ctx); err != nil {
		h.exit(err, "Couldn't start")
	}

	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatal().Err(err).Msg("Couldn't listen")
	}
	h.token = randomToken()
	// The first line on stdout is how the Mac app finds the helper.
	ready, _ := json.Marshal(map[string]any{"ready": true, "url": "http://" + ln.Addr().String(), "token": h.token, "me": h.name, "version": version})
	fmt.Println(string(ready))

	go h.keepCookies(ctx)
	srv := &http.Server{Handler: h.routes(), BaseContext: func(net.Listener) context.Context { return ctx }, ReadHeaderTimeout: 10 * time.Second}
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

type helper struct {
	log     zerolog.Logger
	keys    store
	session Session
	jar     *linkedingo.StringCookieJar
	cli     *linkedingo.Client
	me      linkedingo.URN
	name    string
	token   string

	mu         sync.Mutex
	subs       map[chan []byte]struct{}
	state      string
	stateErr   string
	lastEvent  time.Time
	counts     map[string]int
	saved      string
	savedToken string

	peopleMu sync.Mutex
	people   map[string]person
	threads  map[string]linkedingo.URN

	// send makes LinkedIn's one write call (the client's SendMessage; tests
	// pass a fake). sendMu runs one send at a time, and sent keeps each Rasks
	// id's answer, so a repeat never sends twice.
	send   func(ctx context.Context, conv linkedingo.URN, text, id string) (linkedingo.Message, error)
	sendMu sync.Mutex
	sent   map[string]sentReply
}

// sentReply is what a send answered: the message LinkedIn made, or why it failed.
type sentReply struct {
	ID  string `json:"id,omitempty"`
	At  int64  `json:"at,omitempty"`
	Err string `json:"-"`
}

func newHelper(log zerolog.Logger, keys store) *helper {
	h := &helper{log: log, keys: keys, subs: map[chan []byte]struct{}{}, counts: map[string]int{}, people: map[string]person{}, threads: map[string]linkedingo.URN{}, sent: map[string]sentReply{}, state: "starting"}
	h.send = func(ctx context.Context, conv linkedingo.URN, text, id string) (linkedingo.Message, error) {
		resp, err := h.cli.SendMessage(ctx, conv, linkedingo.SendMessageBody{Text: text}, nil, id)
		if err != nil {
			return linkedingo.Message{}, err
		}
		return resp.Data, nil
	}
	return h
}

// exit says why it stopped on stdout, as the ready line would, and exits: the
// Mac app asks the person to sign in again on signed_out, and retries otherwise.
func (h *helper) exit(err error, what string) {
	signedOut := errors.Is(err, errNoSession) || isSignedOut(err)
	why := "failed"
	code := exitFailed
	if signedOut {
		why, code = "signed_out", exitSignedOut
	}
	line, _ := json.Marshal(map[string]any{"ready": false, "error": why, "why": err.Error()})
	fmt.Println(string(line))
	h.log.Error().Err(err).Msg(what)
	os.Exit(code)
}

func isSignedOut(err error) bool {
	var re *linkedingo.ResponseError
	return errors.Is(err, linkedingo.ErrTokenInvalidated) || (errors.As(err, &re) && (re.StatusCode == http.StatusUnauthorized || re.StatusCode == http.StatusForbidden))
}

func (h *helper) load() error {
	s, err := h.keys.Load()
	if err != nil {
		return err
	}
	h.session = s
	h.jar = linkedingo.NewEmptyStringCookieJar()
	if err := h.jar.UnmarshalJSON(h.session.Cookies); err != nil {
		return fmt.Errorf("cookies: %w", err)
	}
	if h.jar.GetCookie(linkedingo.LinkedInCookieJSESSIONID) == "" {
		return errNoSession
	}
	h.saved, h.savedToken = string(h.session.Cookies), h.session.SyncToken
	return nil
}

// save keeps the cookies LinkedIn rotated and the sync token for the next start,
// in the Keychain. It writes only when something changed.
func (h *helper) save() {
	h.mu.Lock()
	defer h.mu.Unlock()
	cookies, err := h.jar.MarshalJSON()
	if err != nil {
		return
	}
	if string(cookies) == h.saved && h.session.SyncToken == h.savedToken {
		return
	}
	h.session.Cookies = cookies
	if err := h.keys.Save(h.session); err != nil {
		h.log.Warn().Err(err).Msg("Couldn't save the session")
		return
	}
	h.saved, h.savedToken = string(cookies), h.session.SyncToken
}

// keepCookies saves the cookies once a minute when LinkedIn rotated one: a local
// check of the jar, never a call to LinkedIn.
func (h *helper) keepCookies(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cookies, err := h.jar.MarshalJSON()
			h.mu.Lock()
			changed := err == nil && string(cookies) != h.saved
			h.mu.Unlock()
			if changed {
				h.save()
			}
		}
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
		return fmt.Errorf("%w: LinkedIn didn't say who is signed in", linkedingo.ErrTokenInvalidated)
	}
	h.me = linkedingo.NewURN(profile.MiniProfile.EntityURN.ID())
	h.name = strings.TrimSpace(profile.MiniProfile.FirstName + " " + profile.MiniProfile.LastName)
	h.log.Info().Msg("Signed in")

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
			"thread":       h.thread(msg.Conversation.EntityURN),
			"message":      h.message(msg),
			"leftServerAt": ev.LeftServerAt.UnixMilli(),
		})
	case data.DecoratedConversation != nil:
		h.publish("conversation", h.conversation(data.DecoratedConversation.Result))
	case data.DecoratedSeenReceipt != nil:
		h.publish("seen", map[string]any{})
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
	Org      bool   `json:"org,omitempty"`
}

type message struct {
	ID       string   `json:"id"`
	From     person   `json:"from"`
	Text     string   `json:"text"`
	At       int64    `json:"at"`
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

// tail is the last part of a URN's tuple: a conversation's thread id
// ("2-…") or a message's id, as LinkedIn's own web addresses name them.
func tail(u linkedingo.URN) string {
	id := strings.TrimSuffix(strings.TrimPrefix(u.ID(), "("), ")")
	if i := strings.LastIndex(id, ","); i >= 0 {
		return id[i+1:]
	}
	return id
}

// thread names a conversation by its thread id, and remembers its whole URN so
// a later read of the thread asks LinkedIn for exactly that conversation.
func (h *helper) thread(u linkedingo.URN) string {
	t := tail(u)
	if t != "" {
		h.peopleMu.Lock()
		h.threads[t] = u
		h.peopleMu.Unlock()
	}
	return t
}

// listedURN is the whole URN of a conversation the helper listed (on an inbox
// page or in an event), and false for any other: a reply goes only to one.
func (h *helper) listedURN(thread string) (linkedingo.URN, bool) {
	h.peopleMu.Lock()
	defer h.peopleMu.Unlock()
	u, ok := h.threads[thread]
	return u, ok
}

func (h *helper) conversationURN(thread string) linkedingo.URN {
	h.peopleMu.Lock()
	defer h.peopleMu.Unlock()
	if u, ok := h.threads[thread]; ok {
		return u
	}
	return linkedingo.NewURN("urn:li:msg_conversation:(urn:li:fsd_profile:" + h.me.ID() + "," + thread + ")")
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
		out.Org = true
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
		ID:   tail(m.EntityURN),
		From: h.person(m.Sender),
		Text: m.Body.Text,
		At:   m.DeliveredAt.UnixMilli(),
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
		ID:           h.thread(c.EntityURN),
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
	for _, m := range c.Messages.Elements {
		if out.Last == nil || m.DeliveredAt.UnixMilli() > out.Last.At {
			last := h.message(m)
			out.Last = &last
		}
	}
	return out
}

func timeParam(r *http.Request) time.Time {
	if ms, err := strconv.ParseInt(r.URL.Query().Get("before"), 10, 64); err == nil && ms > 0 {
		return time.UnixMilli(ms)
	}
	return time.Now()
}

func (h *helper) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		writeJSON(w, map[string]any{
			"me": h.name, "state": h.state, "error": h.stateErr, "version": version,
			"lastEvent": h.lastEvent.UnixMilli(), "events": h.counts, "listeners": len(h.subs),
		})
	})
	mux.HandleFunc("GET /conversations", func(w http.ResponseWriter, r *http.Request) {
		resp, err := h.cli.GetConversationsUpdatedBefore(r.Context(), timeParam(r))
		if err != nil {
			h.fail(w, err)
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
	mux.HandleFunc("GET /threads/{thread}/messages", func(w http.ResponseWriter, r *http.Request) {
		thread := r.PathValue("thread")
		if thread == "" || strings.ContainsAny(thread, "(),:/ ") || len(thread) > 200 {
			http.Error(w, "not a thread", http.StatusBadRequest)
			return
		}
		count := 20
		if n, err := strconv.Atoi(r.URL.Query().Get("count")); err == nil && n > 0 && n <= 50 {
			count = n
		}
		resp, err := h.cli.GetMessagesBefore(r.Context(), h.conversationURN(thread), timeParam(r), count)
		if err != nil {
			h.fail(w, err)
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
	mux.HandleFunc("POST /threads/{thread}/messages", h.reply)
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

// maxReply is the longest reply, in characters: LinkedIn's own limit for a message.
const maxReply = 8000

// replyID is a Rasks outbox id, a UUID: LinkedIn takes it as the message's origin token.
func replyID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch {
		case i == 8 || i == 13 || i == 18 || i == 23:
			if c != '-' {
				return false
			}
		case (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F'):
		default:
			return false
		}
	}
	return true
}

// reply sends one text message to a conversation the helper listed, as the
// person: {"id": "<Rasks outbox id>", "text": "…"}. It answers {"id", "at"}
// with LinkedIn's id for the new message. The same id again answers the first
// send's result and sends nothing, so a retry never sends twice.
func (h *helper) reply(w http.ResponseWriter, r *http.Request) {
	thread := r.PathValue("thread")
	if thread == "" || strings.ContainsAny(thread, "(),:/ ") || len(thread) > 200 {
		http.Error(w, "not a thread", http.StatusBadRequest)
		return
	}
	var body struct {
		ID   string `json:"id"`
		Text string `json:"text"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&body); err != nil {
		http.Error(w, "not a reply", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(body.Text)
	if !replyID(body.ID) || text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > maxReply {
		http.Error(w, "not a reply", http.StatusBadRequest)
		return
	}
	conv, ok := h.listedURN(thread)
	if !ok {
		http.Error(w, "not a listed conversation", http.StatusNotFound)
		return
	}

	h.sendMu.Lock()
	defer h.sendMu.Unlock()
	if done, ok := h.sent[body.ID]; ok {
		if done.Err != "" {
			http.Error(w, done.Err, http.StatusConflict)
			return
		}
		writeJSON(w, done)
		return
	}
	msg, err := h.send(r.Context(), conv, text, body.ID)
	if err != nil {
		// Whether LinkedIn took it is not known: the id stays spent, so a retry
		// asks the person to look in LinkedIn rather than send it again.
		h.sent[body.ID] = sentReply{Err: "the send failed: " + err.Error()}
		h.log.Warn().Err(err).Msg("Couldn't send a reply")
		h.fail(w, err)
		return
	}
	done := sentReply{ID: tail(msg.EntityURN), At: msg.DeliveredAt.UnixMilli()}
	if done.At <= 0 {
		done.At = time.Now().UnixMilli()
	}
	h.sent[body.ID] = done
	writeJSON(w, done)
}

// auth lets in only the Mac app: the token, and no browser page on any origin.
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

// fail answers a LinkedIn error. Signed out is 401 and an event, so the Mac app
// asks the person to sign in again.
func (h *helper) fail(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	if isSignedOut(err) {
		status = http.StatusUnauthorized
		h.setState("signed_out", err.Error())
		h.publish("signed_out", map[string]any{"error": err.Error()})
	}
	http.Error(w, err.Error(), status)
}

func randomToken() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
