import fs from "node:fs";
import path from "node:path";
import { chromium } from "playwright";

const root = path.dirname(new URL(import.meta.url).pathname);
const args = new Set(process.argv.slice(2));
const configPath = process.env.KOMARI_DMIT_COLLECTOR_CONFIG || path.join(root, "config.json");

function loadConfig() {
  if (!fs.existsSync(configPath)) {
    throw new Error(`config not found: ${configPath}. Copy config.example.json to config.json first.`);
  }
  return JSON.parse(fs.readFileSync(configPath, "utf8"));
}

function abs(p) {
  if (!p) return "";
  return path.isAbsolute(p) ? p : path.join(root, p);
}

function toBytes(value, unit) {
  const n = Number(String(value).replace(/,/g, ""));
  if (!Number.isFinite(n) || n < 0) return 0;
  const u = String(unit || "B").toUpperCase();
  const pow = { B: 0, KB: 1, KIB: 1, MB: 2, MIB: 2, GB: 3, GIB: 3, TB: 4, TIB: 4, PB: 5, PIB: 5 }[u] ?? 0;
  return Math.round(n * Math.pow(1024, pow));
}

function compactText(text) {
  return String(text || "").replace(/\s+/g, " ").trim();
}

function detectBlocked(text, url) {
  const body = compactText(text).toLowerCase();
  if (body.includes("just a moment") || body.includes("checking your browser") || body.includes("verify you are human")) {
    return "Cloudflare 验证失效";
  }
  if (body.includes("login") && body.includes("password") && !body.includes("bandwidth") && !body.includes("traffic")) {
    return "登录状态失效";
  }
  if (url && /login|clientarea\.php$/i.test(url) && body.includes("password")) {
    return "登录状态失效";
  }
  return "";
}

async function extractText(page, selector) {
  selector = String(selector || "").trim();
  if (!selector) return "";
  const locator = page.locator(selector).first();
  if (!(await locator.count())) return "";
  return compactText(await locator.innerText({ timeout: 5000 }));
}

function parseTrafficFromText(text, account) {
  const patterns = [];
  if (account.traffic_regex) patterns.push(new RegExp(account.traffic_regex, "i"));
  patterns.push(/(?:traffic|bandwidth|流量|monthly data|data transfer)[\s\S]{0,240}?([0-9.,]+)\s*(B|KB|KiB|MB|MiB|GB|GiB|TB|TiB)\s*(?:\/|of|out of)\s*([0-9.,]+)\s*(B|KB|KiB|MB|MiB|GB|GiB|TB|TiB)/i);
  patterns.push(/([0-9.,]+)\s*(GB|GiB|TB|TiB)\s*(?:\/|of|out of)\s*([0-9.,]+)\s*(GB|GiB|TB|TiB)/i);
  for (const pattern of patterns) {
    const m = text.match(pattern);
    if (m) {
      return {
        usedBytes: toBytes(m[1], m[2]),
        limitBytes: toBytes(m[3], m[4]),
        remainingBytes: 0,
      };
    }
  }

  const usedMatch = text.match(/(?:已用|used)\s*[:：]?\s*([0-9.,]+)\s*(B|KB|KiB|MB|MiB|GB|GiB|TB|TiB)/i);
  const remainingMatch = text.match(/(?:剩余|remaining)\s*[:：]?\s*([0-9.,]+)\s*(B|KB|KiB|MB|MiB|GB|GiB|TB|TiB)/i);
  if (usedMatch && remainingMatch) {
    const usedBytes = toBytes(usedMatch[1], usedMatch[2]);
    const remainingBytes = toBytes(remainingMatch[1], remainingMatch[2]);
    return {
      usedBytes,
      limitBytes: usedBytes + remainingBytes,
      remainingBytes,
    };
  }
  return null;
}

async function postUpdate(panelBaseUrl, update, collectorKey) {
  const endpoint = new URL("/api/official-traffic/cache", panelBaseUrl).toString();
  const res = await fetch(endpoint, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      "Authorization": `Bearer ${collectorKey}`,
    },
    body: JSON.stringify(update),
  });
  const text = await res.text();
  if (!res.ok) {
    throw new Error(`panel returned ${res.status}: ${text.slice(0, 500)}`);
  }
  return text;
}

