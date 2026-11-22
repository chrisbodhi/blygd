# SDKs

Client libraries for blygd's `/api/`, generated from
`internal/web/openapi.json`, the hand-written description that the server's
tests hold to the router (`openapi_test.go`) and to every response the e2e
suite sees. The document is the one source. An SDK never gets its own copy.

| SDK | Directory | Status |
| --- | --- | --- |
| Swift | [`swift/`](swift/README.md) | generated with swift-openapi-generator; `Package.swift` at the repo root |

## How an SDK lives here

Each SDK follows the same pattern, so the server and its clients can't
drift apart:

1. **Generate from the document; commit the output.** Apps get plain
   source with no generator at build time, and every API change shows up
   in review as a diff to the SDK.
2. **Pin the generator** inside the SDK's directory, so the same document
   always generates the same code.
3. **Stamp the output** with the SHA-256 of the document it came from, and
   check the stamp from a Go test, so `make test` fails without that
   language's toolchain until the SDK is regenerated.
4. **Regenerate and diff** (`make sdk-check` for Swift) where the toolchain
   is available, to catch hand edits.
5. **Fail on anything the generator skips.** Generators drop what they
   cannot model and warn. Turn the warning into an error, and keep
   overrides listed in one place with a test that they are configured.
6. **Keep hand-written code small and separate** from the generated files:
   auth, date handling, and the types a generator gets wrong.
7. **Version with the server.** One `vX.Y.Z` tag covers blygd and its SDKs.

A package manager that must find its manifest at the repository root (as
SwiftPM does) gets that one file at the root, pointing into `sdk/<lang>/`.
