package dev.goobers.fixture;

import static org.junit.jupiter.api.Assertions.assertEquals;

import org.junit.jupiter.api.Test;

final class GreeterTest {
    @Test
    void greetsByName() {
        assertEquals("Hello, Goobers!", Greeter.greet("Goobers"));
    }
}
