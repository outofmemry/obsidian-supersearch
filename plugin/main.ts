import type { ChildProcess } from "child_process";
import {
	App, ItemView, Keymap, MarkdownView, Modal, Notice, Platform, Plugin, PluginSettingTab, Setting, SuggestModal, TFile,
	WorkspaceLeaf, debounce, getLinkpath, requestUrl,
} from "obsidian";

interface Result {
	path: string;
	kind: string;
	page: number; // pdf page, 0 otherwise
	line: number; // first line of the matching note section
	source: string;
	snippet: string; // match wrapped in \x02 … \x03
	note?: string; // image hits: the note that embeds the image
}

interface Settings {
	ignore: string; // one vault folder per line
	ocrLang: string;
	serverUrl: string; // remote server; empty = run one locally
	serverToken: string;
	fallbackLocal: boolean; // desktop only: use the local index when the remote is unreachable
}

const DEFAULTS: Settings = { ignore: "", ocrLang: "", serverUrl: "", serverToken: "", fallbackLocal: true };
const VIEW = "supersearch-view";

// The plugin holds no index. It runs the Go sidecar (or talks to a remote
// one), forwards vault events to it, and renders what it returns.
export default class Supersearch extends Plugin {
	settings = DEFAULTS;
	left = 0; // files still waiting to be indexed
	paused = false;
	private server: ChildProcess | null = null;
	private token = "";
	private base: Promise<string> = new Promise(() => {});
	private crashRestarted = false;
	private restarting = false;
	private unloading = false;
	private warnedMissing = false;
	private idleTicks = 0;
	// Remote fallback (desktop only): while Server URL is set but unreachable,
	// the local index serves instead, and the remote is re-probed in the
	// background until it answers again.
	private remoteBase = "";
	private remoteToken = "";
	private onFallback = false;
	private remoteFails = 0;
	private lastFallbackTry = 0;

	async onload() {
		this.settings = { ...DEFAULTS, ...(await this.loadData()) };
		this.addSettingTab(new SettingsTab(this));
		this.registerView(VIEW, (leaf) => new SearchView(leaf, this));
		this.startServer();

		this.addCommand({ id: "search", name: "Search everything", callback: () => new SearchModal(this).open() });
		this.addCommand({
			id: "search-note",
			name: "Search in current note (including its images)",
			checkCallback: (checking) => {
				const file = this.app.workspace.getActiveFile();
				if (!file) return false;
				if (!checking) new SearchModal(this, [file.path, ...this.embedsOf(file.path)]).open();
				return true;
			},
		});
		this.addCommand({ id: "ask", name: "Ask your vault", callback: () => new AskModal(this).open() });
		this.addCommand({ id: "panel", name: "Open search panel", callback: () => this.openPanel() });
		this.addCommand({ id: "reset", name: "Delete search index (rebuilds from the vault)", callback: () => this.confirmReset() });
		this.addCommand({ id: "pause", name: "Pause / resume background indexing (OCR)", callback: () => this.togglePause() });
		this.addRibbonIcon("search", "Supersearch", () => new SearchModal(this).open());

		// After layout-ready, or Obsidian replays a "create" for every file in the vault.
		this.app.workspace.onLayoutReady(() => {
			this.registerEvent(this.app.vault.on("create", (f) => this.changed(f.path)));
			this.registerEvent(this.app.vault.on("modify", (f) => this.changed(f.path)));
			this.registerEvent(this.app.vault.on("delete", (f) => this.changed(f.path)));
			this.registerEvent(this.app.vault.on("rename", (f, oldPath) => this.changed(f.path, oldPath)));
		});

		const bar = this.addStatusBarItem();
		this.registerInterval(window.setInterval(() => this.refreshStatus(bar), 5000));
	}

	onunload() {
		this.unloading = true;
		this.server?.kill();
	}

	async saveSettings() {
		await this.saveData(this.settings);
	}

	// Settings changes apply by restarting the server with new flags.
	restartServer = debounce(() => {
		if (this.server) {
			this.restarting = true;
			this.server.kill(); // the exit handler starts the new one
		} else {
			this.startServer();
		}
	}, 1500, true);

