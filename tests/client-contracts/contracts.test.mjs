// ═══ 更新日志 ═══
// 2026-09-25：官方 SDK 直接调用真实网关，先锁定 Chat 文本、推理和精确用量。
// 2026-09-25：加入 Responses、Anthropic Messages 和 Google generateContent 的原生流式消费与缓存/推理口径。
// 2026-09-25：覆盖四类官方 SDK 的取消传播及 gzip 请求，并要求损坏压缩体在调用上游之前被拒绝。
import assert from "node:assert/strict";
import {
  assertLedger,
  assertToolHistory,
  contract,
  model,
  ThinkingLevel,
  chatTools,
  responseTools,
  anthropicTools,
  googleTools,
  terminalBarrier,
} from "./support.mjs";

contract(
  "openai chat preserves text, reasoning and usage",
  "text",
  async (ctx) => {
    const response = await ctx
      .openai()
      .chat.completions.create({
        model,
        stream: true,
        stream_options: { include_usage: true },
        messages: [{ role: "user", content: "CONTRACT_INPUT" }],
      });
    let text = "",
      thinking = "",
      usage;
    const finishes = [];
    for await (const chunk of response) {
      text += chunk.choices?.[0]?.delta?.content || "";
      thinking += chunk.choices?.[0]?.delta?.reasoning_content || "";
      if (chunk.usage) usage = chunk.usage;
      if (chunk.choices?.[0]?.finish_reason)
        finishes.push(chunk.choices[0].finish_reason);
    }
    assert.equal(text, "TEXT_FIXTURE");
    assert.equal(thinking, "THINKING_FIXTURE");
    assert.deepEqual(finishes, ["stop"]);
    assert.equal(usage.prompt_tokens, 100);
    assert.equal(usage.completion_tokens, 25);
    assertLedger(await ctx.completeStats());
    assert.equal(
      usage.prompt_tokens_details.cached_tokens,
      40,
      "cached input tokens must remain 40",
    );
    assert.equal(usage.completion_tokens_details.reasoning_tokens, 5);
    ctx.record.usage = usage;
  },
);

contract(
  "openai chat delivers two streamed tools and returns both results",
  "tools",
  async (ctx) => {
    const input = { role: "user", content: "CONTRACT_TOOL_INPUT" };
    const callbacks = [];
    const stream = ctx
      .openai()
      .chat.completions.stream({
        model,
        messages: [input],
        tools: chatTools,
        stream_options: { include_usage: true },
      });
    stream.on("tool_calls.function.arguments.done", (call) =>
      callbacks.push(call.index),
    );
    const first = await stream.finalChatCompletion();
    const message = first.choices[0].message;
    const calls = message.tool_calls;
    assert.equal(first.choices[0].finish_reason, "tool_calls");
    assert.equal(calls.length, 2);
    assert.deepEqual(callbacks, [0, 1]);
    const final = await ctx
      .openai()
      .chat.completions.create({
        model,
        messages: [
          input,
          message,
          ...calls.map((call) => ({
            role: "tool",
            tool_call_id: call.id,
            content: `TOOL_RESULT_FIXTURE:${JSON.parse(call.function.arguments).city}`,
          })),
        ],
        tools: chatTools,
      });
    assert.equal(final.choices[0].message.content, "TOOL_ROUNDTRIP_OK");
    assert.equal(final.choices[0].finish_reason, "stop");
    const stats = await ctx.completeStats(2);
    assertToolHistory(stats);
    assertLedger(stats, { requests: 2 });
  },
);

contract(
  "openai chat hides all tools until late invalid arguments fail",
  "invalid-tools",
  async (ctx) => {
    const barrier = terminalBarrier();
    let toolChunks = 0;
    const callbacks = [];
    const stream = ctx
      .openai()
      .chat.completions.stream({
        model,
        messages: [{ role: "user", content: "CONTRACT_INVALID_TOOLS" }],
        tools: chatTools,
        stream_options: { include_usage: true },
      });
    stream.on("chunk", (chunk) => {
      barrier.feed(chunk.choices?.[0]?.delta?.content);
      toolChunks += chunk.choices?.[0]?.delta?.tool_calls?.length || 0;
    });
    stream.on("tool_calls.function.arguments.done", (event) =>
      callbacks.push(event),
    );
    const outcome = stream.finalChatCompletion().then(
      (value) => ({ value }),
      (error) => ({ error }),
    );
    await barrier.wait();
    assert.equal(toolChunks, 0);
    assert.equal(callbacks.length, 0);
    await ctx.release();
    const final = await outcome;
    assert(
      final.error,
      "late invalid arguments must fail the official SDK stream",
    );
    assert.equal(toolChunks, 0);
    assert.equal(callbacks.length, 0);
    ctx.record.error_terminal = final.error.name;
    assertLedger(await ctx.completeStats(), { failed: 1 });
  },
);

