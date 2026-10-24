import test from "node:test";
import assert from "node:assert/strict";
import { commandDraft } from "./cmux-commands.mjs";

test("快捷指令只填空白或尚未完成的斜杠草稿", () => {
  assert.equal(commandDraft("", "/model"), "/model");
  assert.equal(commandDraft(" /mod ", "/model"), "/model");
  assert.equal(commandDraft("/", "/status"), "/status");
  assert.equal(commandDraft("请帮我修复", "/model"), null);
  assert.equal(commandDraft("/model extra", "/status"), null);
  assert.equal(commandDraft("/model\n", "/status"), "/status");
});

test("拒绝未列入菜单的命令和拼接文本", () => {
  assert.equal(commandDraft("", "/quit"), null);
  assert.equal(commandDraft("", "/model\n/quit"), null);
});