	private startServer() {
		const { serverUrl, serverToken } = this.settings;
		this.remoteBase = serverUrl.trim().replace(/\/+$/, "");
		this.remoteToken = serverToken;
		this.onFallback = false;
		this.remoteFails = 0;
		if (this.remoteBase) {
			// Remote mode: no local server is spawned. On desktop the status
			// tick fails over to the local index if the remote goes quiet.
			this.token = this.remoteToken;
			this.base = Promise.resolve(this.remoteBase);
			return;
		}
		this.startLocal();
	}

	// Spawns the bundled server. Returns true when a local server is running.
	private startLocal(): boolean {
		if (this.server) return true;
		if (!Platform.isDesktopApp) {
			new Notice("Supersearch: on mobile, set a remote server in the plugin settings.", 10000);
			return false;
		}
		// Node modules only exist on desktop; loading them lazily keeps the plugin loadable on mobile.
		const { spawn } = require("child_process") as typeof import("child_process");
		const { existsSync } = require("fs") as typeof import("fs");
		const { join } = require("path") as typeof import("path");
		const vault = (this.app.vault.adapter as any).getBasePath() as string;
		const bin = join(vault, this.manifest.dir!, "supersearch-server");
		if (!existsSync(bin)) {
			new Notice("Supersearch: server binary not found in the plugin folder. Run ./install.sh.", 10000);
			return false;
		}
		this.token = Array.from(crypto.getRandomValues(new Uint8Array(24)), (b) => b.toString(16).padStart(2, "0")).join("");
		// The server reads OCR settings and ignored folders from this plugin's data.json itself.
		const args = ["-vault", vault, "-exit-on-stdin-close"];
		// stdin stays open as a lifeline: if Obsidian dies, the pipe closes and the server exits.
		const server = spawn(bin, args, { env: { ...process.env, SUPERSEARCH_TOKEN: this.token }, stdio: ["pipe", "pipe", "pipe"] });
		this.server = server;
		this.base = new Promise((resolve) => {
			let out = "";
			server.stdout!.on("data", (d) => {
				out += d;
				const m = out.match(/LISTENING (\d+)/);
				if (m) resolve(`http://127.0.0.1:${m[1]}`);
			});
		});
		server.stderr!.on("data", (d) => console.error("supersearch-server:", String(d)));
		server.on("error", (e) => new Notice("Supersearch: could not start server: " + e.message, 10000));
		server.on("exit", (code) => {
			this.server = null;
			if (this.unloading) return;
			if (this.restarting) {
				this.restarting = false;
				this.startServer();
			} else if (!this.crashRestarted) {
				this.crashRestarted = true;
				this.startServer();
			} else {
				new Notice(`Supersearch: server stopped (exit ${code}). Reload the plugin to retry.`, 10000);
			}
		});
		return true;
	}

	// Switches the active backend to the local index after the remote went
	// quiet. At most one attempt per minute, so a missing binary can't spam.
	private startFallback() {
		if (Date.now() - this.lastFallbackTry < 60000) return;
		this.lastFallbackTry = Date.now();
		if (!this.startLocal()) return;
		this.onFallback = true;
		this.remoteFails = 0;
		new Notice("Supersearch: remote server unreachable, using the local index. It switches back automatically.", 8000);
	}

	// The background probe answered again: drop the local server and go back
	// to the remote. Routing through the exit handler keeps the restart logic
	// (and crash guard) in one place.
	private stopFallback() {
		if (!this.onFallback) return;
		this.onFallback = false;
		this.remoteFails = 0;
		new Notice("Supersearch: remote server reachable again.", 5000);
		if (this.server) {
			this.restarting = true;
			this.server.kill();
		} else {
			this.startServer();
		}
	}

	// Quick remote health check, independent of the active backend.
	private async probeRemote(): Promise<any> {
		const res = (await Promise.race([
			requestUrl({
				url: this.remoteBase + "/status",
				method: "GET",
				headers: { Authorization: "Bearer " + this.remoteToken },
				throw: false,
			}),
			new Promise((_, reject) => setTimeout(() => reject(new Error("timeout")), 5000)),
		])) as any;
		if (!res || res.status >= 400) throw new Error("remote status " + (res?.status ?? "failed"));
		return res.json;
	}

