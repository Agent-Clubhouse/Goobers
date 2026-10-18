import type { ComponentPropsWithoutRef, ReactNode } from "react";

export function TableShell({
  ariaLabel,
  children,
  className = "",
  ...props
}: Omit<ComponentPropsWithoutRef<"div">, "aria-label"> & { ariaLabel: string }) {
  return (
    <div
      {...props}
      aria-label={ariaLabel}
      className={`data-table-shell ui-table-shell ${className}`.trim()}
      role="region"
    >
      {children}
    </div>
  );
}

export function TableHeader({
  columns,
  className = "",
  trailingColumn = false,
}: {
  columns: readonly ReactNode[];
  className?: string;
  trailingColumn?: boolean;
}) {
  return (
    <div aria-hidden="true" className={`data-header data-table-header ${className}`.trim()}>
      {columns.map((column, index) => (
        <span key={index}>{column}</span>
      ))}
      {trailingColumn && <span />}
    </div>
  );
}

export function DataTable({
  ariaLabel,
  columns,
  children,
  className = "",
  shellClassName = "",
  shellLabel,
  ...props
}: Omit<ComponentPropsWithoutRef<"table">, "aria-label"> & {
  ariaLabel: string;
  columns: readonly ReactNode[];
  shellClassName?: string;
  shellLabel?: string;
}) {
  return (
    <TableShell
      ariaLabel={shellLabel ?? `${ariaLabel} comparison`}
      className={shellClassName}
      tabIndex={0}
    >
      <table {...props} aria-label={ariaLabel} className={`ui-data-table ${className}`.trim()}>
        <thead className="data-table-header">
          <tr>
            {columns.map((column, index) => (
              <th key={index} scope="col">
                {column}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>{children}</tbody>
      </table>
    </TableShell>
  );
}
