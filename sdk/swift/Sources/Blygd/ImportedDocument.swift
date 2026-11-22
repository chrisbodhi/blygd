// The hand-written stand-in for the ImportedDoc schema, wired in by
// typeOverrides in openapi-generator-config.yaml.
//
// ImportedDoc is `oneOf: [null, ItemDocument, L0Document]`. The generator
// cannot model the null branch (it skips the whole schema, and every
// property that refers to it), so this type takes its place.

/// The content of an imported item: the origin's document for a blyg, the
/// studio's wrapper for an L0 (feed) item, or nothing once the origin
/// withdrew it (§13.4).
///
/// The HTML inside came from another origin and is returned verbatim.
/// Sanitize it before rendering (§14).
public enum ImportedDocument: Codable, Hashable, Sendable {
    /// The origin withdrew the item; the studio keeps the stub (§13.4).
    case withdrawn
    /// A blyg item document (`kind` is `fragment` or `thread`).
    case item(Components.Schemas.ItemDocument)
    /// An L0 item wrapped by the studio (`kind` is `l0`).
    case l0(Components.Schemas.L0Document)

    public init(from decoder: any Decoder) throws {
        if try decoder.singleValueContainer().decodeNil() {
            self = .withdrawn
            return
        }
        // Choose the branch by `kind` rather than by trial, so a document that
        // fails to decode reports its own error, not the last branch's.
        let kind = try decoder.container(keyedBy: KindKey.self).decode(String.self, forKey: .kind)
        if kind == Components.Schemas.L0Document.KindPayload.l0.rawValue {
            self = .l0(try .init(from: decoder))
        } else {
            self = .item(try .init(from: decoder))
        }
    }

    public func encode(to encoder: any Encoder) throws {
        switch self {
        case .withdrawn:
            var container = encoder.singleValueContainer()
            try container.encodeNil()
        case .item(let document):
            try document.encode(to: encoder)
        case .l0(let document):
            try document.encode(to: encoder)
        }
    }

    private enum KindKey: String, CodingKey {
        case kind
    }
}
