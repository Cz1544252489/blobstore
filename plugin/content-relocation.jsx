import { Button, defineWidget, FormDropdownList, NoteLink, RightPanelWidget, useActiveNoteContext, useEffect, useNoteLabel, useNoteProperty, useState } from "trilium:preact";

/*
 * Content relocation, frontend half: a card in the right pane that sends the active file note's
 * content to an external store and brings it back, and under every note shows a short summary of the
 * services with links to the overview page and the config note. All note and network work happens in
 * the child note titled `relocationBackend`, which the backend loads as a module under that name; the
 * functions passed to runOnBackend below run there, not here.
 */

/** The summary is shared by every note the card is shown under and read again after this long. */
const SUMMARY_TTL_MS = 10 * 60 * 1000;

/** A backup older than this is flagged; the timers run once a day. Same threshold as the overview page. */
const BACKUP_STALE_MS = 36 * 3600 * 1000;

/** Module state outlives the card's re-renders: one summary and one pair of links per window. */
let summaryCache = null;
let summaryPending = null;
let linksCache = null;

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
            linksCache = null;
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

/** The config note and the overview page, found by their labels; searches only, no backend call. */
async function loadLinks() {
    if (!linksCache) {
        const [config, overview] = await Promise.all([api.searchForNote("#relocationConfig"), api.searchForNote("#relocationOverview")]);
        linksCache = { configNoteId: config ? config.noteId : null, overviewNoteId: overview ? overview.noteId : null };
    }
    return linksCache;
}

/**
 * The services' figures, through the backend because the tokens stay there. A read that changes nothing
 * can still hang on runOnBackend's catch-up (see loadConfig); then only this section keeps waiting.
 */
function loadSummary(force) {
    if (!force && summaryCache && Date.now() - summaryCache.at < SUMMARY_TTL_MS) {
        return Promise.resolve(summaryCache.data);
    }
    // A forced read starts afresh even while another is pending: that one may be the call that hangs.
    if (!summaryPending || force) {
        const pending = api
            .runAsyncOnBackendWithManualTransactionHandling(async () => await relocationBackend.summary(), [])
            .then((data) => {
                summaryCache = { at: Date.now(), data };
                return data;
            })
            .finally(() => {
                if (summaryPending === pending) {
                    summaryPending = null;
                }
            });
        summaryPending = pending;
    }
    return summaryPending;
}

function formatBytes(bytes) {
    if (typeof bytes !== "number" || !isFinite(bytes)) {
        return "—";
    }
    const units = ["B", "KiB", "MiB", "GiB", "TiB"];
    let value = bytes;
    let unit = 0;
    while (value >= 1024 && unit < units.length - 1) {
        value /= 1024;
        unit++;
    }
    return `${unit === 0 ? value : value.toFixed(value < 10 ? 2 : 1)} ${units[unit]}`;
}