contract(
  "openai chat cancellation reaches upstream and retains measured usage",
  "cancel",
  async (ctx) => {
    const controller = new AbortController();
    const stream = await ctx
      .openai()
      .chat.completions.create(
        {
          model,
          stream: true,
          messages: [{ role: "user", content: "CONTRACT_CANCEL" }],
        },
        { signal: controller.signal },
      );
    let text = "",
      completed = false,
      caught,
      abortedAt;
    try {
      for await (const chunk of stream) {
        text += chunk.choices?.[0]?.delta?.content || "";
        completed ||= Boolean(chunk.choices?.[0]?.finish_reason);
        if (
          text.includes("BEFORE_TERMINAL_FIXTURE") &&
          !controller.signal.aborted
        ) {
          abortedAt = performance.now();
          controller.abort();
        }
      }
    } catch (error) {
      caught = error;
    }
    assert.equal(controller.signal.aborted, true);
    assert.equal(completed, false);
    assert(
      performance.now() - abortedAt < 2000,
      "cancellation must not wait for the SDK request timeout",
    );
    const stats = await ctx.completeStats();
    assert.equal(stats.upstream_canceled, true);
    assertLedger(stats, { failed: 1 });
    ctx.record.client_abort_error = caught?.name || null;
  },
);

contract(
  "openai responses preserves native events and usage",
  "text",
  async (ctx) => {
    const stream = await ctx
      .openai()
      .responses.create({
        model,
        stream: true,
        store: false,
        input: "CONTRACT_INPUT",
        reasoning: { effort: "high", summary: "auto" },
      });
    let text = "",
      thinking = "",
      completed;
    const terminals = [];
    for await (const event of stream) {
      if (event.type === "response.output_text.delta") text += event.delta;
      if (event.type === "response.reasoning_summary_text.delta")
        thinking += event.delta;
      if (
        [
          "response.completed",
          "response.failed",
          "response.incomplete",
        ].includes(event.type)
      )
        terminals.push(event.type);
      if (event.type === "response.completed") completed = event.response;
    }
    assert.equal(text, "TEXT_FIXTURE");
    assert.equal(thinking, "THINKING_FIXTURE");
    assert.deepEqual(terminals, ["response.completed"]);
    assert.equal(completed.status, "completed");
    assert.equal(completed.usage.input_tokens, 100);
    assert.equal(completed.usage.output_tokens, 25);
    assert.equal(completed.usage.input_tokens_details.cached_tokens, 40);
    assert.equal(completed.usage.output_tokens_details.reasoning_tokens, 5);
    assert.equal(completed.usage.total_tokens, 125);
    ctx.record.usage = completed.usage;
    assertLedger(await ctx.completeStats());
  },
);

contract(
  "openai responses streams two tools and accepts function outputs",
  "tools",
  async (ctx) => {
    const input = { role: "user", content: "CONTRACT_TOOL_INPUT" };
    const callbacks = [];
    const stream = ctx
      .openai()
      .responses.stream({
        model,
        store: false,
        input: [input],
        tools: responseTools,
        reasoning: { effort: "high", summary: "auto" },
      });
    stream.on("response.function_call_arguments.done", (event) =>
      callbacks.push(event.item_id),
    );
    const first = await stream.finalResponse();
    const calls = first.output.filter((item) => item.type === "function_call");
    assert.equal(first.status, "completed");
    assert.equal(calls.length, 2);
    assert.equal(callbacks.length, 2);
    assert.equal(new Set(callbacks).size, 2);
    const final = await ctx
      .openai()
      .responses.create({
        model,
        store: false,
        input: [
          input,
          ...first.output,
          ...calls.map((call) => ({
            type: "function_call_output",
            call_id: call.call_id,
            output: `TOOL_RESULT_FIXTURE:${JSON.parse(call.arguments).city}`,
          })),
        ],
        tools: responseTools,
      });
    assert.equal(final.status, "completed");
    assert.equal(final.output_text, "TOOL_ROUNDTRIP_OK");
    const stats = await ctx.completeStats(2);
    assertToolHistory(stats);
    assertLedger(stats, { requests: 2 });
  },
);

