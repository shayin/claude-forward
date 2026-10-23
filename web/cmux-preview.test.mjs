import assert from "node:assert/strict";
import test from "node:test";
import { marked } from "./lib/marked/marked.esm.js";
import { resolvePreviewResource } from "./cmux-preview.mjs";

test("Markdown 渲染标题、代码和图片，最终 HTML 仍需在沙箱中净化", () => {
  const html = marked.parse("# 标题\n\n![图](assets/a.png)\n\n```js\nconsole.log(1)\n```\n");
  assert.match(html, /<h1>标题<\/h1>/);
  assert.match(html, /<img src="assets\/a.png" alt="图">/);
  assert.match(html, /<pre><code class="language-js">/);
});

test("预览关联资源只能位于显式开放根目录", () => {
  const root = "/safe/project", file = "/safe/project/pages/index.html";
  assert.equal(resolvePreviewResource(root, file, "../assets/icon.png?v=1"), "/safe/project/assets/icon.png");
  assert.equal(resolvePreviewResource(root, file, "/assets/icon.png"), "/safe/project/assets/icon.png");
  assert.equal(resolvePreviewResource(root, file, "../style.css"), "/safe/project/style.css");
  for (const reference of [
    "../../secret", "%2e%2e/%2e%2e/secret", "../../../safe/project-copy/file",
    "//example.com/x", "https://example.com/x", "data:image/png;base64,aaa",
    "javascript:alert(1)", "file:///etc/passwd", "#anchor", "image%2f..%2fsecret",
    "..\\..\\secret", "%00file"
  ]) {
    assert.equal(resolvePreviewResource(root, file, reference), null, reference);
  }
});
