import { Button, defineWidget, FormDropdownList, NoteLink, RightPanelWidget, useActiveNoteContext, useEffect, useNoteLabel, useNoteProperty, useState } from "trilium:preact";

/*
 * Content relocation, frontend half: a card in the right pane that sends the active file note's
 * content to an external store and brings it back. All note and network work happens in the child
 * note titled `relocationBackend`, which the backend loads as a module under that name; the
 * functions passed to runOnBackend below run there, not here.
 */

/**
 * Reads the config note directly rather than through runOnBackend: that call waits for this window to
 * catch up with the server's newest entity change, and when that change is never pushed here (a
 * replaced blob, for one) a call that changes nothing waits until the page is reloaded. The backend
 * still validates everything when it moves content; this only checks enough to show what is missing.
 */
async function loadConfig() {
    try {
        let configNote = await api.searchForNote("#relocationConfig");
        if (!configNote) {
            // Creating it changes entities, so the wait above ends as soon as they arrive.
            const noteId = await api.runOnBackend(() => relocationBackend.createConfigNote().noteId, []);
            configNote = await api.getNote(noteId);
        }

        let parsed;
        try {
            parsed = JSON.parse((await configNote.getContent()) || "");
        } catch (e) {
            return { configNoteId: configNote.noteId, services: [], defaultId: null, error: `配置不是有效的 JSON：${e.message}` };
        }

        const services = (Array.isArray(parsed?.services) ? parsed.services : [])
            .filter((entry) => entry?.id)
            .map((entry) => ({ id: String(entry.id), name: entry.name ? String(entry.name) : String(entry.id), url: String(entry.url ?? "").trim(), token: String(entry.token ?? "") }));

        const missing = services.find((service) => !/^https?:\/\/[^/]+/.test(service.url) || !service.token);
        const fallback = services.find((service) => service.id === parsed?.default) ?? services[0];
        return {
            configNoteId: configNote.noteId,
            services: services.map(({ id, name }) => ({ id, name })),
            defaultId: fallback ? fallback.id : null,
            error: !services.length
                ? "配置里没有服务"
                : missing
                  ? `服务 ${missing.name} 的地址或令牌没有填写`
                  : undefined
        };
    } catch (e) {
        return { services: [], defaultId: null, error: e?.message ?? String(e) };
    }
}

export default defineWidget({
    parent: "right-pane",
    position: 50,
    render() {
        const { note } = useActiveNoteContext();
        const type = useNoteProperty(note, "type");
        const [hash] = useNoteLabel(note, "relocationHash");
        const [legacyInfo] = useNoteLabel(note, "relocationInfo");
        const [legacyId] = useNoteLabel(note, "relocationId");
        const [target] = useNoteLabel(note, "relocationTarget");
        const [size] = useNoteLabel(note, "relocationSize");
        const [busy, setBusy] = useState(false);
        const [log, setLog] = useState("");
        const [config, setConfig] = useState(null);
        const [targetId, setTargetId] = useState(null);

        // A message belongs to the note it was about.
        useEffect(() => setLog(""), [note?.noteId]);

        const isOutside = type === "webView" && !!(hash || legacyInfo || legacyId);
        const applies = !!note && (type === "file" || isOutside);
        const scriptingEnabled = api.isBackendScriptingEnabled();

        // Read again on every switch to a note this applies to, so an edited config takes effect
        // once the file note is reopened; other notes cost no request.
        useEffect(() => {
            if (scriptingEnabled && applies) {
                loadConfig().then((loaded) => {
                    setConfig(loaded);
                    setTargetId(loaded.defaultId);
                });
            }
        }, [scriptingEnabled, applies, note?.noteId]);

        if (!applies) {
            return null;
        }

        const nameOf = (id) => config?.services.find((service) => service.id === id)?.name ?? id;

        async function relocate(to) {
            setBusy(true);
            setLog("");
            try {
                const result = await api.runAsyncOnBackendWithManualTransactionHandling(
                    async (noteId, direction, serviceId) => await relocationBackend.move(noteId, direction, serviceId),
                    [note.noteId, to, to === "external" ? targetId : null]
                );
                const message = to === "external"
                    ? `已搬到 ${result.serviceName}（${result.size}）`
                    : `已从 ${result.serviceName} 取回（${result.size}）`;
                setLog(message);
                api.showMessage(message);
            } catch (e) {
                const message = e?.message ?? String(e);
                setLog(message);
                api.showError(message);
            } finally {
                setBusy(false);
            }
        }

        let body;
        if (!scriptingEnabled) {
            body = <p>后端脚本未启用，请在“设置 → 安全”里打开后重启。</p>;
        } else if (config?.error) {
            body = <p>配置有误：{config.error}</p>;
        } else if (!config) {
            body = <p>正在读取配置…</p>;
        } else if (isOutside) {
            body = (
                <>
                    <p>内容在外部存储：{nameOf(target || config?.defaultId)}{size ? `，${size}` : ""}</p>
                    <Button text={busy ? "正在取回…" : "取回数据库"} icon="bx bx-cloud-download" disabled={busy} onClick={() => relocate("local")} />
                </>
            );
        } else {
            // A choice appears only when there is one to make; the default is preselected.
            body = (
                <>
                    {config.services.length > 1 ? (
                        <p>
                            内容在数据库里，搬到：
                            <FormDropdownList values={config.services} keyProperty="id" titleProperty="name" currentValue={targetId} onChange={setTargetId} disabled={busy} />
                        </p>
                    ) : (
                        <p>内容在数据库里，将搬到：{nameOf(targetId)}</p>
                    )}
                    <Button text={busy ? "正在搬出…" : "搬到外部存储"} icon="bx bx-cloud-upload" disabled={busy || !targetId} onClick={() => relocate("external")} />
                </>
            );
        }

        return (
            <RightPanelWidget id="content-relocation-script" title="内容迁移">
                {body}
                {log && <pre style={{ whiteSpace: "pre-wrap", marginTop: "0.5em" }}>{log}</pre>}
                {config?.configNoteId && <p style={{ marginTop: "0.5em" }}>配置：<NoteLink notePath={config.configNoteId} /></p>}
            </RightPanelWidget>
        );
    }
});
