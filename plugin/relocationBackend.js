/*
 * Content relocation, backend half. The widget note loads this note as a module under its title,
 * so the title must stay exactly `relocationBackend`.
 *
 * It mirrors apps/server/src/routes/api/content_relocation.ts on the content-relocation branch of
 * the fork: same labels, same protocol, same order of operations, so notes moved by either one can
 * be brought back by the other, and by blobstore's move and back commands.
 *
 * The services live in a JSON code note labelled #relocationConfig instead of in options:
 *
 *   {
 *     "default": "<service id>",
 *     "locationLabel": "IsLocal",
 *     "services": [ { "id": "...", "name": "...", "url": "https://.../", "token": "..." } ]
 *   }
 *
 * createConfigNote() puts that note beside the widget when there is none, so an import needs no manual
 * setup beyond filling it in, and replacing the widget on an update leaves it alone.
 *
 * Globals are reached through globalThis: this code is evaluated inside a Trilium module whose own
 * imports could otherwise shadow names such as fetch or crypto.
 */

/** How long one transfer is given; a large file takes tens of seconds to travel. */
const TRANSFER_TIMEOUT_MS = 300_000;

/** How long the overview waits for one service's status or list before showing it as unreachable. */
const OVERVIEW_TIMEOUT_MS = 20_000;

/** What a note is left as while its content lives elsewhere. Blank content would read as empty. */
const PLACEHOLDER_CONTENT = " ";

/** Same character set as isValidAttributeName in @triliumnext/commons. */
const ATTRIBUTE_NAME = /^[\p{L}\p{N}_:]+$/u;

/**
 * Notes with a relocation in progress. Kept on globalThis because this module is evaluated afresh on
 * every call; it guards against two windows of the same server, not against another process.
 */
const busy = (globalThis.__contentRelocationBusy ??= new Set());

/**
 * Moves one note's content out (to the service named by serviceId, or the default) or back (to the
 * service the note records). Returns the service's name and the size as Trilium writes it.
 */
async function move(noteId, to, serviceId) {
    if (to !== "local" && to !== "external") {
        throw new Error("to must be 'local' or 'external'");
    }

    const note = api.getNote(noteId);
    if (!note) {
        throw new Error(`Note ${noteId} does not exist`);
    }
    if (busy.has(noteId)) {
        throw new Error(`Note ${noteId} is already being relocated`);
    }

    busy.add(noteId);
    try {
        const config = readConfig();

        if (to === "external") {
            const service = serviceId ? config.services.find((entry) => entry.id === serviceId) : defaultService(config);
            if (!service) {
                throw new Error(serviceId ? `No configured service has the id "${serviceId}"` : "The #relocationConfig note lists no service");
            }
            return await moveOut(note, service, config);
        }

        const service = serviceFor(note, config);
        if (!service) {
            const target = note.getLabelValue("relocationTarget");
            throw new Error(`The content of ${note.noteId} is on "${target}", which is not among the configured services`);
        }
        return await moveBack(note, service, config);
    } finally {
        busy.delete(noteId);
    }
}

/**
 * A template beside the widget note, with an empty url and token so it reads as unconfigured until
 * filled. Called by the widget, through runOnBackend, when no note carries #relocationConfig.
 */
function createConfigNote() {
    const id = api.randomString(16);
    const template = {
        default: id,
        locationLabel: "IsLocal",
        services: [{ id, name: "blobstore", url: "", token: "" }]
    };
    const parent = api.startNote.getParentNotes()[0];
    const { note } = api.createNewNote({
        parentNoteId: parent ? parent.noteId : "root",
        title: "内容迁移配置",
        content: JSON.stringify(template, null, 2),
        type: "code",
        mime: "application/json"
    });
    note.setLabel("relocationConfig", "");
    return note;
}

/**
 * Sends the content away and leaves the note showing it from there. The upload happens first: a
 * note that has lost its content while the upload failed would have lost it altogether.
 */
