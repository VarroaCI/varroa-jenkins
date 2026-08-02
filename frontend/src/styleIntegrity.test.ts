import { describe, expect, it } from "vitest";
import fs from "fs";
import path from "path";

const srcDir = path.resolve(__dirname);

/** Recursively list all files matching a predicate under a directory. */
function findFiles(dir: string, predicate: (name: string) => boolean): string[] {
  const results: string[] = [];
  let entries: string[];
  try {
    entries = fs.readdirSync(dir);
  } catch {
    return results;
  }
  for (const entry of entries) {
    const full = path.join(dir, entry);
    let stat: fs.Stats;
    try {
      stat = fs.statSync(full);
    } catch {
      continue;
    }
    if (stat.isDirectory()) {
      if (entry === "node_modules") continue;
      results.push(...findFiles(full, predicate));
    } else if (stat.isFile() && predicate(entry)) {
      results.push(full);
    }
  }
  return results;
}

/* ------------------------------------------------------------------ */
/*  (a) Orphan classes                                                 */
/* ------------------------------------------------------------------ */
describe("style integrity — orphan classes", () => {
  const tsxFiles = findFiles(srcDir, (name) => name.endsWith(".tsx"));

  for (const tsxPath of tsxFiles) {
    const code = fs.readFileSync(tsxPath, "utf-8");

    // Match `import styles from "<rel>.module.css"`
    const importMatch = code.match(
      /import\s+styles\s+from\s+["']([^"']+\.module\.css)["']/,
    );
    if (!importMatch) continue;

    const cssRel = importMatch[1];
    const cssAbs = path.resolve(path.dirname(tsxPath), cssRel);

    if (!fs.existsSync(cssAbs)) {
      it(`${path.relative(srcDir, tsxPath).split(path.sep).join("/")} — missing module`, () => {
        expect(true).toBe(false);
      });
      continue;
    }

    const cssText = fs.readFileSync(cssAbs, "utf-8");

    // Collect defined class names from the CSS module
    const definedClasses = new Set<string>();
    const classRe = /\.([A-Za-z_][\w-]*)/g;
    let m: RegExpExecArray | null;
    while ((m = classRe.exec(cssText)) !== null) {
      definedClasses.add(m[1]);
    }

    // Collect all styles.X references
    const refs = new Set<string>();
    // styles.SomeName (dotted access)
    const dotRe = /styles\.([A-Za-z_$][\w$]*)/g;
    while ((m = dotRe.exec(code)) !== null) {
      refs.add(m[1]);
    }
    // styles["some-name"] (bracket access)
    const bracketRe = /styles\[["']([^"']+)["']\]/g;
    while ((m = bracketRe.exec(code)) !== null) {
      refs.add(m[1]);
    }

    if (refs.size === 0) continue;

    const rel = path.relative(srcDir, tsxPath).split(path.sep).join("/");

    for (const ref of refs) {

      it(`${rel} → styles.${ref} resolves in ${path.basename(cssRel)}`, () => {
        expect(definedClasses.has(ref)).toBe(true);
      });
    }
  }
});

/* ------------------------------------------------------------------ */
/*  (b) Whole token namespace — every var(--X) must resolve            */
/* ------------------------------------------------------------------ */
/**
 * Supersedes the old per-namespace phantom-token checks (`--color-*`,
 * `--good*`). Those were a narrow ratchet on two namespaces that had
 * already accumulated debt; this check covers every custom-property
 * reference under `src/`, so a new namespace can never go undefined
 * again. A `var(--X)` reference is valid if `--X` is declared either in
 * `styles/tokens.css` (the shared token sheet) or anywhere in the same
 * source file (a locally-scoped custom property, e.g. `--rail-w` in
 * `Sidebar.module.css`). No allowlist or escape hatch — same policy as
 * the orphan-class check above.
 */
