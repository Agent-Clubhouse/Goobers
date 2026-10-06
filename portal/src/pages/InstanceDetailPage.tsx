import { type ReactNode, useEffect, useState } from "react";
import { PageHeading, SectionHeading } from "../ui/Heading";
import { Action } from "../ui/Action";
import { MetadataGrid } from "../ui/MetadataGrid";
import { Timestamp } from "../ui/Timestamp";
import type { Instance, DaemonClient } from "../api/types";
import type { ConfigurationWarningsProps } from "../components/ConfigurationWarnings";
import { ConfigurationWarnings } from "../components/ConfigurationWarnings";
import { DaemonErrorState } from "../components/DaemonQueryState";
import { RecoveryCommand } from "../components/RecoveryAction";
import type { InstanceDetailKind, Navigate } from "../routing";
import { Icon } from "../ui/Icon";

type InstanceState =
  | { status: "loading" }
  | { status: "ready"; instance: Instance }
  | { status: "error"; error: Error };

const detailTitles: Record<InstanceDetailKind, string> = {
  recovery: "Recovery metadata",
  retention: "Telemetry retention",
  warnings: "Configuration warnings",
};

export function InstanceDetailPage({
  client,
  configurationWarnings,
  detail,
  navigate,
  standalone,
}: {
  client: DaemonClient;
  configurationWarnings: Omit<ConfigurationWarningsProps, "context">;
  detail: InstanceDetailKind;
  navigate: Navigate;
  standalone: boolean;
}) {
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<InstanceState>({ status: "loading" });

  useEffect(() => {
    if (detail === "warnings") {
      return;
    }
    const controller = new AbortController();
    setState({ status: "loading" });
    client.getInstance({ signal: controller.signal }).then(
      (instance) => setState({ status: "ready", instance }),
      (cause: unknown) => {
        if (!controller.signal.aborted) {
          setState({
            status: "error",
            error: cause instanceof Error ? cause : new Error("Instance details are unavailable."),
          });
        }
      },
    );
    return () => controller.abort();
  }, [client, detail, revision]);

  const retry = () => setRevision((current) => current + 1);
  const close = () => {
    const historyState = window.history.state as { portalOrigin?: boolean } | null;
    if (historyState?.portalOrigin) {
      window.history.back();
    } else {
      navigate({ page: "overview" });
    }
  };

  return (
    <article aria-labelledby="instance-detail-title" className="instance-detail-page">
      <PageHeading
        className="instance-detail-header"
        title={detailTitles[detail]}
        titleProps={{ id: "instance-detail-title" }}
        beforeTitle={
          <Action
            aria-label="Back to overview"
            className="instance-detail-back"
            size="touch"
            onClick={close}
          >
            <Icon name="chevron" size={16} />
            Back
          </Action>
        }
      />

      {detail === "warnings" ? (
        <>
          <p className="instance-detail-guidance">
            Review configuration findings here. The portal is read-only; update definitions on disk
            and validate them before refreshing.
          </p>
          <ConfigurationWarnings context="instance" {...configurationWarnings} />
        </>
      ) : state.status === "loading" ? (
        <section aria-live="polite" className="daemon-state" role="status">
          <span aria-hidden="true" className="loading-mark" />
          <div>
            <h2>Loading {detailTitles[detail].toLowerCase()}</h2>
            <p>
              Reading the latest instance record from the {standalone ? "local instance" : "daemon"}
              .
            </p>
          </div>
        </section>
      ) : state.status === "error" ? (
        <DaemonErrorState error={state.error} retry={retry} standalone={standalone} />
      ) : detail === "recovery" ? (
        <RecoveryDetail instance={state.instance} />
      ) : (
        <RetentionDetail instance={state.instance} />
      )}
    </article>
  );
}

