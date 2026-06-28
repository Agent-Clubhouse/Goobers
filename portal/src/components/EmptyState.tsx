export function EmptyState() {
  return (
    <section className="empty-state" aria-labelledby="empty-title">
      <p className="eyebrow">First boot</p>
      <h2 id="empty-title">I'm alive — your goober gaggle is ready</h2>
      <p>
        No gaggles are configured yet. Define gaggles, goobers, workflows, and gates in code, redeploy, and they will
        appear here.
      </p>
    </section>
  );
}
