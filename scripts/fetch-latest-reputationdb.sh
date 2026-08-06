#!/usr/bin/env bash

set -euo pipefail

reputationdb_version="$(curl -qfSsL https://maat.probably-not-malware.lol/api/v1/database | jq '.versions[0].versionId' -r)"
presigned_url="$(curl -qfSsL https://maat.probably-not-malware.lol/api/v1/database/${reputationdb_version}/fetch | jq '.presignedUrl' -r)"
curl "${presigned_url}" -o ./var/reputationdb.mmdb.zstd
zstd -d ./var/reputationdb.mmdb.zstd
