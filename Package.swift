// swift-tools-version: 6.0
//
// The Swift SDK for blygd's /api/ (sdk/swift/README.md). It sits at the
// root because SwiftPM resolves a dependency by repository URL and reads
// the manifest from the root; everything else lives under sdk/swift/.
//
//     .package(url: "https://github.com/chrisbodhi/blygd", from: "…")
//     .product(name: "Blygd", package: "blygd")
//
// The client is generated from internal/web/openapi.json by
// scripts/swift-sdk.sh and committed, so apps that use it run no build
// plugin and depend only on the OpenAPI runtime and its URLSession
// transport.
import PackageDescription

let package = Package(
    name: "blygd",
    platforms: [
        .macOS(.v10_15), .macCatalyst(.v13), .iOS(.v13), .tvOS(.v13), .watchOS(.v6), .visionOS(.v1),
    ],
    products: [
        .library(name: "Blygd", targets: ["Blygd"]),
    ],
    dependencies: [
        .package(url: "https://github.com/apple/swift-openapi-runtime", from: "1.13.4"),
        .package(url: "https://github.com/apple/swift-openapi-urlsession", from: "1.4.0"),
        .package(url: "https://github.com/apple/swift-http-types", from: "1.8.0"),
    ],
    targets: [
        .target(
            name: "Blygd",
            dependencies: [
                .product(name: "OpenAPIRuntime", package: "swift-openapi-runtime"),
                .product(name: "OpenAPIURLSession", package: "swift-openapi-urlsession"),
                .product(name: "HTTPTypes", package: "swift-http-types"),
            ],
            path: "sdk/swift/Sources/Blygd"
        ),
        .testTarget(
            name: "BlygdTests",
            dependencies: ["Blygd"],
            path: "sdk/swift/Tests/BlygdTests"
        ),
    ]
)