	async api(path: string, body?: unknown) {
		const res = await requestUrl({
			url: (await this.base) + path,
			method: body ? "POST" : "GET",
			headers: { Authorization: "Bearer " + this.token },
			contentType: "application/json",
			body: body ? JSON.stringify(body) : undefined,
			throw: false,
		});
		if (res.status >= 400) throw new Error(res.text.trim() || `HTTP ${res.status}`); // keep the server's message
		return res.status === 204 ? null : res.json;
	}

	// search returns display-ready results plus a one-line note for the UI.
	async search(q: string, scope?: string[]): Promise<{ results: Result[]; info: string }> {
		if (!q.trim()) return { results: [], info: "" };
		let url = "/search?limit=50&q=" + encodeURIComponent(q);
		for (const p of scope ?? []) url += "&scope=" + encodeURIComponent(p);
		const res = await this.api(url);
		// attachment → first note that embeds it. Built only when a result needs
		// it: walking every link in the vault on each keystroke adds up on big vaults.
		let embeddedIn: Map<string, string> | undefined;
		const hostOf = (path: string) => {
			if (!embeddedIn) {
				embeddedIn = new Map();
				for (const [note, dests] of Object.entries(this.app.metadataCache.resolvedLinks))
					for (const dest in dests) if (!embeddedIn.has(dest)) embeddedIn.set(dest, note);
			}
			return embeddedIn.get(path);
		};
		const results: Result[] = [];
		for (const r of res.results as Result[]) {
			// Self-healing: a hit whose file is gone (a delete event got lost) is
			// never shown, and reporting it makes the server drop it.
			if (!this.app.vault.getAbstractFileByPath(r.path)) {
				this.changed(r.path);
				continue;
			}
			// Text found inside an image or recording is presented as the note that embeds it.
			results.push(r.kind === "image" || r.kind === "audio" ? { ...r, note: hostOf(r.path) } : r);
		}
		let info = res.corrected ? `Showing results for "${res.corrected}"` : "";
		if (!results.length && this.left) info = `No matches yet. Still indexing ${this.left} files…`;
		if (this.onFallback) info += (info ? " · " : "") + "local index";
		return { results, info };
	}

	embedsOf(note: string): string[] {
		const out: string[] = [];
		for (const e of this.app.metadataCache.getCache(note)?.embeds ?? []) {
			const f = this.app.metadataCache.getFirstLinkpathDest(getLinkpath(e.link), note);
			if (f) out.push(f.path);
		}
		return out;
	}

	changed(path: string, oldPath?: string) {
		this.idleTicks = 0; // something may need indexing: check status on the next tick
		this.api("/changed", { path, oldPath }).catch(() => {}); // dot-folders etc. are rejected by design; the periodic rescan is the safety net
	}

	confirmReset() {
		new ConfirmModal(
			this.app,
			"Delete search index?",
			"Everything Supersearch has stored (extracted text, OCR, audio transcripts) is deleted. Your notes and files are not touched. " +
				"While the plugin is enabled the index rebuilds in the background: notes in seconds, images in a few minutes.",
			async () => {
				try {
					await this.api("/reset", {});
					new Notice("Supersearch: index deleted. Rebuilding in the background…");
				} catch (e) {
					new Notice("Supersearch: could not delete the index: " + (e as Error).message);
				}
			},
		).open();
	}

	private async togglePause() {
		try {
			await this.api("/pause", { paused: !this.paused });
			this.paused = !this.paused;
			new Notice(this.paused ? "Supersearch: OCR paused" : "Supersearch: OCR resumed");
		} catch {
			new Notice("Supersearch: server not reachable");
		}
	}

	private async openPanel() {
		const leaf = this.app.workspace.getLeavesOfType(VIEW)[0] ?? this.app.workspace.getRightLeaf(false);
		if (!leaf) return;
		await leaf.setViewState({ type: VIEW, active: true });
		this.app.workspace.revealLeaf(leaf);
	}

