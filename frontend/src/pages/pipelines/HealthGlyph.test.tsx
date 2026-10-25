import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import { HealthGlyph, healthLabel } from "./HealthGlyph";
import type { JobHealth } from "../../types";

const health = (bucket: JobHealth["bucket"], passed = 0, counted = 0): JobHealth => ({
  bucket,
  window: 10,
  counted,
  passed,
  score: counted > 0 ? Math.floor((passed * 100) / counted) : undefined,
});

describe("HealthGlyph", () => {
  it.each([
    ["sunny", 9, 10, /^Sunny: 9 of 10 counted builds passed$/],
    ["partlyCloudy", 8, 10, /^Partly cloudy: 8 of 10 counted builds passed$/],
    ["cloudy", 5, 10, /^Cloudy: 5 of 10 counted builds passed$/],
    ["rainy", 3, 10, /^Rainy: 3 of 10 counted builds passed$/],
    ["stormy", 1, 10, /^Stormy: 1 of 10 counted builds passed$/],
  ] as const)("names the %s bucket for a screen reader", (bucket, passed, counted, want) => {
    render(<HealthGlyph health={health(bucket, passed, counted)} />);
    expect(screen.getByRole("img", { name: want })).toBeInTheDocument();
  });

  it("renders nothing for an unknown job", () => {
    const { container } = render(<HealthGlyph health={health("unknown")} />);
    expect(container).toBeEmptyDOMElement();
    expect(healthLabel(health("unknown"))).toBe("No counted build yet");
  });
});
