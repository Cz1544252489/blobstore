import { Button, LoadingSpinner, NoteLink, useEffect, useState } from "trilium:preact";

/*
 * Content relocation overview: a render note's page that shows, for every service in the config
 * note, what the service holds, how much disk is left, how its last backup went, and where it
 * disagrees with the notes in this database. The figures come from relocationBackend.overview(),
 * reached through a clone of that note placed under this one; tokens never reach this side.
 */

/** A wait longer than this most likely hangs on runOnBackend's catch-up with entity changes. */
const SLOW_AFTER_MS = 15000;

/** A backup older than this is flagged; the timers run once a day. */
const BACKUP_STALE_MS = 36 * 3600 * 1000;

const STATE_TEXT = {
    ok: "正常",
    mismatch: "校验值与笔记记录不符",
    stray: "笔记已取回，这份是多余的副本",
    elsewhere: "笔记记录在另一个服务上",
    orphan: "库里没有这篇笔记"
};

const styles = {
    page: { padding: "1em 1.5em", maxWidth: "1100px" },
    grid: { display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(300px, 1fr))", gap: "1em", margin: "1em 0" },
    card: { border: "1px solid var(--main-border-color)", borderRadius: "8px", padding: "0.8em 1em" },
    muted: { color: "var(--muted-text-color)" },
    warn: { color: "var(--dropdown-item-icon-destructive-color, #c0392b)" },
    table: { width: "100%", borderCollapse: "collapse", fontSize: "0.9em" },
    cell: { borderBottom: "1px solid var(--main-border-color)", padding: "0.25em 0.5em", textAlign: "left", verticalAlign: "top" },
    num: { borderBottom: "1px solid var(--main-border-color)", padding: "0.25em 0.5em", textAlign: "right", whiteSpace: "nowrap" }
};

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

