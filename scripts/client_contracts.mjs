#!/usr/bin/env node
// ═══ 更新日志 ═══
// 2026-09-25：构建独立 Go 回环夹具并运行固定官方 SDK；凭据环境隔离、子进程收尾和临时目录删除均限定本次测试。
// 2026-09-25：增加受控丢缓存反例，只有指定断言确实失败且真实账本仍完整时才确认测试能捕获回归。
// 2026-09-25：允许用显式绝对目录保存本轮证据，CI 默认位置不变，临时运行目录仍单独清理。
// 2026-09-25：按本次运行 ID 绑定客户端报告，防止提前失败时误读上次绿色或预期失败结果。
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { randomUUID } from "node:crypto";
import { createWriteStream } from "node:fs";
import {
  mkdir,
  mkdtemp,
  readFile,
  realpath,
  rm,
  writeFile,
} from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "..");
const testDir = path.join(root, "tests", "client-contracts");
const runID = randomUUID();
const args = process.argv.slice(2);
const proveOracle = args.includes("--prove-oracle");
const race = args.includes("--race");
const mutationArg = args.find((arg) => arg.startsWith("--mutation="));
const mutation = proveOracle ? "drop-cache" : mutationArg?.split("=")[1] || "";
const pattern = proveOracle
  ? "^openai chat preserves text, reasoning and usage$"
  : args
      .find((arg) => arg.startsWith("--test-name-pattern="))
      ?.slice("--test-name-pattern=".length);
assert(
  args.every(
    (arg) =>
      arg === "--prove-oracle" ||
      arg === "--race" ||
      arg.startsWith("--mutation=") ||
      arg.startsWith("--test-name-pattern="),
  ),
  "unknown client-contract option",
);
assert(["", "drop-cache"].includes(mutation), "unknown test mutation");
const outputRoot = process.env.CLIENT_CONTRACT_OUTPUT_ROOT || path.join(testDir, ".output");
assert(path.isAbsolute(outputRoot), 'CLIENT_CONTRACT_OUTPUT_ROOT must be absolute');
const output = path.join(outputRoot, (mutation || "normal") + (race ? "-race" : ""));
await mkdir(output, { recursive: true });
const tempParent = await realpath(tmpdir());
const owned = await mkdtemp(path.join(tempParent, "wb2api-client-contracts-"));
const executable = path.join(
  owned,
  process.platform === "win32" ? "fixture.exe" : "fixture",
);

function isolatedEnvironment() {
  const env = { ...process.env };
  for (const key of Object.keys(env)) {
    if (
      /OPENAI|ANTHROPIC|GOOGLE|GEMINI|VERTEX|CLOUDSDK|AZURE|AWS_|API_KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|^WB2A|^(HTTPS?_PROXY|ALL_PROXY|NODE_USE_ENV_PROXY)$/i.test(
        key,
      )
    )
      delete env[key];
  }
  env.WB2A_DUMP_REQ = "";
  return env;
}

const environment = isolatedEnvironment();
const go = process.env.CLIENT_CONTRACT_GO || "go";
async function run(command, argv, env, logName) {
  const child = spawn(command, argv, {
    cwd: root,
    env,
    stdio: ["ignore", "pipe", "pipe"],
    windowsHide: true,
  });
  const log = createWriteStream(path.join(output, logName));
  for (const stream of [child.stdout, child.stderr])
    stream.on("data", (bytes) => {
      log.write(bytes);
      process.stdout.write(bytes);
    });
  try {
    return await new Promise((resolve, reject) => {
      child.once("error", reject);
      child.once("close", (code) => resolve(code ?? 1));
    });
  } finally {
    await new Promise((resolve) => log.end(resolve));
  }
}

