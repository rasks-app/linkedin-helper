# rasks-linkedin-helper (spike, RM-2069)

Reads one person's LinkedIn inbox on their own Mac, with no Matrix server, for Rasks to read over `127.0.0.1`. AGPL-3.0: it links mautrix-linkedin's LinkedIn client, unmodified, so its own source is AGPL too. Rasks never imports it; it talks to it over localhost only.

## Answer

**Yes.** mautrix-linkedin runs on the Mac with no homeserver. The trick is not to host the bridgev2 *connector* at all: everything that knows LinkedIn lives in its protocol library, `pkg/linkedingo` (endpoints, GraphQL query ids, cookies and CSRF, the realtime stream). The connector only maps that onto Matrix rooms and ghosts. So the helper hosts `linkedingo` directly, about 500 lines of our own Go, with no Matrix, no bridgev2 runtime, and no database.

The same split holds for the next networks: mautrix-meta's `messagix` (Instagram, Messenger) and mautrix-twitter's `twittermeow` (X) are protocol libraries the same way, so each is one thin helper of this shape.

Tested on Rowan's real account, 24 Oct 2026, read-only:

| Check | Result |
|---|---|
| Sign in | Electron window on linkedin.com/login, Rowan typed his password there; the window saved cookies, X-LI headers, and its own user agent (Chrome 130, macOS) |
| Who is signed in | `GET /voyager/api/me`, 150 ms |
| List the inbox | 20 conversations, 2 unread, InMail tagged, in ~1 s |
| Read a thread | 5 messages, oldest first, sender names right |
| Restart from the saved session | Signed in and live in under 1 s, no new sign-in |
| Catch-up | A message sent while the helper was down showed on the next start (sync token) |
| Live message | Paola's DM ("It works!", sent 20:16:35) reached the helper on `messagesTopic` at 20:16:36, about 1 s |
| Live through a long run | 18:21 to 19:12 on one session: LinkedIn closed the stream every ~14 min, the helper reopened it in under 1 s each time |
| LinkedIn warnings | None: no checkpoint, no sign-out, no 401 |
| Memory | 13 to 14 MB phys footprint (RSS 20 MB) |
| CPU | 0.04 s over its first 10 minutes; idle between events |
| Binary | 7.8 MB, arm64, stripped |

## API

Every request needs `Authorization: Bearer <token>`; a request with an `Origin` header is refused, so a web page can't reach it. The first line on stdout is `{"ready":true,"url":"http://127.0.0.1:<port>","token":"…","me":"…"}`.

- `GET /status`: who, realtime state, event counts.
- `GET /conversations?before=<ms>`: the primary inbox, 20 a page, each with `unread`, `lastActivity`, `people`, `last` message, `categories` (INMAIL, …).
- `GET /conversations/{urn}/messages?before=<ms>&count=<n>`: a thread, oldest first.
- `GET /events`: Server-Sent Events: `message`, `conversation`, `seen`, `signed_out`, `failed`.

There is no route that sends, reacts, marks read, or looks up a profile. The only LinkedIn calls are `/me` once at start, the two inbox reads, and the realtime stream the web app itself keeps open (one heartbeat POST a minute, which is the stream's protocol). LinkedIn closes the stream about every 14 minutes; the helper reconnects and catches up by sync token. Nothing polls on a clock.

## Running it

```sh
go build -trimpath -ldflags "-s -w" -o bin/rasks-linkedin-helper .
# sign in (Electron 33, the Mac app's):
…/Electron.app/Contents/MacOS/Electron login/login.js --out session.json
./bin/rasks-linkedin-helper -session session.json [-v]
```

`session.json` holds the session's cookies (mode 0600). It is in `.gitignore`. The sign-in window also keeps its LinkedIn login in Electron's own data folder (`~/Library/Application Support/Electron/Partitions/rasks-linkedin`).

## What broke

1. **Go's HTTP/2 hangs on LinkedIn.** The inbox reads never answered (minutes) and the realtime connect took 30 s. Over HTTP/1.1 each answers in ~150 ms. Fixed with `//go:debug http2client=0`. Worth reporting upstream; Beeper may not see it if its build differs.
2. **Sender names blank in the list.** A listed message names its sender by id only; the conversation's participants carry the names. The helper fills them from the participants it has seen.
3. **`mpName is not voyager-web` warning.** LinkedIn's web app now reports a different front end in `X-LI-Track` (mpVersion 0.2.8097). Upstream then sends its own default `X-LI-Track`. It works, but the requests' X-LI headers no longer match the browser that signed in, which is a small fingerprint risk.
4. **The Homebrew `electron` shim** used a broken Homebrew node; ran Electron's binary directly. Spike-only.
5. **My session restarted** twice mid-test and took the helper with it, so Rowan's own two test messages came in through catch-up on the next start, not live. Paola's reply came live. In the real build the Mac app supervises the helper.

## Risks that stay

- **TLS fingerprint.** Requests come from Go's TLS stack, not Chrome's, through Cloudflare (`__cf_bm`). LinkedIn accepted it today; it is the same exposure every mautrix-linkedin user (and Beeper) has.
- **Same user agent.** The window's own UA and Sec-CH-UA headers are captured and reused, as the mautrix docs require. The sign-in window should keep a fixed Chromium version, or sign in again when Electron updates.
- **Upstream churn.** LinkedIn changes query ids often; upstream had three commits in the last two days. Pin a commit and bump it with the Mac app's releases.

## Estimate for the real build

The `linkedin` source in Rasks, read and live, drafts later. Working days for one agent:

| Piece | Days |
|---|---|
| Helper for real: public AGPL repo, universal binary signed and notarized inside the Mac app, session in the Keychain, rotated cookies saved, upstream pinned | 1 |
| Mac app (`mac/src/linkedin.js`): the sign-in window in a persist partition, consent dialog, `inboxes:set` toggle, start and supervise the helper, push snapshots up on each event (`poke`, as WhatsApp's `liveSoon`), answer `older` and `browse` on the check-in | 2 |
| Server: `messages-linkedin.ts` adapter on the `whatsappAdapter` pattern (snapshot and work rows in `cache`), `/api/v1/bridge/linkedin` ingest, the poll section, and `linkedin` in `SOURCES` and the ~15 `Record<Source, …>` maps (`PROVIDER_INFO`, `MESSAGE_CAPS`, `INBOX_TOOL`, `MAC_SOURCES`, the fake adapter for the preview) | 2.5 |
| Settings › Connections row and sign-up step (`MacInboxRow` states), source logo, tests | 1 |
| **Read and live** | **6.5** |
| Sending drafts (`SendMessage` exists in `linkedingo`; through the draft card and the send guard), as its own decision | +1.5 |

## License

AGPL-3.0, see `LICENSE`. mautrix-linkedin is © Sumner Evans and contributors, AGPL-3.0. When the Mac app ships this binary to other people, the helper's source (this repo, and the pinned upstream) must be offered to them; Rasks's own code stays separate, a program that talks to this one over localhost.
