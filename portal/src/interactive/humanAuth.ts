// Cookies are shared across loopback ports. Keep the independent header binding
// in origin-scoped storage so a preview server cannot reuse a captured cookie.
export function humanControlHeaders(): Record<string, string> {
  try {
    const value = localStorage.getItem("goobers.interactive.human-binding");
    return value ? { "X-Goobers-CSRF": value } : {};
  } catch { return {}; }
}