let fixture;
let fixtureURL;
let fixtureLog;
let fixtureClosed;
let fixtureOutcome;
let exitCode = 1;
let oracleDetected;
let clientExitCode;
try {
  const built = await run(
    go,
    [
      "build",
      ...(race ? ["-race"] : []),
      "-trimpath",
      "-o",
      executable,
      "./tests/client-contracts/fixture",
    ],
    {
      ...environment,
      ...(race ? { CGO_ENABLED: "1" } : {}),
      GOFLAGS: "-mod=readonly",
    },
    "build.log",
  );
  assert.equal(built, 0, "fixture build failed");
  fixtureLog = createWriteStream(path.join(output, "fixture.log"));
  fixture = spawn(
    executable,
    [
      "-state-dir",
      path.join(owned, "state"),
      ...(mutation ? ["-mutation", mutation] : []),
    ],
    {
      cwd: root,
      env: environment,
      stdio: ["ignore", "pipe", "pipe"],
      windowsHide: true,
    },
  );
  fixtureClosed = new Promise((resolve) =>
    fixture.once("close", (code, signal) => {
      fixtureOutcome = { code, signal };
      resolve();
    }),
  );
  fixture.stderr.on("data", (bytes) => fixtureLog.write(bytes));
  fixtureURL = await new Promise((resolve, reject) => {
    const timer = setTimeout(
      () => reject(new Error("fixture ready timeout")),
      10000,
    );
    let buffered = "";
    fixture.once("error", (error) => {
      clearTimeout(timer);
      reject(error);
    });
    fixture.once("exit", (code) => {
      clearTimeout(timer);
      reject(new Error(`fixture exited before readiness (${code})`));
    });
    fixture.stdout.on("data", (bytes) => {
      fixtureLog.write(bytes);
      buffered += bytes;
      while (buffered.includes("\n")) {
        const end = buffered.indexOf("\n");
        const line = buffered.slice(0, end);
        buffered = buffered.slice(end + 1);
        try {
          const data = JSON.parse(line);
          if (data.fixture === "official-client-contracts") {
            clearTimeout(timer);
            resolve(data.base_url);
          }
        } catch {
          /* Ordinary gateway log lines are kept in fixture.log. */
        }
      }
    });
  });
  const url = new URL(fixtureURL);
  assert.equal(url.protocol, "http:");
  assert.equal(url.hostname, "127.0.0.1");
  assert.ok(url.port);
  const argv = [
    "--test",
    "--test-reporter=spec",
    ...(pattern ? [`--test-name-pattern=${pattern}`] : []),
    path.join(testDir, "contracts.test.mjs"),
  ];
  clientExitCode = await run(
    process.execPath,
    argv,
    {
      ...environment,
      CLIENT_CONTRACT_BASE: fixtureURL,
      CLIENT_CONTRACT_OUTPUT: output,
      CLIENT_CONTRACT_MUTATION: mutation,
      CLIENT_CONTRACT_RUN_ID: runID,
    },
    "test.log",
  );
  const result = JSON.parse(
    await readFile(path.join(output, "results.json"), "utf8"),
  );
  assert.equal(result.run_id, runID, 'client report must belong to the current run');
  if (proveOracle) {
    const first = result.results[0];
    oracleDetected =
      clientExitCode !== 0 &&
      result.results.length === 1 &&
      first.error?.name === "AssertionError" &&
      first.error?.actual === 0 &&
      first.error?.expected === 40 &&
      first.error?.message.includes("cached input tokens must remain 40") &&
      first.fixture?.usage?.cached_tokens === 40;
    assert.equal(
      oracleDetected,
      true,
      "controlled cache-loss regression was not detected by the intended assertion",
    );
    console.log(
      "Expected regression detected: SDK cached input became 0 while the gateway ledger retained 40.",
    );
    exitCode = 0;
  } else {
    assert(result.results.length > 0, "no client contract tests ran");
    exitCode = clientExitCode === 0 && result.passed === true ? 0 : 1;
  }
} finally {
  if (fixture && !fixtureOutcome) {
    if (fixtureURL) {
      await fetch(new URL("/__contract/shutdown", fixtureURL), {
        method: "POST",
        headers: { "X-Contract-Control": "fixture-client-contract-control" },
        signal: AbortSignal.timeout(3000),
        redirect: "error",
      }).catch(() => {});
    }
    const terminate = setTimeout(() => fixture.kill(), 5000);
    const force = setTimeout(() => fixture.kill("SIGKILL"), 7000);
    try {
      await fixtureClosed;
    } finally {
      clearTimeout(terminate);
      clearTimeout(force);
    }
  }
  if (fixtureOutcome && (fixtureOutcome.code !== 0 || fixtureOutcome.signal))
    exitCode = 1;
  if (fixtureLog) await new Promise((resolve) => fixtureLog.end(resolve));
  const actual = await realpath(owned);
  assert.equal(
    path.dirname(actual),
    tempParent,
    "cleanup escaped the owned temporary parent",
  );
  assert(path.basename(actual).startsWith("wb2api-client-contracts-"));
  await rm(actual, {
    recursive: true,
    force: true,
    maxRetries: 20,
    retryDelay: 100,
  });
  await writeFile(
    path.join(output, "runner.json"),
    JSON.stringify(
      {
        completed_at: new Date().toISOString(),
        run_id: runID,
        node: process.version,
        mutation,
        race_detector: race,
        client_test_exit_code: clientExitCode,
        fixture_exit: fixtureOutcome,
        expected_regression_detected: oracleDetected,
        exit_code: exitCode,
        temporary_runtime_removed: true,
      },
      null,
      2,
    ) + "\n",
  );
}
process.exitCode = exitCode;
