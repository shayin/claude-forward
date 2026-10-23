import assert from "node:assert/strict";
import test from "node:test";
import { spawn, spawnSync } from "node:child_process";
import { mkdtempSync, mkdirSync, readFileSync, readdirSync, rmSync, writeFileSync } from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";

const folder = path.dirname(fileURLToPath(import.meta.url));
const tempRoot = process.env.CF_TEST_TMP_ROOT || os.tmpdir();
const createHome = () => mkdtempSync(path.join(tempRoot, "cf-autostart-"));
const run = (file, args, env, options = {}) =>
  spawnSync(file, args, { env, encoding: "utf8", ...options });
const waitForExit = child => child.exitCode === null
  ? new Promise(resolve => child.once("exit", resolve)) : Promise.resolve(child.exitCode);

test("cmux autostart requires cmux environment and starts at most one Client",
  { skip: process.platform !== "darwin" }, async () => {
  const home = createHome();
  const socketPath = path.join(home, "cmux.sock");
  const socket = net.createServer();
  await new Promise(resolve => socket.listen(socketPath, resolve));
  try {
    const clientDir = path.join(home, ".claude-forward");
    const project = path.join(home, "project with space");
    mkdirSync(clientDir);
    mkdirSync(project);
    writeFileSync(path.join(clientDir, "client.yaml"), "cmux:\n  enabled: true\n");
    writeFileSync(path.join(clientDir, "client"),
      "#!/bin/sh\nprintf '%s\\n' \"$*\" >>\"$HOME/invocations\"\nsleep 1\n", { mode: 0o700 });
    const script = path.join(folder, "cmux-client-autostart.zsh");
    const env = { ...process.env, HOME: home, CMUX_SOCKET_PATH: socketPath,
      CMUX_SOCKET_CAPABILITY: "isolated-test" };
    assert.equal(run(script, [project], { ...env, CMUX_SOCKET_CAPABILITY: "" }).status, 0);
    assert.equal(run(script, [project], { ...env, CMUX_SOCKET_PATH: path.join(home, "missing") }).status, 0);
    assert.equal(run(script, ["relative"], env).status, 2);
    const first = spawn(script, [project], { env, stdio: "ignore" });
    for (let i = 0; i < 30 && !readdirSync(home).includes("invocations"); i++) {
      await new Promise(resolve => setTimeout(resolve, 20));
    }
    assert.ok(readdirSync(home).includes("invocations"));
    assert.notEqual(run(script, [project], env).status, 0);
    assert.equal(await waitForExit(first), 0);
    assert.equal(run(script, [project], env).status, 0);
    const invocations = readFileSync(path.join(home, "invocations"), "utf8").trim().split("\n");
    assert.equal(invocations.length, 2);
    assert.equal(invocations[0], `-config ${clientDir}/client.yaml -dir ${project}`);
    writeFileSync(path.join(clientDir, "client"), "#!/bin/sh\nexec sleep 20\n", { mode: 0o700 });
    const orphan = run(script, [project, "99999999"], env, { timeout: 5000 });
    assert.equal(orphan.error, undefined);
    assert.notEqual(orphan.status, 0);
  } finally {
    socket.close();
    rmSync(home, { recursive: true, force: true });
  }
});

test("installer writes a reversible, idempotent login hook and plist", { skip: process.platform !== "darwin" }, () => {
  const home = createHome();
  try {
    const clientDir = path.join(home, ".claude-forward");
    const project = path.join(home, "my project");
    mkdirSync(clientDir);
    mkdirSync(project);
    writeFileSync(path.join(clientDir, "client"), "", { mode: 0o700 });
    writeFileSync(path.join(clientDir, "client.yaml"), "cmux:\n  enabled: true\n");
    writeFileSync(path.join(home, ".zshrc"), "# existing setup\n");
    const install = path.join(folder, "install-cmux-autostart.sh");
    const env = { ...process.env, HOME: home };
    assert.equal(run(install, [project, "--no-load"], env).status, 0);
    assert.equal(run(install, [project, "--no-load"], env).status, 0);
    const zshrc = readFileSync(path.join(home, ".zshrc"), "utf8");
    assert.equal(zshrc.match(/# >>> Claude Forward cmux auto-start >>>/g)?.length, 1);
    assert.match(zshrc, /my\\ project/);
    assert.ok(readdirSync(home).some(name => name.startsWith(".zshrc.cf-backup-")));
    const plist = readFileSync(path.join(home, "Library/LaunchAgents/com.claude-forward.cmux-autostart.plist"), "utf8");
    assert.match(plist, /<key>RunAtLoad<\/key><true\/>/);
    assert.match(plist, /<string>cmux<\/string>/);
  } finally {
    rmSync(home, { recursive: true, force: true });
  }
});