/** "9月22日 12:20", with the year in front when it is not this year. Never "9/22", which reads as a fraction. */
function formatShortTime(iso) {
    const date = new Date(iso);
    if (!iso || isNaN(date.getTime())) {
        return "—";
    }
    const pad = (n) => String(n).padStart(2, "0");
    const year = date.getFullYear() === new Date().getFullYear() ? "" : `${date.getFullYear()}年`;
    return `${year}${date.getMonth() + 1}月${date.getDate()}日 ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/**
 * Disk use the way df reports it: used / (used + free), rounded up, so it matches "Use%" on the machine.
 * A service older than v0.5.2 sends no used figure; total minus free stands in for it there.
 */
function diskUsePercent(disk) {
    const used = typeof disk.used === "number" ? disk.used : disk.total - disk.free;
    const base = used + disk.free;
    return base > 0 ? Math.ceil((used / base) * 100) : null;
}

/** Same threshold as the overview page and the card. */
const DISK_WARN_PERCENT = 85;

const COLORS = { ok: "#2e9d5b", warn: "#d08b00", bad: "#c0392b", idle: "var(--muted-text-color)" };
const warnStyle = { color: COLORS.bad };
const mutedStyle = { color: "var(--muted-text-color)" };

/** The right pane is narrow and tall, so the summary stacks one small card per service. */
const ui = {
    section: { display: "flex", flexDirection: "column", gap: "0.6em", minHeight: "33vh" },
    header: { display: "flex", alignItems: "center", gap: "0.5em" },
    totals: {
        display: "grid",
        gridTemplateColumns: "1fr 1fr",
        gap: "0.4em",
        padding: "0.6em 0.7em",
        borderRadius: "8px",
        background: "var(--accented-background-color)"
    },
    figure: { fontSize: "1.35em", fontWeight: 600, lineHeight: 1.2 },
    caption: { fontSize: "0.8em", color: "var(--muted-text-color)" },
    card: {
        border: "1px solid var(--main-border-color)",
        borderRadius: "8px",
        padding: "0.55em 0.7em",
        display: "flex",
        flexDirection: "column",
        gap: "0.35em"
    },
    badge: { fontSize: "0.75em", padding: "0 0.45em", borderRadius: "999px", background: "var(--more-accented-background-color)", color: "var(--muted-text-color)" },
    dot: (color) => ({ width: "0.6em", height: "0.6em", borderRadius: "50%", background: color, flex: "none" }),
    bar: { flex: 1, height: "0.45em", borderRadius: "999px", background: "var(--more-accented-background-color)", overflow: "hidden" },
    fill: (percent, color) => ({ width: `${Math.min(100, Math.max(0, percent))}%`, height: "100%", background: color }),
    line: { display: "flex", alignItems: "center", gap: "0.4em", fontSize: "0.9em" },
    link: { display: "flex", alignItems: "center", gap: "0.5em", padding: "0.45em 0.7em", borderRadius: "8px", background: "var(--accented-background-color)" }
};

/** What one service's card says, and the colour of its dot: red for failures, amber for things to watch. */
function assess(service) {
    if (service.error) {
        return { level: "bad", backup: null, percent: null };
    }
    const percent = service.disk ? diskUsePercent(service.disk) : null;
    const backup = service.backup;
    let view;
    if (!backup) {
        view = { icon: "bx bx-minus-circle", color: COLORS.idle, text: "未配置备份" };
    } else if (backup.error) {
        view = { icon: "bx bx-error-circle", color: COLORS.bad, text: "备份状态读不了" };
    } else if (backup.exitCode !== 0) {
        view = { icon: "bx bx-x-circle", color: COLORS.bad, text: `备份失败，${formatShortTime(backup.finished)}` };
    } else if (backup.finished && Date.now() - new Date(backup.finished).getTime() > BACKUP_STALE_MS) {
        view = { icon: "bx bx-time-five", color: COLORS.warn, text: `上次备份 ${formatShortTime(backup.finished)}，已超过 36 小时` };
    } else {
        view = { icon: "bx bx-check-circle", color: COLORS.ok, text: `已备份，${formatShortTime(backup.finished)}` };
    }
    const bad = view.color === COLORS.bad || service.problems > 0;
    const warn = view.color === COLORS.warn || (percent !== null && percent >= DISK_WARN_PERCENT);
    return { level: bad ? "bad" : warn ? "warn" : "ok", backup: view, percent };
}

function ServiceSummary({ service }) {
    const view = assess(service);
    const diskWarn = view.percent !== null && view.percent >= DISK_WARN_PERCENT;
    const diskColor = diskWarn ? COLORS.warn : COLORS.ok;
    const disk = service.disk;
    return (
        <div style={ui.card}>
            <div style={ui.header}>
                <span style={ui.dot(COLORS[view.level])} />
                <strong>{service.name}</strong>
                {service.isDefault ? <span style={ui.badge}>默认</span> : null}
                {!service.error ? (
                    <span style={{ marginLeft: "auto", ...mutedStyle, fontSize: "0.85em" }}>
                        {service.count ?? "—"} 个 · {formatBytes(service.totalSize)}
                    </span>
                ) : null}
            </div>
            {service.error ? (
                <div style={{ ...ui.line, color: COLORS.bad }}>
                    <span className="bx bx-wifi-off" />
                    读取失败
                </div>
            ) : (
                <>
                    {view.percent !== null ? (
                        <div style={ui.line} title={`已用 ${formatBytes(disk.used)}，可用 ${formatBytes(disk.free)}，共 ${formatBytes(disk.total)}`}>
                            <span className="bx bx-hdd" style={mutedStyle} />
                            <span style={mutedStyle}>硬盘</span>
                            <div style={ui.bar}>
                                <div style={ui.fill(view.percent, diskColor)} />
                            </div>
                            <span style={{ minWidth: "2.8em", textAlign: "right", color: diskWarn ? diskColor : undefined }}>{view.percent}%</span>
                        </div>
                    ) : null}
                    <div style={{ ...ui.line, color: view.backup.color === COLORS.ok ? undefined : view.backup.color }}>
                        <span className={view.backup.icon} style={{ color: view.backup.color }} />
                        {view.backup.text}
                    </div>
                    {service.problems ? (
                        <div style={{ ...ui.line, color: COLORS.bad }}>
                            <span className="bx bx-error" />
                            对不上 {service.problems} 项，详见总览
                        </div>
                    ) : null}
                </>
            )}
        </div>
    );
}

function Summary({ summary, error, loading, onRefresh, links }) {
    const services = summary ? summary.services : [];
    const reachable = services.filter((service) => !service.error);
    const files = reachable.reduce((sum, service) => sum + (service.count || 0), 0);
    const bytes = reachable.reduce((sum, service) => sum + (service.totalSize || 0), 0);
    return (
        <div style={ui.section}>
            <div style={ui.header}>
                <span className="bx bx-cloud" style={{ fontSize: "1.2em" }} />
                <strong>速览</strong>
                {summary ? <span style={{ ...mutedStyle, fontSize: "0.85em" }}>{formatShortTime(summary.generatedAt)}</span> : null}
                <a
                    href="#"
                    title="重新读取各服务的状态"
                    style={{ marginLeft: "auto", display: "flex", alignItems: "center", gap: "0.2em" }}
                    onClick={(e) => {
                        e.preventDefault();
                        onRefresh();
                    }}
                >
                    <span className={loading ? "bx bx-loader-alt bx-spin" : "bx bx-refresh"} />
                    {loading ? "读取中" : "刷新"}
                </a>
            </div>
            {error ? <p style={{ ...warnStyle, margin: 0 }}>{error}</p> : null}
            {summary ? (
                <>
                    <div style={ui.totals}>
                        <div>
                            <div style={ui.figure}>{files}</div>
                            <div style={ui.caption}>个文件在外部</div>
                        </div>
                        <div>
                            <div style={ui.figure}>{formatBytes(bytes)}</div>
                            <div style={ui.caption}>分布在 {services.length} 个服务</div>
                        </div>
                    </div>
                    {services.map((service) => (
                        <ServiceSummary key={service.id} service={service} />
                    ))}
                    {summary.unknownTarget ? (
                        <div style={{ ...ui.line, color: COLORS.bad }}>
                            <span className="bx bx-error" />
                            {summary.unknownTarget} 篇笔记记录的服务不在配置里
                        </div>
                    ) : null}
                </>
            ) : !error ? (
                <p style={{ ...mutedStyle, margin: 0 }}>正在读取各服务的状态…</p>
            ) : null}
            <div style={{ marginTop: "auto", display: "flex", flexDirection: "column", gap: "0.4em" }}>
                {links?.overviewNoteId ? (
                    <div style={ui.link}>
                        <span className="bx bx-bar-chart-alt-2" />
                        <span style={mutedStyle}>总览</span>
                        <NoteLink notePath={links.overviewNoteId} />
                    </div>
                ) : null}
                {links?.configNoteId ? (
                    <div style={ui.link}>
                        <span className="bx bx-cog" />
                        <span style={mutedStyle}>配置</span>
                        <NoteLink notePath={links.configNoteId} />
                    </div>
                ) : null}
            </div>
        </div>
    );
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
        const [links, setLinks] = useState(linksCache);
        const [summary, setSummary] = useState(summaryCache ? summaryCache.data : null);
        const [summaryError, setSummaryError] = useState("");
        const [summaryLoading, setSummaryLoading] = useState(false);

        // A message belongs to the note it was about.
        useEffect(() => setLog(""), [note?.noteId]);

        const isOutside = type === "webView" && !!(hash || legacyInfo || legacyId);
        const applies = !!note && (type === "file" || isOutside);
        const scriptingEnabled = api.isBackendScriptingEnabled();

        // Read again on every switch to a note this applies to, so an edited config takes effect
        // once the file note is reopened.
        useEffect(() => {
            if (scriptingEnabled && applies) {
                loadConfig().then((loaded) => {
                    setConfig(loaded);
                    setTargetId(loaded.defaultId);
                });
            }
        }, [scriptingEnabled, applies, note?.noteId]);

        function refreshSummary(force) {
            setSummaryLoading(true);
            setSummaryError("");
            loadSummary(force)
                .then((data) => setSummary(data))
                .catch((e) => setSummaryError(e?.message ?? String(e)))
                .finally(() => setSummaryLoading(false));
        }

        // Cached for SUMMARY_TTL_MS, so switching notes costs nothing until it is due again.
        useEffect(() => {
            loadLinks().then(setLinks, () => setLinks({ configNoteId: null, overviewNoteId: null }));
            if (scriptingEnabled) {
                refreshSummary(false);
            }
        }, [scriptingEnabled, note?.noteId]);

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
                // The counts just changed.
                refreshSummary(true);
            } catch (e) {
                const message = e?.message ?? String(e);
                setLog(message);
                api.showError(message);
            } finally {
                setBusy(false);
            }
        }

        let body = null;
        if (!scriptingEnabled) {
            body = <p>后端脚本未启用，请在“设置 → 安全”里打开后重启。</p>;
        } else if (!applies) {
            body = null;
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

        const separated = { marginTop: "0.8em", paddingTop: "0.5em", borderTop: "1px solid var(--main-border-color)" };
        return (
            <RightPanelWidget id="content-relocation-script" title="内容迁移">
                {body}
                {log && <pre style={{ whiteSpace: "pre-wrap", marginTop: "0.5em" }}>{log}</pre>}
                {scriptingEnabled ? (
                    <div style={body ? separated : undefined}>
                        <Summary summary={summary} error={summaryError} loading={summaryLoading} onRefresh={() => refreshSummary(true)} links={links} />
                    </div>
                ) : null}
            </RightPanelWidget>
        );
    }
});
