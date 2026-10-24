import { streamFileChunks } from "./cmux-files.mjs?v=20261024-browser-session";

const downloadLimit = 1 << 30;

// HTTP 也可流式下载：浏览器无 Service Worker/文件保存 API 时，Server 仅中继已授权
// file_read 的小分片，不接受任意本机路径，亦不在浏览器拼装整文件 Blob。
export async function downloadThroughHTTP(path, name, request, stillCurrent) {
  const metadata = await request("file_read", { path, offset: 0, read_bytes: 1 });
  if (!stillCurrent() || !Number.isSafeInteger(metadata?.size) ||
      metadata.size < 0 || metadata.size > downloadLimit ||
      metadata.path !== path || typeof metadata.modified_at !== "string") {
    throw new Error("文件大小无效或已切换 Client");
  }
  const prepare = await fetch("./api/cmux-download", {
    method: "POST", credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ name, size: metadata.size }), cache: "no-store"
  });
  if (!prepare.ok) throw new Error(`下载准备失败（HTTP ${prepare.status}）`);
  const { id } = await prepare.json();
  if (!/^[a-f0-9-]{36}$/.test(id)) throw new Error("下载票据格式错误");
  const base = `./api/cmux-download/${id}`;
  let offset = 0;
  try {
    if (!stillCurrent()) throw new Error("已切换 Client 或文件");
    const anchor = document.createElement("a");
    anchor.href = base;
    anchor.download = name || "download";
    anchor.referrerPolicy = "no-referrer";
    document.body.append(anchor);
    anchor.click();
    anchor.remove();
    if (metadata.size === 0) return; // GET 的 Content-Length: 0 已是完整下载。
    await streamFileChunks(path, downloadLimit, async (action, details) => {
      const result = await request(action, details);
      if (result.size !== metadata.size || result.modified_at !== metadata.modified_at) {
        throw new Error("下载前文件发生变化");
      }
      return result;
    }, {
      async write(bytes) {
        const response = await fetch(`${base}/chunk`, {
          method: "POST",
          credentials: "same-origin",
          headers: { "X-Download-Offset": String(offset) },
          body: bytes, cache: "no-store"
        });
        if (!response.ok) throw new Error(`下载分片中断（HTTP ${response.status}）`);
        offset += bytes.length;
      }
    }, stillCurrent);
    const finish = await fetch(`${base}/finish`, {
      method: "POST",
      credentials: "same-origin",
      headers: { "X-Download-Offset": String(offset) },
      cache: "no-store"
    });
    if (!finish.ok) throw new Error(`下载完成确认失败（HTTP ${finish.status}）`);
  } catch (error) {
    await fetch(`${base}/abort`, {
      method: "POST", credentials: "same-origin", cache: "no-store"
    }).catch(() => {});
    throw error;
  }
}