	// Polls every 5 s while there is work, every 30 s when idle. While a remote
	// backend is active with fallback enabled, every tick probes the remote so
	// a dead server fails over within seconds; while on fallback, the local
	// status is polled as usual and the remote is re-probed in the background.
	private async refreshStatus(bar: HTMLElement) {
		const watchRemote = Platform.isDesktopApp && this.settings.fallbackLocal && !!this.remoteBase && !this.onFallback;
		if (!this.left && !watchRemote && this.idleTicks++ % 6) return;
		if (watchRemote) {
			try {
				this.applyStatus(await this.probeRemote(), bar);
				this.remoteFails = 0;
			} catch {
				if (++this.remoteFails >= 2) this.startFallback();
				else bar.setText("Supersearch: remote unreachable, retrying…");
			}
			return;
		}
		try {
			this.applyStatus(await this.api("/status"), bar);
		} catch {
			bar.setText("");
		}
		if (this.onFallback) this.probeRemote().then(
			() => this.stopFallback(),
			() => {},
		);
	}

	private applyStatus(s: any, bar: HTMLElement) {
		this.paused = s.paused;
		const left = (this.left = s.counts.pending + s.counts.ocr);
		bar.setText(!left ? "" : s.paused ? `Supersearch: paused, ${left} left` : `Supersearch: indexing, ${left} left`);
		if (s.missing.length && !this.warnedMissing) {
			this.warnedMissing = true;
			new Notice("Supersearch: supersearch-helper is missing, so PDFs and images can't be read. Re-run ./install.sh.", 15000);
		}
	}
}

function renderResult(app: App, r: Result, el: HTMLElement) {
	el.addClass("mod-complex");
	const content = el.createDiv("suggestion-content");
	const shown = r.note ?? r.path;
	const slash = shown.lastIndexOf("/");
	const title = content.createDiv("suggestion-title");
	title.setText(shown.slice(slash + 1).replace(/\.md$/, ""));
	if (slash > 0) title.createSpan({ cls: "suggestion-note", text: "  " + shown.slice(0, slash) });

	// Built from text nodes, never innerHTML: file contents are untrusted.
	const note = content.createDiv("suggestion-note");
	for (const part of r.snippet.replace(/\s+/g, " ").split("\x02")) {
		const [hit, rest] = part.includes("\x03") ? part.split("\x03") : ["", part];
		if (hit) note.createSpan({ cls: "suggestion-highlight", text: hit });
		note.appendText(rest);
	}

	const aux = el.createDiv("suggestion-aux");
	const file = app.vault.getAbstractFileByPath(r.path);
	if (r.kind === "image" && file instanceof TFile)
		aux.createEl("img", { cls: "supersearch-thumb", attr: { src: app.vault.getResourcePath(file), loading: "lazy" } });
	const ext = r.path.slice(r.path.lastIndexOf(".") + 1).toUpperCase();
	const badge =
		r.source === "ocr" ? (r.note ? "in image" : "OCR " + ext) : r.source === "speech" ? (r.note ? "in recording" : "SPEECH " + ext) : ext;
	aux.createSpan({ cls: "suggestion-hotkey", text: badge + (r.page > 0 ? " p." + r.page : "") });
}

async function openResult(app: App, r: Result, newTab: boolean) {
	const line = r.note ? embedLine(app, r.note, r.path) : await matchLine(app, r);
	const link = r.page > 0 ? `${r.path}#page=${r.page}` : (r.note ?? r.path);
	// eState.line scrolls to and flashes that line, in both editing and reading view.
	await app.workspace.openLinkText(link, "", newTab, line === undefined ? undefined : { eState: { line } });
	// In editing view, also select the matched words.
	const hit = r.note ? undefined : r.snippet.match(/\x02([^\x03]*)\x03/)?.[1];
	const view = app.workspace.getActiveViewOfType(MarkdownView);
	if (hit && line !== undefined && view?.file?.path === r.path && view.getMode() === "source") {
		const text = view.editor.getLine(line);
		const at = text.toLowerCase().indexOf(hit.toLowerCase());
		if (at >= 0) view.editor.setSelection({ line, ch: at }, { line, ch: at + hit.length });
	}
}

// Line of the ![…] embed that shows `image` inside `note`.
function embedLine(app: App, note: string, image: string): number | undefined {
	const embeds = app.metadataCache.getCache(note)?.embeds ?? [];
	const name = image.slice(image.lastIndexOf("/") + 1);
	const embed =
		embeds.find((e) => app.metadataCache.getFirstLinkpathDest(getLinkpath(e.link), note)?.path === image) ??
		embeds.find((e) => e.link.endsWith(name));
	return embed?.position.start.line;
}

