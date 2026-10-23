# davd — Go CalDAV/CardDAV server: design export

Exported 2026-10-22 from a Claude Code session, to continue the work on another machine.
Hand this file to a new session: "Read davd-design-export.md and continue from there."

---

## 0. Context / memory (user preferences and decisions so far)

- **Goal:** a self-hosted Go service that serves CalDAV and CardDAV.
- **Scale:** 1–100 users.
- **Backend:** local, preferably plain files with no database.
- **Dependencies:** as few as possible. Prefer the Go standard library. One accepted exception (2026-10-23): `go.yaml.in/yaml/v3` for a YAML config file.
- **Must-haves:**
  - Calendar (and address book) sharing between users, and by **group**.
  - **Invitation emails** for events.
  - **Invitation confirmations** (RSVP back to the organizer).
  - **Recurring events must work fully** (explicit user requirement).
  - **VTODO** and **VJOURNAL** support.
- **Decisions made:**
  - Sharing is done on the server side: shared collections show up in the sharee's home. No client-side sharing protocol.
  - The server sends invitations itself and advertises `calendar-auto-schedule`.
  - RSVP uses the server's own HMAC-signed links (GET shows a confirmation page, POST records the answer), plus implicit scheduling for internal attendees. Receiving reply emails (IMAP, SMTP receiver, inbound webhook) is deferred.
  - Write our own RRULE expander with no dependencies, tested against ported test vectors. `teambition/rrule-go` is the fallback if this gets out of hand.
- **Status (2026-10-23):** milestones 1–4 implemented (see README.md). Build via Makefile, deploy as a container behind a TLS-terminating reverse proxy. Tested with DAVx⁵. Next: milestone 5 (CardDAV).

---

## 1. Landscape / why build

- **Radicale** (Python) is file-based and close to the goal, but has no group sharing and no invitation emails.
- **`emersion/go-webdav`** is the main Go library. It does CalDAV and CardDAV for server and client, and depends on `go-ical` and `go-vcard`. Its server side is bare: no sharing, no scheduling, and limited sync-collection.
- **`golang.org/x/net/webdav`** has no `REPORT` method, so it can't serve CalDAV or CardDAV.
- **Zero dependencies is realistic.** Use `net/http`, `encoding/xml`, `crypto/pbkdf2` (std since Go 1.24), `crypto/hmac`, `net/smtp`, `mime/multipart` and `time/tzdata`, plus a small hand-written iCal/vCard content-line parser (unfold lines, split on `:` and `;`).
- **Target clients:** Apple Calendar, Contacts and Reminders, Thunderbird, DAVx⁵ (with Tasks.org and jtx Board), Evolution.

## 2. Relevant RFCs

| RFC | What | Needed |
|---|---|---|
| 4918 | WebDAV: PROPFIND, PROPPATCH, MKCOL, PUT, GET, DELETE | yes |
| 4791 | CalDAV: MKCALENDAR, calendar-query, calendar-multiget | yes |
| 6352 | CardDAV: addressbook-query, addressbook-multiget | yes |
| 6578 | sync-collection REPORT (DAVx⁵ and Apple rely on it) | yes |
| 6764 | `/.well-known/caldav` and `/.well-known/carddav` | yes, trivial |
| 5545 / 6350 | iCalendar / vCard | parse lightly; full RRULE expansion |
| 6638 + 6047 | CalDAV scheduling + iMIP (email) | partial (implicit scheduling, iMIP out) |
| 3744 | WebDAV ACL | fake it (`current-user-privilege-set`) |
| Apple calendarserver-sharing | client sharing UI | skip, sharing is server-side |

## 3. URL layout

```
/.well-known/caldav     -> 301 /dav/
/.well-known/carddav    -> 301 /dav/
/dav/principals/<user>/                 current-user-principal, calendar-home-set, addressbook-home-set
/dav/calendars/<user>/                  calendar home
/dav/calendars/<user>/<cal>/            calendar collection
/dav/calendars/<user>/<cal>/<uid>.ics
/dav/addressbooks/<user>/<book>/<uid>.vcf
/rsvp?t=<token>                         RSVP confirmation page (GET) / submit (POST)
```

