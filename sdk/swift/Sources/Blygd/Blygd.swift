// Hand-written conveniences over the generated client (Generated/). The
// generated code is rewritten by scripts/swift-sdk.sh; this file is not.

import Foundation
import HTTPTypes
import OpenAPIRuntime
import OpenAPIURLSession

extension Client {
    /// A client for the blygd instance at `serverURL`, authenticating with a
    /// bearer token.
    ///
    /// `serverURL` is the instance's root, such as `https://blygd.example`,
    /// not a blyg's `~handle` path: every operation's path starts `/api/`.
    /// Pass `token: nil` for the operations that take none
    /// (`getInstance`, `redeemInvite`, `getOpenAPI`).
    public init(
        serverURL: URL,
        token: String?,
        transport: any ClientTransport = URLSessionTransport(),
        middlewares: [any ClientMiddleware] = []
    ) {
        let auth: [any ClientMiddleware] = token.map { [BearerTokenMiddleware(token: $0)] } ?? []
        self.init(
            serverURL: serverURL,
            configuration: .blygd,
            transport: transport,
            middlewares: auth + middlewares
        )
    }
}

extension Configuration {
    /// The configuration blygd's responses need: dates with or without
    /// fractional seconds (see ``BlygdDateTranscoder``).
    public static var blygd: Configuration {
        Configuration(dateTranscoder: BlygdDateTranscoder())
    }
}

/// Reads and writes RFC 3339 timestamps to the microsecond.
///
/// Go writes a timestamp with as many fractional digits as it has (up to
/// nine) and none when they are zero: `2026-11-22T09:14:03.120417Z`,
/// `2026-11-22T09:14:03Z`. The runtime's default transcoder rejects the
/// first form, and its fractional one keeps only milliseconds. Neither will
/// do for a timestamp a client sends back. `POST /api/reader/read` marks
/// read what was seen at or before the feed's `as_of`, so an `as_of` cut to
/// the millisecond leaves unread what arrived in the cut-off part.
///
/// So this reads any number of fractional digits, and writes up to six
/// (microseconds, what Postgres keeps), dropping trailing zeros as Go does.
public struct BlygdDateTranscoder: DateTranscoder {
    private let whole: ISO8601DateTranscoder = .iso8601

    public init() {}

    public func decode(_ string: String) throws -> Date {
        // Split "…:SS.ffffff<zone>" into "…:SS<zone>" and ".ffffff".
        guard let dot = string.firstIndex(of: "."), let t = string.firstIndex(of: "T"), t < dot else {
            return try whole.decode(string)
        }
        let digits = string[string.index(after: dot)...].prefix { $0.isASCII && $0.isNumber }
        guard !digits.isEmpty, let fraction = Double("0." + digits) else {
            return try whole.decode(string)  // malformed: let the runtime say so
        }
        let base = try whole.decode(String(string[..<dot] + string[digits.endIndex...]))
        return base.addingTimeInterval(fraction)
    }

    public func encode(_ date: Date) throws -> String {
        let micros = (date.timeIntervalSince1970 * 1_000_000).rounded()
        let seconds = (micros / 1_000_000).rounded(.down)
        let fraction = Int(micros - seconds * 1_000_000)
        let base = try whole.encode(Date(timeIntervalSince1970: seconds))
        guard fraction > 0 else {
            return base
        }
        guard base.hasSuffix("Z") else {  // not UTC: unexpected, so keep what we can
            return try ISO8601DateTranscoder.iso8601WithFractionalSeconds.encode(date)
        }
        var digits = String(fraction)
        digits = String(repeating: "0", count: 6 - digits.count) + digits
        while digits.hasSuffix("0") {
            digits.removeLast()
        }
        return base.dropLast() + "." + digits + "Z"
    }
}

/// Sends `Authorization: Bearer <token>` with every request.
///
/// A token is minted by `blygd user add`, `blygd token mint`,
/// `POST /api/tokens`, or, as an owner token, `POST /api/invites/redeem`.
/// Each operation needs one scope; the owner token `*` holds them all.
public struct BearerTokenMiddleware: ClientMiddleware {
    private let token: @Sendable () async throws -> String?

    /// Sends the same token with every request.
    public init(token: String) {
        self.token = { token }
    }

    /// Asks `token` for the token before each request, for a client that
    /// keeps it somewhere it can change (the keychain, a sign-in flow).
    /// Returning `nil` sends the request without one.
    public init(token: @escaping @Sendable () async throws -> String?) {
        self.token = token
    }

    public func intercept(
        _ request: HTTPRequest,
        body: HTTPBody?,
        baseURL: URL,
        operationID: String,
        next: @Sendable (HTTPRequest, HTTPBody?, URL) async throws -> (HTTPResponse, HTTPBody?)
    ) async throws -> (HTTPResponse, HTTPBody?) {
        var request = request
        if let token = try await token() {
            request.headerFields[.authorization] = "Bearer \(token)"
        }
        return try await next(request, body, baseURL)
    }
}
