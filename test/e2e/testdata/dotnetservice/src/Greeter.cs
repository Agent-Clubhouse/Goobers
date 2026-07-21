namespace DotnetService;

// Greeter is a trivial reference "service" unit — enough to prove a real
// `dotnet build && dotnet test` runs green through the polyglot CI gate (#1093).
public static class Greeter
{
    public static string Greet(string name) => $"Hello, {name}!";
}
