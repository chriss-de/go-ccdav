# ccdav

A small self-hosted CalDAV and CardDAV server written in Go. It stores everything as plain files. It has two dependencies outside the standard library: the YAML parser for the config file (`go.yaml.in/yaml/v3`) and `golang.org/x/oauth2` for OIDC login. The design is in [`ccdav-design-export.md`](ccdav-design-export.md).

## Status

Milestones 1–7 are implemented:

- **Discovery:** `/.well-known/caldav` and `/.well-known/carddav`, principals, calendar and address book homes, and PROPFIND.
- **Writes:**
  - PUT, GET and DELETE with ETag, `If-Match` and `If-None-Match`.
  - MKCALENDAR and extended MKCOL.
  - PROPPATCH, all-or-nothing.
  - The `calendar-multiget` report.
- **iCalendar parsing, an RRULE expander and timezone resolution** (IANA, Windows zone names, embedded VTIMEZONE).
- **`calendar-query`:** comp, prop and param filters, `text-match`, and `time-range` on VEVENT, VTODO, VJOURNAL and VALARM, with recurrences expanded.
- **`calendar-data`:** partial retrieval and `expand`.
- **CardDAV (RFC 6352):** address books with `addressbook-query` (all match types and collations, `anyof`/`allof`, `limit`), `addressbook-multiget` and partial `address-data`. vCard 3.0 and 4.0 are stored exactly as uploaded, with no conversion between them. Contacts without a UID and vCard 2.1 are rejected. Contact groups are plain client data.
- **Sharing:** a calendar or address book can be shared with users or `config.yaml` groups, read-only or read-write. The shared collection appears automatically in the other user's home as `<owner>~<name>`. Name, colour and order are personal for each user who receives the share. When that user deletes the shared collection, it is only hidden for them. Groups are exposed as principals.
- **Web UI** (under `ui_prefix`, `/ui/` by default):
  - Login with OIDC (authorization code flow with PKCE, then the userinfo endpoint). Only users listed in `config.yaml` can log in. Without OIDC, the UI uses the `config.yaml` password.
  - App passwords for each device: named, revocable, shown only once, with the date each was last used.
  - Calendars and address books with item counts: rename them, change the colour (with a colour wheel), create and delete them (deleting asks for confirmation).
  - Export a collection as `.ics` or `.vcf`, and import such files. Entries whose UID already exists are updated instead of duplicated.
  - Sharing with users or groups, read-only or read-write.
  - For collections shared with you: your own name and colour, and hiding them from your devices.
- **Free/busy:** the `free-busy-query` report on calendars, with recurrences expanded. Transparent and cancelled events are left out.
- **Login protection:** failed logins (DAV and UI) are rate-limited per client IP and user. Behind a reverse proxy, set `trusted_proxies` so the real client IP is used for this and in the log.
- **Sync (RFC 6578):** the `sync-collection` report with `limit`, plus `DAV:sync-token` and `getctag`. Changes to calendar properties also move the token, so clients pick up renames and colour changes quickly.

The following are not implemented yet: scheduling and iMIP (7), and RSVP (8).

## Run

```sh
make run          # builds, seeds data/config.yaml from config.example.yaml (alice / alice), listens on :8080
make check        # gofmt check + vet + tests
make fuzz         # every fuzz target, FUZZTIME=30s each
```

Point a client at `http://localhost:8080/`. Discovery goes through `/.well-known/caldav` or `/.well-known/carddav`, both of which redirect to the DAV prefix (`/dav/` by default). Apart from those, `/healthz` and the configured prefixes, every path returns 404, including `/`. Each user starts with one calendar, `Personal` (events, tasks and journal), and the address book `contacts`.

## Configuration

Settings come from `config.yaml` in the data directory (`config.yml` and `config.json` are found too, in that order). See [`config.example.yaml`](config.example.yaml). Unknown keys are rejected, so typos fail loudly. Send the process `SIGHUP` to reload the file.

```yaml
base_url: https://dav.example.com
users:
  alice:
    password: <hash>
    email: alice@example.com
    name: Alice
groups:
  family: [alice]
```

### Sharing

```sh
ccdav share   alice/calendars/default group:family read
ccdav share   alice/addressbooks/contacts user:bob write
ccdav unshare alice/calendars/default group:family
ccdav shares  [alice]
```

These commands write the collection's `.meta.json`. A running server picks up the change on `SIGHUP`. The web UI does the same thing and applies it immediately.

### Web UI and OIDC

Open `<base_url><ui_prefix>`, for example `https://dav.example.com/ui/`. To use OIDC, add an `oidc:` block to the config (see `config.example.yaml`). In your identity provider, register `<base_url><ui_prefix>callback` as the redirect URI. A user logs in once, then creates an app password for each device and enters it in DAVx⁵ or Apple instead of their real password. Sessions are signed cookies; the signing key is created as `data/session.key`.

To create a password hash, run `make hash-password` or `ccdav hash-password`. Both read the password from stdin.

| Flag | Env | Default |
|---|---|---|
| `-data` | `DAVD_DATA` | `data` |
| `-listen` | `DAVD_LISTEN` | `:8080` |
| `-log` (`text`/`json`) | `DAVD_LOG` | `text` |

## Container

```sh
make image                      # ENGINE=podman by default; ENGINE=docker works too
make image-run                  # mounts ./data at /data, port 8080
```

The image uses distroless `static:nonroot`, keeps its data in the `/data` volume and has a `HEALTHCHECK` that runs `ccdav healthcheck`. The server speaks plain HTTP. TLS is meant to be terminated by a reverse proxy such as Caddy or nginx, and it should pass the `Authorization` header through.

## Layout

```
cmd/ccdav            main, hash-password, healthcheck
internal/config     config.yaml, SIGHUP reload
internal/auth       PBKDF2-SHA256 hashes, Basic auth cache
internal/store      file layout, atomic writes, ETags, change log, per-collection locks
internal/ical       content-line parser/encoder (iCalendar + vCard)
internal/rrule      RRULE expansion, timezone resolution, recurrence sets
internal/dav        XML parsing, multistatus writer
internal/web        web UI: login (OIDC/password), app passwords, collections, sharing
internal/caldav     validation, calendar-query filters, calendar-data / expand
internal/carddav    vCard validation, addressbook-query filters, address-data
internal/server     routing, resources, properties, HTTP methods
```
