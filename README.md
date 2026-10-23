# davd

A small self-hosted CalDAV server written in Go. It stores everything as plain files. Its only dependency outside the standard library is the YAML parser for the config file (`go.yaml.in/yaml/v3`). The design is in [`davd-design-export.md`](davd-design-export.md).

## Status

Milestones 1–3 are implemented:

- **Discovery:** `/.well-known/caldav`, principals, calendar homes and PROPFIND.
- **Writes:**
  - PUT, GET and DELETE with ETag, `If-Match` and `If-None-Match`.
  - MKCALENDAR and extended MKCOL.
  - PROPPATCH, all-or-nothing.
  - The `calendar-multiget` report.
- **iCalendar parsing, an RRULE expander and timezone resolution** (IANA, Windows zone names, embedded VTIMEZONE).
- **`calendar-query`:** comp, prop and param filters, `text-match`, and `time-range` on VEVENT, VTODO, VJOURNAL and VALARM, with recurrences expanded.
- **`calendar-data`:** partial retrieval and `expand`.

The following are not implemented yet: sync-collection (milestone 4), CardDAV (5), sharing (6), scheduling and iMIP (7), and RSVP (8).

## Run

```sh
make run          # builds, seeds data/config.yaml from config.example.yaml (alice / alice), listens on :8080
make check        # gofmt check + vet + tests
make fuzz         # every fuzz target, FUZZTIME=30s each
```

Point a client at `http://localhost:8080/`. Discovery goes through `/.well-known/caldav`, which redirects to `/dav/`.

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

To create a password hash, run `make hash-password` or `davd hash-password`. Both read the password from stdin.

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

The image uses distroless `static:nonroot`, keeps its data in the `/data` volume and has a `HEALTHCHECK` that runs `davd healthcheck`. The server speaks plain HTTP. TLS is meant to be terminated by a reverse proxy such as Caddy or nginx, and it should pass the `Authorization` header through.

## Layout

```
cmd/davd            main, hash-password, healthcheck
internal/config     config.yaml, SIGHUP reload
internal/auth       PBKDF2-SHA256 hashes, Basic auth cache
internal/store      file layout, atomic writes, ETags, change log, per-collection locks
internal/ical       content-line parser/encoder (iCalendar + vCard)
internal/rrule      RRULE expansion, timezone resolution, recurrence sets
internal/dav        XML parsing, multistatus writer
internal/caldav     validation, calendar-query filters, calendar-data / expand
internal/server     routing, resources, properties, HTTP methods
```
