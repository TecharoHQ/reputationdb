# reputationdb

The IP reputation database source for Anubis.

## Why is this open source?

Reputation databases are kind of a pain to implement in general. Fundamentally, they will be used to block people and by the nature of how the Internet works this will result in false-positives. When I was working on this originally, I made the sources and the database a closed-source project as I thought that would be one of the better ways to go about doing this.

After thought and introspection, I think a better way to do this is to make the database generator open source but make prebuilt databases paid. This allows public inspection of the data sources, contribution of better data sources, and centralization of the rather massive amount of compute required to build this database.

## API

`POST /api/v1/query` looks a batch of up to 100 IP addresses up in the database:

```text
curl -s localhost:3823/api/v1/query \
  -H 'Content-Type: application/json' \
  -d '{"ip_addresses":["1.2.3.4","2001:db8::1"]}'
```

```json
{
  "databaseCreatedAt": "2026-07-25T06:00:00Z",
  "records": [
    {
      "ipAddress": "1.2.3.4",
      "isDatacenter": true,
      "categories": ["datacenter"],
      "providers": ["datacentres"],
      "sources": [
        {
          "repository": "github.com/hexydec/ip-ranges",
          "list": "output/datacentres.txt",
          "provider": "datacentres",
          "category": "datacenter"
        }
      ]
    },
    {
      "ipAddress": "2001:db8::1"
    }
  ]
}
```

You get **one record per address you asked about, in the order you asked**, so
you can line the response up against your request without matching on anything.
Each `ipAddress` echoes the string you sent, and duplicate spellings of the same
address collapse to a single record. `databaseCreatedAt` is when the database
snapshot serving the query was published, so you can tell how stale the answer
is.

The second record above is what an address the database has nothing on looks
like: its `ipAddress` and nothing else. **The presence of a record tells you
nothing** — an empty `sources` (equivalently, an empty `categories`) is the
signal that an address is not listed.

One shape detail worth knowing before you parse this, verified against a running
server: **fields that are absent are false or empty.** A record for an address
that is only a datacentre address carries `isDatacenter` and no `isVpn`,
`isCrawler`, or `isProxy` key at all — not `"isVpn": false`. Likewise an
unlisted address carries no `categories`, `providers`, or `sources` keys rather
than empty arrays, so read a missing key as "empty" throughout.

The server keeps its own copy of the newest published database and swaps in a
newer one when it appears. On a cold start that copy takes a few minutes to
download, and until it lands `/api/v1/query` returns `503 Unavailable` rather
than reporting every address as clean.

This endpoint is not authenticated yet.

### Accessing the database builds

`reputationdbd` serves the published database versions. Three endpoints, all
under `/api/v1/database`:

```text
GET /api/v1/database                      # every retained version, newest first
GET /api/v1/database/{version_id}/info    # metadata for one version
GET /api/v1/database/{version_id}/fetch   # a presigned download URL for one version
```

A version ID is the unpadded URL-safe base64 SHA-512 of the uncompressed
database, which is also what names its object in the bucket. List the versions,
pick one, and fetch it:

```text
curl -s localhost:3823/api/v1/database
curl -s localhost:3823/api/v1/database/<version-id>/fetch
```

The `presignedUrl` in that response expires after an hour, so fetch it again
rather than caching the URL itself. `lifetime` is how long to wait before
checking for a newer version; the database is rebuilt daily.

`info` only answers for versions still in the ten-entry index. `fetch` also
serves versions that have aged out of it — their objects stay in the bucket —
but the response then carries only the version ID, because the provenance went
with the index entry.

These endpoints are not authenticated yet.

Anubis will automatically do this once $MILESTONE is reached.

### Free access to database builds

If you want free access to the database builds, please submit honeypot logs to this repo as a PR. See `cmd/mkdatabase/sources.go`'s `var fileSources` and put your IP addresses in `data/manually-submitted/<orgname>/<datetime.txt>`. Please note that for performance reasons the data in that folder is stored in git LFS.

## Free datacentre-only database

A datacentre-only build of the database is published for free as a rolling asset on the `v0.0.0` release:

```text
curl -LO https://github.com/TecharoHQ/reputationdb/releases/download/v0.0.0/datacenter.mmdb.zstd
zstd -d datacenter.mmdb.zstd
```

That asset gets overwritten on every build; it isn't versioned. The release page's upload timestamp is the only freshness signal that means anything here. Don't use `build_epoch` in the file's own metadata for that — it records the commit the `mkdatabase` binary was built from, not when the data was collected, and reading it as a freshness indicator will mislead you.

It has mmdb `database_type` `Techaro-Veil-Datacenter` and the same record schema as the full database, so whatever reader you write for one decodes the other without changes.

It only contains datacentre data. An address that's also a known VPN exit or abuse source shows up here as a datacentre address and nothing more — the combined reputation signal across categories is what the paid database sells.

Build it yourself with `npm run build:datacenter`, which runs `mkdatabase --category=datacenter`. `--category` also takes `abuse`, `crawler`, `proxy`, `tor`, and `vpn`, can be repeated to select more than one, and if you omit it you get everything, which is how the full database is built.

## Publishing databases

`cmd/publish-database` uploads a built database to Tigris:

```text
go build -o ./var/publish-database ./cmd/publish-database
./var/publish-database ./var/reputationdb.mmdb
```

It needs Tigris credentials in `.env`, either as `TIGRIS_STORAGE_ACCESS_KEY_ID` /
`TIGRIS_STORAGE_SECRET_ACCESS_KEY` or as the standard `AWS_*` variables. The
target bucket comes from `-tigris-storage-bucket` (or `TIGRIS_STORAGE_BUCKET`)
and defaults to `techaro-reputationdb`.

Every database is stored as a private, zstd-compressed object keyed by the
unpadded URL-safe base64 SHA-512 of its uncompressed contents:

```text
databases/<version-id>.mmdb.zst
versions.pb.gz
```

`versions.pb.gz` is a gzipped `techaro.lol.reputationdb.fetch.v1.ListResponse`
describing the ten most recent versions, newest first. Older versions age out of
that index, but their objects stay in the bucket so existing clients can still
fetch a version ID they already know.

## Self-hosting the API

`cmd/reputationdbd` serves the API:

```text
go build -o ./var/reputationdbd ./cmd/reputationdbd
./var/reputationdbd
```

It reads the same Tigris bucket `publish-database` writes to: both binaries
take the same `-tigris-storage-bucket` flag (or `TIGRIS_STORAGE_BUCKET` env
var), defaulting to `techaro-reputationdb`, so they point at one bucket by
default. It also needs the same Tigris credentials in `.env` and `-github-token`
(or `GITHUB_TOKEN`) to serve the free datacentre database, whose download URL
comes from the GitHub release rather than from Tigris.

`/api/v1/query` is served out of a local copy of the newest published database,
cached under `-database-cache-dir` (or `DATABASE_CACHE_DIR`), which defaults to
`reputationdb` under the system temporary directory. The full database is around
800 MiB uncompressed, so that path needs room for it. Point the flag at a
mounted volume if you'd rather not re-download it on every restart — the cache
is content-addressed, so an unchanged build is reused rather than fetched again.

It listens on `-bind` (`:3823`) for the API and `-metrics-bind` (`:9090`) for
Prometheus metrics and pprof. `GET /api/openapi.yaml` serves the OpenAPI
description of everything above.
