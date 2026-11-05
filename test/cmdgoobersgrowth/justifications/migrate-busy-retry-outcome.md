# Migrate reports a busy retry's real outcome

`goobers-cloud migrate` measures the time its busy retry actually spent and
reports a SIGTERM during the backoff as stopped, instead of claiming the
database stayed busy for the whole configured budget. Both lines sit in the
command's own exit reporting, next to the retry loop that #8107 added there;
no flag, command or lifecycle is added.
