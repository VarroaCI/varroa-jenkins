import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { LoginPage } from "./LoginPage";

// login() is provided by AuthContext; mock it so the page can be tested
// in isolation without the react-query / router providers.
const login = vi.fn();
vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({ login }),
}));

describe("LoginPage", () => {
  beforeEach(() => {
    login.mockReset();
  });

  it("renders the sign-in form", () => {
    render(<LoginPage />);
    expect(
      screen.getByRole("heading", { name: /sign in to varroa/i }),
    ).toBeInTheDocument();
    expect(screen.getByLabelText(/username/i)).toBeInTheDocument();
    expect(screen.getByLabelText(/password/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /sign in/i })).toBeInTheDocument();
  });

  it("requires both fields before calling login", async () => {
    const user = userEvent.setup();
    render(<LoginPage />);
    await user.click(screen.getByRole("button", { name: /sign in/i }));
    expect(login).not.toHaveBeenCalled();
    expect(
      screen.getByText(/username and password are required/i),
    ).toBeInTheDocument();
  });

  it("calls login with the trimmed username and password on valid submit", async () => {
    login.mockResolvedValueOnce(undefined);
    const user = userEvent.setup();
    render(<LoginPage />);

    await user.type(screen.getByLabelText(/username/i), "  alice  ");
    await user.type(screen.getByLabelText(/password/i), "s3cret");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() => expect(login).toHaveBeenCalledWith("alice", "s3cret"));
  });

  it("shows an error when login rejects", async () => {
    login.mockRejectedValueOnce(new Error("401"));
    const user = userEvent.setup();
    render(<LoginPage />);

    await user.type(screen.getByLabelText(/username/i), "alice");
    await user.type(screen.getByLabelText(/password/i), "wrong");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    expect(
      await screen.findByText(/invalid credentials/i),
    ).toBeInTheDocument();
  });

  it("marks empty fields aria-invalid on submit", async () => {
    const user = userEvent.setup();
    render(<LoginPage />);
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    expect(screen.getByLabelText(/username/i)).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText(/password/i)).toHaveAttribute("aria-invalid", "true");
  });

  it("marks only the missing field aria-invalid", async () => {
    const user = userEvent.setup();
    render(<LoginPage />);
    await user.type(screen.getByLabelText(/username/i), "alice");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    expect(screen.getByLabelText(/username/i)).not.toHaveAttribute("aria-invalid");
    expect(screen.getByLabelText(/password/i)).toHaveAttribute("aria-invalid", "true");
  });

  it("clears a field's aria-invalid once the user types into it", async () => {
    const user = userEvent.setup();
    render(<LoginPage />);
    await user.click(screen.getByRole("button", { name: /sign in/i }));
    expect(screen.getByLabelText(/username/i)).toHaveAttribute("aria-invalid", "true");

    await user.type(screen.getByLabelText(/username/i), "a");
    expect(screen.getByLabelText(/username/i)).not.toHaveAttribute("aria-invalid");
    // the untouched password field stays flagged
    expect(screen.getByLabelText(/password/i)).toHaveAttribute("aria-invalid", "true");
  });

  it("shows a callback error but lets a live validation error take over", async () => {
    const user = userEvent.setup();
    render(<LoginPage authError="access_denied" />);
    // the callback error is surfaced on first render
    expect(screen.getByRole("alert")).toHaveTextContent(/access_denied/i);

    // a live validation error must not be masked by the stale callback error
    await user.click(screen.getByRole("button", { name: /sign in/i }));
    expect(screen.getByRole("alert")).toHaveTextContent(
      /username and password are required/i,
    );
  });

  it("flags both fields aria-invalid when login rejects", async () => {
    login.mockRejectedValueOnce(new Error("401"));
    const user = userEvent.setup();
    render(<LoginPage />);
    await user.type(screen.getByLabelText(/username/i), "alice");
    await user.type(screen.getByLabelText(/password/i), "wrong");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    await screen.findByText(/invalid credentials/i);
    expect(screen.getByLabelText(/username/i)).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByLabelText(/password/i)).toHaveAttribute("aria-invalid", "true");
  });
});