function RecoveryDetail({ instance }: { instance: Instance }) {
  const inventory = instance.recoveryInventory;
  if (!inventory) {
    return (
      <section className="empty-state">
        <div>
          <h2>No recovery metadata reported</h2>
          <p>This daemon does not currently report recovery inventory metadata.</p>
        </div>
      </section>
    );
  }
  return (
    <section aria-label="Recovery inventory details" className="instance-detail-card">
      <p className={`instance-detail-status instance-detail-status-${inventory.state}`}>
        {inventory.state === "unavailable"
          ? "Recovery inventory unavailable"
          : `${inventory.used} of ${inventory.limit} recovery slots are occupied`}
      </p>
      <MetadataGrid className="instance-detail-fields">
        <DetailField label="State" value={inventory.state} />
        <DetailField label="Unreadable reservations" value={String(inventory.unreadable)} />
        <DetailField label="Overflow snapshots" value={String(inventory.overflow)} />
        <DetailField label="High-water threshold" value={`${inventory.highWaterPercent}%`} />
        <DetailField label="Observed" value={<Timestamp value={inventory.observedAt} />} />
        {inventory.earliestRetainUntil && (
          <DetailField
            label="Earliest retention deadline"
            value={<Timestamp value={inventory.earliestRetainUntil} />}
          />
        )}
        {inventory.inventoryRoot && (
          <DetailField label="Inventory root" value={inventory.inventoryRoot} />
        )}
        {inventory.policySource && (
          <DetailField label="Policy source" value={inventory.policySource} />
        )}
        {inventory.error && <DetailField label="Measurement error" value={inventory.error} />}
      </MetadataGrid>
      <div className="instance-detail-actions">
        <SectionHeading title="Operator action" />
        <p>
          The portal is read-only. Inspect retained snapshots from the CLI before changing policy.
        </p>
        <RecoveryCommand command="goobers recovery list" />
      </div>
    </section>
  );
}

function RetentionDetail({ instance }: { instance: Instance }) {
  const retention = instance.telemetryRetention;
  const maintenance = instance.maintenance;
  if (!retention && !maintenance) {
    return (
      <section className="empty-state">
        <div>
          <h2>No retention metadata reported</h2>
          <p>This daemon does not currently report telemetry retention or sweep metadata.</p>
        </div>
      </section>
    );
  }
  return (
    <section aria-label="Telemetry retention details" className="instance-detail-card">
      {retention && (
        <>
          <p className="instance-detail-status">
            Telemetry retention is {retention.enabled ? "enabled" : "disabled"}
          </p>
          <MetadataGrid className="instance-detail-fields">
            <DetailField label="Retention window" value={retention.window} />
            <DetailField label="Maximum runs" value={String(retention.maxRuns)} />
            <DetailField
              label="First enabled"
              value={<Timestamp value={retention.firstEnable} />}
            />
            {retention.enforceAt && (
              <DetailField
                label="Enforcement begins"
                value={<Timestamp value={retention.enforceAt} />}
              />
            )}
            {retention.lastPassAt && (
              <DetailField label="Last pass" value={<Timestamp value={retention.lastPassAt} />} />
            )}
            {retention.lastPassMode && (
              <DetailField label="Last pass mode" value={retention.lastPassMode} />
            )}
            <DetailField label="Candidates" value={String(retention.candidateCount)} />
          </MetadataGrid>
        </>
      )}
      {maintenance && (
        <div className="instance-detail-actions">
          <SectionHeading title="Latest retention sweep" />
          <MetadataGrid className="instance-detail-fields">
            <DetailField label="State" value={maintenance.state} />
            <DetailField label="Trigger" value={maintenance.trigger} />
            <DetailField label="Candidates" value={String(maintenance.candidates)} />
            <DetailField label="Removed" value={String(maintenance.removed)} />
            <DetailField label="Failures" value={String(maintenance.failures)} />
            {maintenance.currentPhase && (
              <DetailField label="Current phase" value={maintenance.currentPhase} />
            )}
            {maintenance.errorSummary && (
              <DetailField label="Error" value={maintenance.errorSummary} />
            )}
          </MetadataGrid>
        </div>
      )}
    </section>
  );
}

function DetailField({ label, value }: { label: string; value: ReactNode }) {
  return (
    <div>
      <dt>{label}</dt>
      <dd>{value}</dd>
    </div>
  );
}
