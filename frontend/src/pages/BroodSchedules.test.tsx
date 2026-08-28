import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { renderWithProviders } from "../test/render-utils";
import BroodSchedules from "./BroodSchedules";

const mockNavigate = vi.fn();
vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return { ...actual, useNavigate: () => mockNavigate };
});

const mockList = vi.fn();
const mockCreate = vi.fn();
const mockDelete = vi.fn();
const mockSuspend = vi.fn();
vi.mock("../api/client", () => ({
  listBroodSchedules: (...args: unknown[]) => mockList(...args),
  createBroodSchedule: (...args: unknown[]) => mockCreate(...args),
  deleteBroodSchedule: (...args: unknown[]) => mockDelete(...args),
  suspendBroodSchedule: (...args: unknown[]) => mockSuspend(...args),
}));

// Stub the controller picker so the schedule form's target-shaping logic can be
// exercised without the picker's data dependencies. The button pushes a
// "cluster/namespace/name" selection key up to the parent, exactly as the real
// picker does.
vi.mock("../components/BroodControllerPicker", () => ({
  BroodControllerPicker: ({
    onSelectionChange,
  }: {
    onSelectionChange: (keys: string[]) => void;
  }) => (
    <button type="button" onClick={() => onSelectionChange(["core/team-ns/ctrl-1"])}>
      select-ctrl-1
    </button>
  ),
}));

vi.mock("../context/AuthContext", () => ({
  useAuth: () => ({
    permissions: {
      global: { controllers: { read: true, manage: true } },
      scopes: [],
    },
  }),
  AuthProvider: ({ children }: { children: React.ReactNode }) => children,
}));

function renderPage() {
  return renderWithProviders(<BroodSchedules />);
}

const scheduleRow = {
  namespace: "team-ns",
  name: "my-sched",
  spec: {
    schedule: "*/5 * * * *",
    suspend: false,
    waitForCompletion: true,
    template: { targets: { names: ["ctrl-1"] }, action: { verb: "reconcile" as const } },
  },
  status: { lastScheduleTime: new Date().toISOString() },
};