## 4. Storage (files only)

```
data/
  config.yaml                 users (pbkdf2 hash), groups, smtp, rsvp secret, base URL
  outbox/*.eml                pending emails (retry queue)
  users/<user>/
    calendars/<cal>/
      .meta.json              displayname, color, owner, components ["VEVENT"|"VTODO"|"VJOURNAL"], shares
      .changes.log            append-only "<token> <PUT|DEL> <href>" for sync-collection
      <uid>.ics
    addressbooks/<book>/...
```

- **Atomic writes:** temp file, `fsync`, then `os.Rename`.
- **ETag:** SHA-256 of the content, cached in memory.
- **Sync token / CTag:** a monotonic counter, the last line of `.changes.log`. Property changes (PROPPATCH) log a `META` line and bump it too. The token URI is `http://davd/ns/sync/<sync_id>-<n>`; `sync_id` is random per collection, so tokens of a recreated collection are rejected.
- **Compaction:** when the log exceeds 2× objects + 1000 lines, keep the newest line per name and at most 1000 tombstones; a `FLOOR n` line marks the oldest answerable token (older → 403 `valid-sync-token`, client resyncs).
- **Locking:** one `sync.RWMutex` per collection. Load indexes into memory at startup; at 100 users that's fine.
- **Search:** none. Queries scan the collection.

## 5. Sharing (users and groups)

```json
"shares": [
  {"group": "family", "access": "read"},
  {"user": "bob", "access": "write"}
]
```

- A PROPFIND on the sharee's home also lists the shared collection, with a displayname like `"work (alice)"`.
- All access checks go through one function, `can(user, collection) -> none|read|write`, which also feeds `current-user-privilege-set`.
- Users and groups live in `config.yaml` and are reloaded on SIGHUP. A small admin page (`html/template`) can come later.

## 6. Invitations (iMIP) and RSVP

**Sending:**

- On PUT, diff the old and new versions: new or removed attendees, time changes, cancellations, EXDATEs and overrides.
- Send a multipart email with a `text/plain` part, a `text/html` part (containing the RSVP links) and a `text/calendar; method=REQUEST|CANCEL` part.
- Advertise `calendar-auto-schedule` in the `DAV:` header. Otherwise Thunderbird and Apple also send their own invitations and attendees get duplicates.
- Skip attendees with `SCHEDULE-AGENT=CLIENT`, and never email the organizer.
- Send through `net/smtp` (STARTTLS, PLAIN auth) from a goroutine, with a file-backed outbox and retries.

**How the native mail-client buttons work:** Accept/Decline in Outlook, Gmail or Apple Mail sends a `METHOD:REPLY` **email** to the ORGANIZER address. There's no webhook or HTTP call, so the server doesn't see it.

**RSVP strategy:**

- **A. Own links (implement).** `https://host/rsvp?t=<token>`, where the token is `base64(uid|attendee|sequence)` plus `HMAC-SHA256(secret, …)`. It's stateless.
  - **GET only shows a confirmation page; the POST records the answer.** Mail security scanners (Safe Links, Defender) follow every GET link in an email and would otherwise accept invitations on the user's behalf.
  - On POST, set the attendee's `PARTSTAT` in the organizer's `.ics` (per instance, through an override, when the answer is for a single occurrence), bump the change log, and optionally email the organizer.
  - The token carries `SEQUENCE`, so answers to outdated invitations are rejected or flagged.
- **B. Internal attendees (implement).** The server copies the event into the attendee's default calendar with `PARTSTAT=NEEDS-ACTION`. When the attendee's client PUTs back a changed PARTSTAT, the server merges it into the organizer's original (attendee side of RFC 6638).
- **C. Receiving reply emails (deferred; webhook only).** No IMAP or SMTP receiver and no mail parsing. If ever needed, an inbound-mail webhook service (Mailgun, Postmark). Meanwhile the native-button replies reach the organizer's inbox, and Thunderbird or Apple Mail can apply them, which syncs back through a normal PUT.

## 7. VTODO and VJOURNAL

