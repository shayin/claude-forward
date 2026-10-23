const textExtensions = new Set([
  "txt", "md", "markdown", "csv", "json", "log", "js", "mjs", "ts", "css",
  "html", "htm", "xml", "yaml", "yml", "go", "lua", "py", "sh", "sql"
]);
const imageTypes = new Map([
  ["png", "image/png"], ["jpg", "image/jpeg"], ["jpeg", "image/jpeg"],
  ["gif", "image/gif"], ["webp", "image/webp"], ["avif", "image/avif"]
]);
const chunkSize = 256 * 1024;

export function filePreviewKind(name) {
  const extension = String(name || "").split(".").at(-1)?.toLowerCase();
  if (imageTypes.has(extension)) return { kind: "image", mime: imageTypes.get(extension) };
  if (extension === "pdf") return { kind: "pdf", mime: "application/pdf" };
  if (textExtensions.has(extension)) return { kind: "text", mime: "text/plain" };
  return { kind: "other", mime: "application/octet-stream" };
}

export function decodeFileChunk(encoded) {
  if (typeof encoded !== "string" || encoded.length > 4 * Math.ceil(chunkSize / 3) + 4) {
    throw new Error("文件分片格式错误");
  }
  const binary = atob(encoded);
  const bytes = Uint8Array.from(binary, character => character.charCodeAt(0));
  if (bytes.length > chunkSize) throw new Error("文件分片超出限制");
  return bytes;
}

export async function readFileChunks(path, maxBytes, request, stillCurrent = () => true) {
  let size = null;
  let modifiedAt = null;
  let name = "";
  let offset = 0;
  const chunks = [];
  while (size === null || offset < size) {
    if (!stillCurrent()) throw new Error("已切换 Client 或文件");
    const result = await request("file_read", { path, offset, read_bytes: chunkSize });
    if (!result || result.path !== path || !Number.isSafeInteger(result.size) ||
        result.size < 0 || result.size > maxBytes || typeof result.modified_at !== "string") {
      throw new Error(`文件无法读取或超过 ${Math.ceil(maxBytes / (1024 * 1024))} MiB 限制`);
    }
    if (size === null) {
      size = result.size;
      modifiedAt = result.modified_at;
      name = String(result.name || "");
    } else if (result.size !== size || result.modified_at !== modifiedAt) {
      throw new Error("读取期间文件发生变化，请重新打开");
    }
    if (result.offset !== offset) throw new Error("文件分片顺序错误");
    const bytes = decodeFileChunk(result.data);
    if (bytes.length === 0 && offset < size) throw new Error("文件读取未完成");
    if (offset + bytes.length > size) throw new Error("文件长度发生变化");
    chunks.push(bytes);
    offset += bytes.length;
  }
  const content = new Uint8Array(size);
  let position = 0;
  for (const chunk of chunks) {
    content.set(chunk, position);
    position += chunk.length;
  }
  return { content, name, size, modifiedAt };
}

// 支持浏览器原生保存流：大文件逐片写入，不在网页内累积成一个 Blob。
export async function streamFileChunks(path, maxBytes, request, writable, stillCurrent = () => true) {
  let size = null;
  let modifiedAt = null;
  let offset = 0;
  while (size === null || offset < size) {
    if (!stillCurrent()) throw new Error("已切换 Client 或文件");
    const result = await request("file_read", { path, offset, read_bytes: chunkSize });
    if (!result || result.path !== path || !Number.isSafeInteger(result.size) ||
        result.size < 0 || result.size > maxBytes || typeof result.modified_at !== "string" ||
        result.offset !== offset || size !== null &&
        (result.size !== size || result.modified_at !== modifiedAt)) {
      throw new Error("文件长度、修改时间或分片顺序发生变化");
    }
    if (size === null) {
      size = result.size;
      modifiedAt = result.modified_at;
    }
    const bytes = decodeFileChunk(result.data);
    if ((!bytes.length && offset < size) || offset + bytes.length > size) {
      throw new Error("文件读取未完成或长度发生变化");
    }
    if (!stillCurrent()) throw new Error("已切换 Client 或文件");
    if (size === 0) break;
    const written = bytes.length; // writer 可能通过 postMessage 转移并分离底层 ArrayBuffer。
    await writable.write(bytes);
    offset += written;
  }
  if (!stillCurrent()) throw new Error("已切换 Client 或文件");
  return size;
}
