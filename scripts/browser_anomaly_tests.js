#!/usr/bin/env node

const fs = require("fs");
const path = require("path");
const crypto = require("crypto");
const http = require("http");
const https = require("https");
const { chromium } = require("playwright");

const BASE_URL = process.env.BASE_URL || "http://host.docker.internal:8000";
const DATA_DIR = process.env.DATA_DIR || "/data";
const OUTPUT_PATH = process.env.OUTPUT_PATH || "";
const SCENARIOS = (process.env.SCENARIOS || "abandon_cleanup,same_token_race,slow_consumer_large")
  .split(",")
  .map((item) => item.trim())
  .filter(Boolean);

function httpClient(url) {
  return url.startsWith("https://") ? https : http;
}

function getWithHeaders(url) {
  return new Promise((resolve, reject) => {
    const req = httpClient(url).get(url, (res) => {
      resolve(res);
    });
    req.on("error", reject);
  });
}

function downloadToFile(url, outPath, options = {}) {
  const delayMs = options.delayMs ?? 0;
  const chunkPauseBytes = options.chunkPauseBytes ?? 32 * 1024;

  return new Promise((resolve, reject) => {
    const req = httpClient(url).get(url, (res) => {
      const file = fs.createWriteStream(outPath);
      let received = 0;

      const finish = () => {
        file.close(() => {
          resolve({
            statusCode: res.statusCode,
            headers: res.headers,
            bytes: received,
          });
        });
      };

      res.on("data", (chunk) => {
        received += chunk.length;
        file.write(chunk);
        if (delayMs > 0 && received % chunkPauseBytes === 0) {
          res.pause();
          setTimeout(() => res.resume(), delayMs);
        }
      });

      res.on("end", finish);
      res.on("error", (err) => {
        file.destroy();
        reject(err);
      });
    });

    req.on("error", reject);
  });
}

function fileSha256(filePath) {
  const hash = crypto.createHash("sha256");
  hash.update(fs.readFileSync(filePath));
  return hash.digest("hex");
}

async function uploadThroughBrowser(browser, filename) {
  const context = await browser.newContext({ acceptDownloads: true });
  const page = await context.newPage();

  await page.goto(`${BASE_URL}/`, {
    waitUntil: "networkidle",
    timeout: 60000,
  });

  await page.setInputFiles("#fileInput", path.join(DATA_DIR, filename));
  await page.click("#uploadBtn");
  await page.waitForSelector("#downloadLink a", {
    state: "visible",
    timeout: 60000,
  });

  const downloadUrl = await page.locator("#downloadLink a").getAttribute("href");
  const tokenMatch = downloadUrl && downloadUrl.match(/\/download\/([^/]+)\//);
  if (!tokenMatch) {
    throw new Error(`无法从下载链接中提取 token: ${downloadUrl}`);
  }

  return {
    context,
    page,
    downloadUrl,
    token: tokenMatch[1],
  };
}

async function uploadThroughBrowserWithContent(browser, filename) {
  const upload = await uploadThroughBrowser(browser, filename);
  return {
    ...upload,
    expectedSha256: fileSha256(path.join(DATA_DIR, filename)),
    sourcePath: path.join(DATA_DIR, filename),
    filename,
  };
}

async function pollStatusCode(url, expectedStatus, timeoutMs) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    const res = await getWithHeaders(url);
    res.resume();
    if (res.statusCode === expectedStatus) {
      return true;
    }
    await new Promise((resolve) => setTimeout(resolve, 300));
  }
  return false;
}

async function scenarioAbandonCleanup(browser) {
  const upload = await uploadThroughBrowser(browser, "small.txt");
  await upload.page.close();
  await upload.context.close();

  const cleaned = await pollStatusCode(
    `${BASE_URL}/status/${upload.token}`,
    404,
    10000,
  );

  return {
    scenario: "abandon_cleanup",
    token: upload.token,
    cleaned,
  };
}