contract(
  "openai responses exposes failure and no premature tool items",
  "invalid-tools",
  async (ctx) => {
    const barrier = terminalBarrier();
    const tools = [],
      terminals = [];
    const stream = ctx
      .openai()
      .responses.stream({
        model,
        store: false,
        input: "CONTRACT_INVALID_TOOLS",
        tools: responseTools,
      });
    stream.on("response.output_text.delta", (event) =>
      barrier.feed(event.delta),
    );
    stream.on("response.output_item.added", (event) => {
      if (event.item.type === "function_call") tools.push(event.item);
    });
    stream.on("response.completed", () => terminals.push("completed"));
    stream.on("response.failed", () => terminals.push("failed"));
    const outcome = stream.finalResponse().then(
      (value) => ({ value }),
      (error) => ({ error }),
    );
    await barrier.wait();
    assert.equal(tools.length, 0);
    await ctx.release();
    const final = await outcome;
    assert(final.error || final.value?.status === "failed");
    assert.equal(tools.length, 0);
    assert(!terminals.includes("completed"));
    ctx.record.terminals = terminals;
    ctx.record.error_terminal = final.error?.name || final.value.status;
    assertLedger(await ctx.completeStats(), { failed: 1 });
  },
);

contract(
  "openai responses cancellation reaches upstream before timeout",
  "cancel",
  async (ctx) => {
    const controller = new AbortController();
    const stream = await ctx
      .openai()
      .responses.create(
        { model, stream: true, store: false, input: "CONTRACT_CANCEL" },
        { signal: controller.signal },
      );
    let text = "",
      completed = false,
      caught,
      abortedAt;
    try {
      for await (const event of stream) {
        if (event.type === "response.output_text.delta") text += event.delta;
        completed ||= event.type === "response.completed";
        if (
          text.includes("BEFORE_TERMINAL_FIXTURE") &&
          !controller.signal.aborted
        ) {
          abortedAt = performance.now();
          controller.abort();
        }
      }
    } catch (error) {
      caught = error;
    }
    assert.equal(controller.signal.aborted, true);
    assert.equal(completed, false);
    assert(
      performance.now() - abortedAt < 2000,
      "cancellation must not wait for the SDK request timeout",
    );
    const stats = await ctx.completeStats();
    assert.equal(stats.upstream_canceled, true);
    assertLedger(stats, { failed: 1 });
    ctx.record.client_abort_error = caught?.name || null;
  },
);

contract(
  "anthropic messages preserves thinking and reconciles input cache",
  "text",
  async (ctx) => {
    const events = [];
    const stream = ctx
      .anthropic()
      .messages.stream({
        model,
        max_tokens: 2048,
        thinking: { type: "enabled", budget_tokens: 1024 },
        messages: [{ role: "user", content: "CONTRACT_INPUT" }],
      });
    stream.on("streamEvent", (event) => events.push(structuredClone(event)));
    const final = await stream.finalMessage();
    assert.equal(final.stop_reason, "end_turn");
    assert.equal(
      final.content
        .filter((b) => b.type === "text")
        .map((b) => b.text)
        .join(""),
      "TEXT_FIXTURE",
    );
    assert.equal(
      final.content
        .filter((b) => b.type === "thinking")
        .map((b) => b.thinking)
        .join(""),
      "THINKING_FIXTURE",
    );
    assert.equal(events.filter((e) => e.type === "message_stop").length, 1);
    assert.equal(
      events.find((e) => e.type === "message_start").message.usage.input_tokens,
      0,
      "unreconciled gross input must not become uncached input",
    );
    assert.equal(final.usage.input_tokens, 60);
    assert.equal(final.usage.cache_read_input_tokens, 40);
    assert.equal(final.usage.output_tokens, 25);
    assert.equal(Object.hasOwn(final.usage, "gateway_usage_incomplete"), false);
    ctx.record.usage = final.usage;
    assertLedger(await ctx.completeStats());
  },
);

