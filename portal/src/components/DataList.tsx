import type { ReactNode } from "react";
import { Icon } from "../foundation/Icon";

export function DataList({
  ariaLabel,
  children,
  headings,
  headerClassName,
}: {
  ariaLabel: string;
  children: ReactNode;
  headings?: string[];
  headerClassName?: string;
}) {
  return (
    <div aria-label={ariaLabel} className="data-table">
      {headings && (
        <div aria-hidden="true" className={`data-header ${headerClassName ?? ""}`.trim()}>
          {headings.map((heading, index) => <span key={`${heading}-${index}`}>{heading}</span>)}
        </div>
      )}
      {children}
    </div>
  );
}

export function DataRow({
  children,
  onClick,
  label,
}: {
  children: ReactNode;
  onClick: () => void;
  label: string;
}) {
  return (
    <button aria-label={label} className="data-row" onClick={onClick} type="button">
      {children}
      <span className="row-arrow">
        <Icon name="chevron" size={16} />
      </span>
    </button>
  );
}
