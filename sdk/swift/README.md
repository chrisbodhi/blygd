# Blygd for Swift

A Swift client for blygd's `/api/`, generated from
[`internal/web/openapi.json`](../../internal/web/openapi.json) by
[swift-openapi-generator](https://github.com/apple/swift-openapi-generator).
It covers every operation in the document, typed: requests, responses,
error bodies, the event stream, media uploads and the export download.

## Adding it

In `Package.swift`:

```swift
.package(url: "https://github.com/chrisbodhi/blygd", branch: "main"),
// …
.product(name: "Blygd", package: "blygd"),
```

In Xcode: File → Add Package Dependencies…, and the same URL.

Use `branch: "main"` until blygd tags a release. Releases are tagged
`vX.Y.Z` (SwiftPM reads the `v`), and one tag versions the server and the
SDK together, since both come from the same document. Before 1.0, pin with
`.upToNextMinor(from: "0.5.0")`. SwiftPM's `from:` allows every 0.x
release up to 1.0, but a 0.x minor release of blygd may change the API.

The package has no build plugin, so Xcode asks you to trust nothing, and
your app depends only on `swift-openapi-runtime`,
`swift-openapi-urlsession` and `swift-http-types`.

## Using it

```swift
import Blygd

let client = Client(serverURL: URL(string: "https://blygd.example")!, token: token)

let me = try await client.getMe().ok.body.json
print(me.handle, me.origin)            // origin: https://blygd.example/~ada/

let draft = try await client.createItem(body: .json(.init(kind: .thread, contentMd: "Hello"))).created.body.json
_ = try await client.publishItem(path: .init(id: draft.id)).ok

let feed = try await client.readerFeed().ok.body.json
for entry in feed.items ?? [] {
    switch entry.doc {
    case .item(let doc): show(doc.contentHtml)   // another origin's HTML: sanitize it (§14)
    case .l0(let doc): show(doc.summaryHtml)     // likewise
    case .withdrawn: break
    }
}
// Mark read what you showed, and nothing that arrived since.
_ = try await client.markAllRead(body: .json(.init(asOf: feed.asOf)))
```

`serverURL` is the instance's root, not a blyg's `~handle` path. Pass
`token: nil` for the three operations that take none (`getInstance`,
`redeemInvite`, `getOpenAPI`). To read the token from the keychain on each
request, build the client with a `BearerTokenMiddleware { … }`, and pass
`configuration: .blygd` if you use the generated initializer yourself.

Every response is an enum of its documented statuses. `.ok` and the other
status accessors throw when the response was a different one. Switch on
the output to handle errors:

```swift
switch try await client.publishItem(path: .init(id: id), body: .json(.init(draftRev: rev))) {
case .ok(let ok): …
case .conflict(let conflict): …       // 409 stale: the draft moved on
case .tooManyRequests(let refused): … // refused.headers.retryAfter
case let other: …
}
```

Live updates come from the event stream. blygd opens it with a
`: connected` comment and sends `: ping` every 25 seconds, and the runtime
yields each of those as an event with no type, so skip them:

```swift
let events = try await client.streamEvents().ok.body.textEventStream.asDecodedServerSentEvents()
for try await event in events {
    guard let type = event.event else { continue }
    // type is import, subscribe, item, mention…; event.data is its JSON
}
```

### What the SDK adds to the generated code

Two files are hand-written. The rest of `Sources/Blygd` is generated.

- **`Blygd.swift`** has the token initializer, `BearerTokenMiddleware`, and
  `BlygdDateTranscoder`. That transcoder reads timestamps to the
  microsecond. The runtime's default rejects fractional seconds, and its
  fractional option keeps only milliseconds, which would make an `as_of`
  sent back to `markAllRead` miss what arrived in the lost microseconds.
- **`ImportedDocument.swift`** replaces the document's `ImportedDoc` schema
  (null, a blyg document, or an L0 wrapper). The generator cannot model
  the null branch. Without this type it would drop the `doc` of every
  reader item.

### Things to know

- **Lists can be `nil`.** Where the document says a list may be `null`
  (`"type": ["array", "null"]`), the Swift property is optional:
  `feed.items ?? []`.
- **Enums are closed.** A value the SDK doesn't know, such as a new scope or
  a new item `kind`, fails to decode the whole response. Update the SDK
  along with the server.

## Keeping it current

The generated code is committed, and it is regenerated from the document,
never edited by hand.

| What | Where |
| --- | --- |
| The API description, the one source | `internal/web/openapi.json` |
| Generator config: overrides, scope names | `sdk/swift/openapi-generator-config.yaml` |
| Generator version (pinned) | `sdk/swift/Generator/Package.swift`, `Package.resolved` |
| Document prep for the generator | `sdk/swift/gendoc` (Go) |
| Generated client (committed) | `sdk/swift/Sources/Blygd/Generated/` |
| Hand-written additions | `sdk/swift/Sources/Blygd/*.swift` |
| Tests | `sdk/swift/Tests/BlygdTests/` |

```sh
make sdk            # regenerate after editing openapi.json (needs Swift 6)
make sdk-check      # the committed code is what the document generates
make sdk-test       # sdk-check, then swift test
make sdk-test-live  # the SDK against this checkout served on localhost
```

Several checks keep the SDK in step with the server:

- Each generated file records the SHA-256 of the document it came from.
  `TestSwiftSDKIsCurrent` (`internal/web/sdk_test.go`) runs in `make test`
  without Swift, and fails until a change to `openapi.json` is followed by
  `make sdk`.
- `make sdk-check` regenerates the code and diffs it against what is
  committed, so a hand edit or a stale regeneration fails too.
- The generator skips any schema it cannot model with only a warning.
  `scripts/swift-sdk.sh` turns that warning into a failure, so a field can't
  quietly disappear from the SDK. When it fires, reshape the schema, or add
  an override: a `typeOverrides` entry, a line in `gendoc`'s `overridden`,
  and a Swift type. The gendoc tests check that all three agree.
- Each scope needs a Swift name in `nameOverrides` (`TestScopesAreNamed`).
  Without one it would ship as `items_colon_read`, and renaming a case later
  breaks the apps that use it.

To move the generator to a new version, edit the version in
`Generator/Package.swift`, run `swift package --package-path
sdk/swift/Generator update`, run `make sdk`, and review the diff.
