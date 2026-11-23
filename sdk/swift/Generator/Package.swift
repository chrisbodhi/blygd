// swift-tools-version: 6.0
// Pins the code generator for the Swift SDK. It is not part of the SDK:
// apps never depend on it. scripts/swift-sdk.sh runs it to write
// ../Sources/Blygd/Generated from internal/web/openapi.json.
import PackageDescription

let package = Package(
    name: "BlygdSDKGenerator",
    dependencies: [
        .package(url: "https://github.com/apple/swift-openapi-generator", exact: "1.14.4"),
    ]
)
