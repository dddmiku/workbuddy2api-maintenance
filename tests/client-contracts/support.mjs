// ═══ 更新日志 ═══
// 2026-09-25：固定客户端版本并限制所有 SDK fetch 到测试回环地址；支持受控 gzip 请求与每例证据。
// 2026-09-25：以 Handler 完成状态等待账本收尾，避免把 SDK 已读终态误当作服务端已完成记账。
// 2026-09-25：报告携带运行 ID，确保运行器拒绝陈旧结果。
import assert from "node:assert/strict";
import { readFile, writeFile } from "node:fs/promises";
import { after, test } from "node:test";
import { gzipSync } from "node:zlib";
import path from "node:path";
import { setTimeout as delay } from "node:timers/promises";

const base = new URL(process.env.CLIENT_CONTRACT_BASE);
assert.equal(base.protocol, "http:");
assert.equal(base.hostname, "127.0.0.1");
assert.ok(base.port);
assert.equal(base.pathname, "/");
assert.equal(base.username + base.password + base.search + base.hash, "");
const nativeFetch = globalThis.fetch;
globalThis.fetch = async (input, options) => {
  let request = new Request(input, options);
  assert.equal(
    new URL(request.url).origin,
    base.origin,
    "SDK attempted an external request",
  );
  const encoding = request.headers.get("X-Contract-Encoding");
  if (encoding) {
    assert(["gzip", "invalid-gzip"].includes(encoding));
    const headers = new Headers(request.headers);
    headers.delete("X-Contract-Encoding");
    headers.delete("Content-Length");
    headers.set("Content-Encoding", "gzip");
    const body =
      encoding === "gzip"
        ? gzipSync(Buffer.from(await request.clone().arrayBuffer()))
        : Buffer.from("not-a-gzip-member");
    request = new Request(request.url, {
      method: request.method,
      headers,
      body,
      signal: request.signal,
      duplex: "half",
    });
  }
  return nativeFetch(request, { redirect: "error" });
};

const manifest = JSON.parse(
  await readFile(new URL("./package.json", import.meta.url), "utf8"),
);
export const versions = {};
for (const [name, expected] of Object.entries(manifest.dependencies)) {
  const installed = JSON.parse(
    await readFile(
      new URL(`./node_modules/${name}/package.json`, import.meta.url),
      "utf8",
    ),
  );
  assert.equal(installed.version, expected, `unexpected ${name} version`);
  versions[name] = installed.version;
}
export const { default: OpenAI } = await import("openai");
export const { default: Anthropic } = await import("@anthropic-ai/sdk");
export const { GoogleGenAI, ThinkingLevel, Type } = await import(
  "@google/genai"
);

export const model = "global:contract-fixture";
export const apiKey = "fixture-client-contract-key";
export const weatherSchema = {
  type: "object",
  properties: { city: { type: "string" } },
  required: ["city"],
  additionalProperties: false,
};
export const chatTools = [
  {
    type: "function",
    function: {
      name: "lookup_weather",
      description: "Synthetic fixture; never execute.",
      parameters: weatherSchema,
    },
  },
];
export const responseTools = [
  {
    type: "function",
    name: "lookup_weather",
    description: "Synthetic fixture; never execute.",
    parameters: weatherSchema,
    strict: true,
  },
];
export const anthropicTools = [
  {
    name: "lookup_weather",
    description: "Synthetic fixture; never execute.",
    input_schema: weatherSchema,
  },
];
export const googleTools = [
  {
    functionDeclarations: [
      {
        name: "lookup_weather",
        description: "Synthetic fixture; never execute.",
        parameters: {
          type: Type.OBJECT,
          properties: { city: { type: Type.STRING } },
          required: ["city"],
        },
      },
    ],
  },
];
export const results = [];
let sequence = 0;

export async function control(path, body) {
  const response = await fetch(new URL(path, base), {
    method: body === undefined ? "GET" : "POST",
    headers: {
      "Content-Type": "application/json",
      "X-Contract-Control": "fixture-client-contract-control",
    },
    body: body === undefined ? undefined : JSON.stringify(body),
    signal: AbortSignal.timeout(3000),
  });
  assert(
    response.ok,
    `fixture control failed: ${response.status} ${await response.clone().text()}`,
  );
  return response.status === 204 ? undefined : response.json();
}

