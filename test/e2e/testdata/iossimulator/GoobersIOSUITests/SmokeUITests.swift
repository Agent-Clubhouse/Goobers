import XCTest

final class SmokeUITests: XCTestCase {
    func testLaunchesRepresentativeTarget() {
        let app = XCUIApplication()
        app.launch()

        XCTAssertTrue(
            app.staticTexts["ready-label"].waitForExistence(timeout: 10),
            "representative app did not expose its ready label"
        )
    }
}
