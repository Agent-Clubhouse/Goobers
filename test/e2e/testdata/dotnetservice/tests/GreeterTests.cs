using DotnetService;
using Xunit;

public class GreeterTests
{
    [Fact]
    public void GreetsByName() => Assert.Equal("Hello, Goobers!", Greeter.Greet("Goobers"));
}
