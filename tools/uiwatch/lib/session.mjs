// Long-session probe: one page held open, sampled over time.
//
// The periodic probe reloads the dashboard every few minutes and therefore
// only ever sees a COLD page. A tab a human leaves open for hours fails
// differently: heap and DOM nodes accumulate, an SSE stream dies on
// suspend/resume and reconnects badly, a view drifts after hundreds of
// incremental updates. None of that is reachable by reloading.
//
// This keeps one page open and watches those quantities move.
const CONNECTED = "up";
const DISCONNECTED = "down";

// Runs inside the page. Sampling the badge only when Node takes a sample would
// miss a flap that resolves between samples, so the page counts transitions
// continuously and Node reads the running total.
function watcherSource(connectedText) {
  return `
    window.__uiwatch = { flaps: 0, last: null, states: [] };
    const connected = ${JSON.stringify(connectedText)};
    setInterval(() => {
      const text = document.body ? document.body.innerText : "";
      const state = text.includes(connected)
        ? "up"
        : (/Reconnecting|Connecting to daemon/i.test(text) ? "down" : "other");
      const w = window.__uiwatch;
      if (w.last !== null && state !== w.last) {
        w.flaps += 1;
        w.states.push({ from: w.last, to: state, at: Date.now() });
        if (w.states.length > 50) w.states.shift();
      }
      w.last = state;
    }, 1000);
  `;
}

export class LongSession {
  constructor(playwright, config) {
    this.pw = playwright;
    this.config = config;
    this.opts = config.session ?? {};
    this.browserName = this.opts.browser ?? "firefox";
    this.samples = [];
    this.consoleErrors = [];
    this.sseRequests = 0;
    this.openedAt = null;
  }

  async open() {
    this.browser = await this.pw[this.browserName].launch();
    this.context = await this.browser.newContext();
    this.page = await this.context.newPage();

    this.page.on("console", (m) => {
      if (m.type() === "error") this.consoleErrors.push({ at: Date.now(), text: m.text() });
    });
    this.page.on("pageerror", (e) => this.consoleErrors.push({ at: Date.now(), text: "pageerror: " + e.message }));
    // Every request to the events endpoint after the first is a reconnect.
    this.page.on("request", (r) => {
      if (r.url().includes("/api/v1/events")) this.sseRequests += 1;
    });

    await this.page.goto(`${this.config.baseUrl}/#/overview`, { waitUntil: "domcontentloaded" });
    await this.page.evaluate(watcherSource(this.config.connectedText));

    // Chromium exposes real heap/node/listener counts over CDP; Firefox and
    // WebKit do not, so those engines fall back to a DOM node count only.
    if (this.browserName === "chromium") {
      try {
        this.cdp = await this.context.newCDPSession(this.page);
        await this.cdp.send("Performance.enable");
      } catch {
        this.cdp = null;
      }
    }
    this.openedAt = Date.now();
    await this.sample();
  }

  async sample() {
    const t0 = Date.now();
    const inPage = await this.page.evaluate(() => ({
      nodes: document.getElementsByTagName("*").length,
      listeners: null,
      flaps: window.__uiwatch?.flaps ?? 0,
      state: window.__uiwatch?.last ?? null,
      heap: performance?.memory?.usedJSHeapSize ?? null,
    }));
    // Round-trip latency of a trivial evaluate is a cheap responsiveness proxy:
    // it climbs when the main thread is busy or the page is thrashing.
    const rtt = Date.now() - t0;

    let heap = inPage.heap;
    let listeners = null;
    if (this.cdp) {
      try {
        const { metrics } = await this.cdp.send("Performance.getMetrics");
        const get = (n) => metrics.find((m) => m.name === n)?.value ?? null;
        heap = get("JSHeapUsedSize") ?? heap;
        listeners = get("JSEventListeners");
      } catch {
        // CDP can drop on navigation; keep the in-page numbers.
      }
    }

    const sample = {
      at: new Date().toISOString(),
      uptimeSec: Math.round((Date.now() - this.openedAt) / 1000),
      nodes: inPage.nodes,
      heap,
      listeners,
      rttMs: rtt,
      flaps: inPage.flaps,
      state: inPage.state,
      sseRequests: this.sseRequests,
      consoleErrors: this.consoleErrors.length,
    };
    this.samples.push(sample);
    return sample;
  }

