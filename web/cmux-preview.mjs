import { marked } from "./lib/marked/marked.esm.js?v=20261024-scroll-inertia";
import { readFileChunks } from "./cmux-files.mjs?v=20261024-scroll-inertia";

const previewCsp = "default-src 'none'; img-src data:; style-src 'unsafe-inline'; " +
  "font-src 'none'; script-src 'none'; connect-src 'none'; form-action 'none'; " +
  "object-src 'none'; frame-src 'none'; base-uri 'none'";
const documentStyle = `body{margin:0;padding:24px;font:16px/1.7 system-ui,sans-serif;color:#202b3b;overflow-wrap:anywhere}
img{max-width:100%;height:auto}pre{padding:12px;background:#f2f4f8;overflow:auto}code{font-family:monospace}
table{border-collapse:collapse;display:block;overflow:auto}td,th{border:1px solid #ccd4df;padding:6px}
blockquote{border-left:3px solid #aabbd1;margin-left:0;padding-left:15px}`;
const allowedElements = new Set([
  "HTML", "HEAD", "BODY", "TITLE", "DIV", "SPAN", "P", "BR", "HR",
  "H1", "H2", "H3", "H4", "H5", "H6", "B", "STRONG", "I", "EM", "SMALL",
  "CODE", "PRE", "BLOCKQUOTE", "UL", "OL", "LI", "TABLE", "THEAD",
  "TBODY", "TFOOT", "TR", "TD", "TH", "IMG", "A", "FIGURE", "FIGCAPTION",
  "SECTION", "ARTICLE", "MAIN", "HEADER", "FOOTER", "NAV", "STYLE", "LINK"
]);
const allowedAttributes = new Set([
  "class", "id", "title", "alt", "width", "height", "colspan", "rowspan", "style"
]);
const imageTypes = new Map([
  ["png", "image/png"], ["jpg", "image/jpeg"], ["jpeg", "image/jpeg"],
  ["gif", "image/gif"], ["webp", "image/webp"], ["avif", "image/avif"]
]);

// 页面相对路径和根相对路径都映射到当前显式开放目录；绝不访问外部 URL 或越根文件。
export function resolvePreviewResource(root, file, value) {
  if (!root?.startsWith("/") || !file?.startsWith("/") ||
      typeof value !== "string" || !value || /^(?:[a-z][\w+.-]*:|\/\/|#)/i.test(value) ||
      value.includes("\\") || /[\u0000-\u001f]/.test(value)) return null;
  const base = `https://preview.invalid${file.split("/").map(encodeURIComponent).join("/")}`;
  const reference = value.startsWith("/")
    ? `${root.replace(/\/$/, "")}/${value.slice(1)}` : value;
  let url;
  try { url = new URL(reference, base); } catch { return null; }
  if (url.origin !== "https://preview.invalid") return null;
  let target;
  try {
    const segments = url.pathname.split("/").slice(1).map(decodeURIComponent);
    if (segments.some(segment => segment === "." || segment === ".." ||
      segment.includes("/") || segment.includes("\\") || segment.includes("\0"))) return null;
    target = `/${segments.join("/")}`;
  } catch { return null; }
  return target.startsWith(`${root.replace(/\/$/, "")}/`) ? target : null;
}

function dataURL(bytes, mime) {
  const parts = [];
  for (let offset = 0; offset < bytes.length; offset += 32768) {
    parts.push(String.fromCharCode(...bytes.subarray(offset, offset + 32768)));
  }
  return `data:${mime};base64,${btoa(parts.join(""))}`;
}

// 在 iframe 的唯一来源沙箱内展示静态内容。资源只能经 Client 的 file_read 受限 RPC 获取。
export async function renderSandboxedPreview({ content, format, filePath, root, request, isCurrent }) {
  const html = format === "markdown" ? marked.parse(content, { async: false }) : content;
  const parser = new DOMParser();
  const document = parser.parseFromString(
    `<html><head><meta http-equiv="Content-Security-Policy" content="${previewCsp}"></head><body>${html}</body></html>`,
    "text/html"
  );
  const policy = document.querySelector('meta[http-equiv="Content-Security-Policy"]');
  let remainingBytes = 8 * 1024 * 1024;
  let resourceCount = 0;
  async function resource(value, sourceFile, types, limit) {
    const target = resolvePreviewResource(root, sourceFile, value);
    const extension = target?.split(".").at(-1)?.toLowerCase();
    if (!target || !types.has(extension) || !isCurrent() ||
        resourceCount++ >= 40 || remainingBytes <= 0) return null;
    try {
      const file = await readFileChunks(target, Math.min(limit, remainingBytes), request, isCurrent);
      remainingBytes -= file.size;
      return { file, mime: types.get(extension), path: target };
    } catch { return null; }
  }
  async function sanitizeCSS(css, sourceFile) {
    css = css.replace(/@import\b[^;]*;?/gi, "");
    const references = [...css.matchAll(/url\(\s*(['"]?)(.*?)\1\s*\)/gi)];
    for (const match of references) {
      const item = await resource(match[2], sourceFile, imageTypes, 2 * 1024 * 1024);
      css = css.replace(match[0], item ? `url("${dataURL(item.file.content, item.mime)}")` : "url('')");
    }
    return css;
  }
  for (const node of [...document.querySelectorAll("*")]) {
    if (node === policy) continue;
    if (!allowedElements.has(node.tagName)) {
      node.remove();
      continue;
    }
    const imageSource = node.tagName === "IMG" ? node.getAttribute("src") : null;
    const stylesheet = node.tagName === "LINK" && node.getAttribute("rel")?.toLowerCase() === "stylesheet"
      ? node.getAttribute("href") : null;
    if (node.tagName === "LINK" && !stylesheet) { node.remove(); continue; }
    for (const attribute of [...node.attributes]) {
      if (!allowedAttributes.has(attribute.name)) node.removeAttribute(attribute.name);
    }
    if (node.hasAttribute("style")) {
      node.setAttribute("style", await sanitizeCSS(node.getAttribute("style"), filePath));
    }
    if (imageSource) {
      const item = await resource(imageSource, filePath, imageTypes, 2 * 1024 * 1024);
      if (item) node.setAttribute("src", dataURL(item.file.content, item.mime));
    }
    if (stylesheet) {
      const item = await resource(stylesheet, filePath, new Map([["css", "text/css"]]), 256 * 1024);
      const style = document.createElement("style");
      if (item) {
        try {
          style.textContent = await sanitizeCSS(
            new TextDecoder("utf-8", { fatal: true }).decode(item.file.content), item.path);
        } catch { /* 无效的 CSS 不展示。 */ }
      }
      node.replaceWith(style);
    } else if (node.tagName === "STYLE") {
      node.textContent = await sanitizeCSS(node.textContent, filePath);
    }
  }
  if (!isCurrent()) throw new Error("已切换 Client 或文件");
  const style = document.createElement("style");
  style.textContent = documentStyle;
  document.head.append(style);
  return `<!doctype html>${document.documentElement.outerHTML}`;
}
