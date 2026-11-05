# Keep scheduler telemetry outside tick ownership (#6295)

The command package owns the daemon scheduler setup and shutdown sequence, including its telemetry exporter flush and rollup ingestion. The added private worker coalesces those two daemon-owned operations, gives each pass a bounded context, and drains it before the same setup closes its exporters and stores.

The worker belongs beside that lifecycle assembly: it does not change the reusable scheduler or telemetry protocols, and neither internal package should depend on the other's shutdown order. The tick callback only signals this private worker. No new CLI command, public worker API or general scheduling abstraction is introduced.