  /** Baseline is the median of the first N samples, so one cold outlier
   *  (first paint, lazy chunks) cannot masquerade as a leak. */
  baseline(key) {
    const warmup = this.opts.baselineSamples ?? 3;
    const values = this.samples.slice(0, warmup).map((s) => s[key]).filter((v) => typeof v === "number");
    if (!values.length) return null;
    const sorted = [...values].sort((a, b) => a - b);
    return sorted[Math.floor(sorted.length / 2)];
  }

  /** Emits probe-shaped checks so long-session findings land in the same
   *  ledger, with the same fingerprinting and promotion rules. */
  checks() {
    const out = [];
    const last = this.samples[this.samples.length - 1];
    const min = this.opts.minSamples ?? 5;
    const browser = this.browserName;
    const push = (kind, ok, detail) => out.push({ browser, kind, route: "session", ok, detail, ms: null });

    if (!last || this.samples.length < min) return out; // Too early to judge.

    const growth = (key) => {
      const base = this.baseline(key);
      if (!base || typeof last[key] !== "number") return null;
      return Math.round(((last[key] - base) / base) * 100);
    };

    const nodePct = growth("nodes");
    if (nodePct !== null && nodePct > (this.opts.maxNodeGrowthPct ?? 50)) {
      push("session-dom", false, `DOM nodes +${nodePct}% over ${last.uptimeSec}s (${this.baseline("nodes")} -> ${last.nodes})`);
    }

    const heapPct = growth("heap");
    if (heapPct !== null && heapPct > (this.opts.maxHeapGrowthPct ?? 60)) {
      push("session-heap", false, `JS heap +${heapPct}% over ${last.uptimeSec}s`);
    }

    const reconnects = Math.max(0, last.sseRequests - 1);
    if (reconnects > (this.opts.maxSseReconnects ?? 5)) {
      push("session-sse", false, `event stream reconnected ${reconnects}x in ${last.uptimeSec}s`);
    }

    if (last.flaps > (this.opts.maxBadgeFlaps ?? 3)) {
      push("session-flap", false, `connection badge changed state ${last.flaps}x in ${last.uptimeSec}s`);
    }

    if (last.state === DISCONNECTED) {
      push("session-state", false, `session has been showing a disconnected state for the last sample`);
    }

    if (last.consoleErrors > 0) {
      const recent = this.consoleErrors[this.consoleErrors.length - 1];
      push("session-console", false, `${last.consoleErrors} console error(s) during session; latest: ${recent.text.slice(0, 120)}`);
    }

    if (out.length === 0) push("session", true, null);
    return out;
  }

  summary() {
    const last = this.samples[this.samples.length - 1];
    if (!last) return "no samples";
    const base = { nodes: this.baseline("nodes"), heap: this.baseline("heap") };
    const pct = (a, b) => (a && b ? `${a > b ? "+" : ""}${Math.round(((b - a) / a) * 100)}%` : "n/a");
    return [
      `uptime ${last.uptimeSec}s`,
      `nodes ${last.nodes} (${pct(base.nodes, last.nodes)})`,
      last.heap ? `heap ${(last.heap / 1e6).toFixed(1)}MB (${pct(base.heap, last.heap)})` : "heap n/a",
      last.listeners !== null ? `listeners ${last.listeners}` : null,
      `rtt ${last.rttMs}ms`,
      `sse ${Math.max(0, last.sseRequests - 1)} reconnects`,
      `flaps ${last.flaps}`,
      `state ${last.state}`,
      `errors ${last.consoleErrors}`,
    ].filter(Boolean).join("  ");
  }

  async close() {
    await this.browser?.close().catch(() => {});
  }
}
