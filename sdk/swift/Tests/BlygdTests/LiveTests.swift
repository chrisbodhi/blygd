import Foundation
import Testing

@testable import Blygd

/// Against a running blygd: `make sdk-test-live` starts one, or point these
/// at your own with BLYGD_SDK_TEST_URL (the instance root) and
/// BLYGD_SDK_TEST_TOKEN (an owner token for a user you can write to). It
/// must read its own origin, so run it with BLYGD_ALLOW_PRIVATE_FETCH=true
/// on localhost.
let liveURL = ProcessInfo.processInfo.environment["BLYGD_SDK_TEST_URL"].flatMap(URL.init(string:))
let liveToken = ProcessInfo.processInfo.environment["BLYGD_SDK_TEST_TOKEN"]

@Suite(.enabled(if: liveURL != nil && liveToken != nil, "set BLYGD_SDK_TEST_URL and BLYGD_SDK_TEST_TOKEN"))
struct LiveTests {
    let client = Client(serverURL: liveURL ?? server, token: liveToken)

    @Test func instanceAndMe() async throws {
        let anonymous = Client(serverURL: liveURL ?? server, token: nil)
        let instance = try await anonymous.getInstance().ok.body.json
        #expect(instance.originTemplate.contains("{handle}"))

        let me = try await client.getMe().ok.body.json
        #expect(me.origin.contains(me.handle))
        let tokens = try await client.listTokens().ok.body.json.tokens ?? []
        #expect(!tokens.isEmpty)
    }

    // Publish, read it back through a subscription to our own origin, mark it
    // read with the feed's as_of, then withdraw it and see the null doc.
    @Test func publishReadAndWithdraw() async throws {
        let me = try await client.getMe().ok.body.json
        let marker = "swift sdk \(UUID().uuidString)"

        let draft = try await client.createItem(body: .json(.init(kind: .fragment, contentMd: marker))).created.body.json
        let published = try await client.publishItem(path: .init(id: draft.id)).ok.body.json
        #expect(published.version.version == 1)
        let items = try await client.listItems().ok.body.json.items
        #expect(items.contains { $0.id == draft.id })

        let sid = try await subscription(to: me.origin)
        try await poll(sid)

        let feed = try await client.readerFeed(query: .init(subscription: sid)).ok.body.json
        let entry = try #require((feed.items ?? []).first { $0.itemId == draft.id })
        guard case .item(let doc) = entry.doc else {
            Issue.record("doc is \(entry.doc)")
            return
        }
        #expect(doc.contentMd == marker)

        // The as_of goes back as it came, to the microsecond.
        let marked = try await client.markAllRead(body: .json(.init(asOf: feed.asOf, subscriptionId: sid))).ok.body.json
        #expect(marked.marked >= 1)
        let read = try await client.readerItem(path: .init(sid: sid, iid: draft.id)).ok.body.json.item
        #expect(read.read)

        _ = try await client.withdrawItem(path: .init(id: draft.id)).ok
        try await poll(sid)
        let withdrawn = try await client.readerItem(path: .init(sid: sid, iid: draft.id)).ok.body.json.item
        #expect(withdrawn.doc == .withdrawn)
    }

    /// Polls now, waiting out a short manual poll floor. A long one fails:
    /// serve with BLYGD_MANUAL_POLL_FLOOR=1, as scripts/swift-sdk-live.sh does.
    func poll(_ sid: Int) async throws {
        for _ in 0..<5 {
            switch try await client.pollSubscription(path: .init(sid: sid)) {
            case .ok:
                return
            case .tooManyRequests(let refused) where (refused.headers.retryAfter ?? .max) <= 5:
                try await Task.sleep(for: .seconds(refused.headers.retryAfter ?? 1))
            case let other:
                Issue.record("poll: \(other); is BLYGD_MANUAL_POLL_FLOOR above 5?")
                return
            }
        }
    }

    func subscription(to origin: String) async throws -> Int {
        let subscriptions = try await client.listSubscriptions().ok.body.json.subscriptions ?? []
        if let existing = subscriptions.first(where: { $0.origin == origin }) {
            return existing.id
        }
        return try await client.subscribe(body: .json(.init(url: origin))).created.body.json.subscription.id
    }
}
