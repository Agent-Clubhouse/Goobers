import SwiftUI

@main
struct GoobersIOSApp: App {
    var body: some Scene {
        WindowGroup {
            Text("Goobers simulator ready")
                .accessibilityIdentifier("ready-label")
        }
    }
}
