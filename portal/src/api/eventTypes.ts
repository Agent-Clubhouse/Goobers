/** Explicit external producer input. Payload fields never carry host authority. */
export interface GaggleEventEnvelope {
  specversion: "1.0";
  id: string;
  source: string;
  type: string;
  subject?: string;
  time?: string;
  dataschema?: string;
  datacontenttype?: string;
  data?: unknown;
}
export interface GaggleEventDelivery {
  consumer: string;
  groupId?: string;
  reason?: string;
  state?: string;
  acceptanceId?: string;
  runId?: string;
}
/** Durable acceptance and consumer links; no raw payload or machine identity. */
export interface GaggleEventReceipt {
  receiptId: string;
  gaggle: string;
  binding: string;
  source: string;
  eventId: string;
  digest: string;
  acceptedAt: string;
  duplicate: boolean;
  state: "routing_pending" | "accepted_unmatched" | "routing_failed" | "routed" | "routing_partial";
  statusUrl: string;
  tombstoned: boolean;
  deliveries: GaggleEventDelivery[];
}