async function moveOut(note, service, config) {
    if (note.type !== "file") {
        throw new Error(`Note ${note.noteId} holds no content to move out`);
    }

    const blobId = note.blobId;
    const content = note.getContent();
    const bytes = typeof content === "string" ? new globalThis.TextEncoder().encode(content) : content;
    const stored = await upload(service, note, bytes, note.mime);
    const hash = stored.sha256 ?? (await sha256(bytes));

    try {
        api.transactional(() => {
            // The upload took a while; another process may have moved the note or its content may
            // have been replaced meanwhile, and what was uploaded would then not be the note's.
            if (note.type !== "file" || note.blobId !== blobId) {
                throw new Error(`Note ${note.noteId} changed while its content was being uploaded; nothing was changed`);
            }
            note.setLabel("relocationHash", hash.slice(0, 16));
            note.setLabel("relocationTarget", service.id);
            note.setLabel("relocationSize", formatSize(stored.size ?? bytes.length));
            note.setLabel("webViewSrc", stored.url);
            if (config.locationLabel) {
                note.setLabel(config.locationLabel, "false");
            }
            // The mime is kept: Trilium ignores it on a webView note, and the way back restores it.
            note.type = "webView";
            note.save();
            note.setContent(PLACEHOLDER_CONTENT);
        });
    } catch (e) {
        // Without the note's record the upload is unreachable, so it is taken back — unless the note
        // now points outside itself: the id is the note's, and the copy there is someone else's.
        if (note.type === "file") {
            await remove(service, stored.id).catch((removeError) => {
                api.log(`Content relocation: could not undo the upload of ${note.noteId}: ${removeError}`);
            });
        }
        throw e;
    }

    return { ok: true, serviceName: service.name, size: formatSize(bytes.length) };
}

/** Fetches the content back and restores the note to what it was before it left. */
async function moveBack(note, service, config) {
    // A file note carrying a stale checksum label would otherwise have its content overwritten.
    if (note.type !== "webView") {
        throw new Error(`Note ${note.noteId} is a ${note.type} note, not relocated content`);
    }
    const recorded = readHash(note);
    if (recorded === null) {
        throw new Error(`Note ${note.noteId} has no relocated content`);
    }

    const { bytes, mime, storedSha256 } = await download(service, note.noteId);
    await verify(note, bytes, recorded, storedSha256);

    api.transactional(() => {
        if (note.type !== "webView" || readHash(note) !== recorded) {
            throw new Error(`Note ${note.noteId} changed while its content was being fetched; nothing was changed`);
        }
        // Notes moved out by other implementations had their mime cleared; the service's then serves.
        note.mime = note.mime || mime;
        note.type = "file";
        note.save();
        note.setContent(bytes);
        for (const name of [
            "webViewSrc",
            "relocationHash",
            "relocationTarget",
            "relocationSize",
            // Written by earlier versions of the fork.
            "relocationInfo",
            "relocationId",
            "relocationMime",
            "relocationSha256"
        ]) {
            note.removeLabel(name);
        }
        if (config.locationLabel) {
            note.setLabel(config.locationLabel, "true");
        }
    });

    // The copy outside is now a duplicate. Failing to remove it leaves a stray file, which is worth
    // reporting but not worth undoing a restore that has already succeeded.
    await remove(service, note.noteId).catch((e) => {
        api.log(`Content relocation: ${note.noteId} is back, but the copy outside remains: ${e}`);
    });

    return { ok: true, serviceName: service.name, size: formatSize(bytes.length) };
}

/**
 * Everything the overview page shows, one entry per configured service: what the service reports
 * about itself (GET /status), what it holds (GET /blobs), and how that lines up with the notes that
 * record content there. Tokens stay here; the page receives names, hosts and figures only.
 */
