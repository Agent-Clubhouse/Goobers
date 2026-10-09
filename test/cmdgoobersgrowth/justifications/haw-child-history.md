# HAW-CHD child acceptance history composition

One small command adapter and one LocalSources binding connect the read service
with the daemon-owned durable child queue. The adapter is evaluated only when
serving reads, after coordination initialization; an unavailable queue is explicit.
It does not open another database or authorize, reconcile or mutate custody.
Parent identity binding, bounded pagination and the response projection belong
in internal/readservice; HTTP and Portal presentation stay in their adapters.
