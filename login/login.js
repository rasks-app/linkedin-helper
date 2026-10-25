// The LinkedIn sign-in window, as the Mac app would show it. Run with Electron:
//   electron login/login.js --out session.json
// The person signs in to linkedin.com in its own browser session. Once the feed
// loads, it saves that session's cookies, the X-LI headers the page's own
// requests carried, and the exact browser headers, so the helper's requests
// carry the same user agent as the browser that signed in. It never reads a
// password: LinkedIn's own page takes it.
const { app, BrowserWindow, session } = require("electron");
const fs = require("fs");

const out = process.argv[process.argv.indexOf("--out") + 1] || "session.json";
const KEEP = ["user-agent", "sec-ch-ua", "sec-ch-ua-platform", "sec-ch-ua-mobile", "sec-ch-prefers-color-scheme"];

app.whenReady().then(() => {
  const ses = session.fromPartition("persist:rasks-linkedin");
  // A plain Chrome user agent, without Electron's or the app's own words in it.
  ses.setUserAgent(ses.getUserAgent().replace(/\s(Electron|rasks[^\s]*|Rasks[^\s]*)\/[^\s]+/g, ""));

  const seen = { xLiTrack: "", pageInstance: "", headers: {} };
  ses.webRequest.onBeforeSendHeaders({ urls: ["https://www.linkedin.com/*"] }, (details, done) => {
    const h = {};
    for (const [k, v] of Object.entries(details.requestHeaders)) h[k.toLowerCase()] = v;
    if (h["x-li-track"] && h["x-li-track"].includes("clientVersion")) seen.xLiTrack = h["x-li-track"];
    if (h["x-li-page-instance"] && h["x-li-page-instance"].startsWith("urn:li:page")) seen.pageInstance = h["x-li-page-instance"];
    for (const k of KEEP) if (h[k]) seen.headers[k] = h[k];
    done({ requestHeaders: details.requestHeaders });
  });

  const win = new BrowserWindow({ width: 480, height: 720, title: "Sign in to LinkedIn", webPreferences: { session: ses } });
  win.loadURL("https://www.linkedin.com/login");

  let saved = false;
  const tryFinish = async () => {
    if (saved) return;
    const url = win.webContents.getURL();
    if (!/^https:\/\/www\.linkedin\.com\/(feed|messaging|in\/|mynetwork)/.test(url)) return;
    if (!seen.xLiTrack || !seen.pageInstance) return; // wait for the page's own API calls
    const cookies = await ses.cookies.get({ url: "https://www.linkedin.com" });
    if (!cookies.some((c) => c.name === "JSESSIONID") || !cookies.some((c) => c.name === "li_at")) return;
    saved = true;
    const header = cookies.map((c) => `${c.name}=${c.value}`).join("; ");
    const headers = {};
    for (const k of KEEP) if (seen.headers[k]) headers[k.replace(/(^|-)([a-z])/g, (_, d, c) => d + c.toUpperCase())] = seen.headers[k];
    headers["User-Agent"] = headers["User-Agent"] || ses.getUserAgent();
    fs.writeFileSync(out, JSON.stringify({ cookies: header, xLiTrack: seen.xLiTrack, pageInstance: seen.pageInstance, headers }, null, 2), { mode: 0o600 });
    console.log(JSON.stringify({ saved: out, cookies: cookies.map((c) => c.name), headers: Object.keys(headers) }));
    win.close();
  };
  win.webContents.on("did-navigate", tryFinish);
  win.webContents.on("did-navigate-in-page", tryFinish);
  const timer = setInterval(tryFinish, 1000); // until the page's first API call lands
  win.on("closed", () => {
    clearInterval(timer);
    app.quit();
  });
});