async function overview() {
    const config = readConfig();
    const fallback = defaultService(config);

    // Every note whose content is away, by the service its record points at.
    const assigned = new Map(config.services.map((service) => [service.id, []]));
    const unknownTarget = [];
    for (const note of relocatedNotes()) {
        const service = serviceFor(note, config);
        if (service) {
            assigned.get(service.id).push(note);
        } else {
            unknownTarget.push({ ...noteRef(note), target: note.getLabelValue("relocationTarget") });
        }
    }

    const services = await Promise.all(
        config.services.map(async (service) => {
            const [status, list] = await Promise.all([
                getJSON(service, "status").then((value) => ({ value }), (e) => ({ error: e.message })),
                getJSON(service, "blobs").then((value) => ({ value }), (e) => ({ error: e.message }))
            ]);
            const notes = assigned.get(service.id);
            const entry = {
                id: service.id,
                name: service.name,
                host: service.baseUrl.replace(/^https?:\/\//, "").replace(/\/$/, ""),
                isDefault: fallback !== null && fallback.id === service.id,
                status: status.value ?? null,
                statusError: status.error ?? null,
                listError: list.error ?? null,
                noteCount: notes.length,
                blobs: [],
                missing: []
            };
            if (!list.value) {
                return entry;
            }

            const held = new Set();
            for (const blob of Array.isArray(list.value.blobs) ? list.value.blobs : []) {
                held.add(blob.id);
                const note = api.getNote(blob.id);
                entry.blobs.push({
                    id: blob.id,
                    filename: blob.filename ?? "",
                    size: blob.size ?? 0,
                    storedAt: blob.storedAt ?? null,
                    versions: blob.versions ?? 0,
                    note: note ? noteRef(note) : null,
                    state: blobState(note, blob, service, config)
                });
            }
            for (const note of notes) {
                if (!held.has(note.noteId)) {
                    entry.missing.push({ ...noteRef(note), size: note.getLabelValue("relocationSize") ?? "" });
                }
            }
            return entry;
        })
    );

    return { generatedAt: new Date().toISOString(), services, unknownTarget };
}

/**
 * The overview cut down to what the right-pane card shows under every note: per service, the figures
 * that say whether anything needs a look, and how many records disagree.
 */
async function summary() {
    const full = await overview();
    return {
        generatedAt: full.generatedAt,
        unknownTarget: full.unknownTarget.length,
        services: full.services.map((service) => {
            const status = service.status || {};
            const group = status.group || null;
            const backup = status.backup || null;
            return {
                id: service.id,
                name: service.name,
                isDefault: service.isDefault,
                error: service.statusError || service.listError || null,
                count: group ? group.count : null,
                totalSize: group ? group.totalSize : null,
                disk: status.disk || null,
                backup: backup ? { exitCode: backup.exitCode, finished: backup.finished || null, error: backup.error || null } : null,
                problems: service.blobs.filter((blob) => blob.state !== "ok").length + service.missing.length
            };
        })
    };
}

/** Notes showing content from outside: a webView carrying any of the relocation records. */
function relocatedNotes() {
    const found = new Map();
    for (const name of ["relocationHash", "relocationTarget", "relocationInfo", "relocationId"]) {
        for (const note of api.getNotesWithLabel(name)) {
            if (note.type === "webView" && readHash(note) !== null) {
                found.set(note.noteId, note);
            }
        }
    }
    return found.values();
}

function noteRef(note) {
    return { noteId: note.noteId, title: note.title };
}

/**
 * How one stored file relates to the note with its id:
 *   ok        the note shows it from this service, checksums agree
 *   mismatch  the note shows it from here, but its recorded checksum is not this file's
 *   stray     the note holds its content in the database again; this copy was left behind
 *   elsewhere the note's content is recorded on another service
 *   orphan    no note has this id (deleted, or from another database)
 */
function blobState(note, blob, service, config) {
    if (!note) {
        return "orphan";
    }
    if (note.type !== "webView" || readHash(note) === null) {
        return "stray";
    }
    const owner = serviceFor(note, config);
    if (!owner || owner.id !== service.id) {
        return "elsewhere";
    }
    const recorded = readHash(note);
    return recorded && typeof blob.sha256 === "string" && !blob.sha256.startsWith(recorded) ? "mismatch" : "ok";
}

async function getJSON(service, path) {
    const response = await request("GET", `${service.baseUrl}${path}`, {
        headers: { Authorization: `Bearer ${service.token}` },
        timeoutMs: OVERVIEW_TIMEOUT_MS
    });
    return await response.json();
}

async function upload(service, note, bytes, mime) {
    const headers = {
        Authorization: `Bearer ${service.token}`,
        "Content-Type": mime || "application/octet-stream",
        "X-Relocation-Note-Id": note.noteId
    };

    // Percent-encoded: a header value carries Latin-1 only. The service decodes it.
    const filename = note.getLabelValue("originalFileName");
    if (filename) {
        headers["X-Relocation-Filename"] = encodeURIComponent(filename);
    }

    const url = `${service.baseUrl}blobs`;
    const response = await request("POST", url, { headers, body: bytes });
    const payload = await response.json().catch(() => ({}));
    if (typeof payload?.id !== "string" || typeof payload?.url !== "string") {
        throw new Error(`POST ${url} answered without an id and a url`);
    }
    return {
        id: payload.id,
        url: payload.url,
        sha256: typeof payload.sha256 === "string" ? payload.sha256 : undefined,
        size: typeof payload.size === "number" ? payload.size : undefined
    };
}

async function download(service, id) {
    const response = await request("GET", `${service.baseUrl}blobs/${encodeURIComponent(id)}`, {
        headers: { Authorization: `Bearer ${service.token}` }
    });

    // Parameters such as "; charset=utf-8" are dropped: the note's mime may be restored from this.
    const contentType = (response.headers.get("content-type") ?? "").split(";")[0].trim();

    return {
        bytes: new Uint8Array(await response.arrayBuffer()),
        mime: contentType || "application/octet-stream",
        storedSha256: response.headers.get("x-blob-sha256") ?? undefined
    };
}

async function remove(service, id) {
    await request("DELETE", `${service.baseUrl}blobs/${encodeURIComponent(id)}`, {
        headers: { Authorization: `Bearer ${service.token}` },
        allowNotFound: true
    });
}

/**
 * One call to the service. Every failure names the method and the address, so a wrong url in the
 * config reads as such instead of as a bare status code.
 */
async function request(method, url, { headers, body, allowNotFound = false, timeoutMs = TRANSFER_TIMEOUT_MS }) {
    let response;
    try {
        response = await globalThis.fetch(url, { method, headers, body, signal: AbortSignal.timeout(timeoutMs) });
    } catch (e) {
        throw new Error(`${method} ${url} failed: ${e.cause?.message ?? e.message}`);
    }
    if (response.ok || (allowNotFound && response.status === 404)) {
        return response;
    }
    const detail = await response.json().then((payload) => payload?.error, () => undefined);
    throw new Error(`${method} ${url} returned HTTP ${response.status}${detail ? `: ${detail}` : ""}`);
}

function readConfig() {
    const configNote = api.getNoteWithLabel("relocationConfig");
    if (!configNote) {
        throw new Error("No note carries the #relocationConfig label");
    }

    let parsed;
    try {
        parsed = JSON.parse(configNote.getContent());
    } catch (e) {
        throw new Error(`The #relocationConfig note (${configNote.noteId}) is not valid JSON: ${e.message}`);
    }

    const services = (Array.isArray(parsed?.services) ? parsed.services : [])
        .filter((entry) => entry?.id)
        .map((entry) => {
            const url = String(entry.url ?? "").trim();
            return {
                id: String(entry.id),
                name: entry.name ? String(entry.name) : String(entry.id),
                baseUrl: url.endsWith("/") ? url : `${url}/`,
                token: entry.token ? String(entry.token) : ""
            };
        });

    // Checked here rather than left to fetch, whose complaints name neither the service nor the field.
    for (const service of services) {
        if (!/^https?:\/\/[^/]+/.test(service.baseUrl)) {
            throw new Error(`The url of service "${service.name}" is missing or is not an http(s) address`);
        }
        if (!/^[\x21-\x7e]+$/.test(service.token)) {
            throw new Error(`The token of service "${service.name}" is missing or contains characters other than printable ASCII`);
        }
    }

    // A bad name is treated as none, so it cannot turn a relocation into a failure.
    const label = typeof parsed?.locationLabel === "string" ? parsed.locationLabel.trim() : "";

    return {
        services,
        defaultId: typeof parsed?.default === "string" ? parsed.default : "",
        locationLabel: ATTRIBUTE_NAME.test(label) ? label : null
    };
}

function defaultService(config) {
    return config.services.find((service) => service.id === config.defaultId) ?? config.services[0] ?? null;
}

/**
 * The service holding one note's content. A note relocated before services could be named carries
 * no target, and its content is wherever the default is.
 */
function serviceFor(note, config) {
    const target = note.getLabelValue("relocationTarget");
    if (!target) {
        return defaultService(config);
    }
    return config.services.find((service) => service.id === target) ?? null;
}

/** The checksum recorded when the content left, or null when nothing left. */
function readHash(note) {
    const hash = note.getLabelValue("relocationHash");
    if (hash) {
        return hash;
    }

    const packed = note.getLabelValue("relocationInfo");
    if (packed) {
        try {
            return JSON.parse(packed)?.sha256 ?? "";
        } catch {
            return "";
        }
    }

    if (note.getLabelValue("relocationId")) {
        return note.getLabelValue("relocationSha256") ?? "";
    }
    return null;
}

/**
 * Checks the returned bytes against the note's record and against what the service says it holds.
 * Bytes matching the service but not the record mean the record was edited, and the restore goes on;
 * bytes matching neither are a damaged transfer, and stop it.
 */
async function verify(note, bytes, recorded, storedSha256) {
    const actual = await sha256(bytes);
    if (storedSha256 && actual !== storedSha256) {
        throw new Error(`The content of ${note.noteId} arrived damaged: the service holds ${storedSha256} but ${actual} arrived. Nothing was changed.`);
    }

    // Compared over the recorded length: the note keeps a prefix, earlier versions kept all 64.
    if (!recorded || actual.startsWith(recorded)) {
        return;
    }
    if (storedSha256 && actual === storedSha256) {
        api.log(`Content relocation: ${note.noteId} carries a checksum of ${recorded} but the service holds ${actual}; the content is intact, so the record is being replaced.`);
        return;
    }
    throw new Error(`The content of ${note.noteId} does not match the checksum recorded when it left; nothing was changed`);
}

async function sha256(bytes) {
    const digest = await globalThis.crypto.subtle.digest("SHA-256", bytes);
    return Array.from(new Uint8Array(digest), (b) => b.toString(16).padStart(2, "0")).join("");
}

/** Same output as formatSize in trilium-core, so both implementations write the same label. */
function formatSize(size) {
    const kib = Math.max(Math.round(size / 1024), 1);
    if (kib < 1024) {
        return `${kib} KiB`;
    }
    return `${Math.round(kib / 102.4) / 10} MiB`;
}

module.exports = { move, createConfigNote, overview, summary };
