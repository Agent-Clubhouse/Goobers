import { Icon } from "./Icon";

export type DataListLayout =
  | "run-grid"
  | "outcome-grid"
  | "workflow-grid"
  | "all-runs-grid"
  | "history-grid";

export function DataList({
  headers,
  layout,
  children,
}: {
  headers?: string[];
  layout: DataListLayout;
  children: React.ReactNode;
}) {
  return (
    <div className="data-table">
      {headers && (
        <div aria-hidden="true" className={`data-header ${layout}`}>
          {headers.map((header, index) => <span key={`${header}-${index}`}>{header}</span>)}
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
    <button aria-label={label} className={`data-row ${layout}`} onClick={onClick} type="button">
      {children}
      <span className="row-arrow">
        <Icon name="chevron" size={16} />
      </span>
    </button>
  );
}
