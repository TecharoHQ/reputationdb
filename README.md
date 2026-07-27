# reputationdb

The IP reputation database source for Anubis.

## Why is this open source?

Reputation databases are kind of a pain to implement in general. Fundamentally, they will be used to block people and by the nature of how the Internet works this will result in false-positives. When I was working on this originally, I made the sources and the database a closed-source project as I thought that would be one of the better ways to go about doing this.

After thought and introspection, I think a better way to do this is to make the database generator open source but make prebuilt databases paid. This allows public inspection of the data sources, contribution of better data sources, and centralization of the rather massive amount of compute required to build this database.

## API

If you have a Thoth API key, you can use it to query entries at $TODO.techaro.lol:

```text
TODO: curl example once the API service is functional
```

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

It listens on `-bind` (`:3823`) for the API and `-metrics-bind` (`:9090`) for
Prometheus metrics and pprof. `GET /api/openapi.yaml` serves the OpenAPI
description of everything above.
