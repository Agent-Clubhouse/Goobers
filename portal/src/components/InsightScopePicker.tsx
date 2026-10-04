import { useId, useRef, useState } from "react";
import {
  insightScopeFromKey,
  insightScopeKey,
  insightScopeOption,
  type InsightScope,
} from "../insightScope";
import { Icon } from "../ui/Icon";

interface ScopeNode {
  key: string;
  label: string;
  name: string;
  children: ScopeNode[];
}

function scopeTree(options: { key: string; label: string }[]): ScopeNode[] {
  const nodes = new Map<string, ScopeNode>();
  const add = (scope: InsightScope): ScopeNode => {
    const option = insightScopeOption(scope);
    const existing = nodes.get(option.key);
    if (existing) return existing;
    const name = scope.kind === "instance" ? "Instance"
      : scope.kind === "gaggle" ? scope.gaggle
        : scope.kind === "workflow" ? scope.workflow : scope.stage;
    const node = { ...option, name, children: [] };
    nodes.set(option.key, node);
    return node;
  };
  const instance = add({ kind: "instance" });
  for (const option of options) {
    const scope = insightScopeFromKey(option.key);
    if (scope.kind === "instance") continue;
    const gaggle = add({ kind: "gaggle", gaggle: scope.gaggle });
    if (scope.kind === "gaggle") continue;
    const workflow = add({ kind: "workflow", gaggle: scope.gaggle, workflow: scope.workflow });
    if (!gaggle.children.includes(workflow)) gaggle.children.push(workflow);
    if (scope.kind === "stage") {
      const stage = add(scope);
      if (!workflow.children.includes(stage)) workflow.children.push(stage);
    }
  }
  const roots = [instance, ...[...nodes.values()].filter((node) => insightScopeFromKey(node.key).kind === "gaggle")];
  const sortChildren = (node: ScopeNode) => {
    node.children.sort((left, right) => left.name.localeCompare(right.name));
    node.children.forEach(sortChildren);
  };
  roots.forEach(sortChildren);
  return [instance, ...roots.slice(1).sort((left, right) => left.name.localeCompare(right.name))];
}

export function InsightScopePicker({
  ariaLabel = "Scope",
  searchPlaceholder = "Find gaggle, workflow, or stage",
  onChange,
  scopes,
  value,
}: {
  ariaLabel?: string;
  searchPlaceholder?: string;
  onChange: (value: string) => void;
  scopes: { key: string; label: string }[];
  value: string;
}) {
  const popupId = useId();
  const trigger = useRef<HTMLButtonElement>(null);
  const [open, setOpen] = useState(false);
  const [search, setSearch] = useState("");
  const [expanded, setExpanded] = useState(new Set<string>());
  const selected = insightScopeFromKey(value);
  const roots = scopeTree(scopes);
  const query = search.trim().toLowerCase();
  const matches = (node: ScopeNode): boolean =>
    node.label.toLowerCase().includes(query) || node.children.some(matches);
  const close = () => {
    setOpen(false);
    setSearch("");
  };
  const choose = (key: string) => {
    close();
    trigger.current?.focus();
    onChange(key);
  };
  const toggle = (key: string) => setExpanded((current) => {
    const next = new Set(current);
    if (next.has(key)) next.delete(key);
    else next.add(key);
    return next;
  });
  const renderNode = (node: ScopeNode) => {
    if (query && !matches(node)) return null;
    const hasChildren = node.children.length > 0;
    const isExpanded = Boolean(query) || expanded.has(node.key);
    return (
      <li key={node.key}>
        <div className="insight-scope-row">
          {hasChildren ? (
            <button
              aria-label={`${isExpanded ? "Collapse" : "Expand"} ${node.label}`}
              aria-expanded={isExpanded}
              className="insight-scope-expand"
              onClick={() => toggle(node.key)}
              type="button"
            >
              <Icon name="chevron" />
            </button>
          ) : <span className="insight-scope-indent" />}
          <button
            aria-label={node.label}
            aria-current={node.key === value ? "true" : undefined}
            className="insight-scope-choice"
            onClick={() => choose(node.key)}
            type="button"
          >
            <span>{node.name}</span>
            <small>{insightScopeFromKey(node.key).kind}</small>
          </button>
        </div>
        {hasChildren && isExpanded && <ul>{node.children.map(renderNode)}</ul>}
      </li>
    );
  };

  return (
    <div className="insight-scope-picker" onBlur={(event) => {
      if (!event.currentTarget.contains(event.relatedTarget)) close();
    }} onKeyDown={(event) => {
      if (event.key === "Escape" && open) {
        event.preventDefault();
        event.stopPropagation();
        close();
        trigger.current?.focus();
      }
    }}>
      <button
        aria-controls={popupId}
        aria-expanded={open}
        aria-haspopup="dialog"
        aria-label={ariaLabel}
        className="insight-scope-trigger"
        onClick={() => {
          if (open) close();
          else {
            setExpanded((current) => {
              const next = new Set(current);
              if (selected.kind !== "instance") next.add(insightScopeKey({ kind: "gaggle", gaggle: selected.gaggle }));
              if (selected.kind === "stage") next.add(insightScopeKey({ kind: "workflow", gaggle: selected.gaggle, workflow: selected.workflow }));
              return next;
            });
            setOpen(true);
          }
        }}
        ref={trigger}
        type="button"
      >
        <span>{insightScopeOption(selected).label}</span>
        <Icon name="chevron" />
      </button>
      {open && (
        <div aria-label="Select scope" className="insight-scope-popup" id={popupId} role="dialog">
          <input
            aria-label="Find scope"
            autoFocus
            onChange={(event) => setSearch(event.target.value)}
            placeholder={searchPlaceholder}
            type="search"
            value={search}
          />
          <ul>{roots.map(renderNode)}</ul>
          {!roots.some(matches) && <p className="inline-empty">No matching scopes.</p>}
        </div>
      )}
    </div>
  );
}
