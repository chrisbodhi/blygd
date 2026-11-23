import Foundation
import HTTPTypes
import OpenAPIRuntime
import Testing

@testable import Blygd

/// Answers every request with `respond`, so a test sees what the client sent.
struct StubTransport: ClientTransport {
    var respond: @Sendable (HTTPRequest, HTTPBody?) async throws -> (HTTPResponse, HTTPBody?)

    func send(_ request: HTTPRequest, body: HTTPBody?, baseURL: URL, operationID: String) async throws
        -> (HTTPResponse, HTTPBody?)
    {
        try await respond(request, body)
    }
}

func json(_ text: String) -> (HTTPResponse, HTTPBody?) {
    (HTTPResponse(status: .ok, headerFields: [.contentType: "application/json"]), HTTPBody(text))
}

let server = URL(string: "https://blygd.example")!

@Suite struct ClientTests {
    @Test func sendsTheBearerToken() async throws {
        let client = Client(
            serverURL: server, token: "tok_123",
            transport: StubTransport { request, _ in
                #expect(request.path == "/api/me")
                #expect(request.headerFields[.authorization] == "Bearer tok_123")
                return json(
                    #"{"handle": "ada", "scopes": ["*"], "origin": "https://blygd.example/~ada/", "expires_at": null}"#)
            })
        let me = try await client.getMe().ok.body.json
        #expect(me.handle == "ada")
        #expect(me.scopes == [.all])
        #expect(me.expiresAt == nil)
    }

    @Test func sendsNoTokenWithoutOne() async throws {
        let client = Client(
            serverURL: server, token: nil,
            transport: StubTransport { request, _ in
                #expect(request.headerFields[.authorization] == nil)
                return json(#"{"name": "Example", "origin_template": "https://blygd.example/~{handle}/"}"#)
            })
        let instance = try await client.getInstance().ok.body.json
        #expect(instance.originTemplate == "https://blygd.example/~{handle}/")
    }

    @Test func asksTheProviderBeforeEachRequest() async throws {
        let tokens = Tokens()
        let client = Client(
            serverURL: server,
            transport: StubTransport { request, _ in
                #expect(request.headerFields[.authorization] == "Bearer t\(await tokens.served)")
                return json(#"{"name": "Example", "origin_template": "https://blygd.example/~{handle}/"}"#)
            },
            middlewares: [BearerTokenMiddleware { await tokens.next() }])
        _ = try await client.getInstance()
        _ = try await client.getInstance()
        #expect(await tokens.served == 2)
    }

    // A ReaderItem whose doc is null (the origin withdrew it) decodes, as do
    // timestamps with and without fractional seconds.
    @Test func decodesAWithdrawnReaderItem() async throws {
        let client = Client(
            serverURL: server, token: "tok",
            transport: StubTransport { request, _ in
                #expect(request.path == "/api/reader/items/7/01jd0000000000000000000000")
                return json(
                    """
                    {"notice": "imported", "item": {
                      "subscription_id": 7, "origin": "https://other.example/", "source": "blyg",
                      "item_id": "01jd0000000000000000000000", "kind": "withdrawn", "l0": false,
                      "state": "withdrawn", "version": 3, "page": null, "claimed_updated": null,
                      "first_seen_at": "2026-11-22T09:14:03.120417Z", "sort_at": "2026-11-22T09:14:03Z",
                      "read": false, "discrepancy": null, "doc": null}}
                    """)
            })
        let item = try await client.readerItem(path: .init(sid: 7, iid: "01jd0000000000000000000000")).ok.body.json.item
        #expect(item.doc == .withdrawn)
        #expect(item.state == .withdrawn)
        #expect(abs(item.firstSeenAt.timeIntervalSince(item.sortAt) - 0.120417) < 1e-6)
    }
}

@Suite struct EventTests {
    // The README's example. blygd opens with `retry` and a `: connected`
    // comment and pings with `: ping` (internal/web/events_api.go); the
    // runtime yields each as an event with no type, so a client skips those.
    @Test func readsTheEventStream() async throws {
        let client = Client(
            serverURL: server, token: "tok",
            transport: StubTransport { request, _ in
                #expect(request.path == "/api/events")
                return (
                    HTTPResponse(status: .ok, headerFields: [.contentType: "text/event-stream"]),
                    HTTPBody(
                        "retry: 3000\n: connected\n\n"
                            + "event: item\ndata: {\"id\": \"01jd0000000000000000000000\", \"change\": \"published\"}\n\n"
                            + ": ping\n\n")
                )
            })
        var seen: [(String, String?)] = []
        let events = try await client.streamEvents().ok.body.textEventStream.asDecodedServerSentEvents()
        for try await event in events {
            guard let type = event.event else { continue }  // connected, ping
            seen.append((type, event.data))
        }
        #expect(seen.count == 1)
        #expect(seen.first?.0 == "item")
        #expect(seen.first?.1?.contains("published") == true)
    }
}

actor Tokens {
    var served = 0
    func next() -> String {
        served += 1
        return "t\(served)"
    }
}

@Suite struct DateTests {
    let transcoder = BlygdDateTranscoder()

    @Test func readsEveryFormGoWrites() throws {
        let whole = try transcoder.decode("2026-11-22T09:14:03Z")
        #expect(abs(try transcoder.decode("2026-11-22T09:14:03.120417Z").timeIntervalSince(whole) - 0.120417) < 1e-6)
        #expect(abs(try transcoder.decode("2026-11-22T09:14:03.123456789Z").timeIntervalSince(whole) - 0.123456789) < 1e-6)
        #expect(abs(try transcoder.decode("2026-11-22T10:14:03.5+01:00").timeIntervalSince(whole) - 0.5) < 1e-6)
    }

    // An as_of sent back to POST /api/reader/read must be the one received,
    // to the microsecond.
    @Test(arguments: [
        ("2026-11-22T09:14:03Z", "2026-11-22T09:14:03Z"),
        ("2026-11-22T09:14:03.120417Z", "2026-11-22T09:14:03.120417Z"),
        ("2026-11-22T09:14:03.000001Z", "2026-11-22T09:14:03.000001Z"),
        ("2026-11-22T09:14:03.5Z", "2026-11-22T09:14:03.5Z"),
        ("2026-11-22T09:14:03.123456789Z", "2026-11-22T09:14:03.123457Z"),
        ("2026-11-22T09:14:03.9999999Z", "2026-11-22T09:14:04Z"),
    ])
    func roundTripsToTheMicrosecond(_ input: String, _ written: String) throws {
        #expect(try transcoder.encode(transcoder.decode(input)) == written)
    }

    // Why the SDK has its own transcoder: the runtime's default reads only
    // whole seconds, and its fractional one keeps only milliseconds.
    @Test func theRuntimesTranscodersLosePrecision() throws {
        #expect(throws: (any Error).self) { try ISO8601DateTranscoder.iso8601.decode("2026-11-22T09:14:03.120417Z") }
        let fractional = ISO8601DateTranscoder.iso8601WithFractionalSeconds
        #expect(try fractional.encode(fractional.decode("2026-11-22T09:14:03.120417Z")) != "2026-11-22T09:14:03.120417Z")
    }

    @Test func rejectsWhatIsNotATimestamp() {
        for bad in ["", "yesterday", "2026-11-22T09:14:03.Z", "2026-11-22"] {
            #expect(throws: (any Error).self, "\(bad)") { try transcoder.decode(bad) }
        }
    }
}

@Suite struct ImportedDocumentTests {
    func decode(_ text: String) throws -> ImportedDocument {
        try JSONDecoder().decode(ImportedDocument.self, from: Data(text.utf8))
    }

    @Test func nullIsWithdrawn() throws {
        #expect(try decode("null") == .withdrawn)
        #expect(String(decoding: try JSONEncoder().encode(ImportedDocument.withdrawn), as: UTF8.self) == "null")
    }

    @Test func l0ByKind() throws {
        let doc = try decode(
            #"{"kind": "l0", "summary_html": "<p>hi</p>", "summary_text": "hi", "feed": "https://f.example/feed.xml", "note": "wrapped"}"#
        )
        guard case .l0(let l0) = doc else {
            Issue.record("decoded \(doc)")
            return
        }
        #expect(l0.summaryText == "hi")
    }

    @Test func itemByKind() throws {
        let doc = try decode(#"{"id": "01jd0000000000000000000000", "kind": "thread", "version": 2}"#)
        guard case .item(let item) = doc else {
            Issue.record("decoded \(doc)")
            return
        }
        #expect(item.kind == .thread)
        #expect(item.version == 2)
    }

    // Choosing by kind means a broken document reports its own error.
    @Test func aBrokenL0IsNotReadAsAnItem() {
        #expect(throws: DecodingError.self) { try decode(#"{"kind": "l0", "summary_html": "<p>hi</p>"}"#) }
    }

    @Test func roundTrips() throws {
        let original = try decode(#"{"id": "01jd0000000000000000000000", "kind": "fragment", "version": 1}"#)
        let again = try decode(String(decoding: try JSONEncoder().encode(original), as: UTF8.self))
        #expect(again == original)
    }
}
