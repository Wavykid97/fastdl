// Tests the browser extension with Laneway on a real Mac: loads the
// extension into Chromium, clicks a download link on a web page, and checks
// that Laneway received it. Run by .github/workflows/mac-test.yml after
// test_mac.py has installed Laneway.
import { chromium } from "playwright";
import fs from "fs";
import http from "http";
import os from "os";
import path from "path";

const OUT = process.argv[2] || "out";
const SUPPORT = path.join(os.homedir(), "Library", "Application Support");
const EXT = "/Applications/Laneway.app/Contents/Resources/extension";
const DEV_ID = "eobniehblnoejkafhnckdnbffpcnnamk"; // the extension's ID when loaded from a folder
const sleep = ms => new Promise(r => setTimeout(r, ms));
const results = [];
const check = (name, ok, detail = "") => { results.push([name, !!ok, String(detail)]); console.log((ok ? "PASS " : "FAIL ") + name + (detail ? ": " + detail : "")); };

// A tiny website with a file link.
const file = Buffer.alloc(8 << 20, 1);
const server = http.createServer((req, res) => {
  if (req.url === "/") { res.writeHead(200, { "Content-Type": "text/html" }); return res.end('<a id="f" href="/Test%20Video.mp4">Test Video.mp4</a>'); }
  res.writeHead(200, { "Content-Type": "video/mp4", "Content-Length": file.length, "Accept-Ranges": "bytes" }); res.end(file);
}).listen(8127, "127.0.0.1");

// Laneway wrote its browser helper file for Chrome; give the test browser a copy.
const chromeHost = path.join(SUPPORT, "Google", "Chrome", "NativeMessagingHosts", "com.laneway.app.json");
const profile = fs.mkdtempSync(path.join(os.tmpdir(), "lw-profile-"));
const host = JSON.parse(fs.readFileSync(chromeHost, "utf8"));
for (const dir of [path.join(profile, "NativeMessagingHosts"), path.join(SUPPORT, "Chromium", "NativeMessagingHosts"), path.join(SUPPORT, "Google", "Chrome for Testing", "NativeMessagingHosts")]) {
  fs.mkdirSync(dir, { recursive: true });
  fs.writeFileSync(path.join(dir, "com.laneway.app.json"), JSON.stringify(host));
}
check("Laneway's helper file lets the folder copy of the extension in", host.allowed_origins.includes(`chrome-extension://${DEV_ID}/`));

const inst = () => JSON.parse(fs.readFileSync(path.join(SUPPORT, "Laneway", "instance.json"), "utf8"));
const state = async () => { const i = inst(); return (await fetch(`http://${i.addr}/api/state`, { headers: { "X-Token": i.token } })).json(); };

let ctx;
try {
  ctx = await chromium.launchPersistentContext(profile, { headless: false, viewport: { width: 1000, height: 700 },
    args: [`--disable-extensions-except=${EXT}`, `--load-extension=${EXT}`] });
  const sw = ctx.serviceWorkers()[0] || await ctx.waitForEvent("serviceworker", { timeout: 20000 });
  check("The extension loads", !!sw);
  const ping = await sw.evaluate(() => ask({ type: "ping" }));
  check("The extension finds Laneway", ping.ok, JSON.stringify(ping));
  // If Laneway was closed, the extension starts it; wait until it answers.
  for (let i = 0; i < 40; i++) { try { await state(); break; } catch { await sleep(500); } }

  const page = await ctx.newPage();
  await page.goto("http://127.0.0.1:8127/");
  await page.click("#f");
  let job = null;
  for (let i = 0; i < 60 && !job; i++) {
    await sleep(500);
    try { job = (await state()).jobs.find(j => j.name === "Test Video.mp4"); } catch {}
  }
  check("Clicking a file link sends it to Laneway", job, job && job.status);
  for (let i = 0; i < 60 && job && job.status !== "done"; i++) { await sleep(500); job = (await state()).jobs.find(j => j.name === "Test Video.mp4"); }
  check("…and Laneway downloads it", job && job.status === "done", job && job.status);

  const pop = await ctx.newPage();
  await pop.setViewportSize({ width: 360, height: 560 });
  await pop.goto(`chrome-extension://${DEV_ID}/popup.html`);
  await sleep(2500);
  const status = await pop.textContent("#status");
  check("The extension's popup is connected", /Connected/.test(status), status);
  await pop.screenshot({ path: path.join(OUT, "4-extension-popup.png") });
} catch (e) {
  check("Extension test ran", false, e.message.split("\n")[0]);
} finally {
  if (ctx) await ctx.close();
  server.close();
  try { const i = inst(); await fetch(`http://${i.addr}/api/quit`, { method: "POST", headers: { "X-Token": i.token }, body: "{}" }); } catch {}
}

const lines = ["", "### Browser extension", "", "| | Check | Details |", "|---|---|---|",
  ...results.map(([n, ok, d]) => `| ${ok ? "✅" : "❌"} | ${n} | ${d.replace(/\|/g, "/").slice(0, 160)} |`)];
fs.appendFileSync(process.env.GITHUB_STEP_SUMMARY || path.join(OUT, "summary.md"), lines.join("\n") + "\n");
process.exit(results.every(r => r[1]) ? 0 : 1);
