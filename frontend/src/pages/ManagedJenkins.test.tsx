import { describe, it, expect, vi, beforeEach } from "vitest";
import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router-dom";
import ManagedJenkins from "./ManagedJenkins";

const queryClient = new QueryClient({
  defaultOptions: { queries: { retry: false } },
});

function renderWithProviders(ui: React.ReactElement) {
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>,
  );
}

vi.mock("react-router-dom", async () => {
  const actual = await vi.importActual("react-router-dom");
  return {
    ...actual,
    useParams: () => ({ cluster: "core", namespace: "my-ns", name: "my-ctrl" }),
  };
});

const mockUseController = vi.fn();
vi.mock("../hooks/useControllers", () => ({
  useController: () => mockUseController(),
}));

describe("ManagedJenkins", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  describe("Loading state", () => {
    it("shows a loading indicator when controller data is loading", () => {
      mockUseController.mockReturnValue({
        data: null,
        isLoading: true,
        error: null,
      });
      renderWithProviders(<ManagedJenkins />);
      expect(screen.getByText("Loading...")).toBeInTheDocument();
    });
  });

  describe("Path mode with endpoint - embedded iframe", () => {
    it("renders an iframe for path-mode controllers with an endpoint", () => {
      mockUseController.mockReturnValue({
        data: {
          name: "my-ctrl",
          namespace: "my-ns",
          phase: "Running",
          endpoint: "https://varroa.example.com/jenkins/my-ns/my-ctrl/",
          miteConnected: true,
          routingMode: "path",
        },
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ManagedJenkins />);

      const iframe = document.querySelector("iframe");
      expect(iframe).toBeInTheDocument();
      expect(iframe).toHaveAttribute("src", "https://varroa.example.com/jenkins/my-ns/my-ctrl/");
    });
  });

  describe("Subdomain mode with endpoint - embed info", () => {
    it("shows a message and external link for subdomain controllers", () => {
      mockUseController.mockReturnValue({
        data: {
          name: "my-ctrl",
          namespace: "my-ns",
          phase: "Running",
          endpoint: "https://jenkins.example.com",
          miteConnected: true,
          routingMode: "subdomain",
        },
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ManagedJenkins />);

      expect(screen.getByText(/Cannot embed/)).toBeInTheDocument();
      expect(screen.getByText(/subdomain routing/)).toBeInTheDocument();
      const link = screen.getByText("Open Jenkins ↗");
      expect(link).toBeInTheDocument();
      expect(link.closest("a")).toHaveAttribute("href", "https://jenkins.example.com");
    });

    it("shows back link for subdomain controllers", () => {
      mockUseController.mockReturnValue({
        data: {
          name: "my-ctrl",
          namespace: "my-ns",
          phase: "Running",
          endpoint: "https://jenkins.example.com",
          miteConnected: true,
          routingMode: "subdomain",
        },
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ManagedJenkins />);

      const backLink = screen.getByText(/Back to controller/);
      expect(backLink).toBeInTheDocument();
    });

    it("does not render iframe for subdomain controllers", () => {
      mockUseController.mockReturnValue({
        data: {
          name: "my-ctrl",
          namespace: "my-ns",
          phase: "Running",
          endpoint: "https://jenkins.example.com",
          miteConnected: true,
          routingMode: "subdomain",
        },
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ManagedJenkins />);

      expect(document.querySelector("iframe")).not.toBeInTheDocument();
    });
  });

  describe("Without endpoint - not yet provisioned", () => {
    it("shows the 'not yet provisioned' card when controller has no endpoint", () => {
      mockUseController.mockReturnValue({
        data: {
          name: "my-ctrl",
          namespace: "my-ns",
          phase: "Provisioning",
          endpoint: "",
          miteConnected: false,
        },
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ManagedJenkins />);

      expect(screen.getByText(/Controller not yet provisioned/)).toBeInTheDocument();
      expect(screen.getByText("my-ctrl")).toBeInTheDocument();
      expect(screen.getByText(/still provisioning/)).toBeInTheDocument();
    });

    it("shows back link to controller detail", () => {
      mockUseController.mockReturnValue({
        data: {
          name: "my-ctrl",
          namespace: "my-ns",
          phase: "Pending",
          endpoint: "",
          miteConnected: false,
        },
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ManagedJenkins />);

      const backLink = screen.getByText(/Back to controller/);
      expect(backLink).toBeInTheDocument();
      expect(backLink.closest("a")).toHaveAttribute(
        "href",
        "/controllers/core/my-ns/my-ctrl",
      );
    });

    it("shows the controller phase and mite connection status", () => {
      mockUseController.mockReturnValue({
        data: {
          name: "my-ctrl",
          namespace: "my-ns",
          phase: "Provisioning",
          endpoint: "",
          miteConnected: true,
        },
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ManagedJenkins />);

      expect(screen.getByText("Provisioning")).toBeInTheDocument();
      expect(screen.getByText(/mite connected/)).toBeInTheDocument();
    });

    it("shows mite disconnected when not connected", () => {
      mockUseController.mockReturnValue({
        data: {
          name: "my-ctrl",
          namespace: "my-ns",
          phase: "Pending",
          endpoint: "",
          miteConnected: false,
        },
        isLoading: false,
        error: null,
      });
      renderWithProviders(<ManagedJenkins />);

      expect(screen.getByText(/mite disconnected/)).toBeInTheDocument();
    });
  });
});
