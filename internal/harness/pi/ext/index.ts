// __CROSSTALK_MARKER__
import { randomBytes } from "node:crypto";
import { connect, type Socket } from "node:net";
import { homedir } from "node:os";
import { join } from "node:path";
import { VERSION, type ExtensionAPI, type ExtensionContext } from "@earendil-works/pi-coding-agent";

const BIN = "__CROSSTALK_BIN__";
const VERSION_TAG = "__CROSSTALK_VERSION__";
const noRefresh: boolean = false;
const PROTO = 1;

export default function crosstalk(pi: ExtensionAPI) {
	const token = randomBytes(16).toString("hex");
	pi.registerMcpServer("crosstalk", {
		command: BIN,
		args: ["mcp"],
		env: { CROSSTALK_PI_TOKEN: token, ...(noRefresh ? { CROSSTALK_NO_AUTO_REFRESH: "1" } : {}) },
		exposure: "direct",
	});

	let ctx: ExtensionContext | undefined;
	let sock: Socket | undefined;
	let retry: ReturnType<typeof setTimeout> | undefined;
	let noShim: ReturnType<typeof setTimeout> | undefined;
	let delay = 200;
	let stopped = false;
	let footer: string | undefined;
	// Ids of triggering frames sent on the live connection and not yet admitted.
	const pending = new Set<string>();

	// An uncaught exception in a socket or timer callback takes Pi down.
	const safe =
		<A extends unknown[]>(fn: (...args: A) => void) =>
		(...args: A) => {
			try {
				fn(...args);
			} catch (err) {
				console.error("crosstalk:", err);
			}
		};
	const write = (value: object) => sock?.write(JSON.stringify(value) + "\n");
	const setFooter = (text: string | undefined) => {
		if (text === footer) return;
		footer = text;
		ctx?.ui.setStatus("crosstalk", text);
	};

	const receive = safe((line: string) => {
		const frame = JSON.parse(line);
		if (typeof frame.error === "string") return setFooter("crosstalk: " + frame.error);
		const { id, text, trigger } = frame;
		if (typeof id !== "string" || typeof text !== "string") throw new Error("not a delivery frame");
		if (trigger) pending.add(id);
		pi.sendMessage(
			{ customType: "crosstalk", content: text, display: true, details: { crosstalk_id: id } },
			{ deliverAs: "steer", triggerTurn: !!trigger },
		);
		// Pi emits no event for a message that triggers no turn, so look for its entry.
		const stored = (e: any) => e.type === "custom_message" && e.details?.crosstalk_id === id;
		if (!trigger && ctx?.sessionManager.getEntries().some(stored)) write({ id, ok: true });
	});

	const open = safe(() => {
		if (stopped || !ctx) return;
		const s = connect(join(homedir(), ".local", "state", "crosstalk", "sock", "pi", `${process.pid}.sock`));
		let buf = "";
		s.setEncoding("utf8");
		s.on("connect", safe(() => {
			delay = 200;
			clearTimeout(noShim);
			// A refusal reason stays up across the reconnects that follow it.
			if (footer === "crosstalk: no shim") setFooter(undefined);
			sock = s;
			const sm = ctx!.sessionManager;
			write({
				proto: PROTO, token, ext_version: VERSION_TAG, pi_version: VERSION,
				session_id: sm.getSessionId(), session_file: sm.getSessionFile(), cwd: sm.getCwd(),
				name: pi.getSessionName() ?? "",
			});
		}));
		s.on("data", safe((chunk: string) => {
			const lines = (buf + chunk).split("\n");
			buf = lines.pop()!;
			for (const line of lines) receive(line);
		}));
		s.on("error", () => {});
		s.on("close", safe(() => {
			if (sock === s) {
				sock = undefined;
				pending.clear();
			}
			if (stopped) return;
			retry = setTimeout(open, delay);
			delay = Math.min(delay * 2, 2000);
		}));
	});

	const admitted = (event: { message: any }) => {
		const id = event.message?.details?.crosstalk_id;
		if (pending.delete(id)) write({ id, ok: true });
	};
	pi.on("message_start", admitted);
	pi.on("message_end", admitted);
	pi.on("agent_start", () => void write({ status: "busy" }));
	pi.on("agent_settled", () => void write({ status: "idle" }));
	pi.on("session_info_changed", (event) => void write({ name: event.name ?? "" }));
	// Returns at once: Pi starts MCP servers, the shim included, after this handler.
	pi.on("session_start", (_event, context) => {
		ctx = context;
		noShim = setTimeout(safe(() => setFooter("crosstalk: no shim")), 30000);
		open();
	});
	pi.on("session_shutdown", () => {
		stopped = true;
		clearTimeout(retry);
		clearTimeout(noShim);
		sock?.destroy();
	});
}