// Line of the first highlighted match, searching from the section's first line.
async function matchLine(app: App, r: Result): Promise<number | undefined> {
	const file = app.vault.getAbstractFileByPath(r.path);
	if (r.kind !== "text" || !(file instanceof TFile) || file.extension !== "md") return;
	const hit = r.snippet.match(/\x02([^\x03]*)\x03/)?.[1]?.toLowerCase();
	if (!hit) return r.line;
	const lines = (await app.vault.cachedRead(file)).split("\n");
	for (let i = r.line; i < lines.length; i++) if (lines[i].toLowerCase().includes(hit)) return i;
	return r.line;
}

class SearchModal extends SuggestModal<Result> {
	private seq = 0;
	private latest: Result[] = [];
	private info: HTMLElement;

	constructor(private plugin: Supersearch, private paths?: string[]) {
		super(plugin.app);
		this.limit = 50;
		this.setPlaceholder(paths ? "Search this note and its images…" : "Search notes, PDFs, images, docs…  (path: ext: in: -word \"phrase\")");
		this.emptyStateText = "No matches";
		this.info = createDiv("supersearch-info");
		this.modalEl.insertBefore(this.info, this.resultContainerEl);
	}

	// Requests can resolve out of order while typing fast. Every call returns
	// the newest results known, so an old response can never paint over a newer one.
	async getSuggestions(query: string): Promise<Result[]> {
		const n = ++this.seq;
		const res = await this.plugin.search(query, this.paths).catch(() => ({ results: [] as Result[], info: "Search server not reachable" }));
		if (n === this.seq) {
			this.latest = res.results;
			this.info.setText(res.info);
		}
		return this.latest;
	}

	renderSuggestion(r: Result, el: HTMLElement) {
		renderResult(this.app, r, el);
	}

	onChooseSuggestion(r: Result, evt: MouseEvent | KeyboardEvent) {
		openResult(this.app, r, Keymap.isModEvent(evt) !== false);
	}
}

// Ask a question; Apple's on-device model answers from your notes and cites them.
class AskModal extends Modal {
	constructor(private plugin: Supersearch) {
		super(plugin.app);
	}

	onOpen() {
		const { contentEl } = this;
		contentEl.addClass("supersearch-ask");
		const input = contentEl.createEl("input", { type: "text", cls: "supersearch-input", attr: { placeholder: "Ask your vault a question…" } });
		const answer = contentEl.createDiv("supersearch-answer");
		const sources = contentEl.createDiv();
		input.focus();
		input.addEventListener("keydown", async (e) => {
			if (e.key !== "Enter" || !input.value.trim()) return;
			answer.setText("Thinking…");
			sources.empty();
			try {
				const res = await this.plugin.api("/ask", { question: input.value });
				answer.setText(res.answer); // plain text: the answer is built from untrusted note content
				for (const [i, r] of (res.sources as Result[]).entries()) {
					if (!this.app.vault.getAbstractFileByPath(r.path)) continue;
					const el = sources.createDiv("suggestion-item");
					renderResult(this.app, r, el);
					el.querySelector(".suggestion-title")?.prepend(`[${i + 1}] `);
					el.addEventListener("click", (ev) => {
						openResult(this.app, r, Keymap.isModEvent(ev) !== false);
						this.close();
					});
				}
			} catch (err) {
				answer.setText("Could not answer: " + ((err as Error).message ?? err));
			}
		});
	}

	onClose() {
		this.contentEl.empty();
	}
}

// The same search in a sidebar: results stay visible while you click through them.
class SearchView extends ItemView {
	constructor(leaf: WorkspaceLeaf, private plugin: Supersearch) {
		super(leaf);
	}
	getViewType() {
		return VIEW;
	}
	getDisplayText() {
		return "Supersearch";
	}
	getIcon() {
		return "search";
	}

