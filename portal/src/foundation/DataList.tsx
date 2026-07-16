import { Icon } from "./Icon";

export type DataListLayout = "run-grid" | "outcome-grid" | "workflow-grid" | "all-runs-grid";

export interface DataListHeader {
  className?: string;
  label: string;
}

export function DataList({
  label,
  layout,
  headers,
  children,
}: {
  label: string;
  layout: DataListLayout;
  headers?: DataListHeader[];
  children: React.ReactNode;
}) {
  return (
    <div aria-label={label} className="data-table">
      {headers && (
        <div aria-hidden="true" className={`data-header ${layout}`}>
          {headers.map((header, index) => (
            <span className={header.className} key={`${header.label}-${index}`}>
              {header.label}
            </span>
          ))}
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
  layout,
}: {
  children: React.ReactNode;
  onClick: () => void;
  label: string;
  layout: DataListLayout;
}) {
  return (
    <button className={`data-row ${layout}`} onClick={onClick} type="button">
      <span className="sr-only">{label}. </span>
      {children}
      <span className="row-arrow">
        <Icon name="chevron" size={16} />
      </span>
    </button>
  );
}