async function reportNeedsVerification(config, account, reason) {
  await postUpdate(config.panel_base_url, {
    client_uuid: account.client_uuid,
    source_name: account.source_name || account.name,
    needs_verification: true,
    error: reason,
  }, account.collector_key);
}

async function verifyAccount(config, account) {
  const profile = abs(account.profile_dir || `profiles/${account.name}`);
  const context = await chromium.launchPersistentContext(profile, {
    channel: account.browser_channel || config.browser_channel || undefined,
    headless: false,
    viewport: { width: 1280, height: 860 },
  });
  const page = context.pages()[0] || await context.newPage();
  await page.goto(account.login_url || account.target_url || "https://www.dmit.io/clientarea.php", { waitUntil: "domcontentloaded", timeout: 60000 });
  console.log(`[${account.name}] browser opened. Finish login/Cloudflare manually, then close the browser window.`);
}

async function collectAccount(config, account) {
  const profile = abs(account.profile_dir || `profiles/${account.name}`);
  const context = await chromium.launchPersistentContext(profile, {
    channel: account.browser_channel || config.browser_channel || undefined,
    headless: account.headless !== false,
    viewport: { width: 1280, height: 860 },
  });
  try {
    const page = context.pages()[0] || await context.newPage();
    await page.goto(account.target_url || account.login_url || "https://www.dmit.io/clientarea.php", {
      waitUntil: "networkidle",
      timeout: 60000,
    });
    const text = await page.locator("body").innerText({ timeout: 10000 }).catch(() => "");
    const blocked = detectBlocked(text, page.url());
    if (blocked) {
      await reportNeedsVerification(config, account, blocked);
      console.log(`[${account.name}] needs verification: ${blocked}`);
      return;
    }

    let usedBytes = 0;
    let limitBytes = 0;
    let remainingBytes = 0;
    const usedText = await extractText(page, account.used_selector);
    const limitText = await extractText(page, account.limit_selector);
    if (usedText && limitText) {
      const um = usedText.match(/([0-9.,]+)\s*(B|KB|KiB|MB|MiB|GB|GiB|TB|TiB)/i);
      const lm = limitText.match(/([0-9.,]+)\s*(B|KB|KiB|MB|MiB|GB|GiB|TB|TiB)/i);
      if (um && lm) {
        usedBytes = toBytes(um[1], um[2]);
        limitBytes = toBytes(lm[1], lm[2]);
      }
    }
    if (!usedBytes && !limitBytes) {
      const parsed = parseTrafficFromText(text, account);
      if (parsed) {
        usedBytes = parsed.usedBytes;
        limitBytes = parsed.limitBytes;
        remainingBytes = parsed.remainingBytes || 0;
      }
    }
    if (!usedBytes && !limitBytes) {
      await reportNeedsVerification(config, account, "页面已打开，但未识别到流量字段；需要调整 selector/regex");
      console.log(`[${account.name}] extract failed`);
      return;
    }

    await postUpdate(config.panel_base_url, {
      client_uuid: account.client_uuid,
      source_name: account.source_name || account.name,
      used_bytes: usedBytes,
      limit_bytes: limitBytes,
      remaining_bytes: remainingBytes,
      updated_at: new Date().toISOString(),
    }, account.collector_key);
    console.log(`[${account.name}] updated: used=${usedBytes} limit=${limitBytes}`);
  } finally {
    await context.close();
  }
}

async function main() {
  const config = loadConfig();
  const accounts = Array.isArray(config.accounts) ? config.accounts : [];
  if (!accounts.length) throw new Error("config.accounts is empty");

  const verifyName = process.argv[process.argv.indexOf("--verify") + 1];
  if (args.has("--verify")) {
    const selected = verifyName && !verifyName.startsWith("--") ? accounts.filter(a => a.name === verifyName) : accounts;
    for (const account of selected) await verifyAccount(config, account);
    return;
  }

  for (const account of accounts) {
    try {
      await collectAccount(config, account);
    } catch (err) {
      console.error(`[${account.name}] failed:`, err.message);
      try {
        await reportNeedsVerification(config, account, err.message);
      } catch (reportErr) {
        console.error(`[${account.name}] report failed:`, reportErr.message);
      }
    }
  }
}

main().catch(err => {
  console.error(err);
  process.exit(1);
});
