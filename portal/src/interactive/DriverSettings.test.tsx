import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { DriverSettingsPills } from "./DriverSettings";

const settings = {model: "model-a", level: "ask" as const, effort: "high", models: [{id: "model-a", label: "Model A"}, {id: "model-b", label: "Model B"}], efforts: ["low", "high"]};
const capabilities = {steer: true, queue: "none" as const, interrupt: false, liveLevels: ["ask", "plan"] as ("ask" | "plan")[], modelSwitch: "per_turn" as const, effortSwitch: "live" as const};
describe("driver settings", () => {
  it("offers only advertised live permission levels and waits for confirmed values", () => {
    const onChange = vi.fn();
    render(<DriverSettingsPills settings={settings} capabilities={capabilities} disabled={false} onChange={onChange} />);
    expect(screen.queryByRole("option", {name: "Full access"})).not.toBeInTheDocument();
    fireEvent.change(screen.getByRole("combobox", {name: "Permission level"}), {target: {value: "plan"}});
    expect(onChange).toHaveBeenCalledWith({type: "session.set_level", level: "plan"});
    expect(screen.getByRole("combobox", {name: "Permission level"})).toHaveValue("ask");
    fireEvent.change(screen.getByRole("combobox", {name: "Model"}), {target: {value: "model-b"}});
    expect(onChange).toHaveBeenCalledWith({type: "session.set_model", model: "model-b"});
    fireEvent.change(screen.getByRole("combobox", {name: "Effort"}), {target: {value: ""}});
    expect(onChange).toHaveBeenCalledWith({type: "session.set_effort", effort: ""});
  });
  it("disables changes when the driver does not support them", () => {
    render(<DriverSettingsPills settings={settings} capabilities={{...capabilities, liveLevels: [], modelSwitch: "none", effortSwitch: "none"}} disabled={false} onChange={vi.fn()} />);
    for (const select of screen.getAllByRole("combobox")) expect(select).toBeDisabled();
  });
});
