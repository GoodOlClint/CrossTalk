import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync } from "node:fs";
import { registerHooks } from "node:module";
import { createServer } from "node:net";
import { join, resolve } from "node:path";
import { afterEach, beforeEach, mock, test } from "node:test";
import { pathToFileURL } from "node:url";

registerHooks({
	resolve(specifier, context, next) {
		if (specifier === "@earendil-works/pi-coding-agent") {
			return { url: "data:text/javascript,export const VERSION = '9.9.9';", shortCircuit: true };
		}
		return next(specifier, context);
	},
});

const file = process.env.CROSSTALK_PI_EXT;
const { default: extension } = await import(file ? pathToFileURL(resolve(file)).href : "./index.ts");

let home, cleanup;
beforeEach(() => {
	// /tmp, not os.tmpdir(): a unix socket path is limited to 104 bytes on macOS.
	home = mkdtempSync("/tmp/ctpi-");
	process.env.HOME = home;
	cleanup = [];
});
afterEach(async () => {
	for (const f of cleanup.reverse()) await f();
	mock.timers.reset();
	mock.restoreAll();
	rmSync(home, { recursive: true, force: true });
});

const tick = () => new Promise((r) => setImmediate(r));
async function until(cond, what) {
	// A wall-clock deadline through Date, which the timer mocks leave alone.
	for (const end = Date.now() + 5000; Date.now() < end; ) {
		if (cond()) return;
		await tick();
	}
	assert.fail("timed out waiting for " + what);
}
// Lets any in-flight socket I/O land before asserting that nothing arrived.
const settle = async () => {
	for (let i = 0; i < 50; i++) await tick();
};

function load({ idle = true, send } = {}) {
	const handlers = {};
	const s = { sent: [], entries: [], status: [], idle, server: undefined, name: "review", handlers };
	s.pi = {
		on: (ev, h) => (handlers[ev] = h),
		registerMcpServer: (name, config) => (s.server = { name, config }),
		getSessionName: () => s.name,
		sendMessage: (message, options) => {
			s.sent.push({ message, options });
			send?.(s, message, options);
		},
	};
	s.ctx = {
		sessionManager: {
			getSessionId: () => "sess-1",
			getSessionFile: () => "/s/sess-1.jsonl",
			getCwd: () => "/work",
			getEntries: () => s.entries,
		},
		isIdle: () => s.idle,
		ui: { setStatus: (key, text) => s.status.push([key, text]) },
	};
	s.emit = (ev, event = {}) => handlers[ev]?.({ type: ev, ...event }, s.ctx);
	extension(s.pi);
	cleanup.push(() => s.emit("session_shutdown", { reason: "quit" }));
	return s;
}

async function shim() {
	const dir = join(home, ".local", "state", "crosstalk", "sock", "pi");
	mkdirSync(dir, { recursive: true });
	const path = join(dir, process.pid + ".sock");
	const srv = { conns: [], lines: [] };
	const server = createServer((c) => {
		srv.conns.push(c);
		let buf = "";
		c.setEncoding("utf8");
		c.on("error", () => {});
		c.on("data", (d) => {
			const parts = (buf + d).split("\n");
			buf = parts.pop();
			for (const p of parts) srv.lines.push(JSON.parse(p));
		});
	});
	await new Promise((r) => server.listen(path, r));
	srv.send = (v) => srv.conns.at(-1).write(typeof v === "string" ? v : JSON.stringify(v) + "\n");
	srv.acks = () => srv.lines.filter((l) => "ok" in l);
	cleanup.push(() => {
		for (const c of srv.conns) c.destroy();
		server.close();
	});
	return srv;
}

async function connected(opts) {
	const srv = await shim();
	const s = load(opts);
	await s.emit("session_start", { reason: "startup" });
	await until(() => srv.lines.length > 0, "hello");
	return { s, srv };
}

const frame = (id, trigger) => ({ id, deliver: "steer", trigger, text: "body of " + id });
const admitted = (id) => ({ message: { role: "custom", customType: "crosstalk", details: { crosstalk_id: id } } });
const appendEntry = (s, message) => s.entries.push({ type: "custom_message", ...message });

