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

package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-linkedin/pkg/linkedingo"
	"go.mau.fi/util/jsontime"
)

type memStore struct {
	s     *Session
	saves int
}

func (m *memStore) Load() (Session, error) {
	if m.s == nil {
		return Session{}, errNoSession
	}
	return *m.s, nil
}
func (m *memStore) Save(s Session) error { m.s = &s; m.saves++; return nil }
func (m *memStore) Forget() error        { m.s = nil; return nil }

func TestTail(t *testing.T) {
	conv := linkedingo.NewURN("urn:li:msg_conversation:(urn:li:fsd_profile:ACoAAB,2-MjEwNWU1ZjA=)")
	if got := tail(conv); got != "2-MjEwNWU1ZjA=" {
		t.Fatalf("thread = %q", got)
	}
	msg := linkedingo.NewURN("urn:li:msg_message:(urn:li:fsd_profile:ACoAAB,2-ODk3NzM=)")
	if got := tail(msg); got != "2-ODk3NzM=" {
		t.Fatalf("message = %q", got)
	}
	if got := tail(linkedingo.NewURN("urn:li:fsd_profile:ACoAAB")); got != "ACoAAB" {
		t.Fatalf("plain = %q", got)
	}
}

func TestThreadURN(t *testing.T) {
	h := newHelper(zerolog.Nop(), &memStore{})
	h.me = linkedingo.NewURN("urn:li:fsd_profile:ME")
	full := linkedingo.NewURN("urn:li:msg_conversation:(urn:li:fsd_profile:ME,2-abc=)")
	if got := h.thread(full); got != "2-abc=" {
		t.Fatalf("thread = %q", got)
	}
	if got := h.conversationURN("2-abc=").String(); got != full.String() {
		t.Fatalf("known urn = %q", got)
	}
	if got := h.conversationURN("2-new").String(); got != "urn:li:msg_conversation:(urn:li:fsd_profile:ME,2-new)" {
		t.Fatalf("built urn = %q", got)
	}
}

func TestAuth(t *testing.T) {
	h := newHelper(zerolog.Nop(), &memStore{})
	h.token = "secret"
	ok := h.auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	cases := []struct {
		name   string
		header map[string]string
		want   int
	}{
		{"no token", nil, http.StatusUnauthorized},
		{"wrong token", map[string]string{"Authorization": "Bearer nope"}, http.StatusUnauthorized},
		{"token", map[string]string{"Authorization": "Bearer secret"}, http.StatusNoContent},
		{"from a web page", map[string]string{"Authorization": "Bearer secret", "Origin": "https://evil.example"}, http.StatusUnauthorized},
		{"from a null origin", map[string]string{"Authorization": "Bearer secret", "Origin": "null"}, http.StatusUnauthorized},
	}
	for _, c := range cases {
		req := httptest.NewRequest("GET", "/status", nil)
		for k, v := range c.header {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		ok.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: got %d, want %d", c.name, rec.Code, c.want)
		}
	}
}

func TestSaveFrom(t *testing.T) {
	m := &memStore{}
	err := saveFrom(strings.NewReader(`{"cookies":"li_at=x; bcookie=y","headers":{}}`), m)
	if err == nil || m.s != nil {
		t.Fatalf("a session with no JSESSIONID was kept: %v", err)
	}
	err = saveFrom(strings.NewReader(`{"cookies":"li_at=x; JSESSIONID=\"ajax:1\"","xLiTrack":"{}","pageInstance":"urn:li:page:x","headers":{"User-Agent":"Mozilla"}}`), m)
	if err != nil || m.s == nil || m.s.Headers["User-Agent"] != "Mozilla" {
		t.Fatalf("session not kept: %v", err)
	}
	if err := saveFrom(strings.NewReader(`not json`), m); err == nil {
		t.Fatal("bad json kept")
	}
}

func TestLoadNoSession(t *testing.T) {
	h := newHelper(zerolog.Nop(), &memStore{})
	if err := h.load(); !errors.Is(err, errNoSession) {
		t.Fatalf("load = %v", err)
	}
}

