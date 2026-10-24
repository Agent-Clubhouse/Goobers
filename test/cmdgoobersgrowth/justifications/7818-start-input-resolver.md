# HA-P3-18 pinned start-input composition

Tracks #7818 and the resolver delivered by #8103. The cloud command registers
its required Resolve seam and connects the controller instance identity to the
already-composed applied configuration and lease-bound generation source.
This is composition-root glue; pinned RunInput reconstruction, digest checking,
placement and admission policy remain in internal/cloud/startinput. There is no
local-command growth or local fallback, and the growth snapshot is unchanged.