/** "2026年9月22日 12:20". Not toLocaleString("zh-CN"), whose "2026/9/22" reads as a fraction at a glance. */
function formatTime(iso) {
    if (!iso) {
        return "—";
    }
    const date = new Date(iso);
    if (isNaN(date.getTime())) {
        return String(iso);
    }
    const pad = (n) => String(n).padStart(2, "0");
    return `${date.getFullYear()}年${date.getMonth() + 1}月${date.getDate()}日 ${pad(date.getHours())}:${pad(date.getMinutes())}`;
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

function Row({ label, children }) {
    return (
        <tr>
            <td style={{ ...styles.muted, paddingRight: "1em", whiteSpace: "nowrap", verticalAlign: "top" }}>{label}</td>
            <td>{children}</td>
        </tr>
    );
}

function BackupRows({ backup }) {
    if (!backup) {
        return <Row label="备份">未配置</Row>;
    }
    if (backup.error) {
        return (
            <Row label="备份">
                <span style={styles.warn}>状态文件读不了：{backup.error}</span>
            </Row>
        );
    }
    const finished = backup.finished ? new Date(backup.finished).getTime() : NaN;
    const stale = !isNaN(finished) && Date.now() - finished > BACKUP_STALE_MS;
    const failed = backup.exitCode !== 0;
    const local = backup.local || {};
    const remote = backup.remote || null;
    return (
        <>
            <Row label="备份">
                <span style={failed || stale ? styles.warn : undefined}>
                    {failed ? `失败（退出码 ${backup.exitCode}）` : "成功"}，{formatTime(backup.finished)}
                    {stale ? "，已超过 36 小时" : ""}
                </span>
            </Row>
            <Row label="备份位置">{backup.dest || "—"}</Row>
            <Row label="本地 / 云端">
                {local.count ?? "—"} 个文件 {formatBytes(local.bytes)}
                {" / "}
                {remote ? `${remote.count} 个文件 ${formatBytes(remote.bytes)}` : <span style={styles.warn}>未取到</span>}
            </Row>
        </>
    );
}

function ServiceCard({ service }) {
    const status = service.status;
    const group = status ? status.group : null;
    const disk = status ? status.disk : null;
    const usePercent = disk ? diskUsePercent(disk) : null;
    const problems = service.blobs.filter((blob) => blob.state !== "ok").length + service.missing.length;
    return (
        <div style={styles.card}>
            <h4 style={{ margin: "0 0 0.4em" }}>
                {service.name}
                {service.isDefault ? <span style={{ ...styles.muted, fontWeight: "normal" }}>（默认）</span> : null}
            </h4>
            <div style={{ ...styles.muted, marginBottom: "0.5em", wordBreak: "break-all" }}>{service.host}</div>
            {service.statusError ? <p style={styles.warn}>状态读取失败：{service.statusError}</p> : null}
            <table>
                <tbody>
                    {group ? (
                        <>
                            <Row label="文件">
                                {group.count} 个，{formatBytes(group.totalSize)}
                            </Row>
                            <Row label="旧版本">
                                {group.oldFiles} 个，{formatBytes(group.oldSize)}
                            </Row>
                            <Row label="最近存入">{formatTime(group.lastStoredAt)}</Row>
                        </>
                    ) : null}
                    <Row label="库里指向这里">{service.noteCount} 篇笔记</Row>
                    {disk ? (
                        <Row label="硬盘">
                            <span style={usePercent !== null && usePercent >= DISK_WARN_PERCENT ? styles.warn : undefined}>
                                已用 {usePercent !== null ? `${usePercent}%` : "—"}（已用 {formatBytes(disk.used ?? disk.total - disk.free)}，可用 {formatBytes(disk.free)}，共 {formatBytes(disk.total)}）
                            </span>
                        </Row>
                    ) : null}
                    {status ? <BackupRows backup={status.backup} /> : null}
                    {status ? (
                        <Row label="服务">
                            {status.version}，{formatTime(status.startedAt)} 起运行
                        </Row>
                    ) : null}
                    <Row label="对不上的">
                        <span style={problems || service.listError ? styles.warn : undefined}>{service.listError ? "列表读取失败" : `${problems} 项`}</span>
                    </Row>
                </tbody>
            </table>
        </div>
    );
}

function Problems({ services, unknownTarget }) {
    const rows = [];
    for (const service of services) {
        if (service.listError) {
            rows.push({ key: `${service.id}-list`, service: service.name, what: `文件列表读取失败：${service.listError}` });
        }
        for (const note of service.missing) {
            rows.push({
                key: `${service.id}-m-${note.noteId}`,
                service: service.name,
                noteId: note.noteId,
                what: `笔记记录内容在这里（${note.size || "大小未知"}），服务上没有这个文件`
            });
        }
        for (const blob of service.blobs) {
            if (blob.state !== "ok") {
                rows.push({
                    key: `${service.id}-b-${blob.id}`,
                    service: service.name,
                    noteId: blob.note ? blob.note.noteId : null,
                    id: blob.id,
                    what: `${STATE_TEXT[blob.state] || blob.state}（${blob.filename || blob.id}，${formatBytes(blob.size)}）`
                });
            }
        }
    }
    for (const note of unknownTarget) {
        rows.push({ key: `u-${note.noteId}`, service: note.target || "—", noteId: note.noteId, what: "笔记记录的服务不在配置里" });
    }

    if (!rows.length) {
        return <p>服务上的文件与库里的记录一一对应，没有对不上的。</p>;
    }
    return (
        <table style={styles.table}>
            <thead>
                <tr>
                    <th style={styles.cell}>服务</th>
                    <th style={styles.cell}>笔记</th>
                    <th style={styles.cell}>情况</th>
                </tr>
            </thead>
            <tbody>
                {rows.map((row) => (
                    <tr key={row.key}>
                        <td style={styles.cell}>{row.service}</td>
                        <td style={styles.cell}>{row.noteId ? <NoteLink notePath={row.noteId} /> : <code>{row.id || "—"}</code>}</td>
                        <td style={styles.cell}>{row.what}</td>
                    </tr>
                ))}
            </tbody>
        </table>
    );
}

function FileList({ service }) {
    if (!service.blobs.length) {
        return null;
    }
    const total = service.blobs.reduce((sum, blob) => sum + (blob.size || 0), 0);
    return (
        <details style={{ margin: "0.5em 0" }}>
            <summary>
                {service.name}：{service.blobs.length} 个文件，{formatBytes(total)}
            </summary>
            <table style={styles.table}>
                <thead>
                    <tr>
                        <th style={styles.cell}>笔记</th>
                        <th style={styles.num}>大小</th>
                        <th style={styles.cell}>存入时间</th>
                        <th style={styles.num}>旧版本</th>
                        <th style={styles.cell}>情况</th>
                    </tr>
                </thead>
                <tbody>
                    {service.blobs.map((blob) => (
                        <tr key={blob.id}>
                            <td style={styles.cell}>{blob.note ? <NoteLink notePath={blob.note.noteId} /> : <code>{blob.filename || blob.id}</code>}</td>
                            <td style={styles.num}>{formatBytes(blob.size)}</td>
                            <td style={styles.cell}>{formatTime(blob.storedAt)}</td>
                            <td style={styles.num}>{blob.versions}</td>
                            <td style={{ ...styles.cell, ...(blob.state === "ok" ? styles.muted : styles.warn) }}>{STATE_TEXT[blob.state] || blob.state}</td>
                        </tr>
                    ))}
                </tbody>
            </table>
        </details>
    );
}

export default function RelocationOverview() {
    const [data, setData] = useState(null);
    const [error, setError] = useState("");
    const [loading, setLoading] = useState(false);
    const [slow, setSlow] = useState(false);
    const [round, setRound] = useState(0);

    useEffect(() => {
        if (!api.isBackendScriptingEnabled()) {
            setError("后端脚本未启用，请在“设置 → 安全”里打开后重启。");
            return;
        }
        let current = true;
        setLoading(true);
        setSlow(false);
        setError("");
        const timer = setTimeout(() => current && setSlow(true), SLOW_AFTER_MS);
        api.runAsyncOnBackendWithManualTransactionHandling(async () => await relocationBackend.overview(), [])
            .then((result) => current && setData(result))
            .catch((e) => current && setError(e && e.message ? e.message : String(e)))
            .finally(() => {
                clearTimeout(timer);
                if (current) {
                    setLoading(false);
                }
            });
        return () => {
            current = false;
            clearTimeout(timer);
        };
    }, [round]);

    return (
        <div style={styles.page}>
            <div style={{ display: "flex", alignItems: "center", gap: "1em" }}>
                <h3 style={{ margin: 0 }}>内容迁移总览</h3>
                <Button text="刷新" icon="bx bx-refresh" disabled={loading} onClick={() => setRound(round + 1)} />
                {loading ? <LoadingSpinner /> : null}
                {data ? <span style={styles.muted}>更新于 {formatTime(data.generatedAt)}</span> : null}
            </div>
            {slow && loading ? (
                <p style={styles.muted}>等待时间偏长：每个服务的请求最多等 20 秒，超过之后多半是在等本窗口与服务器同步，刷新整个页面即可。</p>
            ) : null}
            {error ? <p style={styles.warn}>{error}</p> : null}
            {data ? (
                <>
                    <div style={styles.grid}>
                        {data.services.map((service) => (
                            <ServiceCard key={service.id} service={service} />
                        ))}
                    </div>
                    <h4>对不上的记录</h4>
                    <Problems services={data.services} unknownTarget={data.unknownTarget} />
                    <h4>文件清单</h4>
                    {data.services.map((service) => (
                        <FileList key={service.id} service={service} />
                    ))}
                </>
            ) : null}
        </div>
    );
}