contract(
  "anthropic messages completes each tool once and accepts both results",
  "tools",
  async (ctx) => {
    const input = { role: "user", content: "CONTRACT_TOOL_INPUT" };
    const callbacks = [];
    const stream = ctx
      .anthropic()
      .messages.stream({
        model,
        max_tokens: 2048,
        thinking: { type: "enabled", budget_tokens: 1024 },
        messages: [input],
        tools: anthropicTools,
      });
    stream.on("contentBlock", (block) => {
      if (block.type === "tool_use") callbacks.push(block.id);
    });
    const first = await stream.finalMessage();
    const calls = first.content.filter((block) => block.type === "tool_use");
    assert.equal(first.stop_reason, "tool_use");
    assert.equal(calls.length, 2);
    assert.deepEqual(
      callbacks,
      calls.map((call) => call.id),
    );
    const final = await ctx
      .anthropic()
      .messages.create({
        model,
        max_tokens: 2048,
        tools: anthropicTools,
        messages: [
          input,
          { role: "assistant", content: first.content },
          {
            role: "user",
            content: calls.map((call) => ({
              type: "tool_result",
              tool_use_id: call.id,
              content: `TOOL_RESULT_FIXTURE:${call.input.city}`,
            })),
          },
        ],
      });
    assert.equal(final.stop_reason, "end_turn");
    assert.equal(
      final.content
        .filter((block) => block.type === "text")
        .map((block) => block.text)
        .join(""),
      "TOOL_ROUNDTRIP_OK",
    );
    const stats = await ctx.completeStats(2);
    assertToolHistory(stats);
    assertLedger(stats, { requests: 2 });
  },
);

contract(
  "anthropic messages fails without any tool callback on late corruption",
  "invalid-tools",
  async (ctx) => {
    const barrier = terminalBarrier();
    const starts = [],
      callbacks = [],
      stops = [];
    const stream = ctx
      .anthropic()
      .messages.stream({
        model,
        max_tokens: 2048,
        messages: [{ role: "user", content: "CONTRACT_INVALID_TOOLS" }],
        tools: anthropicTools,
      });
    stream.on("streamEvent", (event) => {
      if (
        event.type === "content_block_delta" &&
        event.delta.type === "text_delta"
      )
        barrier.feed(event.delta.text);
      if (
        event.type === "content_block_start" &&
        event.content_block.type === "tool_use"
      )
        starts.push(event);
      if (event.type === "message_stop") stops.push(event);
    });
    stream.on("contentBlock", (block) => {
      if (block.type === "tool_use") callbacks.push(block);
    });
    const outcome = stream.finalMessage().then(
      (value) => ({ value }),
      (error) => ({ error }),
    );
    await barrier.wait();
    assert.equal(starts.length, 0);
    assert.equal(callbacks.length, 0);
    await ctx.release();
    const final = await outcome;
    assert(final.error, "the official SDK must reject the error terminal");
    assert.equal(starts.length + callbacks.length + stops.length, 0);
    ctx.record.error_terminal = final.error.name;
    assertLedger(await ctx.completeStats(), { failed: 1 });
  },
);

contract(
  "anthropic messages cancellation reaches upstream before timeout",
  "cancel",
  async (ctx) => {
    const controller = new AbortController();
    const stream = ctx
      .anthropic()
      .messages.stream(
        {
          model,
          max_tokens: 2048,
          messages: [{ role: "user", content: "CONTRACT_CANCEL" }],
        },
        { signal: controller.signal },
      );
    let text = "",
      completed = false,
      abortedAt;
    stream.on("streamEvent", (event) => {
      if (
        event.type === "content_block_delta" &&
        event.delta.type === "text_delta"
      )
        text += event.delta.text;
      completed ||= event.type === "message_stop";
      if (
        text.includes("BEFORE_TERMINAL_FIXTURE") &&
        !controller.signal.aborted
      ) {
        abortedAt = performance.now();
        controller.abort();
      }
    });
    const final = await stream.finalMessage().then(
      (value) => ({ value }),
      (error) => ({ error }),
    );
    assert.equal(controller.signal.aborted, true);
    assert.equal(completed, false);
    assert(final.error);
    assert(
      performance.now() - abortedAt < 2000,
      "cancellation must not wait for the SDK request timeout",
    );
    const stats = await ctx.completeStats();
    assert.equal(stats.upstream_canceled, true);
    assertLedger(stats, { failed: 1 });
    ctx.record.client_abort_error = final.error.name;
  },
);