func TestSaveOnlyWhenChanged(t *testing.T) {
	m := &memStore{s: &Session{Cookies: []byte(`"li_at=x; JSESSIONID=\"ajax:1\""`)}}
	h := newHelper(zerolog.Nop(), m)
	if err := h.load(); err != nil {
		t.Fatal(err)
	}
	h.save()
	before := m.saves
	h.session.SyncToken = "tok"
	h.save()
	if m.saves != before+1 || m.s.SyncToken != "tok" {
		t.Fatalf("sync token not saved: saves %d -> %d", before, m.saves)
	}
	h.save()
	if m.saves != before+1 {
		t.Fatal("saved with nothing changed")
	}
}

func TestReply(t *testing.T) {
	h := newHelper(zerolog.Nop(), &memStore{})
	h.me = linkedingo.NewURN("urn:li:fsd_profile:ME")
	h.token = "secret"
	listed := linkedingo.NewURN("urn:li:msg_conversation:(urn:li:fsd_profile:ME,2-abc=)")
	h.thread(listed)
	var calls []string
	fail := false
	h.send = func(_ context.Context, conv linkedingo.URN, text, id string) (linkedingo.Message, error) {
		calls = append(calls, conv.String()+" "+text+" "+id)
		if fail {
			return linkedingo.Message{}, errors.New("boom")
		}
		return linkedingo.Message{
			EntityURN:   linkedingo.NewURN("urn:li:msg_message:(urn:li:fsd_profile:ME,2-msg=)"),
			DeliveredAt: jsontime.UMInt(1761300000000),
		}, nil
	}
	srv := h.routes()
	post := func(thread, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/threads/"+thread+"/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}
	const id = "0f8b1c2d-3e4f-4a5b-8c6d-7e8f9a0b1c2d"

	if rec := post("2-abc=", `{"id":"`+id+`","text":"  Thanks, Tuesday works.  "}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"id":"2-msg="`) || !strings.Contains(rec.Body.String(), `"at":1761300000000`) {
		t.Fatalf("send = %d %s", rec.Code, rec.Body)
	}
	if len(calls) != 1 || calls[0] != listed.String()+"   Thanks, Tuesday works.   "+id {
		t.Fatalf("calls = %q", calls)
	}
	// The same id again answers the first send and sends nothing.
	if rec := post("2-abc=", `{"id":"`+id+`","text":"Thanks, Tuesday works."}`); rec.Code != 200 || len(calls) != 1 {
		t.Fatalf("repeat = %d, calls %d", rec.Code, len(calls))
	}
	// Never a conversation it did not list.
	if rec := post("2-other", `{"id":"1f8b1c2d-3e4f-4a5b-8c6d-7e8f9a0b1c2d","text":"Hi"}`); rec.Code != 404 || len(calls) != 1 {
		t.Fatalf("unlisted = %d", rec.Code)
	}
	for _, bad := range []string{`{"id":"nope","text":"Hi"}`, `{"id":"` + id[:35] + `x","text":"Hi"}`, `{"id":"2f8b1c2d-3e4f-4a5b-8c6d-7e8f9a0b1c2d","text":"   "}`, `{"id":"2f8b1c2d-3e4f-4a5b-8c6d-7e8f9a0b1c2d","text":"` + strings.Repeat("a", maxReply+1) + `"}`, `not json`} {
		if rec := post("2-abc=", bad); rec.Code != 400 {
			t.Errorf("bad body %.40q = %d", bad, rec.Code)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("a bad body sent: %d", len(calls))
	}
	// A failed send keeps its id spent: a retry is refused, never sent again.
	fail = true
	failed := "3f8b1c2d-3e4f-4a5b-8c6d-7e8f9a0b1c2d"
	if rec := post("2-abc=", `{"id":"`+failed+`","text":"Hi"}`); rec.Code != 502 {
		t.Fatalf("failed send = %d", rec.Code)
	}
	fail = false
	if rec := post("2-abc=", `{"id":"`+failed+`","text":"Hi"}`); rec.Code != 409 || len(calls) != 2 {
		t.Fatalf("retry of a failed send = %d, calls %d", rec.Code, len(calls))
	}
	// GET stays a read.
	req := httptest.NewRequest("DELETE", "/threads/2-abc=/messages", nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE = %d", rec.Code)
	}
}
