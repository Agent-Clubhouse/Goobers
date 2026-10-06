import type { ComponentPropsWithoutRef, ReactNode } from "react";

export function HeadingContent({
  title,
  description,
  titleProps,
  titleActions,
  beforeTitle,
  children,
  className = "",
}: {
  title: ReactNode;
  description?: ReactNode;
  titleProps?: ComponentPropsWithoutRef<"h1">;
  titleActions?: ReactNode;
  beforeTitle?: ReactNode;
  children?: ReactNode;
  className?: string;
}) {
  return (
    <div className={`ui-heading-content ${className}`.trim()}>
      {beforeTitle}
      <div className="ui-heading-title">
        <h1 {...titleProps}>{title}</h1>
        {titleActions}
      </div>
      {description && <p className="ui-heading-description">{description}</p>}
      {children}
    </div>
  );
}

export function PageHeading({
  actions,
  className = "",
  fullWidth = false,
  contentClassName,
  ...content
}: ComponentPropsWithoutRef<typeof HeadingContent> & {
  actions?: ReactNode;
  fullWidth?: boolean;
  contentClassName?: string;
}) {
  return (
    <header
      className={`page-heading ui-page-heading${fullWidth ? " ui-page-heading-full" : ""} ${className}`.trim()}
    >
      <HeadingContent {...content} className={contentClassName} />
      {actions}
    </header>
  );
}

export function SectionHeading({
  title,
  actions,
  children,
  className = "",
  titleId,
  eyebrow,
  level = 2,
}: {
  title: ReactNode;
  actions?: ReactNode;
  children?: ReactNode;
  className?: string;
  titleId?: string;
  eyebrow?: ReactNode;
  level?: 2 | 3;
}) {
  const Tag = level === 3 ? "h3" : "h2";
  return (
    <div className={`section-heading ui-section-heading ${className}`.trim()}>
      <div>
        {eyebrow && <p className="section-kicker">{eyebrow}</p>}
        <Tag id={titleId}>{title}</Tag>
        {children}
      </div>
      {actions}
    </div>
  );
}
