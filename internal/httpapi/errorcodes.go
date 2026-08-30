package httpapi

// CodeInvalidRequest is the stable wire code for malformed or incomplete
// requests. Clients branch on this value, so handlers must not restate it.
const CodeInvalidRequest = "invalid_request"
