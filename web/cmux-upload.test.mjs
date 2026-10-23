import assert from "node:assert/strict";
import test from "node:test";
import { encodeUploadChunk, uploadChunkedFile } from "./cmux-upload.mjs";

test("uploads byte-safe chunks bound to the original terminal", async () => {
  const actions = [];
  const bytes = Buffer.from([0, 127, 255, 10, 11]);
  const file = {
    name: "图片.png", size: bytes.length,
    slice: (start, end) => ({ arrayBuffer: async () => bytes.subarray(start, end) })
  };
  const request = async (action, details) => {
    actions.push([action, details]);
    if (action === "upload_begin") return { upload_id: "u" };
    if (action === "upload_chunk") return { written: bytes.length };
    return { path: "/private/attachment.png", size: bytes.length };
  };
  const result = await uploadChunkedFile(file, {
    kind: "attachment", workspaceID: "w", surfaceID: "s"
  }, request, () => true);
  assert.equal(result.path, "/private/attachment.png");
  assert.deepEqual(Buffer.from(actions[1][1].data, "base64"), bytes);
  assert.equal(actions[0][1].workspace_id, "w");
  assert.equal(actions[2][1].surface_id, "s");
  assert.equal(encodeUploadChunk(Uint8Array.from([0, 255])), "AP8=");
});

test("aborts when target changes and rejects dangerous filenames or size", async () => {
  let current = true;
  const file = {
    name: "a.txt", size: 2,
    slice: () => ({ arrayBuffer: async () => Uint8Array.from([1, 2]) })
  };
  const actions = [];
  const request = async (action) => {
    actions.push(action);
    if (action === "upload_begin") { current = false; return { upload_id: "u" }; }
    return { ok: true };
  };
  await assert.rejects(uploadChunkedFile(file, { kind: "file", path: "/root" },
    request, () => current), /已切换/);
  assert.deepEqual(actions, ["upload_begin", "upload_abort"]);
  await assert.rejects(uploadChunkedFile({ ...file, name: "../oops" }, {}, request,
    () => true), /无效/);
  await assert.rejects(uploadChunkedFile({ ...file, size: 20 * 1024 * 1024 + 1 }, {},
    request, () => true), /无效/);
});
