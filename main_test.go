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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.mau.fi/mautrix-linkedin/pkg/linkedingo"
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
