import { describe, it, expect } from "vitest";
import {
  DEFAULT_FEED_QUERY,
  applyStatusSelect,
  feedQueryIsDefault,
  parseFeedQuery,
  statusSelectValue,
  writeFeedQuery,
} from "./feedQuery";

describe("feedQuery", () => {
  it("round-trips every field", () => {
    const sp = new URLSearchParams(
      "tenant=a&tenant=b&job=a%2Fx&trigger=cron&result=FAILURE&since=24h&q=fix&sort=duration&order=asc",
    );
    expect(writeFeedQuery(parseFeedQuery(sp)).toString()).toBe(sp.toString());
  });

  it("omits defaults", () => {
    expect(writeFeedQuery(DEFAULT_FEED_QUERY).toString()).toBe("");
  });

  it("drops an unknown sort", () => {
    expect(parseFeedQuery(new URLSearchParams("sort=size")).sort).toBe("started");
  });

  it("drops an unknown order", () => {
    expect(parseFeedQuery(new URLSearchParams("order=up")).order).toBe("desc");
  });

  it("maps the status select to status for a run state", () => {
    const next = applyStatusSelect({ ...DEFAULT_FEED_QUERY, result: "FAILURE" }, "WAITING_INPUT");
    expect(next.status).toBe("WAITING_INPUT");
    expect(next.result).toBe("");
  });

  it("maps the status select to result for a result", () => {
    const next = applyStatusSelect({ ...DEFAULT_FEED_QUERY, status: "RUNNING" }, "FAILURE");
    expect(next.result).toBe("FAILURE");
    expect(next.status).toBe("");
  });

  it("clears both fields for the All choice", () => {
    const next = applyStatusSelect({ ...DEFAULT_FEED_QUERY, status: "QUEUED", result: "" }, "");
    expect(next.status).toBe("");
    expect(next.result).toBe("");
  });

  it("reads the result before the status", () => {
    expect(statusSelectValue({ ...DEFAULT_FEED_QUERY, status: "QUEUED", result: "SUCCESS" })).toBe("SUCCESS");
    expect(statusSelectValue({ ...DEFAULT_FEED_QUERY, status: "QUEUED" })).toBe("QUEUED");
    expect(statusSelectValue(DEFAULT_FEED_QUERY)).toBe("");
  });

  it("reports the default query", () => {
    expect(feedQueryIsDefault(DEFAULT_FEED_QUERY)).toBe(true);
    expect(feedQueryIsDefault({ ...DEFAULT_FEED_QUERY, sort: "duration" })).toBe(false);
  });
});
