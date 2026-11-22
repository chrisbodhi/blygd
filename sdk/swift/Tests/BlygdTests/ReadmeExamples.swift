import Foundation
import OpenAPIRuntime

@testable import Blygd

// The examples in sdk/swift/README.md, compiled but never run, so the
// README cannot drift from the generated API. Edit both together.

private func show(_ html: String?) {}

private func readmeUsing(token: String) async throws {
    let client = Client(serverURL: URL(string: "https://blygd.example")!, token: token)

    let me = try await client.getMe().ok.body.json
    print(me.handle, me.origin)

    let draft = try await client.createItem(body: .json(.init(kind: .thread, contentMd: "Hello"))).created.body.json
    _ = try await client.publishItem(path: .init(id: draft.id)).ok

    let feed = try await client.readerFeed().ok.body.json
    for entry in feed.items ?? [] {
        switch entry.doc {
        case .item(let doc): show(doc.contentHtml)
        case .l0(let doc): show(doc.summaryHtml)
        case .withdrawn: break
        }
    }
    _ = try await client.markAllRead(body: .json(.init(asOf: feed.asOf)))
}

private func readmeErrors(client: Client, id: String, rev: String) async throws {
    switch try await client.publishItem(path: .init(id: id), body: .json(.init(draftRev: rev))) {
    case .ok(let ok): _ = ok
    case .conflict(let conflict): _ = conflict
    case .tooManyRequests(let refused): _ = refused.headers.retryAfter
    case let other: _ = other
    }
}

private func readmeEvents(client: Client) async throws {
    let events = try await client.streamEvents().ok.body.textEventStream.asDecodedServerSentEvents()
    for try await event in events {
        guard let type = event.event else { continue }
        _ = (type, event.data)
    }
}

private func readmeKeychain() {
    _ = Client(
        serverURL: URL(string: "https://blygd.example")!,
        configuration: .blygd,
        transport: StubTransport { _, _ in json("{}") },
        middlewares: [BearerTokenMiddleware { "from the keychain" }])
}