- Both are stored as opaque components. Each collection declares its allowed types in `supported-calendar-component-set`; Apple expects task lists to be separate collections.
- Time-range rules follow RFC 4791 §9.9: VTODO uses DUE, COMPLETED and CREATED; VJOURNAL uses DTSTART.
- Client support:

| Client | VTODO | VJOURNAL |
|---|---|---|
| Apple Reminders | yes | no |
| Thunderbird | yes | no |
| DAVx⁵ + Tasks.org / jtx Board | yes | jtx Board yes |
| Evolution | yes | yes (Memos) |

## 8. Recurring events (must work)

**Storage:** one `.ics` file per series, containing the master (RRULE, RDATE, EXDATE) and its overrides (`RECURRENCE-ID`), all with the same UID. The server never splits it and round-trips it byte for byte. The calendar apps expand it for display.

**Where the server needs to expand recurrences:**

- **calendar-query time-range.** Returning too few results is the dangerous bug.
- **Invitation emails:** human-readable rule text; EXDATE becomes a per-instance CANCEL, an override becomes a REQUEST with RECURRENCE-ID; series splits.
- **Per-instance RSVP**, through an override that carries only that attendee's PARTSTAT.
- **Internal attendee copies:** overrides and EXDATEs must propagate and merge back.
- **`expand` / `limit-recurrence-set`** in calendar-data.
- **Free/busy** (optional).

**Own expander (std lib only), about 1.5–2k lines including tests:**

- **Frequencies:** DAILY, WEEKLY, MONTHLY and YEARLY are essential; HOURLY, MINUTELY and SECONDLY are easy extras.
- **Rule parts:** INTERVAL, COUNT, UNTIL, BYDAY with ordinals (`-1FR`, `2MO`), BYMONTHDAY (including negatives), BYMONTH, BYSETPOS, BYYEARDAY, BYWEEKNO, WKST.
- **Invalid dates are skipped, not clamped.** "Monthly on the 31st" skips February.
- **DST:** wall-clock times stay put. Build occurrences with `time.Date(..., loc)`; never add 24h steps.
- **Value types:** DATE (all-day), floating, UTC.
- **Exceptions:** RDATE (including PERIOD) and EXDATE. Match EXDATE and RECURRENCE-ID in UTC after resolving each one's TZID.
- **RANGE=THISANDFUTURE:** clients usually split the series instead (UNTIL on the old one, new UID), but support it.
- **Safety:** cap iterations and the scan horizon so impossible rules (e.g. `BYMONTHDAY=31;BYMONTH=2`) can't loop forever.

```go
type Occurrence struct{ Start, End time.Time; RecurrenceID time.Time; Override *Component }
func Expand(cal *ical.Calendar, uid string, from, to time.Time) iter.Seq[Occurrence]
```

**Timezones:**

- Embed tzdata with `import _ "time/tzdata"`.
- **Windows zone names → IANA:** about 140 entries, generated from CLDR `windowsZones.xml` into a `.go` file (Outlook sends names like `W. Europe Standard Time`).
- **Unknown TZID:** build a zone from the file's own VTIMEZONE block. Its STANDARD and DAYLIGHT parts are RRULEs, so the same expander handles them.
- **Lookup order:** IANA name, then the Windows mapping, then the embedded VTIMEZONE, then UTC with a warning.

**Testing:**

1. Port test vectors from python-dateutil (`test_rrule.py`), libical and rrule-go into Go table tests.
2. Golden `.ics` files from Apple, Thunderbird, Outlook and Google for these cases:
   - a moved instance
   - a deleted instance
   - "this and all future"
   - last Friday of each month
   - yearly on Feb 29
   - an all-day series
   - a series crossing DST
3. Round-trip tests: PUT then GET is identical; after an RSVP override, nothing else has changed.
4. `go test -fuzz` on the parser and expander: no panics, no hangs, results sorted and inside the requested range.

## 9. Known hard spots

- **XML namespaces:** `DAV:`, caldav, carddav, `http://calendarserver.org/ns/` and `http://apple.com/ns/ical/` are mixed together. Build multistatus responses by hand instead of with `encoding/xml` struct tags.
- **Apple:** needs `getctag`, `calendar-color`, `supported-calendar-component-set` and `resourcetype` to be exact, or it silently ignores the account. Test Apple early.
- **Auth:** Basic auth (TLS terminated by the reverse proxy), with passwords hashed using PBKDF2-SHA256 (`crypto/pbkdf2`). App passwords can come later.

