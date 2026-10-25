# rasks-linkedin-helper

Reads one person's LinkedIn inbox on their own Mac, for the Rasks Mac app to show in Rasks's Messages, and sends the replies they approve there. It has no Matrix server: it hosts [mautrix-linkedin](https://github.com/mautrix/linkedin)'s LinkedIn client library, `pkg/linkedingo`, unmodified and pinned by commit in `go.mod`, behind a small API on `127.0.0.1`.

AGPL-3.0, since it links mautrix-linkedin. Rasks never imports it: the Mac app starts it as its own program and talks to it over the loopback address only. The Rasks Mac app ships this binary built from a pinned commit of this repo.

## What it does, and what it never does

- Reads the primary inbox (20 conversations a page), a thread's messages, and the realtime stream LinkedIn's own web app keeps open, with its one heartbeat a minute. LinkedIn closes the stream about every 14 minutes; the helper reconnects and catches up by sync token. Nothing polls on a clock.
- Sends one kind of thing: a text reply in a conversation it already listed, which the person approved in Rasks. Each carries the id Rasks queued it under; the same id again gets the first answer and sends nothing.
- Never starts a conversation, sends a file, reacts, marks read, or looks up a profile. There is no route that could.
- Keeps the session in the macOS Keychain (service `Rasks LinkedIn`), never in a file, and saves the cookies LinkedIn rotates back to it.
- Sends every request with the browser headers and user agent of the window the person signed in with.
- HTTP/1.1 only (`//go:debug http2client=0`): over Go's HTTP/2, LinkedIn left the inbox reads hanging for minutes and the realtime connect for 30 s.

LinkedIn's User Agreement (section 8.2) does not allow third-party software to access an account. The Rasks Mac app says so before the person signs in.

## Commands

```sh
rasks-linkedin-helper save < session.json   # keep a session in the Keychain (the Mac app pipes it in)
rasks-linkedin-helper forget                # delete it
rasks-linkedin-helper serve [-addr 127.0.0.1:0] [-v]
rasks-linkedin-helper version
```

`serve` prints one line on stdout and then serves:

- `{"ready":true,"url":"http://127.0.0.1:<port>","token":"…","me":"…","version":"…"}`, or
- `{"ready":false,"error":"signed_out"|"failed","why":"…"}` and exits (3 for signed out: sign in again; 1 otherwise).

A session is what the sign-in window captured: `{"cookies":"li_at=…; JSESSIONID=…", "xLiTrack":"…", "pageInstance":"urn:li:page:…", "headers":{"User-Agent":"…","Sec-Ch-Ua":"…"}}`. `login/login.js` is the sign-in window on its own, for trying the helper outside Rasks (`electron login/login.js --out session.json`).

## API

Every request needs `Authorization: Bearer <token>`; a request with an `Origin` header is refused, so no web page can reach it.

- `GET /status`: who is signed in, the realtime state (`connected`, `reconnecting`, `signed_out`, `failed`), event counts.
- `GET /conversations?before=<ms>`: the primary inbox, 20 a page, newest first. Each has `id` (LinkedIn's thread id, `2-…`, as in `linkedin.com/messaging/thread/<id>/`), `title`, `group`, `unread`, `lastActivity`, `people` (name, headline, photo, profile link), `last` message, and `categories` (`INMAIL`, …).
- `GET /threads/{id}/messages?before=<ms>&count=<n>`: a thread's messages, oldest first, at most 50.
- `POST /threads/{id}/messages` with `{"id":"<uuid>","text":"…"}`: send a text reply, at most 8,000 characters, to a conversation the helper listed (404 otherwise). Answers `{"id","at"}`, LinkedIn's id and time for the new message. A send that failed keeps its id spent: the same id again answers 409, so a retry never sends twice.
- `GET /events`: Server-Sent Events, `message`, `conversation`, `seen`, `signed_out`, `failed`.

A 401 from LinkedIn answers 401 and sends `signed_out`.

## Building

```sh
./build.sh [out] [version]   # one universal binary, Apple silicon and Intel; needs Go and Xcode's command line tools
go test ./...
```

Go 1.27 runs on macOS 13 and later. cgo links the Security framework for the Keychain.

## Upstream

LinkedIn changes its query ids often, and mautrix-linkedin follows. To move to a newer upstream: `go get go.mau.fi/mautrix-linkedin@<commit>`, `go mod tidy`, test on a real account, commit, and bump the commit the Rasks Mac app pins (`mac/linkedin-helper.json` in Rasks).

Known risks: requests come from Go's TLS stack, not Chrome's (every mautrix-linkedin user has the same exposure), and a new Electron in the Mac app changes the sign-in window's user agent, so the person signs in again after one.

## License

AGPL-3.0, see `LICENSE`. mautrix-linkedin is © Sumner Evans and contributors, AGPL-3.0. Anyone the Rasks Mac app ships this binary to can get its source here, at the commit the app names in `Contents/Resources/linkedin-helper/SOURCE.txt`.