test("registers the server at load with a per-instance token", () => {
	const a = load();
	const b = load();
	assert.equal(a.server.name, "crosstalk");
	assert.deepEqual(a.server.config.args, ["mcp"]);
	assert.equal(a.server.config.exposure, "direct");
	assert.match(a.server.config.env.CROSSTALK_PI_TOKEN, /^[0-9a-f]{32}$/);
	assert.notEqual(a.server.config.env.CROSSTALK_PI_TOKEN, b.server.config.env.CROSSTALK_PI_TOKEN);
});

test("session_start returns before any connection, then connects when the server appears", async () => {
	const s = load();
	const returned = s.emit("session_start", { reason: "startup" });
	assert.equal(returned, undefined);
	await settle();
	const srv = await shim();
	await until(() => srv.lines.length > 0, "hello after a late server");
});

test("sends the hello with the token on connect", async () => {
	const { s, srv } = await connected();
	assert.deepEqual(srv.lines[0], {
		proto: 1,
		token: s.server.config.env.CROSSTALK_PI_TOKEN,
		ext_version: srv.lines[0].ext_version,
		pi_version: "9.9.9",
		session_id: "sess-1",
		session_file: "/s/sess-1.jsonl",
		cwd: "/work",
		name: "review",
	});
	assert.ok(srv.lines[0].ext_version);
});

test("sends name changes and busy or idle lines", async () => {
	const { s, srv } = await connected();
	s.emit("session_info_changed", { name: "other" });
	s.emit("agent_start");
	s.emit("agent_settled");
	await until(() => srv.lines.length === 4, "three lines");
	assert.deepEqual(srv.lines.slice(1), [{ name: "other" }, { status: "busy" }, { status: "idle" }]);
});

test("an immediate frame becomes sendMessage and is answered only on the admission event", async () => {
	for (const event of ["message_start", "message_end"]) {
		const { s, srv } = await connected();
		srv.send(frame("m1", true));
		await until(() => s.sent.length === 1, "delivery");
		assert.deepEqual(s.sent[0], {
			message: { customType: "crosstalk", content: "body of m1", display: true, details: { crosstalk_id: "m1" } },
			options: { deliverAs: "steer", triggerTurn: true },
		});
		s.emit(event, admitted("someone-else"));
		await settle();
		assert.deepEqual(srv.acks(), []);
		s.emit(event, admitted("m1"));
		s.emit(event, admitted("m1"));
		await until(() => srv.acks().length > 0, "ack");
		await settle();
		assert.deepEqual(srv.acks(), [{ id: "m1", ok: true }]);
		for (const f of cleanup.splice(0).reverse()) await f();
	}
});

for (const idle of [true, false]) {
	test(`a next_turn frame is answered when the entry is there (isIdle ${idle})`, async () => {
		const { s, srv } = await connected({ idle, send: appendEntry });
		srv.send(frame("n1", false));
		await until(() => srv.acks().length === 1, "ack");
		assert.deepEqual(s.sent[0].options, { deliverAs: "steer", triggerTurn: false });
		assert.deepEqual(srv.acks(), [{ id: "n1", ok: true }]);
	});

	test(`a next_turn frame with no entry is not answered (isIdle ${idle})`, async () => {
		const { s, srv } = await connected({ idle });
		s.entries.push({ type: "custom_message", details: { crosstalk_id: "older" } });
		srv.send(frame("n2", false));
		await until(() => s.sent.length === 1, "delivery");
		await settle();
		assert.deepEqual(srv.acks(), []);
	});
}

test("a rejected send gets no answer", async () => {
	mock.method(console, "error", () => {});
	const { s, srv } = await connected({
		send: () => {
			throw new Error("rejected");
		},
	});
	srv.send(frame("r1", false));
	srv.send(frame("r2", true));
	await until(() => s.sent.length === 2, "both attempts");
	await settle();
	assert.deepEqual(srv.acks(), []);
});

test("a malformed line is dropped and the valid frame after it is processed", async () => {
	const log = mock.method(console, "error", () => {});
	const { s, srv } = await connected({ send: appendEntry });
	srv.send('{"id":"bad\nnull\n{"id":7,"text":"x"}\n' + JSON.stringify(frame("ok1", false)) + "\n");
	await until(() => srv.acks().length === 1, "ack of the valid frame");
	assert.equal(s.sent.length, 1);
	assert.deepEqual(srv.acks(), [{ id: "ok1", ok: true }]);
	assert.equal(log.mock.callCount(), 3);
});

