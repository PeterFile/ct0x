import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { App } from "./App";

describe("workspace preview", () => {
  it("names the workspace and its three semantic columns", () => {
    render(<App />);

    expect(screen.getByRole("banner")).toContainElement(
      screen.getByRole("heading", { level: 1, name: "ct0x" }),
    );
    expect(screen.getByRole("complementary", { name: "群组" })).toBeVisible();
    expect(screen.getByRole("main", { name: "讨论" })).toBeVisible();
    expect(screen.getByRole("complementary", { name: "成员" })).toBeVisible();
  });

  it("provides a skip link to a focusable discussion landmark", () => {
    render(<App />);

    const link = screen.getByRole("link", { name: "跳至讨论区" });
    const main = screen.getByRole("main", { name: "讨论" });
    expect(link).toHaveAttribute("href", `#${main.id}`);
    expect(main.id).not.toBe("");
    expect(main).toHaveAttribute("tabindex", "-1");
    main.focus();
    expect(main).toHaveFocus();
  });

  it("distinguishes uninitialized views from empty server data", () => {
    render(<App />);

    expect(screen.getByText("界面预览 · 尚未接入本地服务")).toBeVisible();
    expect(screen.getByText(/未读取任何群组、成员或消息/)).toBeVisible();
    expect(within(screen.getByRole("complementary", { name: "群组" }))
      .getByRole("heading", { name: "尚未载入群组" })).toBeVisible();
    expect(within(screen.getByRole("complementary", { name: "成员" }))
      .getByRole("heading", { name: "尚未载入成员" })).toBeVisible();
    expect(within(screen.getByRole("main"))
      .getByRole("heading", { name: "讨论区尚未启用" })).toBeVisible();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("explains unavailable operations without offering dead controls", () => {
    render(<App />);

    expect(screen.getByText("接入服务后，才能创建或选择群组。")).toBeVisible();
    expect(screen.getByText("消息发送与成员操作尚未开放。")).toBeVisible();
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
    expect(screen.getAllByRole("link")).toHaveLength(1);
  });
});
