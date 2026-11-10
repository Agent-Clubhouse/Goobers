import { useState } from "react";
import { useFluidUI } from "../fluidUI";
import { routeHash } from "../routing";
import { Action } from "../ui/Action";

const fluidUIHref = routeHash({ page: "labs", feature: "fluid-ui" });

/**
 * Shell-level Fluid UI status: the unsaved-preview banner and the fallback or
 * ignored-items notice, so a customization problem is visible on every page
 * instead of silently changing (or breaking) one.
 */
export function FluidUIStatus() {
  const { notice, preview, stopPreview } = useFluidUI();
  const [dismissedNotice, setDismissedNotice] = useState<string>();
  return (
    <>
      {preview && (
        <div className="fluid-ui-status fluid-ui-preview" role="status">
          <span>
            <strong>Previewing a Fluid UI customization.</strong> Nothing is saved; reloading or stopping
            the preview returns to your applied customization.
          </span>
          <a href={fluidUIHref}>Edit</a>
          <Action variant="text" className="text-button" onClick={stopPreview} type="button">
            Stop preview
          </Action>
        </div>
      )}
      {notice && dismissedNotice !== notice.message && (
        <div className={`fluid-ui-status fluid-ui-notice-${notice.kind}`} role="status">
          <span>{notice.message}</span>
          <a href={fluidUIHref}>Review in Labs</a>
          <Action
            variant="text"
            className="text-button"
            onClick={() => setDismissedNotice(notice.message)}
            type="button"
          >
            Dismiss
          </Action>
        </div>
      )}
    </>
  );
}

/** Saved views from the effective customization, as in-app quick links. */
export function FluidUISavedViews({ className, label = "Saved views" }: { className?: string; label?: string }) {
  const views = (useFluidUI().effective?.savedViews ?? []).filter(({ route }) => route.startsWith("#/"));
  if (views.length === 0) return null;
  return (
    <nav aria-label={label} className={["fluid-ui-saved-views", className].filter(Boolean).join(" ")}>
      <span className="fluid-ui-saved-views-title">{label}</span>
      <ul>
        {views.map((view) => (
          <li key={view.name}>
            <a href={view.route}>{view.name}</a>
          </li>
        ))}
      </ul>
    </nav>
  );
}
