import assert from "node:assert/strict";
import test from "node:test";
import { downloadThroughHTTP } from "./cmux-download-http.mjs";

const id = "01234567-89ab-cdef-0123-456789abcdef";

function fixture(fetchImpl) {
  const before = {
    document: globalThis.document,
    fetch: globalThis.fetch
  };
  const links = [];
  globalThis.document = {
    body: { append() {} },
    createElement: () => {
      const anchor = { click() { links.push(anchor.href); }, remove() {} };
      return anchor;
    }
  };
  globalThis.fetch = fetchImpl;
  return { links, restore() {
    for (const [key, value] of Object.entries(before)) {
      if (value === undefined) delete globalThis[key];
      else globalThis[key] = value;
    }
  } };
}

test("HTTP 大文件传输经一次性票据和有限分片，不累积整文件", async () => {
  const calls = [];
  const env = fixture(async (url, options) => {
    calls.push({ url, ...options });
    if (url === "./api/cmux-download") return { ok: true, json: async () => ({ id }) };
    return { ok: true };
  });
  const reads = [];
  try {
    await downloadThroughHTTP("/root/large", "large.bin", async (_, details) => {
      reads.push(details.offset);
      return {
        path: "/root/large", size: 4, modified_at: "now", offset: details.offset,
        data: Buffer.from(details.read_bytes === 1 ? "a" :
          details.offset === 0 ? "ab" : "cd").toString("base64")
      };
    }, () => true);
    assert.deepEqual(reads, [0, 0, 2]);
    assert.deepEqual(env.links, [`./api/cmux-download/${id}`]);
    assert.deepEqual(calls.map(call => call.url), [
      "./api/cmux-download", `./api/cmux-download/${id}/chunk`,
      `./api/cmux-download/${id}/chunk`, `./api/cmux-download/${id}/finish`
    ]);
    assert.deepEqual(calls.slice(1, 3).map(call => call.headers["X-Download-Offset"]), ["0", "2"]);
    assert.ok(calls.every(call => call.credentials === "same-origin"));
    assert.ok(calls.every(call => !call.headers?.Authorization));
  } finally { env.restore(); }
});

test("文件变化或错误时终止 HTTP 下载，不确认完成", async () => {
  const calls = [];
  const env = fixture(async (url) => {
    calls.push(url);
    if (url === "./api/cmux-download") return { ok: true, json: async () => ({ id }) };
    return { ok: true };
  });
  try {
    await assert.rejects(downloadThroughHTTP("/root/large", "large.bin", async (_, details) => ({
      path: "/root/large", size: 4, offset: details.offset,
      modified_at: details.read_bytes === 1 ? "old" : "new",
      data: Buffer.from("a").toString("base64")
    }), () => true), /下载前文件发生变化/);
    assert.deepEqual(calls, ["./api/cmux-download", `./api/cmux-download/${id}/abort`]);
  } finally { env.restore(); }
});
