import { Children } from "react";
import { Icon } from "./Icon";

// Defense-in-depth cap (DASH-15): operational lists are fed bounded data
// (DASH-12 Overview groups, DASH-14 paginated Runs history), but DataList also
// refuses to mount an unbounded number of interactive rows so no future caller
// can accidentally render thousands of nodes. When input exceeds the cap the
// extra rows are dropped from the DOM and an explicit overflow affordance names
// how many are hidden and where the full list lives.
const DEFAULT_MAX_ROWS = 200;

interface DataListOverflow {
  href?: string;
  label?: string;
}

interface DataListProps {
  ariaLabel: string;
  children: React.ReactNode;
  columns?: readonly string[];
  gridClassName?: string;
  maxRows?: number;
  overflow?: DataListOverflow;
}

export function DataList({
  ariaLabel,
  children,
  columns,
  gridClassName = "",
  maxRows = DEFAULT_MAX_ROWS,
  overflow,
}: DataListProps) {
  const rows = Children.toArray(children);
  const cap = Math.max(0, maxRows);
  const visible = rows.length > cap ? rows.slice(0, cap) : rows;
  const hidden = rows.length - visible.length;

  return (
    <div aria-label={ariaLabel} className="data-table" role="region">
      {columns && (
        <div aria-hidden="true" className={`data-header ${gridClassName}`}>
          {columns.map((column, index) => (
            <span key={`${column}-${index}`}>{column}</span>
          ))}
          <span />
        </div>
      )}
      {visible}
      {hidden > 0 && (
        <p className="data-overflow">
          Showing {visible.length} of {rows.length}.{" "}
          {overflow?.href ? (
            <a href={overflow.href}>{overflow.label ?? "View all"}</a>
          ) : (
            <span>{overflow?.label ?? `${hidden} more not shown.`}</span>
          )}
        </p>
      )}
    </div>
  );
}

interface DataRowProps {
  children: React.ReactNode;
  href?: string;
  label: string;
  onClick?: () => void;
}

export function DataRow({ children, href, label, onClick }: DataRowProps) {
  const content = (
    <>
      {children}
      <span className="row-arrow">
        <Icon name="chevron" size={16} />
      </span>
    </>
  );

  if (href) {
    return (
      <a aria-label={label} className="data-row" href={href}>
        {content}
      </a>
    );
  }

  if (!onClick) {
    throw new TypeError("DataRow requires either href or onClick.");
  }

  return (
    <button aria-label={label} className="data-row" onClick={onClick} type="button">
      {content}
    </button>
  );
}
