import { Icon } from "./Icon";

interface DataListProps {
  ariaLabel: string;
  children: React.ReactNode;
  columns?: readonly string[];
  gridClassName?: string;
}

export function DataList({ ariaLabel, children, columns, gridClassName = "" }: DataListProps) {
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
      {children}
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