describe("BroodSchedules", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("shows loading text when data is loading", () => {
    mockList.mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(screen.getByText("Loading…")).toBeInTheDocument();
  });

  it("shows empty state when no schedules exist", async () => {
    mockList.mockResolvedValue({ items: [] });
    renderPage();
    expect(await screen.findByText(/No brood schedules yet/)).toBeInTheDocument();
  });

  it("renders a table with schedule rows", async () => {
    mockList.mockResolvedValue({ items: [scheduleRow] });
    renderPage();
    expect(await screen.findByText("team-ns/my-sched")).toBeInTheDocument();
    expect(screen.getByText("*/5 * * * *")).toBeInTheDocument();
  });

  it("shows the create button for admin users", async () => {
    mockList.mockResolvedValue({ items: [] });
    renderPage();
    expect(await screen.findByText("Create schedule…")).toBeInTheDocument();
  });

  it("navigates from the controller button and schedule row", async () => {
    const user = userEvent.setup();
    mockList.mockResolvedValue({ items: [scheduleRow] });
    renderPage();
    await user.click(await screen.findByText("Back to Controllers"));
    expect(mockNavigate).toHaveBeenCalledWith("/controllers");
    await user.click(screen.getByText("team-ns/my-sched"));
    expect(mockNavigate).toHaveBeenCalledWith("/brood-schedules/team-ns/my-sched");
  });

  it("suspends and deletes schedules after confirmation", async () => {
    const user = userEvent.setup();
    mockList.mockResolvedValue({ items: [scheduleRow] });
    mockSuspend.mockResolvedValue(undefined);
    mockDelete.mockResolvedValue(undefined);
    renderPage();

    await user.click(await screen.findByText("Suspend"));
    const suspendButtons = screen.getAllByRole("button", { name: "Suspend" });
    await user.click(suspendButtons[suspendButtons.length - 1]);
    await waitFor(() => expect(mockSuspend).toHaveBeenCalledWith("team-ns", "my-sched", true));

    await user.click(screen.getByText("Delete"));
    await user.click(screen.getByText("Yes, Delete"));
    await waitFor(() => expect(mockDelete).toHaveBeenCalledWith("team-ns", "my-sched"));
  });

  it("validates required fields and creates a schedule with correctly-shaped targets", async () => {
    const user = userEvent.setup();
    mockList.mockResolvedValue({ items: [] });
    mockCreate.mockResolvedValue(undefined);
    renderPage();

    await user.click(await screen.findByText("Create schedule…"));

    // Name and cron are required before anything else.
    await user.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByText("Name is required")).toBeInTheDocument();
    await user.type(screen.getByLabelText("Name:"), "nightly");
    await user.type(screen.getByLabelText("Cron:"), "0 2 * * *");

    // At least one target controller must be selected.
    await user.click(screen.getByRole("button", { name: "Create" }));
    expect(await screen.findByText("Select at least one controller")).toBeInTheDocument();

    // Select a controller via the (stubbed) picker.
    await user.click(screen.getByText("select-ctrl-1"));

    await user.selectOptions(screen.getByLabelText("Concurrency policy:"), "Forbid");
    await user.type(screen.getByLabelText("Max parallel:"), "2");
    await user.type(screen.getByLabelText("TTL seconds:"), "60");
    await user.click(screen.getByRole("button", { name: "Create" }));

    await waitFor(() => expect(mockCreate).toHaveBeenCalled());
    // A single cluster + single namespace selection must produce an explicit
    // namespace with bare names (team-mode) and a clusters list — not the bare,
    // unqualified names the old comma-field sent, which the BFF rejected with a
    // 400 in operator-namespace mode.
    const req = mockCreate.mock.calls[0][0];
    expect(req).toMatchObject({
      name: "nightly",
      namespace: "team-ns",
      spec: {
        schedule: "0 2 * * *",
        concurrencyPolicy: "Forbid",
        waitForCompletion: true,
        template: {
          targets: { names: ["ctrl-1"] },
          action: { verb: "reconcile" },
          clusters: ["core"],
          execution: { order: "rolloutWave", failurePolicy: "FailTidy" },
          ttlSecondsAfterFinished: 60,
        },
      },
    });
    // maxParallel > 1 flows through as a number (the exact digits depend on the
    // controlled number input, which jsdom can't select-all-replace).
    expect(req.spec.template.execution.maxParallel).toBeGreaterThan(1);
  });

  it("lets max parallel and TTL be cleared and retyped instead of appending to the default (#428)", async () => {
    const user = userEvent.setup();
    mockList.mockResolvedValue({ items: [] });
    renderPage();

    await user.click(await screen.findByText("Create schedule…"));

    const maxParallel = screen.getByLabelText("Max parallel:") as HTMLInputElement;
    expect(maxParallel.value).toBe("1");
    await user.click(maxParallel);
    await user.keyboard("{Backspace}");
    expect(maxParallel.value).toBe("");
    await user.keyboard("5");
    expect(maxParallel.value).toBe("5");

    const ttl = screen.getByLabelText("TTL seconds:") as HTMLInputElement;
    expect(ttl.value).toBe("0");
    await user.click(ttl);
    await user.keyboard("{Backspace}");
    expect(ttl.value).toBe("");
    await user.keyboard("30");
    expect(ttl.value).toBe("30");
  });

  it("clamps max parallel and TTL back to their defaults on blur when left empty", async () => {
    const user = userEvent.setup();
    mockList.mockResolvedValue({ items: [] });
    renderPage();

    await user.click(await screen.findByText("Create schedule…"));

    const maxParallel = screen.getByLabelText("Max parallel:") as HTMLInputElement;
    await user.click(maxParallel);
    await user.keyboard("{Backspace}");
    expect(maxParallel.value).toBe("");
    await user.tab();
    expect(maxParallel.value).toBe("1");

    const ttl = screen.getByLabelText("TTL seconds:") as HTMLInputElement;
    await user.click(ttl);
    await user.keyboard("{Backspace}");
    expect(ttl.value).toBe("");
    await user.tab();
    expect(ttl.value).toBe("0");
  });
});
