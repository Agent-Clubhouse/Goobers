# pr-select runtime compatibility

`cmd/goobers/prselect.go` grows to select the credential authority explicitly
accepted by this command's manifest. The CLI must inspect the delivered stage
environment and retain the selected capability through its existing provider
factory for authorization fencing, authentication scheme, expiry, and refresh.
This cannot be a global credential alias: other commands must retain their
existing exact capability requirements. The provider implementations and shared
credential machinery remain unchanged.