describe("style integrity — token namespace", () => {
  const tokensPath = path.join(srcDir, "styles", "tokens.css");
  const tokensText = fs.readFileSync(tokensPath, "utf-8");

  // Custom-property declarations look like `--name:` (a colon immediately
  // follows the name, modulo whitespace) — this reliably distinguishes a
  // declaration from a `var(--name)` / `var(--name, fallback)` usage,
  // which is always followed by `)` or `,`.
  const declRe = /--([A-Za-z0-9_-]+)\s*:/g;

  const definedTokens = new Set<string>();
  let dm: RegExpExecArray | null;
  while ((dm = declRe.exec(tokensText)) !== null) {
    definedTokens.add(dm[1]);
  }

  const sourceFiles = findFiles(
    srcDir,
    (name) => name.endsWith(".css") || name.endsWith(".tsx"),
  ).filter((p) => p !== tokensPath);

  const violations: string[] = [];

  for (const sourcePath of sourceFiles) {
    const sourceText = fs.readFileSync(sourcePath, "utf-8");

    const localTokens = new Set<string>();
    const localDeclRe = /--([A-Za-z0-9_-]+)\s*:/g;
    let lm: RegExpExecArray | null;
    while ((lm = localDeclRe.exec(sourceText)) !== null) {
      localTokens.add(lm[1]);
    }

    const referenced = new Set<string>();
    const varRe = /var\(\s*--([A-Za-z0-9_-]+)/g;
    let vm: RegExpExecArray | null;
    while ((vm = varRe.exec(sourceText)) !== null) {
      referenced.add(vm[1]);
    }

    for (const name of referenced) {
      if (!definedTokens.has(name) && !localTokens.has(name)) {
        const rel = path.relative(srcDir, sourcePath).split(path.sep).join("/");
        violations.push(`${rel} → var(--${name})`);
      }
    }
  }

  it("every var(--X) resolves to a token in tokens.css or a local declaration", () => {
    expect(violations).toEqual([]);
  });
});

/* ------------------------------------------------------------------ */
/*  (c) Text-color contrast ratchet — --text-3 / raw status tokens     */
/* ------------------------------------------------------------------ */
/**
 * frontend/AGENTS.md's contrast rule: `--text-3` fails the 4.5:1 floor as
 * a foreground color and is decorative/non-text only (use `--text-2` for
 * text). Raw status tokens (`--ok`/`--warn`/`--bad`/`--info`/`--idle`)
 * are reserved for non-text surfaces (dots, bars, borders, rings); text
 * must use the matching `-text` variant. This only inspects the literal
 * `color:` property — a negative lookbehind excludes `border-color:`,
 * `background-color:`, `outline-color:`, etc., which are legitimate
 * non-text uses — so it needs no CSS parser. No allowlist or escape
 * hatch — same policy as the checks above.
 */
describe("style integrity — text-color token ratchet", () => {
  const cssFiles = findFiles(srcDir, (name) => name.endsWith(".css")).filter(
    (p) => p !== path.join(srcDir, "styles", "tokens.css"),
  );

  const textColorRe = /(?<![\w-])color:\s*var\(\s*--([A-Za-z0-9_-]+)/g;
  const rawStatusTokens = new Set(["ok", "warn", "bad", "info", "idle"]);

  const text3Violations: string[] = [];
  const rawStatusViolations: string[] = [];

  for (const cssPath of cssFiles) {
    const cssText = fs.readFileSync(cssPath, "utf-8");
    const rel = path.relative(srcDir, cssPath).split(path.sep).join("/");

    let m: RegExpExecArray | null;
    textColorRe.lastIndex = 0;
    while ((m = textColorRe.exec(cssText)) !== null) {
      const token = m[1];
      if (token === "text-3") {
        text3Violations.push(rel);
      } else if (rawStatusTokens.has(token)) {
        rawStatusViolations.push(`${rel} → var(--${token})`);
      }
    }
  }

  it("no `color:` declaration uses --text-3 (use --text-2)", () => {
    expect(text3Violations).toEqual([]);
  });

  it("no `color:` declaration uses a raw status token (use the -text variant)", () => {
    expect(rawStatusViolations).toEqual([]);
  });
});
