import assert from "node:assert/strict";
import test from "node:test";
import { decodeFileChunk, filePreviewKind, readFileChunks, streamFileChunks } from "./cmux-files.mjs";

test("classifies supported previews and never executes SVG as inline content", () => {
  assert.deepEqual(filePreviewKind("a.PNG"), { kind: "image", mime: "image/png" });
  assert.equal(filePreviewKind("file.pdf").kind, "pdf");
  assert.equal(filePreviewKind("index.html").kind, "text");
  assert.equal(filePreviewKind("unsafe.svg").kind, "other");
  assert.equal(filePreviewKind("program.exe").kind, "other");
});

test("streams download chunks without accumulating and rejects changed file", async () => {
  const writes = [];
  const writer = { async write(bytes) { writes.push(Buffer.from(bytes).toString()); } };
  let current = true;
  const request = async (_action, details) => ({
    path: "/root/data", size: 5, offset: details.offset, modified_at: "now",
    data: Buffer.from(details.offset === 0 ? "hel" : "lo").toString("base64")
  });
  assert.equal(await streamFileChunks("/root/data", 100, request, writer, () => current), 5);
  assert.deepEqual(writes, ["hel", "lo"]);
  await assert.rejects(streamFileChunks("/root/data", 4, request, writer), /变化/);
  const changed = async (_action, details) => ({
    path: "/root/data", size: 5, offset: details.offset,
    modified_at: details.offset === 0 ? "old" : "new",
    data: Buffer.from(details.offset === 0 ? "hel" : "lo").toString("base64")
  });
  await assert.rejects(streamFileChunks("/root/data", 100, changed, writer), /变化/);
  current = false;
  await assert.rejects(streamFileChunks("/root/data", 100, request, writer, () => current), /切换/);
});

test("流式 writer 转移 ArrayBuffer 后仍按原字节数推进", async () => {
  const offsets = [];
  const request = async (_action, details) => {
    offsets.push(details.offset);
    return {
      path: "/root/data", size: 4, offset: details.offset, modified_at: "now",
      data: Buffer.from(details.offset === 0 ? "ab" : "cd").toString("base64")
    };
  };
  const channel = new MessageChannel();
  const written = [];
  channel.port2.onmessage = event => written.push(Buffer.from(event.data).toString());
  assert.equal(await streamFileChunks("/root/data", 10, request, {
    async write(bytes) { channel.port1.postMessage(bytes.buffer, [bytes.buffer]); }
  }), 4);
  assert.deepEqual(offsets, [0, 2]);
  channel.port1.close();
  channel.port2.close();
});

test("空文件下载不发送零长度分片", async () => {
  const writes = [];
  const size = await streamFileChunks("/root/empty", 10, async (_action, details) => ({
    path: "/root/empty", size: 0, offset: details.offset, modified_at: "now", data: ""
  }), { async write(bytes) { writes.push(bytes); } });
  assert.equal(size, 0);
  assert.equal(writes.length, 0);
});

test("reads base64 chunks and rejects changed/oversized/short file data", async () => {
  const payload = { path: "/root/a", size: 4, name: "a", modified_at: "now" };
  const request = async (_action, details) => ({
    ...payload, offset: details.offset,
    data: Buffer.from(details.offset === 0 ? "ab" : "cd").toString("base64")
  });
  const file = await readFileChunks("/root/a", 4, request);
  assert.equal(Buffer.from(file.content).toString(), "abcd");
  assert.equal(decodeFileChunk("YQ==")[0], 97);
  await assert.rejects(readFileChunks("/root/a", 3, request), /超过/);
  await assert.rejects(readFileChunks("/root/a", 10, async () => ({
    ...payload, offset: 0, data: ""
  })), /未完成/);
  let calls = 0;
  await assert.rejects(readFileChunks("/root/a", 10, async (_action, details) => ({
    ...payload, offset: details.offset, modified_at: ++calls === 1 ? "now" : "later",
    data: Buffer.from("ab").toString("base64")
  })), /发生变化/);
  await assert.rejects(readFileChunks("/root/a", 10, request, () => false), /已切换/);
});
