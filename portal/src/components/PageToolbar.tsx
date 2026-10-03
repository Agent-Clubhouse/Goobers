import {
  type KeyboardEvent,
  type ReactNode,
  useEffect,
  useId,
  useLayoutEffect,
  useRef,
  useState,
} from "react";
import { Icon } from "../ui/Icon";

export interface ActivePageFilter {
  key: string;
  label: string;
  onRemove: () => void;
}

interface PageToolbarProps {
  activeFilters: ActivePageFilter[];
  count: number;
  description: string;
  filterError?: string;
  filters: (mobile: boolean) => ReactNode;
  onApplyFilters: () => string | undefined;
  onOpenFilters: () => void;
  onResetFilters: () => void;
  search?: ReactNode;
  title: string;
}

const FOCUSABLE = [
  "button:not([disabled])",
  "input:not([disabled])",
  "select:not([disabled])",
  "textarea:not([disabled])",
  "a[href]",
  "[tabindex]:not([tabindex='-1'])",
].join(",");

export function PageToolbar({
  activeFilters,
  count,
  description,
  filterError,
  filters,
  onApplyFilters,
  onOpenFilters,
  onResetFilters,
  search,
  title,
}: PageToolbarProps) {
  const [open, setOpen] = useState(false);
  const [draftError, setDraftError] = useState<string>();
  const id = useId();
  const panel = useRef<HTMLDivElement>(null);
  const trigger = useRef<HTMLButtonElement>(null);
  const wasOpen = useRef(false);
  const historyKey = `page-filter-sheet:${id}`;

  useLayoutEffect(() => {
    if (wasOpen.current && !open) {
      trigger.current?.focus();
    }
    wasOpen.current = open;
  }, [open]);

  useEffect(() => {
    if (!open) return;
    const previousOverflow = document.body.style.overflow;
    document.body.style.overflow = "hidden";
    panel.current?.querySelector<HTMLElement>(FOCUSABLE)?.focus();
    const dismiss = () => {
      setOpen(false);
      setDraftError(undefined);
    };
    window.addEventListener("popstate", dismiss, { once: true });
    return () => {
      document.body.style.overflow = previousOverflow;
      window.removeEventListener("popstate", dismiss);
    };
  }, [open]);

  const openSheet = () => {
    onOpenFilters();
    setDraftError(undefined);
    window.history.pushState(
      { ...window.history.state, pageFilterSheet: historyKey },
      "",
      window.location.href,
    );
    setOpen(true);
  };
  const cancel = () => {
    if (window.history.state?.pageFilterSheet === historyKey) {
      window.history.back();
      return;
    }
    setOpen(false);
  };
  const apply = () => {
    const error = onApplyFilters();
    if (error) {
      setDraftError(error);
      return;
    }
    const { pageFilterSheet: _sheet, ...state } = window.history.state ?? {};
    window.history.replaceState(state, "", window.location.href);
    setOpen(false);
  };
  const containFocus = (event: KeyboardEvent<HTMLDivElement>) => {
    if (event.key === "Escape") {
      event.preventDefault();
      cancel();
      return;
    }
    if (event.key !== "Tab") return;
    const controls = [...(panel.current?.querySelectorAll<HTMLElement>(FOCUSABLE) ?? [])];
    if (controls.length === 0) return;
    const first = controls[0];
    const last = controls.at(-1);
    if (event.shiftKey && document.activeElement === first) {
      event.preventDefault();
      last?.focus();
    } else if (!event.shiftKey && document.activeElement === last) {
      event.preventDefault();
      first.focus();
    }
  };

  return (
    <header className="page-toolbar">
      <div className="page-toolbar-heading">
        <div className="page-toolbar-title">
          <h1>{title}</h1>
          <span aria-label={`${count} records`} className="page-toolbar-count">{count}</span>
        </div>
        <p>{description}</p>
      </div>
      <div className="page-toolbar-actions">
        {search}
        <button
          aria-label="Filters"
          aria-expanded={open}
          aria-haspopup="dialog"
          className="page-toolbar-filter-trigger"
          onClick={openSheet}
          ref={trigger}
          type="button"
        >
          <Icon name="menu" size={17} />
          Filters
          {activeFilters.length > 0 && (
            <span aria-label={`${activeFilters.length} active filters`}>
              {activeFilters.length}
            </span>
          )}
        </button>
      </div>
      <div className="page-toolbar-desktop-filters">{filters(false)}</div>
      {(activeFilters.length > 0 || filterError) && (
        <div className="page-toolbar-feedback">
          {filterError && <p className="page-toolbar-error" role="alert">{filterError}</p>}
          {activeFilters.length > 0 && (
            <div aria-label="Active filters" className="page-filter-chips">
              {activeFilters.map((filter) => (
                <button
                  aria-label={`Remove ${filter.label} filter`}
                  className="page-filter-chip"
                  key={filter.key}
                  onClick={filter.onRemove}
                  type="button"
                >
                  {filter.label}
                  <Icon name="close" size={14} />
                </button>
              ))}
              <button className="page-filter-reset" onClick={onResetFilters} type="button">
                Reset filters
              </button>
            </div>
          )}
        </div>
      )}
      {open && (
        <div
          aria-labelledby={`${id}-title`}
          aria-modal="true"
          className="page-filter-sheet-backdrop"
          onMouseDown={(event) => {
            if (event.target === event.currentTarget) cancel();
          }}
          role="dialog"
        >
          <div className="page-filter-sheet" onKeyDown={containFocus} ref={panel}>
            <div className="page-filter-sheet-heading">
              <div>
                <span className="page-kicker">Refine results</span>
                <h2 id={`${id}-title`}>Filters</h2>
              </div>
              <button aria-label="Cancel filter changes" onClick={cancel} type="button">
                <Icon name="close" />
              </button>
            </div>
            <div className="page-filter-sheet-fields">{filters(true)}</div>
            {draftError && <p className="page-toolbar-error" role="alert">{draftError}</p>}
            <div className="page-filter-sheet-actions">
              <button className="secondary-button" onClick={cancel} type="button">Cancel</button>
              <button className="page-filter-apply" onClick={apply} type="button">Apply filters</button>
            </div>
          </div>
        </div>
      )}
    </header>
  );
}
