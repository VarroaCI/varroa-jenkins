import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ToastProvider, useToast } from "./Toast";

/** Helper component that exposes the toast function via a button click. */
function ToastHarness({ message = "test message" }: { message?: string }) {
  const { toast } = useToast();
  return <button onClick={() => toast(message)}>Show Toast</button>;
}

describe("Toast", () => {
  it("useToast() returns a toast function", () => {
    let toastFn: unknown;
    function TestComponent() {
      const { toast } = useToast();
      toastFn = toast;
      return null;
    }
    render(
      <ToastProvider>
        <TestComponent />
      </ToastProvider>,
    );
    expect(typeof toastFn).toBe("function");
  });

  it("calling toast('message') shows the message", async () => {
    render(
      <ToastProvider>
        <ToastHarness />
      </ToastProvider>,
    );
    await userEvent.click(screen.getByText("Show Toast"));
    expect(screen.getByText(/test message/)).toBeInTheDocument();
  });

  it("throws if used outside ToastProvider", () => {
    function TestComponent() {
      useToast();
      return null;
    }
    expect(() => render(<TestComponent />)).toThrow(
      "useToast must be used within ToastProvider",
    );
  });

  it("multiple toasts render simultaneously", async () => {
    render(
      <ToastProvider>
        <ToastHarness />
      </ToastProvider>,
    );
    const btn = screen.getByText("Show Toast");
    await userEvent.click(btn);
    await userEvent.click(btn);
    await userEvent.click(btn);
    const toasts = screen.getAllByText(/test message/);
    expect(toasts).toHaveLength(3);
  });

  it("each toast displays its own message text", async () => {
    render(
      <ToastProvider>
        <ToastHarness message="first toast" />
        <ToastHarness message="second toast" />
      </ToastProvider>,
    );
    await userEvent.click(screen.getAllByText("Show Toast")[0]);
    await userEvent.click(screen.getAllByText("Show Toast")[1]);
    expect(screen.getByText(/first toast/)).toBeInTheDocument();
    expect(screen.getByText(/second toast/)).toBeInTheDocument();
  });
});
