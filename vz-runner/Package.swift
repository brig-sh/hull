// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "vz-runner",
    platforms: [
        .macOS(.v14)
    ],
    products: [
        .executable(name: "vz-runner", targets: ["VzRunner"])
    ],
    targets: [
        .executableTarget(
            name: "VzRunner",
            dependencies: [],
            path: "Sources"
        )
    ]
)
