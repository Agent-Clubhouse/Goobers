# Authenticated external event ingress installation

The command package adds only existing-listener installation and an applied
catalog/config-generation lease around configured machine bindings. Envelope
validation, rate bounds, durable receipt acceptance and human receipt visibility
remain in internal packages. Composition tests exercise authenticated HTTP intake
through the real shared queue, archived consumer launcher, and policy reload.
No command baseline or complexity ratchet increase is requested.