async function scenarioSameTokenRace(browser) {
  const upload = await uploadThroughBrowser(browser, "large.bin");

  const out1 = path.join(DATA_DIR, "browser-race-1.bin");
  const out2 = path.join(DATA_DIR, "browser-race-2.bin");

  const [first, second] = await Promise.all([
    downloadToFile(upload.downloadUrl, out1),
    downloadToFile(upload.downloadUrl, out2),
  ]);

  await upload.page.close();
  await upload.context.close();

  return {
    scenario: "same_token_race",
    token: upload.token,
    firstStatus: first.statusCode,
    secondStatus: second.statusCode,
    firstBytes: first.bytes,
    secondBytes: second.bytes,
    firstSha256: first.statusCode === 200 ? fileSha256(out1) : null,
    secondSha256: second.statusCode === 200 ? fileSha256(out2) : null,
  };
}

async function scenarioSlowConsumerLarge(browser) {
  const upload = await uploadThroughBrowserWithContent(browser, "large.bin");

  const out = path.join(DATA_DIR, "browser-slow-large.bin");
  const result = await downloadToFile(upload.downloadUrl, out, {
    delayMs: 50,
    chunkPauseBytes: 32 * 1024,
  });

  await upload.page.close();
  await upload.context.close();

  return {
    scenario: "slow_consumer_large",
    token: upload.token,
    status: result.statusCode,
    bytes: result.bytes,
    sha256: result.statusCode === 200 ? fileSha256(out) : null,
    expectedSha256: upload.expectedSha256,
  };
}

async function scenarioPrepareRestartBeforeDownload(browser) {
  const upload = await uploadThroughBrowserWithContent(browser, "small.txt");
  await upload.page.close();
  await upload.context.close();

  return {
    scenario: "prepare_restart_before_download",
    token: upload.token,
    downloadUrl: upload.downloadUrl,
    expectedSha256: upload.expectedSha256,
    filename: upload.filename,
  };
}

async function scenarioMixedConcurrentBrowserUploads(browser) {
  const uploads = await Promise.all([
    uploadThroughBrowserWithContent(browser, "small.txt"),
    uploadThroughBrowserWithContent(browser, "medium.bin"),
    uploadThroughBrowserWithContent(browser, "large.bin"),
  ]);

  const downloadPlans = [
    {
      name: "small",
      outPath: path.join(DATA_DIR, "browser-mixed-small.bin"),
      options: {},
    },
    {
      name: "medium",
      outPath: path.join(DATA_DIR, "browser-mixed-medium.bin"),
      options: { delayMs: 20, chunkPauseBytes: 32 * 1024 },
    },
    {
      name: "large",
      outPath: path.join(DATA_DIR, "browser-mixed-large.bin"),
      options: { delayMs: 50, chunkPauseBytes: 32 * 1024 },
    },
  ];

  const downloads = await Promise.all(
    uploads.map((upload, index) =>
      downloadToFile(upload.downloadUrl, downloadPlans[index].outPath, downloadPlans[index].options),
    ),
  );

  await Promise.all(uploads.map(async (upload) => {
    await upload.page.close();
    await upload.context.close();
  }));

  return {
    scenario: "mixed_concurrent_browser_uploads",
    results: uploads.map((upload, index) => ({
      filename: upload.filename,
      token: upload.token,
      status: downloads[index].statusCode,
      bytes: downloads[index].bytes,
      sha256: downloads[index].statusCode === 200 ? fileSha256(downloadPlans[index].outPath) : null,
      expectedSha256: upload.expectedSha256,
    })),
  };
}

async function main() {
  const browser = await chromium.launch({ headless: true });
  const results = [];

  try {
    for (const scenario of SCENARIOS) {
      if (scenario === "abandon_cleanup") {
        results.push(await scenarioAbandonCleanup(browser));
      } else if (scenario === "same_token_race") {
        results.push(await scenarioSameTokenRace(browser));
      } else if (scenario === "slow_consumer_large") {
        results.push(await scenarioSlowConsumerLarge(browser));
      } else if (scenario === "prepare_restart_before_download") {
        results.push(await scenarioPrepareRestartBeforeDownload(browser));
      } else if (scenario === "mixed_concurrent_browser_uploads") {
        results.push(await scenarioMixedConcurrentBrowserUploads(browser));
      } else {
        throw new Error(`未知场景: ${scenario}`);
      }
    }
  } finally {
    await browser.close();
  }

  if (OUTPUT_PATH) {
    fs.writeFileSync(OUTPUT_PATH, JSON.stringify(results, null, 2));
  }
  process.stdout.write(`${JSON.stringify(results, null, 2)}\n`);
}

main().catch((err) => {
  console.error(err);
  process.exit(1);
});
