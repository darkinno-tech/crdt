// swift-tools-version: 6.0
import PackageDescription
import Foundation

let nativeLibraryDirectory = ProcessInfo.processInfo.environment["CRDT_RGA_LIBRARY_DIR"] ?? "../rust/target/debug"

let package = Package(
    name: "DarkInnoCRDTRGA",
    products: [
        .library(name: "CRDTRGA", targets: ["CRDTRGA"]),
        .executable(name: "crdt-rga-swift-conformance", targets: ["CRDTRGAConformance"]),
    ],
    targets: [
        .target(name: "CRDTRGAFFI", path: "Sources/CRDTRGAFFI", publicHeadersPath: "include"),
        .target(
            name: "CRDTRGA",
            dependencies: ["CRDTRGAFFI"],
            linkerSettings: [
                .unsafeFlags(["-L\(nativeLibraryDirectory)"]),
                .linkedLibrary("darkinno_crdt_rga"),
            ],
        ),
        .executableTarget(name: "CRDTRGAConformance", dependencies: ["CRDTRGA"]),
    ],
)
