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

Fetch a presigned database download link by $TODO through the API.

Anubis will automatically do this once $MILESTONE is reached.

### Free access to database builds

If you want free access to the database builds, please submit honeypot logs to this repo as a PR. See `cmd/mkdatabase/sources.go`'s `var fileSources` and put your IP addresses in `data/manually-submitted/<orgname>/<datetime.txt>`. Please note that for performance reasons the data in that folder is stored in git LFS.

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

TODO: write this once the API server is written.
