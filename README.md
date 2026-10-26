# ccdav

A small self-hosted CalDAV and CardDAV server in Go. Calendars and contacts are stored as plain files, with no database. Besides the standard library, it uses only `go.yaml.in/yaml/v3` and `golang.org/x/oauth2`. It includes a web UI for app passwords, collections, import/export and sharing. Tested against Apple Calendar/Contacts, Thunderbird, DAVx⁵ and Evolution.

## Features

- Calendars (events, tasks, journals) and address books, with recurrences, timezones and free/busy.
- Efficient sync: clients fetch only what changed.
- Sharing with users or groups, read-only or read-write. Shared collections appear in the other user's home as `<owner>~<name>`.
- A generated, read-only `Directory` address book in every user's home, listing all users and groups.
- Web UI login with OIDC or password. Devices use named, revocable app passwords.
- Login rate limiting per client IP and user. Plain HTTP; TLS is left to a reverse proxy.

Not implemented yet: scheduling/iMIP and RSVP links. The design is in [`ccdav-design-export.md`](ccdav-design-export.md).

## Standards

| RFC | What | Notes |
|---|---|---|
| [4918](https://www.rfc-editor.org/rfc/rfc4918) | WebDAV | class 1 and 3, no locking |
| [3744](https://www.rfc-editor.org/rfc/rfc3744) | WebDAV ACL | read-only privilege and principal properties |
| [5397](https://www.rfc-editor.org/rfc/rfc5397) | `current-user-principal` | |
| [5689](https://www.rfc-editor.org/rfc/rfc5689) | Extended MKCOL | |
| [6578](https://www.rfc-editor.org/rfc/rfc6578) | `sync-collection` | plus CalendarServer `getctag` |
| [6764](https://www.rfc-editor.org/rfc/rfc6764) | `/.well-known/caldav`, `/.well-known/carddav` | redirect only, no DNS SRV |
| [4791](https://www.rfc-editor.org/rfc/rfc4791) | CalDAV | `calendar-query`, `calendar-multiget`, `free-busy-query`, `expand`, MKCALENDAR |
| [5545](https://www.rfc-editor.org/rfc/rfc5545) | iCalendar | RRULE expansion; IANA, Windows and embedded VTIMEZONE zones |
| [6352](https://www.rfc-editor.org/rfc/rfc6352) | CardDAV | `addressbook-query`, `addressbook-multiget`, partial `address-data` |
| [6350](https://www.rfc-editor.org/rfc/rfc6350) / [2426](https://www.rfc-editor.org/rfc/rfc2426) | vCard 4.0 / 3.0 | stored as uploaded; vCard 2.1 is rejected |
| [6868](https://www.rfc-editor.org/rfc/rfc6868) | Parameter value encoding | |
| [7636](https://www.rfc-editor.org/rfc/rfc7636) | PKCE | OIDC login of the web UI |

**JMAP:** in progress, off by default (`jmap: true`). The [core protocol](https://www.rfc-editor.org/rfc/rfc8620) works (session, result references, blobs), and so do [contacts](https://www.rfc-editor.org/rfc/rfc9610): `AddressBook/get`, `/changes` and `/set`; `ContactCard/get`, `/changes`, `/query`, `/set` and `/copy`. vCards stay the stored format and are converted to and from [JSContact](https://www.rfc-editor.org/rfc/rfc9553) ([RFC 9555](https://www.rfc-editor.org/rfc/rfc9555)). A JMAP edit rewrites only the vCard lines it changes, so what DAV clients stored (Apple's `X-` properties, groups, labels) stays. New cards are written as vCard 3.0. JSContact data a vCard can't hold goes into `X-JSPROP` lines (`JSPROP` in vCard 4), and entry ids into `X-PROP-ID`/`PROP-ID` parameters. A DAV client that rewrites the card may drop those. Not yet: push, sharing via `shareWith`, and [calendars](https://datatracker.ietf.org/doc/draft-ietf-jmap-calendars/). App passwords work for JMAP as they do for DAV.

## Quick start

```sh
make run      # seeds data/config.yaml (user alice / password alice), listens on :8080
make check    # gofmt + vet + tests
make fuzz     # all fuzz targets, FUZZTIME=30s each
```

Point a client at `http://localhost:8080/`; discovery goes through `/.well-known/`. The web UI is at `/ui/`.

## Configuration

### Flags

| Flag | Env | Default | |
|---|---|---|---|
| `-config FILE` | `CCDAV_CONFIG` | `config.yaml` in the data directory | `config.yml` and `config.json` are found too |
| `-data DIR` | `CCDAV_DATA` | `data_dir`, else the config's directory | without `-config`, the config is looked for in `data` |
| `-listen ADDR` | `CCDAV_LISTEN` | `:8080` | |
| `-log FORMAT` | `CCDAV_LOG` | `text` | `text` or `json` |
| `-trace DIR` | `CCDAV_TRACE` | off | records every DAV request and response, including calendar data; for debugging only |

### `config.yaml`

Unknown keys are rejected. `SIGHUP` reloads the file. See [`config.example.yaml`](config.example.yaml).

| Key | Default | |
|---|---|---|
| `data_dir` | the config's directory | collections, `session.key`, app passwords; relative to the config file; restart required |
| `dav_prefix` | `/dav/` | URL path of the DAV tree; restart required |
| `ui_prefix` | `/ui/` | URL path of the web UI; must not overlap `dav_prefix`; restart required |
| `jmap` | `false` | serve JMAP under `jmap_prefix` and `/.well-known/jmap`; restart required |
| `jmap_prefix` | `/jmap/` | URL path of the JMAP API; must not overlap the other prefixes; restart required |
| `base_url` | | public URL, e.g. `https://dav.example.com`; needed for OIDC |
| `trusted_proxies` | | CIDRs or addresses whose `X-Forwarded-*` headers are trusted |
| `users.<name>.password` | | hash from `ccdav hash-password`; fallback for DAV, and UI login without OIDC |
| `users.<name>.email`, `.name` | | used for OIDC matching and the directory |
| `groups.<name>` | | list of user names; groups are share targets and principals |
| `directory` | `true` | `false` removes the generated `Directory` address book |
| `oidc.issuer`, `.client_id` | | required for OIDC login; redirect URI is `<base_url><ui_prefix>callback` |
| `oidc.client_secret` | | |
| `oidc.username_claim` | `preferred_username` | must equal a user name or email |
| `oidc.scopes` | `openid profile email` | |
| `oidc.name` | `single sign-on` | login button label |
| `smtp.*`, `rsvp_secret` | | reserved for invitations, not used yet |

### Sharing from the command line

```sh
ccdav share   alice/calendars/personal group:family read
ccdav share   alice/addressbooks/contacts user:bob write
ccdav unshare alice/calendars/personal group:family
ccdav shares  [alice]
```

The web UI does the same. A running server picks up command-line changes on `SIGHUP`.

## Container

```sh
make image                                               # podman; ENGINE=docker works too
make image-run                                           # ./data mounted at /data, port 8080
make image-login   REGISTRY=registry.example.com/me
make image-release REGISTRY=registry.example.com/me TAG=1.2.0   # pushes :1.2.0 and :latest
```

The image is based on distroless `static:nonroot` and runs as UID 65532, which matters when you map UIDs. The config is expected at `/data/config.yaml`. There is no built-in healthcheck; use `ccdav healthcheck` as an exec probe.

CI builds `linux/amd64` and `linux/arm64` images and publishes them to Docker Hub: `edge` from `master`, and semver tags from `v*`.

### Reverse proxy

Add the proxy to `trusted_proxies`. Caddy works as is (`reverse_proxy ccdav:8080`). With nginx, set `Host`, `X-Forwarded-For` and `X-Forwarded-Proto`, set `client_max_body_size 60m` (imports go up to 50 MB) and `proxy_buffering off`. Don't strip `Authorization`.
