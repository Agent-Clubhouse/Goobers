import { useState } from "react";
import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { Action, ActionLink } from "./Action";
import { DataTable } from "./DataTable";
import { ControlGroup, FilterField, FilterOption, FilterOptions } from "./Filters";
import { HeadingContent, PageHeading, SectionHeading } from "./Heading";
import { MetadataGrid } from "./MetadataGrid";
import { Tab, TabList } from "./Tabs";
import { Timestamp } from "./Timestamp";
import primitives from "./primitives.css?inline";
import tokens from "../tokens.css?inline";

describe("shared portal primitives", () => {
  it("uses the same heading content on pages and toolbars, preserving descriptions and actions", () => {
    render(
      <>
        <PageHeading
          title="Cost"
          description="Selected-scope spend."
          fullWidth
          titleActions={<Action>Refresh cost</Action>}
        />
        <HeadingContent
          title="Runs"
          description="Recorded run history."
          className="page-toolbar-heading"
        />
        <SectionHeading
          title="Recent runs"
          actions={<ActionLink href="#/runs">View runs</ActionLink>}
        />
      </>,
    );
    for (const title of ["Cost", "Runs"]) {
      expect(
        screen.getByRole("heading", { name: title, level: 1 }).closest(".ui-heading-content"),
      ).not.toBeNull();
    }
    expect(screen.getByText("Selected-scope spend.")).toHaveClass("ui-heading-description");
    expect(
      screen.getByRole("heading", { name: "Recent runs", level: 2 }).closest(".ui-section-heading"),
    ).not.toBeNull();
    expect(screen.getByRole("link", { name: "View runs" })).toHaveAttribute("href", "#/runs");
  });

  it("keeps action buttons non-submitting and preserves disabled state and link semantics", async () => {
    const submit = vi.fn();
    const user = userEvent.setup();
    render(
      <form onSubmit={submit}>
        <Action variant="primary">Refresh</Action>
        <Action disabled>Blocked</Action>
        <ActionLink href="#/runs" size="compact">
          View run
        </ActionLink>
      </form>,
    );
    await user.click(screen.getByRole("button", { name: "Refresh" }));
    expect(submit).not.toHaveBeenCalled();
    expect(screen.getByRole("button", { name: "Blocked" })).toBeDisabled();
    expect(screen.getByRole("link", { name: "View run" })).toHaveClass("ui-action-compact");
  });

  it("preserves accessible filter labels and pressed state", async () => {
    const change = vi.fn();
    const user = userEvent.setup();
    render(
      <ControlGroup label="Scope and time">
        <FilterField label="Time window">
          <select defaultValue="7d">
            <option value="7d">Last 7 days</option>
          </select>
        </FilterField>
        <FilterOptions label="Type">
          <FilterOption selected>All</FilterOption>
          <FilterOption selected={false} onClick={change}>
            Issues
          </FilterOption>
        </FilterOptions>
      </ControlGroup>,
    );
    expect(screen.getByRole("combobox", { name: "Time window" })).toHaveValue("7d");
    expect(screen.getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "true");
    await user.click(screen.getByRole("button", { name: "Issues" }));
    expect(change).toHaveBeenCalledOnce();
  });

  it("gives native tables a shared header and keyboard-accessible scroll container", () => {
    render(
      <DataTable ariaLabel="Goobers" columns={["Goober", "Role"]}>
        <tr>
          <th scope="row">Implementer</th>
          <td>Implementation</td>
        </tr>
      </DataTable>,
    );
    const table = screen.getByRole("table", { name: "Goobers" });
    expect(
      within(table)
        .getAllByRole("columnheader")
        .map((cell) => cell.textContent),
    ).toEqual(["Goober", "Role"]);
    expect(screen.getByRole("region", { name: "Goobers comparison" })).toHaveAttribute(
      "tabindex",
      "0",
    );
    expect(table).toHaveClass("ui-data-table");
  });

  it("keeps metadata values separate, including each owner", () => {
    render(
      <MetadataGrid aria-label="Workflow metadata">
        <div>
          <dt>Owners</dt>
          <dd>
            <div>gaggle/implementer</div>
            <div>gaggle/reviewer</div>
          </dd>
        </div>
      </MetadataGrid>,
    );
    const owners = screen.getByText("Owners").nextElementSibling;
    expect(owners?.children).toHaveLength(2);
  });

  it("supports roving tab focus, wrapping, and Home/End without changing tab semantics", async () => {
    function Example() {
      const [selected, setSelected] = useState("fields");
      return (
        <TabList aria-label="Config view">
          {["fields", "yaml"].map((value) => (
            <Tab aria-selected={selected === value} key={value} onClick={() => setSelected(value)}>
              {value}
            </Tab>
          ))}
        </TabList>
      );
    }
    const user = userEvent.setup();
    render(<Example />);
    screen.getByRole("tab", { name: "fields" }).focus();
    await user.keyboard("{ArrowRight}");
    expect(screen.getByRole("tab", { name: "yaml" })).toHaveFocus();
    expect(screen.getByRole("tab", { name: "yaml" })).toHaveAttribute("aria-selected", "true");
    await user.keyboard("{ArrowRight}");
    expect(screen.getByRole("tab", { name: "fields" })).toHaveFocus();
    await user.keyboard("{End}");
    expect(screen.getByRole("tab", { name: "yaml" })).toHaveFocus();
    await user.keyboard("{Home}");
    expect(screen.getByRole("tab", { name: "fields" })).toHaveFocus();
  });

  it("renders valid dates semantically and does not emit invalid datetime attributes", () => {
    const { container } = render(
      <>
        <Timestamp value="2026-10-04T06:00:00Z" prefix="Completed " />
        <Timestamp value="invalid" />
        <Timestamp value={undefined} missing="In progress" />
      </>,
    );
    expect(container.querySelector("time")).toHaveAttribute("datetime", "2026-10-04T06:00:00Z");
    expect(container.querySelector("time")).toHaveAttribute("title");
    expect(container.querySelectorAll("time")).toHaveLength(1);
    expect(screen.getByText("Unavailable")).toBeInTheDocument();
    expect(screen.getByText("In progress")).toBeInTheDocument();
  });

  it("uses semantic color and typography tokens rather than hard-coded brand colors or font sizes", () => {
    expect(primitives).not.toMatch(/#[0-9a-f]{3,8}\b|font-size:\s*[\d.]+(?:px|rem)/i);
    for (const token of [
      "--font-page-title",
      "--font-section-title",
      "--font-ui",
      "--font-meta",
      "--on-accent",
    ]) {
      expect(primitives).toContain(`var(${token})`);
      expect(tokens).toContain(`${token}:`);
    }
    expect(primitives).toContain("var(--accent-ink)");
    expect(tokens).toContain(':root[data-theme="dark"]');
  });
});
