package dev.goobers.fixture;

public final class Greeter {
    private Greeter() {}

    public static String greet(String name) {
        return "Hello, " + name + "!";
    }
}