contract(
  "google generateContent preserves thought and visible output split",
  "text",
  async (ctx) => {
    const stream = await ctx
      .google()
      .models.generateContentStream({
        model,
        contents: "CONTRACT_INPUT",
        config: {
          thinkingConfig: {
            includeThoughts: true,
            thinkingLevel: ThinkingLevel.HIGH,
          },
        },
      });
    let text = "",
      thinking = "",
      usage;
    const finishes = [];
    for await (const chunk of stream) {
      for (const part of chunk.candidates?.[0]?.content?.parts || []) {
        if (part.thought) thinking += part.text || "";
        else text += part.text || "";
      }
      if (chunk.usageMetadata) usage = chunk.usageMetadata;
      if (chunk.candidates?.[0]?.finishReason)
        finishes.push(chunk.candidates[0].finishReason);
    }
    assert.equal(text, "TEXT_FIXTURE");
    assert.equal(thinking, "THINKING_FIXTURE");
    assert.deepEqual(finishes, ["STOP"]);
    assert.equal(usage.promptTokenCount, 100);
    assert.equal(usage.cachedContentTokenCount, 40);
    assert.equal(usage.candidatesTokenCount, 20);
    assert.equal(usage.thoughtsTokenCount, 5);
    assert.equal(usage.totalTokenCount, 125);
    ctx.record.usage = usage;
    assertLedger(await ctx.completeStats());
  },
);

contract(
  "google generateContent streams two functions and accepts both responses",
  "tools",
  async (ctx) => {
    const input = { role: "user", parts: [{ text: "CONTRACT_TOOL_INPUT" }] };
    const stream = await ctx
      .google()
      .models.generateContentStream({
        model,
        contents: [input],
        config: {
          tools: googleTools,
          thinkingConfig: {
            includeThoughts: true,
            thinkingLevel: ThinkingLevel.HIGH,
          },
        },
      });
    const parts = [];
    for await (const chunk of stream)
      parts.push(...(chunk.candidates?.[0]?.content?.parts || []));
    const calls = parts
      .filter((part) => part.functionCall)
      .map((part) => part.functionCall);
    assert.equal(calls.length, 2);
    assert.deepEqual(
      calls.map((call) => call.args.city),
      ["Oslo", "Paris"],
    );
    const final = await ctx
      .google()
      .models.generateContent({
        model,
        contents: [
          input,
          { role: "model", parts },
          {
            role: "user",
            parts: calls.map((call) => ({
              functionResponse: {
                name: call.name,
                id: call.id,
                response: { output: `TOOL_RESULT_FIXTURE:${call.args.city}` },
              },
            })),
          },
        ],
        config: { tools: googleTools },
      });
    assert.equal(final.text, "TOOL_ROUNDTRIP_OK");
    const stats = await ctx.completeStats(2);
    assertToolHistory(stats);
    assertLedger(stats, { requests: 2 });
  },
);

contract(
  "google generateContent preserves OTHER failure without tool delivery",
  "invalid-tools",
  async (ctx) => {
    const barrier = terminalBarrier();
    const calls = [],
      finishes = [];
    const stream = await ctx
      .google()
      .models.generateContentStream({
        model,
        contents: "CONTRACT_INVALID_TOOLS",
        config: { tools: googleTools },
      });
    const outcome = (async () => {
      for await (const chunk of stream) {
        for (const part of chunk.candidates?.[0]?.content?.parts || []) {
          if (!part.thought) barrier.feed(part.text);
          if (part.functionCall) calls.push(part.functionCall);
        }
        if (chunk.candidates?.[0]?.finishReason)
          finishes.push(chunk.candidates[0].finishReason);
      }
    })();
    await barrier.wait();
    assert.equal(calls.length, 0);
    await ctx.release();
    await outcome;
    assert.equal(calls.length, 0);
    assert.deepEqual(finishes, ["OTHER"]);
    const stats = await ctx.completeStats();
    assert(
      stats.exchanges[0].response.includes('"error"'),
      "raw Gemini response must retain the diagnostic error",
    );
    assertLedger(stats, { failed: 1 });
    ctx.record.native_failure = "OTHER";
    ctx.record.sdk_limitation =
      "Google SDK 2.24.0 discards SSE error/finishMessage details; callers must inspect OTHER.";
  },
);

