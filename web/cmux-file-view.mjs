import { filePreviewKind, readFileChunks, streamFileChunks } from "./cmux-files.mjs?v=20261025-return-bottom-down";
import { maxUploadFiles, uploadChunkedFile } from "./cmux-upload.mjs?v=20261025-return-bottom-down";
import { renderSandboxedPreview } from "./cmux-preview.mjs?v=20261025-return-bottom-down";
import { downloadThroughHTTP } from "./cmux-download-http.mjs?v=20261025-return-bottom-down";

const textLimit = 1024 * 1024;
const binaryLimit = 20 * 1024 * 1024;

export function mountCmuxFiles({ request, currentClient, canUpload, onShow, onHide }) {
  const terminalPane = document.getElementById("terminalPane");
  const filePane = document.getElementById("filePane");
  const status = document.getElementById("fileStatus");
  const entries = document.getElementById("fileEntries");
  const rootsSelect = document.getElementById("fileRootSelect");
  const search = document.getElementById("fileSearch");
  const more = document.getElementById("fileMoreButton");
  const up = document.getElementById("fileUpButton");
  const preview = document.getElementById("filePreview");
  const previewHeader = document.getElementById("filePreviewHeader");
  const text = document.getElementById("fileText");
  const image = document.getElementById("fileImage");
  const frame = document.getElementById("fileFrame");
  const autoRefresh = document.getElementById("fileAutoRefresh");
  const fileUploadInput = document.getElementById("fileUploadInput");
  const fileUploadButton = document.getElementById("fileUploadButton");
  let path = "";
  let parent = "";
  let offset = 0;
  let version = 0;
  let blobURL = "";
  let searchTimer;
  let previewPath = "";
  let previewName = "";
  let previewStamp = "";
  let polling = false;

  function clearPreview() {
    previewPath = "";
    previewStamp = "";
    preview.hidden = true;
    previewHeader.replaceChildren();
    text.textContent = "";
    text.hidden = true;
    image.hidden = true;
    image.removeAttribute("src");
    frame.hidden = true;
    frame.removeAttribute("src");
    frame.removeAttribute("srcdoc");
    if (blobURL) URL.revokeObjectURL(blobURL);
    blobURL = "";
  }

  function reset() {
    version++;
    clearTimeout(searchTimer);
    clearPreview();
    path = "";
    parent = "";
    offset = 0;
    entries.replaceChildren();
    rootsSelect.replaceChildren();
    more.hidden = true;
  }

  function renderFileEntry(entry) {
    const button = document.createElement("button");
    button.type = "button";
    button.className = "file-entry";
    button.textContent = `${entry.type === "directory" ? "📁" : "📄"} ${entry.name}` +
      `${entry.type === "file" ? ` · ${entry.size} B` : ""}`;
    button.disabled = entry.type === "other";
    button.addEventListener("click", () => entry.type === "directory"
      ? void browse(entry.path) : void openFile(entry.path, entry.name));
    entries.append(button);
  }

  async function browse(target = path, page = 0) {
    if (!currentClient()) return;
    const client = currentClient();
    const current = ++version;
    try {
      const listing = await request("file_list", {
        path: target, query: search.value.trim(), offset: page
      });
      if (current !== version || client !== currentClient()) return;
      if (page === 0) {
        entries.replaceChildren();
        clearPreview();
      }
      path = listing.path;
      parent = listing.parent || "";
      offset = page + (Array.isArray(listing.entries) ? listing.entries.length : 0);
      up.disabled = !parent;
      for (const entry of listing.entries || []) {
        renderFileEntry(entry);
      }
      more.hidden = !listing.has_more;
      status.textContent = `${listing.path} · 已显示 ${offset}/${listing.total}` +
        `${listing.truncated ? "（仅搜索前 5000 项）" : ""}`;
    } catch (error) {
      if (current === version) status.textContent = error.message;
    }
  }

  async function recent() {
    if (!currentClient() || !rootsSelect.value) return;
    const client = currentClient(), current = ++version;
    try {
      const result = await request("file_recent", { path: rootsSelect.value });
      if (current !== version || client !== currentClient()) return;
      entries.replaceChildren();
      clearPreview();
      more.hidden = true;
      for (const file of result.files || []) renderFileEntry(file);
      status.textContent = `最近文件：${result.files?.length || 0} 个` +
        `${result.truncated ? "（达到扫描上限）" : ""}`;
    } catch (error) {
      if (current === version) status.textContent = error.message;
    }
  }

  async function refreshRoots() {
    if (filePane.hidden || !currentClient()) return;
    const client = currentClient();
    const current = ++version;
    try {
      const result = await request("file_roots");
      if (current !== version || client !== currentClient()) return;
      rootsSelect.replaceChildren();
      const roots = Array.isArray(result?.roots) ? result.roots : [];
      if (!roots.length) {
        status.textContent = "文件管理默认关闭：请在本机 Client 的 cmux.file_roots 显式配置开放目录";
        entries.replaceChildren();
        return;
      }
      for (const root of roots) rootsSelect.add(new Option(root, root));
      const target = roots.some(root => path === root || path.startsWith(`${root}/`)) ? path : roots[0];
      rootsSelect.value = roots.find(root => target === root || target.startsWith(`${root}/`)) || roots[0];
      await browse(target);
    } catch (error) {
      if (current === version) status.textContent = error.message;
    }
  }

  function download(content, name) {
    const url = URL.createObjectURL(new Blob([content], { type: "application/octet-stream" }));
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = name || "download";
    document.body.append(anchor);
    anchor.click();
    anchor.remove();
    setTimeout(() => URL.revokeObjectURL(url), 30000);
  }

  async function openFile(target, name) {
    const client = currentClient();
    const current = ++version;
    clearPreview();
    preview.hidden = false;
    const label = document.createElement("span");
    label.textContent = `正在读取 ${name}…`;
    const downloadButton = document.createElement("button");
    downloadButton.type = "button";
    downloadButton.textContent = "下载";
    let cachedContent = null;
    downloadButton.addEventListener("click", async () => {
      downloadButton.disabled = true;
      try {
        const stillCurrent = () => current === version && client === currentClient() && !filePane.hidden;
        if (typeof window.showSaveFilePicker === "function" && !cachedContent) {
          // Picker 必须在用户点击时立即打开，不可先等待异步 RPC。
          const handle = await window.showSaveFilePicker({ suggestedName: name });
          const writable = await handle.createWritable();
          try {
            await streamFileChunks(target, 1 << 30, request, writable, stillCurrent);
            await writable.close();
          } catch (error) {
            await writable.abort().catch(() => {});
            throw error;
          }
        } else if (!cachedContent) {
          await downloadThroughHTTP(target, name, request, stillCurrent);
        } else {
          if (stillCurrent()) download(cachedContent, name);
        }
      } catch (error) {
        if (current === version) label.textContent = `下载失败：${error.message}`;
      } finally {
        downloadButton.disabled = false;
      }
    });
    previewHeader.replaceChildren(label, downloadButton);
    const kind = filePreviewKind(name);
    try {
      const file = await readFileChunks(target, kind.kind === "text" ? textLimit : binaryLimit,
        request, () => current === version && client === currentClient() && !filePane.hidden);
      if (current !== version || client !== currentClient() || filePane.hidden) return;
      label.textContent = `${name} · ${file.size} B`;
      cachedContent = file.content;
      if (kind.kind === "image" || kind.kind === "pdf") {
        blobURL = URL.createObjectURL(new Blob([file.content], { type: kind.mime }));
        if (kind.kind === "image") {
          image.src = blobURL;
          image.hidden = false;
        } else {
          frame.src = blobURL;
          frame.hidden = false;
        }
      } else if (kind.kind === "text") {
        const content = new TextDecoder("utf-8", { fatal: true }).decode(file.content);
        if (/\.(?:html?|md|markdown)$/i.test(name)) {
          const root = rootsSelect.value;
          const srcdoc = await renderSandboxedPreview({
            content, format: /\.html?$/i.test(name) ? "html" : "markdown",
            filePath: target, root, request,
            isCurrent: () => current === version && client === currentClient() && !filePane.hidden
          });
          if (current !== version || client !== currentClient() || filePane.hidden) return;
          frame.srcdoc = srcdoc;
          frame.hidden = false;
          const sourceButton = document.createElement("button");
          sourceButton.type = "button";
          sourceButton.textContent = "查看源码";
          sourceButton.addEventListener("click", () => {
            frame.hidden = !frame.hidden;
            text.hidden = !text.hidden;
          });
          previewHeader.append(sourceButton);
        } else {
          text.hidden = false;
        }
        text.textContent = content;
      }
      previewPath = target;
      previewName = name;
      previewStamp = `${file.size}\x00${file.modifiedAt}`;
    } catch (error) {
      if (current === version) label.textContent = `预览失败：${error.message}`;
    }
  }

  async function pollPreview() {
    if (polling || !autoRefresh.checked || !previewPath || filePane.hidden ||
        document.visibilityState === "hidden" || !currentClient()) return;
    polling = true;
    const client = currentClient(), current = version, target = previewPath, name = previewName;
    try {
      const result = await request("file_read", { path: target, read_bytes: 1, offset: 0 });
      if (current === version && client === currentClient() && previewPath === target &&
          `${result.size}\x00${result.modified_at}` !== previewStamp) {
        await openFile(target, name);
      }
    } catch (error) {
      if (current === version && previewPath === target) {
        status.textContent = `自动刷新失败：${error.message}`;
      }
    } finally {
      polling = false;
    }
  }
  setInterval(() => void pollPreview(), 2500);

  document.getElementById("filesToggleButton").addEventListener("click", () => {
    if (!currentClient()) {
      status.textContent = "请先连接 cmux Client";
      return;
    }
    onShow();
    terminalPane.hidden = true;
    filePane.hidden = false;
    void refreshRoots();
  });
  document.getElementById("filesBackButton").addEventListener("click", () => {
    reset();
    filePane.hidden = true;
    terminalPane.hidden = false;
    onHide();
  });
  rootsSelect.addEventListener("change", () => { search.value = ""; void browse(rootsSelect.value); });
  up.addEventListener("click", () => { if (parent) void browse(parent); });
  more.addEventListener("click", () => void browse(path, offset));
  document.getElementById("fileRefreshButton").addEventListener("click", () => void browse(path));
  document.getElementById("fileRecentButton").addEventListener("click", () => void recent());
  search.addEventListener("input", () => {
    clearTimeout(searchTimer);
    searchTimer = setTimeout(() => void browse(path), 180);
  });
  fileUploadButton.addEventListener("click", async () => {
    const files = Array.from(fileUploadInput.files || []);
    if (!canUpload() || !path || !files.length || files.length > maxUploadFiles) {
      status.textContent = `请先取得管理权，并选择 1–${maxUploadFiles} 个文件和开放目录`;
      return;
    }
    const destination = path, client = currentClient();
    const stillCurrent = () => currentClient() === client && canUpload() &&
      !filePane.hidden && path === destination;
    fileUploadButton.disabled = true;
    try {
      for (const file of files) {
        await uploadChunkedFile(file, { kind: "file", path: destination },
          request, stillCurrent, (written, total) => {
            status.textContent = `上传 ${file.name}：${written}/${total} B`;
          });
      }
      fileUploadInput.value = "";
      if (stillCurrent()) await browse(destination);
    } catch (error) {
      status.textContent = `上传中断：${error.message}`;
    } finally {
      fileUploadButton.disabled = !canUpload();
    }
  });
  return {
    reset, refreshRoots,
    setUploadEnabled(owned) { fileUploadButton.disabled = !owned; }
  };
}
