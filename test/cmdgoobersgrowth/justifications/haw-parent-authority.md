# Contained parent attempt authority

The command package binds existing daemon services: the pinned run journal,
current gaggle policy, child queue, signed pod principal, credential resolver,
and the single live-journal writer. These adapters must use daemon-owned current
state instead of accepting origin, credential or artifact authority from a pod.
Reusable signed credentials, contract parsing, bounded custody storage, journal
blob overrides and transport guards remain in their respective internal packages.

The parent access endpoint issues and revokes only an exact active physical
attempt's child grant. Credential delivery checks policy and durable cancellation.
Journal operations and surrendered results share exact stage identity and scoped
artifact custody; ordinary worker behavior continues through the existing services.
The same review slice includes composed human-restart acceptance with only the
model process and provider transport simulated, and worker-contained transport.