export function contract(name, scenario, action) {
  test(name, { timeout: 15000 }, async () => {
    const id = `case_${++sequence}`;
    const record = { name, id, scenario, passed: false };
    results.push(record);
    await control("/__contract/cases", { id, scenario });
    const headers = { "X-Client-Contract-ID": id };
    const ctx = {
      id,
      record,
      headers,
      openai(extra = {}) {
        return new OpenAI({
          apiKey,
          baseURL: new URL("/v1", base).href,
          maxRetries: 0,
          timeout: 10000,
          defaultHeaders: { ...headers, ...extra },
          fetch: globalThis.fetch,
        });
      },
      anthropic(extra = {}) {
        return new Anthropic({
          apiKey,
          authToken: null,
          webhookKey: null,
          baseURL: base.origin,
          maxRetries: 0,
          timeout: 10000,
          logLevel: "off",
          defaultHeaders: { ...headers, ...extra },
          fetch: globalThis.fetch,
        });
      },
      google(extra = {}) {
        return new GoogleGenAI({
          apiKey,
          vertexai: false,
          enterprise: false,
          httpOptions: {
            baseUrl: base.origin,
            apiVersion: "v1beta",
            headers: { ...headers, ...extra },
            timeout: 10000,
            retryOptions: { attempts: 1 },
          },
        });
      },
      async stats() {
        return control(`/__contract/case?id=${id}`);
      },
      async release() {
        return control(`/__contract/release?id=${id}`, {});
      },
      async completeStats(count = 1) {
        const deadline = performance.now() + 5000;
        do {
          const stats = await this.stats();
          if (stats.finished_requests >= count) return stats;
          await delay(10);
        } while (performance.now() < deadline);
        throw new Error(
          "gateway request did not finish within the fixture deadline",
        );
      },
    };
    try {
      await action(ctx);
      record.passed = true;
    } catch (error) {
      record.error = {
        name: error.name,
        message: error.message,
        actual: error.actual,
        expected: error.expected,
      };
      throw error;
    } finally {
      if (scenario === "invalid-tools") await ctx.release();
      record.fixture = await ctx.stats();
    }
  });
}

export function terminalBarrier() {
  let resolve;
  const arrived = new Promise((done) => {
    resolve = done;
  });
  let text = "";
  return {
    feed(delta) {
      text += delta || "";
      if (text.includes("BEFORE_TERMINAL_FIXTURE")) resolve();
    },
    async wait() {
      let timer;
      try {
        await Promise.race([
          arrived,
          new Promise((_, reject) => {
            timer = setTimeout(
              () => reject(new Error("pre-terminal marker was not received")),
              3000,
            );
          }),
        ]);
      } finally {
        clearTimeout(timer);
      }
    },
  };
}

export function assertLedger(stats, { requests = 1, failed = 0 } = {}) {
  assert.equal(stats.upstream_calls, requests, "unexpected upstream retry");
  assert.equal(stats.usage.requests, requests);
  assert.equal(stats.usage.failed_requests, failed);
  assert.equal(stats.usage.unreported_requests, 0);
  assert.equal(stats.usage.prompt_tokens, requests * 100);
  assert.equal(stats.usage.completion_tokens, requests * 25);
  assert.equal(stats.usage.cached_tokens, requests * 40);
  assert.equal(stats.usage.total_tokens, requests * 125);
}

export function assertToolHistory(stats) {
  assert.equal(stats.upstream_requests.length, 2);
  const second = stats.upstream_requests[1];
  const calls = second.messages.flatMap((message) => message.tool_calls || []);
  const replies = second.messages.filter((message) => message.role === "tool");
  assert.equal(
    calls.length,
    2,
    "both prior tool calls must reach the upstream",
  );
  assert.equal(replies.length, 2, "both tool replies must reach the upstream");
  assert.deepEqual(
    calls.map((call) => JSON.parse(call.function.arguments).city).sort(),
    ["Oslo", "Paris"],
  );
  for (const call of calls) {
    assert.equal(call.function.name, "lookup_weather");
    const reply = replies.find((item) => item.tool_call_id === call.id);
    assert(reply, "tool result lost its matching call id");
    assert(
      JSON.stringify(reply.content).includes(
        `TOOL_RESULT_FIXTURE:${JSON.parse(call.function.arguments).city}`,
      ),
    );
  }
  const declared = second.tools.find(
    (tool) => tool.function?.name === "lookup_weather",
  );
  assert.equal(declared.function.parameters.properties.city.type, "string");
}

after(async () => {
  await writeFile(
    path.join(process.env.CLIENT_CONTRACT_OUTPUT, "results.json"),
    JSON.stringify(
      {
        node: process.version,
        run_id: process.env.CLIENT_CONTRACT_RUN_ID,
        versions,
        official_clients: true,
        fake_upstream: true,
        live_credentials: false,
        mutation: process.env.CLIENT_CONTRACT_MUTATION || null,
        passed: results.length > 0 && results.every((r) => r.passed),
        results,
      },
      null,
      2,
    ) + "\n",
  );
});