## 10. Package sketch

```
cmd/davd/main.go
internal/
  config/     users, groups, shares, reload
  auth/       basic auth, pbkdf2
  store/      file layout, atomic write, etag, changelog, locks
  ical/       content-line parser, components/props/params
  rrule/      recurrence expansion, tz resolution (windowsZones table, VTIMEZONE)
  vcard/      minimal parser
  dav/        PROPFIND/PROPPATCH/REPORT, multistatus builder
  caldav/     calendar-query (time-range incl. VTODO/VJOURNAL), multiget, MKCALENDAR, expand
  carddav/    addressbook-query, multiget
  sched/      attendee diffing, implicit scheduling for internal users, iMIP builder, smtp outbox
  rsvp/       HMAC tokens, confirm page (GET), submit (POST)
```

## 11. Milestones

1. **Discovery:** principals, homes, PROPFIND, GET, well-known URLs. DAVx⁵ can see a calendar.
2. **Writes:** PUT, DELETE, MKCALENDAR, ETag / `If-Match`, multiget. Single-user CalDAV works.
3. **ical parser, RRULE expander and timezone resolution, with full tests** (milestone 2.5 in the chat; the biggest single piece). Time-range queries for VEVENT, VTODO and VJOURNAL.
4. **Sync:** sync-collection and ctag.
5. **CardDAV.**
6. **Sharing:** shares in `.meta.json`, the `can()` check, shared collections listed in the sharee's home.
7. **Scheduling:** attendee diffing, iMIP REQUEST and CANCEL (including per-instance), internal attendee copies and PARTSTAT merge-back.
8. **RSVP links:** HMAC tokens, confirmation page, POST, PARTSTAT update, organizer notification.
9. **Hardening:** client matrix tests, fuzzing, deployment behind a TLS-terminating reverse proxy.
10. **(Optional) Admin UI:** users, groups and shares via `html/template`.

Rough size: about 5–7k lines of Go.

## 12. Resolved questions (2026-10-23)

- **Receiving reply emails (option C):** no IMAP polling, no SMTP receiver, no mail parsing. At most, later, an inbound-mail webhook (e.g. Mailgun/Postmark posting the parsed `text/calendar` part).
- **Admin UI:** not now; editing `config.yaml` (reload on SIGHUP) is enough. Possible milestone 10.
- **TLS:** not in the binary. A reverse proxy (Caddy/nginx) terminates TLS; davd listens on plain HTTP and must honor `X-Forwarded-Proto`/`X-Forwarded-Host` when building absolute URLs (RSVP links, hrefs).

---

## Appendix: conversation log (condensed)

1. **User:** wants a Go service serving CalDAV and CardDAV; local backend, possibly plain files with no DB; calendar sharing between users, maybe by group; email invitations; 1–100 users; few dependencies, std lib preferred.
   **Assistant:** first draft: landscape, RFC table, URL layout, file storage, server-side sharing, iMIP sending with the `calendar-auto-schedule` duplicate gotcha, hard spots, package sketch, milestones. Asked about confirmations and VTODO.
2. **User:** wants invitation confirmations. Asked whether that works by webhook and whether clients show buttons with a webhook link behind them, and asked about VTODO and VJOURNAL.
   **Assistant:** native buttons send a REPLY email to the organizer, not a webhook. Recommended own HMAC RSVP links (GET page, POST submit, because of link scanners) plus implicit scheduling for internal users; deferred receiving reply mail. VTODO and VJOURNAL are nearly free on the server; listed client support.
3. **User:** recurring events are a must-have and have to work.
   **Assistant:** storage model (master plus overrides in one file), where the server must expand, a dependency-free RRULE expander spec, timezone handling (tzdata embed, Windows zone mapping, VTIMEZONE fallback), and a testing strategy. Added a milestone ahead of sync and scheduling.
4. **User:** export everything to markdown to continue on another machine (this file).
