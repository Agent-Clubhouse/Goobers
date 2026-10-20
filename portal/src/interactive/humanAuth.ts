// The auth cookie is HttpOnly. Only its separate CSRF binding is exposed to the
// browser client; neither value is kept in pending-command/session storage.
export function humanControlHeaders(): Record<string, string> {
  if (typeof document === "undefined") return {};
  const value = document.cookie.split(";").map(part => part.trim()).find(part => part.startsWith("goobers-human-csrf="))?.slice("goobers-human-csrf=".length);
  return value ? { "X-Goobers-CSRF": value } : {};
}
