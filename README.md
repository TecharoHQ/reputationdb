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

## Self-hosting the API

TODO: write this once the API server is written.