	async onOpen() {
		const root = this.contentEl;
		root.empty();
		root.addClass("supersearch-view");
		const input = root.createEl("input", { type: "search", cls: "supersearch-input", attr: { placeholder: "Search everything…" } });
		const info = root.createDiv("supersearch-info");
		const list = root.createDiv();
		let seq = 0;
		const run = debounce(async () => {
			const n = ++seq;
			const res = await this.plugin.search(input.value).catch(() => ({ results: [] as Result[], info: "Search server not reachable" }));
			if (n !== seq) return;
			info.setText(res.info);
			list.empty();
			for (const r of res.results) {
				const el = list.createDiv("suggestion-item");
				renderResult(this.app, r, el);
				el.addEventListener("click", (e) => openResult(this.app, r, Keymap.isModEvent(e) !== false));
			}
		}, 30, true);
		input.addEventListener("input", run);
		input.focus();
	}
}

class ConfirmModal extends Modal {
	constructor(app: App, private heading: string, private body: string, private onConfirm: () => void) {
		super(app);
	}

	onOpen() {
		this.titleEl.setText(this.heading);
		this.contentEl.createEl("p", { text: this.body });
		new Setting(this.contentEl)
			.addButton((b) => b.setButtonText("Cancel").onClick(() => this.close()))
			.addButton((b) =>
				b.setButtonText("Delete").setWarning().onClick(() => {
					this.close();
					this.onConfirm();
				}),
			);
	}

	onClose() {
		this.contentEl.empty();
	}
}

class SettingsTab extends PluginSettingTab {
	constructor(private plugin: Supersearch) {
		super(plugin.app, plugin);
	}

	display() {
		const { containerEl } = this;
		const s = this.plugin.settings;
		// Saved on every keystroke, applied (server restart) once: on leaving a
		// text field, or right away for a toggle. Restarting per keystroke ran
		// the server with half-typed settings like "en-U".
		const apply = () => this.plugin.restartServer();
		const save = async () => {
			await this.plugin.saveSettings();
		};
		containerEl.empty();
		new Setting(containerEl)
			.setName("Ignored folders")
			.setDesc("One vault folder per line. Nothing inside is indexed or OCR'd.")
			.addTextArea((t) => {
				t.setPlaceholder("Templates\nArchive").setValue(s.ignore).onChange(async (v) => ((s.ignore = v), save()));
				t.inputEl.addEventListener("blur", apply);
			});
		new Setting(containerEl)
			.setName("OCR languages")
			.setDesc("Comma-separated, e.g. en-US,hi-IN. Empty = English; \"auto\" = detect. Changing this re-reads every image and recording.")
			.addText((t) => {
				t.setPlaceholder("en-US").setValue(s.ocrLang).onChange(async (v) => ((s.ocrLang = v), save()));
				t.inputEl.addEventListener("blur", apply);
			});
		new Setting(containerEl)
			.setName("Delete search index")
			.setDesc("Deletes all stored search data (not your notes). It rebuilds in the background while the plugin is enabled.")
			.addButton((b) => b.setButtonText("Delete…").setWarning().onClick(() => this.plugin.confirmReset()));
		new Setting(containerEl).setName("Remote server").setHeading();
		new Setting(containerEl)
			.setName("Server URL")
			.setDesc("Use a supersearch-server running elsewhere (needed on mobile). Empty = run one on this computer.")
			.addText((t) => {
				t.setPlaceholder("https://mac-mini.example.ts.net").setValue(s.serverUrl).onChange(async (v) => ((s.serverUrl = v), save()));
				t.inputEl.addEventListener("blur", apply);
			});
		new Setting(containerEl)
			.setName("Server token")
			.setDesc("The SUPERSEARCH_TOKEN the remote server was started with.")
			.addText((t) => {
				t.inputEl.type = "password";
				t.setValue(s.serverToken).onChange(async (v) => ((s.serverToken = v), save()));
				t.inputEl.addEventListener("blur", apply);
			});
		if (Platform.isDesktopApp) {
			new Setting(containerEl)
				.setName("Fall back to local server")
				.setDesc("When the remote server is unreachable, run the local index instead and switch back automatically.")
				.addToggle((t) => t.setValue(s.fallbackLocal).onChange(async (v) => ((s.fallbackLocal = v), await save(), apply())));
		}
	}
}
