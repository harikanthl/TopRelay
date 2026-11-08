// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "TopRelayKit",
    platforms: [.macOS(.v13)],
    products: [
        .library(name: "TopRelayKit", targets: ["TopRelayKit"]),
        .library(name: "TopRelayKitExample", targets: ["TopRelayKitExample"]),
    ],
    targets: [
        .target(name: "TopRelayKit"),
        .target(name: "TopRelayKitExample", dependencies: ["TopRelayKit"]),
        // Used by the agent's end-to-end test (TOPRELAY_SWIFT_TESTS=1 go test ./internal/cli).
        .executableTarget(name: "trkit-smoke", dependencies: ["TopRelayKit"]),
        .testTarget(name: "TopRelayKitTests", dependencies: ["TopRelayKit"]),
    ]
)
