# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

<!-- This changes the project to: -->

- `reputationdbd` now serves `POST /api/v1/query`, looking a batch of up to 100
  IP addresses up in a local copy of the newest published database. The copy is
  refreshed in the background and swapped in atomically; queries return
  `Unavailable` until the first one has downloaded.
- Fixed the `QueryRequest.ip_addresses` validation rule, which was applied to
  the whole list rather than to each item and so rejected every request.