test("a line split across two data callbacks is delivered and answered once", async () => {
	const { s, srv } = await connected({ send: appendEntry });
	const bytes = Buffer.from(JSON.stringify({ ...frame("s1", false), text: "é€" }) + "\n");
	const cut = bytes.indexOf(Buffer.from("€")) + 1;
	srv.conns[0].write(bytes.subarray(0, cut));
	await settle();
	assert.equal(s.sent.length, 0);
	srv.conns[0].write(bytes.subarray(cut));
	await until(() => srv.acks().length === 1, "ack");
	await settle();
	assert.equal(s.sent.length, 1);
	assert.equal(s.sent[0].message.content, "é€");
	assert.equal(srv.acks().length, 1);
});

test("an error in the connect handler is caught, not thrown", async () => {
	const log = mock.method(console, "error", () => {});
	await shim();
	const s = load();
	s.ctx.sessionManager.getSessionId = () => {
		throw new Error("stale context");
	};
	s.emit("session_start", { reason: "startup" });
	await until(() => log.mock.callCount() === 1, "logged error");
});

test("reconnects after close with a new hello and discards the unfinished tail", async () => {
	mock.method(console, "error", () => {});
	const { s, srv } = await connected({ send: appendEntry });
	const line = JSON.stringify(frame("t1", false));
	srv.conns[0].write(line.slice(0, 10));
	await settle();
	srv.conns[0].end();
	await until(() => srv.lines.length === 2, "second hello");
	assert.equal(srv.lines[1].token, srv.lines[0].token);
	srv.send(line + "\n");
	await until(() => srv.acks().length === 1, "ack on the new connection");
	assert.equal(s.sent.length, 1);
});

test("an admission seen after a reconnect is not answered on the new connection", async () => {
	const { s, srv } = await connected();
	srv.send(frame("late", true));
	await until(() => s.sent.length === 1, "delivery");
	srv.conns[0].end();
	await until(() => srv.lines.length === 2, "second hello");
	s.emit("message_start", admitted("late"));
	await settle();
	assert.deepEqual(srv.acks(), []);
});

test("stops after session_shutdown", async () => {
	const { s, srv } = await connected();
	mock.timers.enable({ apis: ["setTimeout"] });
	s.emit("session_shutdown", { reason: "new" });
	await until(() => srv.conns[0].destroyed || srv.conns[0].readableEnded, "close");
	for (let i = 0; i < 20; i++) {
		mock.timers.tick(5000);
		await settle();
	}
	assert.equal(srv.conns.length, 1);
	assert.deepEqual(s.status, []);
});

async function advance(ms) {
	for (let i = 0; i < ms; i += 500) {
		mock.timers.tick(500);
		await tick();
		await tick();
	}
}

for (const [what, prefix] of [
	["no server", ""],
	["a bad socket path", "/" + "x".repeat(120)],
]) {
	test(`sets the footer status once when no shim ever answers (${what})`, async () => {
		mock.timers.enable({ apis: ["setTimeout"] });
		process.env.HOME = home + prefix;
		const s = load();
		s.emit("session_start", { reason: "startup" });
		await advance(29000);
		assert.deepEqual(s.status, []);
		await advance(60000);
		assert.deepEqual(s.status, [["crosstalk", "crosstalk: no shim"]]);
	});
}

test("keeps retrying after the footer status and clears it on connect", async () => {
	mock.timers.enable({ apis: ["setTimeout"] });
	const s = load();
	s.emit("session_start", { reason: "startup" });
	await advance(31000);
	const srv = await shim();
	await advance(4000);
	await until(() => srv.lines.length === 1, "hello");
	assert.deepEqual(s.status.at(-1), ["crosstalk", undefined]);
});

test("shows a refusal reason once", async () => {
	const { s, srv } = await connected();
	srv.send({ error: "protocol 2 wanted" });
	srv.send({ error: "protocol 2 wanted" });
	await settle();
	assert.deepEqual(s.status, [["crosstalk", "crosstalk: protocol 2 wanted"]]);
	assert.equal(s.sent.length, 0);
});