contract(
  "google generateContent cancellation reaches upstream before timeout",
  "cancel",
  async (ctx) => {
    const controller = new AbortController();
    const stream = await ctx
      .google()
      .models.generateContentStream({
        model,
        contents: "CONTRACT_CANCEL",
        config: { abortSignal: controller.signal },
      });
    let text = "",
      completed = false,
      caught,
      abortedAt;
    try {
      for await (const chunk of stream) {
        for (const part of chunk.candidates?.[0]?.content?.parts || [])
          if (!part.thought) text += part.text || "";
        completed ||= chunk.candidates?.[0]?.finishReason === "STOP";
        if (
          text.includes("BEFORE_TERMINAL_FIXTURE") &&
          !controller.signal.aborted
        ) {
          abortedAt = performance.now();
          controller.abort();
        }
      }
    } catch (error) {
      caught = error;
    }
    assert.equal(controller.signal.aborted, true);
    assert.equal(completed, false);
    assert(
      performance.now() - abortedAt < 2000,
      "cancellation must not wait for the SDK request timeout",
    );
    const stats = await ctx.completeStats();
    assert.equal(stats.upstream_canceled, true);
    assertLedger(stats, { failed: 1 });
    ctx.record.client_abort_error = caught?.name || null;
  },
);

const jsonClients = [
  {
    name: "openai chat",
    async request(ctx, headers, input) {
      const value = await ctx
        .openai(headers)
        .chat.completions.create({
          model,
          messages: [{ role: "user", content: input }],
        });
      return { text: value.choices[0].message.content, usage: value.usage };
    },
  },
  {
    name: "openai responses",
    async request(ctx, headers, input) {
      const value = await ctx
        .openai(headers)
        .responses.create({ model, store: false, input });
      return { text: value.output_text, usage: value.usage };
    },
  },
  {
    name: "anthropic messages",
    async request(ctx, headers, input) {
      const value = await ctx
        .anthropic(headers)
        .messages.create({
          model,
          max_tokens: 2048,
          messages: [{ role: "user", content: input }],
        });
      return {
        text: value.content
          .filter((block) => block.type === "text")
          .map((block) => block.text)
          .join(""),
        usage: value.usage,
      };
    },
  },
  {
    name: "google generateContent",
    async request(ctx, headers, input) {
      const value = await ctx
        .google(headers)
        .models.generateContent({ model, contents: input });
      return { text: value.text, usage: value.usageMetadata };
    },
  },
];

for (const client of jsonClients) {
  contract(
    `${client.name} accepts a gzipped SDK request without losing input`,
    "text",
    async (ctx) => {
      const input =
        "COMPRESSION_FIXTURE:" + "preserve this input ".repeat(2000);
      const result = await client.request(
        ctx,
        { "X-Contract-Encoding": "gzip" },
        input,
      );
      assert.equal(result.text, "TEXT_FIXTURE");
      const stats = await ctx.completeStats();
      assert.equal(stats.exchanges[0].encoding, "gzip");
      assert(
        stats.exchanges[0].bytes < 1024,
        "fixture payload should actually be compressed",
      );
      const texts = stats.upstream_requests[0].messages.flatMap((message) =>
        typeof message.content === "string"
          ? [message.content]
          : (message.content || []).map((part) => part.text || ""),
      );
      assert(
        texts.includes(input),
        "decoded request lost the original SDK input",
      );
      assertLedger(stats);
      ctx.record.usage = result.usage;
    },
  );

  contract(
    `${client.name} rejects broken gzip before upstream consumption`,
    "text",
    async (ctx) => {
      let caught;
      try {
        await client.request(
          ctx,
          { "X-Contract-Encoding": "invalid-gzip" },
          "COMPRESSION_FIXTURE",
        );
      } catch (error) {
        caught = error;
      }
      assert.equal(caught?.status, 400);
      const stats = await ctx.completeStats();
      assert.equal(stats.exchanges[0].status, 400);
      assert.equal(stats.upstream_calls, 0);
      assert.equal(stats.usage.requests, 0);
      ctx.record.error_terminal = { name: caught.name, status: caught.status };
    },
  );
}
