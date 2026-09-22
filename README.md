# blobstore

把 Trilium 笔记的文件内容搬到外部存放、库里只留一个占位，需要时再取回。数据库和它的每一份备份
因此不必反复携带大文件。

一个程序，三个角色：

| 角色 | 怎么用 | 在哪运行 |
|---|---|---|
| 存放服务 | `blobstore`(不带参数,systemd 这样启动);`blobstore install` 装成服务 | 服务器 |
| 命令行 | `blobstore list/move/back/status/label` | 能连到 Trilium 的本机 |
| 插件打包 | `blobstore plugin [-o <目录>]` | 任意机器 |

**服务端不持有任何 Trilium 凭据**:它只收字节、存起来、给链接、按需取回或删除,不认识笔记、
标签、库这些概念,不读 ETAPI 令牌,也不读 Trilium 的数据库。改笔记的是插件(在 Trilium 里)
与命令行(在本机),令牌只在它们那一侧。所以服务端的接口即便被人摸到,也改不了任何一条笔记。

```
main.go              分发子命令;不带参数时只调用 server.Run
internal/server/     存放服务:协议、分组、存储
internal/relocate/   命令行:经 ETAPI 批量搬运
plugin/              Trilium 插件的两份脚本,编进程序,由 plugin 子命令打成导入 zip
internal/install/    install / uninstall:装成 systemd 服务
deploy/              systemd 单元(编进程序)、加密备份脚本与单元
```

## 协议

| 方法 | 路径 | 鉴权 | 作用 |
|---|---|---|---|
| POST | `/blobs` | 需要 | 存入一份文件,返回标识与可直接打开的地址 |
| GET | `/status` | 需要 | 本组的文件数与大小、硬盘剩余、最近一次备份的结果 |
| GET | `/blobs` | 需要 | 列出本组全部当前文件与合计 |
| GET | `/blobs/<id>` | 需要 | 按标识取回文件字节 |
| DELETE | `/blobs/<id>` | 需要 | 按标识删除文件的当前版本 |
| GET | `/files/[<分组>/]<id>?k=<访问串>` | **不需要** | 给浏览器直接打开用 |

`POST /blobs`:

- 请求头 `Authorization: Bearer <token>`。
- 请求头 `X-Relocation-Note-Id`:调用方指定的标识,直接用作返回的 `id` 与磁盘上的文件名(就是笔记 id)。
- 请求头 `X-Relocation-Filename`(可选):原文件名,百分号编码(相当于 JavaScript 的
  `encodeURIComponent`),只用于索引里的展示。HTTP 头只能带 Latin-1,中文文件名不编码就发不出去。
- 请求头 `Content-Type`:原 mime,原样记下,之后的 GET 原样返回;不带则记为 `application/octet-stream`。
- 请求体:文件字节。
- 返回 200:`{"id", "url", "sha256", "size"}`。已存在同一 `id` 时,旧文件归档进 `blobs-old/`、
  索引追加一条历史;字节完全相同则不产生新版本,返回原有记录。写入后重新计算硬盘上文件的
  sha256,对不上就报错,不留下半份文件、不动现有的当前版本。

`GET /blobs/<id>`:200 返回字节流,`Content-Type` 是存入时记下的 mime,响应头 `X-Blob-Sha256`
是服务记录的校验值;404 返回 `{"error": "..."}`。

`GET /blobs`:返回本组全部当前记录,按存入时间从新到旧排:

```json
{"count": 2, "totalSize": 8,
 "blobs": [{"id": "…", "filename": "…", "mime": "…", "size": 4, "sha256": "…",
            "storedAt": "<RFC 3339>", "versions": 1, "url": "<与 POST 返回的相同>"}]}
```

`versions` 是历史版本数(不含当前这一份),已删除的 id 不列出。

`GET /status`:

```json
{"version": "v0.5.1", "startedAt": "<RFC 3339>", "now": "<RFC 3339>",
 "group": {"name": "", "count": 133, "totalSize": 1066165395, "versions": 0,
           "oldFiles": 0, "oldSize": 0, "lastStoredAt": "<RFC 3339>"},
 "disk": {"total": 10462990336, "used": 4953022464, "free": 4956450816},
 "backup": {"started": "…", "finished": "…", "exitCode": 0, "dest": "blobcrypt:hd",
            "local": {"count": 135, "bytes": 1066215380},
            "remote": {"count": 137, "bytes": 1093117145, "sizeless": 0}}}
```

`group` 只统计令牌所属的分组;`versions` 是索引里的历史条数,`oldFiles`/`oldSize` 是 `blobs-old/` 里实际的文件。
`disk` 是存放目录所在文件系统的总量、已用量与普通进程还能写入的量,与 `df` 的三列相同,只在 Linux 上给出;
三者之差是留给 root 的块(ext4 默认 5%)。页面上的使用率照 `df` 算:`used / (used + free)`,向上取整。`backup` 原样转发备份脚本
写下的 `<root>/backup-status.json`(见「备份到云盘」),没有这个文件时是 `null`,读不了时是 `{"error": "…"}`;
它覆盖整个存放目录,所以每个分组看到的是同一份。

`DELETE /blobs/<id>`:204;不存在时 404。只删当前版本与索引里的当前记录,历史文件与历史记录保留。

**鉴权**:`/status` 与四个 `/blobs` 接口都要 `Authorization: Bearer <token>`,与每个分组的令牌逐一用
`crypto/subtle.ConstantTimeCompare` 比较。跨源调用还要来源在 `BLOBSTORE_ORIGINS` 里。

**`/files/` 的访问串**:网页视图用 iframe 加载这个地址,加不上请求头,所以它不校验令牌,改为在
url 里带 `?k=<访问串>`。访问串每存入一次新内容重新生成,只出现在 POST 与列表的返回值里;光知道
id(笔记 id 不难猜)取不到内容。串不对与 id 不存在一样回 404,不能拿来试探哪些 id 存在。
访问串会出现在代理的 access log 里,这个路径的日志建议关掉。

### 分组

一台服务可以给几个互不相干的调用方用(如正式库与测试库),每个调用方一个分组。**令牌决定请求
作用于哪个分组**,四个 `/blobs` 接口都只看得到、改得到本组的文件,同一个 id 在不同分组里是不同的文件。

- `BLOBSTORE_KEY` 与 `BLOBSTORE_ROOT` 是默认分组:文件在 `<root>/blobs/`,地址 `/files/<id>`。
- `BLOBSTORE_TENANTS=名字:令牌,名字:令牌` 另设分组:文件在 `<root>/tenants/<名字>/`,地址
  `/files/<名字>/<id>`。名字只用小写字母、数字、`-`、`_`(至多 32 个字符);令牌至少 16 个字符,
  不得与默认令牌或别的分组相同。任何一项不合法,服务拒绝启动并写明原因。
- 加一个分组:在 `/etc/blobstore/config` 的 `BLOBSTORE_TENANTS` 里追加一项,`systemctl restart blobstore`。
  令牌可以用 `head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'` 生成。启动日志只列分组名,不打印令牌。

## 服务端

### 存储布局

```
<root>/blobs/<id>                     默认分组当前这一份
<root>/blobs-old/<id>-<UTC 时间戳>     被覆盖或删除下来的旧版本
<root>/index.json                     索引
<root>/tenants/<名字>/                 其他分组,内部结构同上
<root>/backup-status.json             备份脚本写的最近一次结果(服务只读)
```

`index.json` 有两个顶层字段:`blobs` 是每个 id 的当前记录(mime、原文件名、大小、sha256、存入时间、
访问串),`DELETE` 删掉其中一条;`history` 是每个 id 的全部历史,只增不删。索引每次都先写临时文件
再改名到位,不会出现写到一半的索引。用文件加 JSON 而不是 SQLite 是有意的:服务只按 id 存取,
不需要按字段查询。`blobs-old/` 没有清理机制,会一直累积。

### 装在服务器上

交叉编译出 Linux 版本(见「构建」),传到服务器上,以 root 执行 `install`:

```bash
scp deploy/blobstore 服务器:/tmp/
ssh 服务器 'chmod +x /tmp/blobstore && /tmp/blobstore install -public-base https://example.com/blobstore && rm /tmp/blobstore'
```

`install` 把正在运行的这个程序复制到 `/usr/local/bin/blobstore`(先写临时文件再改名,服务在运行也能替换)、
写入编进程序的 systemd 单元、`/etc/blobstore/config` 不存在时生成一份(随机令牌,权限 0600),然后
`daemon-reload`、`enable`、`restart`,最后打印要填进插件配置的地址与令牌。**已有配置保留不动**,所以升级
用同一条命令,不换令牌;`-addr`(默认 `127.0.0.1:37862`)与 `-public-base`(默认 `http://<addr>`)只用于
新生成的配置。`blobstore uninstall` 停止并删掉服务与程序,保留数据与配置。从 Windows 用 scp 传过去的文件
没有执行权限,所以要先 `chmod +x`。

单元用 `DynamicUser=yes` 与 `StateDirectory=blobstore`:服务跑在临时 uid 下,只能写
`/var/lib/blobstore`,另有一整套沙箱。

挂在反向代理后面时监听回环(默认就是)、TLS 交给代理,`-public-base` 写对外的地址,含代理转发的路径前缀。
nginx 那侧有四项不配会出问题:

```nginx
location /blobstore/ {
    proxy_pass http://127.0.0.1:37862/;
    client_max_body_size 2000m;      # 默认 1M 会把大文件挡掉
    proxy_read_timeout 600s;         # 大文件传输超过默认的 60 秒
    proxy_send_timeout 600s;
    proxy_request_buffering off;     # 默认先把整个请求体存盘再转发
}
```

如果 443 被 `stream` 块按 SNI 分流,`http` 块里写 `listen 443` 不起作用,要在分流表里加一条、
让这个 server 监听内部端口。

### 备份到云盘

`deploy/` 下另有每天一次的加密备份:`blobstore-backup`(脚本放进 `/usr/local/bin/`)、
`blobstore-backup.service` 与 `.timer`(放进 `/etc/systemd/system/`),再写一份 `/etc/blobstore/backup`:

```
BLOBSTORE_BACKUP_DEST=blobcrypt:<机器名>
```

然后 `systemctl daemon-reload && systemctl enable --now blobstore-backup.timer`。脚本没有读到
`BLOBSTORE_BACKUP_DEST` 就拒绝运行,不猜目标。

它把整个存放目录(含各分组)经 rclone 的 crypt 远端 `copy` 到云盘,云盘上只有密文。要先配好两个远端:
一个指向云盘(建议用 Google Drive 的 `drive.file` 这类受限凭据),一个 `type = crypt` 包住前者。
**crypt 的口令原文要另外存在取不到这台机器也能打开的地方**,恢复全靠它。用 `copy` 不用 `sync`:
这边误删的文件不会连带从备份里删掉。

**几台机器备份到同一个 crypt 远端时,目标路径必须各不相同**(按机器名分):口令相同时,同名文件加密后的
名字也相同,写到同一路径下,各台的 `index.json` 会互相覆盖。

每次运行(成功与否)结束后,脚本在存放目录顶层写 `backup-status.json`:开始与结束时间、rclone 的退出码、
目标、本地的文件数与字节数、`rclone size` 统计的目标上的文件数与字节数。这个文件本身不参与复制,
由 `GET /status` 转发。目标上的数目可能多于本地:本地删掉的文件在备份里保留。

各台错开运行时间时,用 drop-in 覆盖定时器,不改单元文件本身:

```
# /etc/systemd/system/blobstore-backup.timer.d/time.conf
[Timer]
OnCalendar=
OnCalendar=*-*-* 05:00:00
```

### 环境变量

| 变量 | 作用 | 默认值 |
|---|---|---|
| `BLOBSTORE_ADDR` | 监听地址 | `127.0.0.1:37862` |
| `BLOBSTORE_KEY` | 默认分组的令牌 | 不设则随机生成并在启动时打印 |
| `BLOBSTORE_ROOT` | 存放目录(**必需**) | 不设则报错退出 |
| `BLOBSTORE_TENANTS` | 其他分组,`名字:令牌` 逗号分隔 | 不设则只有默认分组 |
| `BLOBSTORE_ORIGINS` | 允许的浏览器来源,逗号分隔 | `http://localhost:8080,http://127.0.0.1:8080` |
| `BLOBSTORE_PUBLIC_BASE` | 写进 `url` 的地址前缀 | `http://<BLOBSTORE_ADDR>` |

## 插件

一个 Trilium 插件(前端小部件加后端脚本),在 Trilium 界面里一篇一篇地搬出与取回。按 Trilium v0.105.0
编写,依赖 Preact 小部件、新布局的右侧面板、后端脚本里的 `fetch` 与 `crypto.subtle`。

### 安装

1. `blobstore plugin` 在当前目录生成 `blobstore-plugin-<版本>.zip`(`-o` 指定别的目录)。
2. 在 Trilium 的笔记树里右键任意一篇笔记,选「导入」,选这个 zip,**取消勾选「安全导入」**。
   勾着时 `#widget` 会被改名为 `#disabled:widget`,小部件不会装载;改回 `#widget` 即可。
3. 「设置 → 安全」里打开后端脚本(`config.ini` 里 `[Security]` 下的 `backendScriptingEnabled`)。
   打开后,库里所有后端脚本都会被允许运行。
4. 刷新页面。打开一篇文件笔记,右侧面板的「组件」页里出现「内容迁移」卡片。第一次出现时,插件在
   小部件笔记旁边建一篇「内容迁移配置」,卡片底部有它的链接。

导入后得到两棵子树:

```
内容迁移                (JSX,带 #widget)
└─ relocationBackend    (JS backend)
内容迁移总览            (渲染笔记,#relocationOverview,~renderNote 指向下一篇)
└─ 内容迁移总览页面     (JSX)
   └─ relocationBackend (上面那篇的克隆)
```

**`relocationBackend` 的标题就是脚本调用它时用的名字,不能改。** 后端调用只在发起调用的脚本笔记的子笔记里
找模块,所以两段 JSX 下面都要有它;总览页下面那篇是克隆,代码只有一份。更新插件:删掉「内容迁移」与
「内容迁移总览」两棵子树,导入新的 zip;配置笔记不受影响。

### 配置

一篇带 `#relocationConfig` 标签的 JSON 代码笔记,插件与命令行共用:

```json
{
  "default": "tm6pyVPgMQqPk7QH",
  "locationLabel": "IsLocal",
  "services": [
    { "id": "tm6pyVPgMQqPk7QH", "name": "hd", "url": "https://example.com/blobstore/", "token": "…" }
  ]
}
```

- `services`:可以配多个;两个以上时卡片给出下拉框选择目标。`id` 写进每篇迁出笔记的
  `relocationTarget`,取回时按它找服务,**已有笔记用过的 `id` 不能改**。
- `default`:默认选中的服务;不写或写错时用第一个。
- `locationLabel`:搬出时把这个标签写成 `false`,取回时写成 `true`;留空则不写。
- `url`:`http://` 或 `https://` 开头;`token`:只允许可打印的 ASCII 字符,用这个库对应分组的令牌。
  令牌是笔记内容,会随同步与导出一起带走;可以把配置笔记设为受保护,代价是使用前要先进入受保护会话。

### 界面

**总览页**:打开「内容迁移总览」。每个服务一张卡片:文件数与大小、旧版本、最近存入时间、库里指向它的笔记数、
硬盘使用率(与 `df` 的 Use% 相同,达到 85% 标红)、最近一次备份(失败或超过 36 小时标红)与本地/云端的文件数、服务的版本与启动时间。
下面列出对不上的记录:服务上有、库里没有这篇笔记;笔记已取回、外面还留着副本;笔记记在另一个服务上;
校验值与笔记记录不符;笔记记着在这里、服务上却没有这个文件;笔记记录的服务不在配置里。最后是各服务的文件清单,
每行可点到对应的笔记。数据经后端取得(令牌不进前端),每个服务的请求最多等 20 秒;页面一直停在加载中,
多半是 `runOnBackend` 在等本窗口追上服务器的实体变更,刷新整个页面即可。

**卡片**在每篇笔记的右侧面板里都有。上半部分只在两种笔记上出现:`file` 笔记给「搬到外部存储」;带迁移标签的
`webView` 笔记显示存放的服务与大小,给「取回数据库」。下半部分是速览:顶上是外部文件的总数与总大小,下面每个服务一张小卡片,
带状态圆点(绿:正常;黄:备份超过 36 小时或硬盘使用率达到 85%;红:读取失败、备份失败或有对不上的记录)、文件数与大小、
硬盘使用率进度条(与 `df` 的 Use% 相同)、最近一次备份、对不上的项数;整块至少占面板三分之一的高度,再下面是到总览页与配置笔记的链接(按 `#relocationOverview`、
`#relocationConfig` 找)。速览在同一个窗口里缓存 10 分钟,切换笔记不重复请求;搬出、取回之后与点「刷新」时重读。结果(如「已搬到 hd(4.2 MiB)」)与出错原因显示在卡片里,同时弹出提示;
请求失败时写明方法与地址。只记录不报错的情况(撤销上传失败、删除外部副本失败)写在 Trilium 数据
目录 `log/` 下当天的日志里。

## 命令行

批量处理用。经 ETAPI 读写笔记,读 PDF 体积清单时以只读方式直接打开 Trilium 的 `document.db`
(靠系统的 `python`,标准库自带 sqlite3)。

| 命令 | 作用 |
|---|---|
| `blobstore list [-min <MB>] [-max <MB>] [-n <篇数>]` | 按体积降序列出还在库里的 PDF 笔记(只读数据库,不要令牌) |
| `blobstore move [-min <MB>] [-n <篇数>] [-service <名字>] [-dry-run] [<noteId>...]` | 搬出;不给 `-service` 用配置的 `default` |
| `blobstore back <noteId>...` | 取回 |
| `blobstore status [-verify]` | 列出已迁移的笔记;`-verify` 逐篇核对服务上的字节 |
| `blobstore label [-dry-run]` | 按笔记现在的位置批量写位置标签 |

服务读配置笔记(见上一节),`back` 与 `status -verify` 按笔记的 `relocationTarget` 找服务。令牌只用于
请求头,不打印。`move` 目前只处理 PDF 文件笔记;受保护的笔记经 ETAPI 读不到内容,直接跳过。

**move 的步骤**:1 确认是未受保护的 PDF 文件笔记;2 取字节算 sha256;3 上传;4 核对返回的 id 与
sha256,不一致就删掉上传并报错,笔记不动;5 类型改为 `webView`(mime 保持);6 内容写一个空格;
7 写四条迁移标签;8 写位置标签,笔记本来就有 `#ToLocal` 时一并改成 `false`。第 5 步起任何一步失败
都不自动撤回,打印停在第几步。

**back 的步骤**:1 确认是带 `relocationTarget` 的 `webView` 笔记;2 取字节与 `Content-Type`;3 核对
sha256 前 16 位与 `relocationHash`,不一致中止;4 类型改回 `file`,mime 用笔记保留的,为空时用
`Content-Type`;5 写回内容并回读核对;6 删四条迁移标签,写位置标签;7 删除服务上的副本,失败只提示。

| 变量 | 作用 | 默认值 |
|---|---|---|
| `RELOCATE_ETAPI` | Trilium 的 ETAPI 地址 | `http://127.0.0.1:37840/etapi` |
| `RELOCATE_ETAPI_TOKEN` | ETAPI 令牌 | 从 `~/.claude.json` 里 trilium MCP 的 `Authorization` 读取 |
| `RELOCATE_DB` | Trilium 主库路径(`list` 用) | `D:\TriliumNotes-data\trilium-data\document.db` |
| `RELOCATE_PYTHON` | 读数据库用的 Python 命令 | `python` |

这些变量都以 `RELOCATE_` 开头,与服务端的 `BLOBSTORE_*` 分开:服务端从不读它们。

## 笔记上的记录

插件与命令行记的是同一套账,交替使用不冲突。搬出后笔记变成 `webView` 类型,内容只剩一个空格,
mime 保持原样(Trilium 不看 `webView` 笔记的 mime),并写上:

| 标签 | 内容 |
|---|---|
| `webViewSrc` | 服务返回的地址,网页视图从这里加载内容 |
| `relocationHash` | 内容 sha256 的前 16 个十六进制字符 |
| `relocationTarget` | 服务的 `id` |
| `relocationSize` | 大小,写法与 Trilium 界面相同:`n = max(round(字节数/1024), 1)`,`n < 1024` 写 `<n> KiB`,否则写 `<round(n/102.4)/10> MiB` |

取回时恢复为 `file` 类型、删掉这四个标签;早期的 fork 版本搬出时清空了 mime,这种笔记取回时用服务
返回的 `Content-Type`。

**操作次序**的原则是宁可外面多留一份用不着的副本,也不接受笔记内容丢失或被悄悄改掉:搬出先上传再改
笔记,取回先校验再写回、最后才删外部副本。插件还在写笔记的同一个事务里再确认一次笔记的状态,不符就
放弃;搬出放弃时只有笔记仍是 `file` 才删掉刚上传的那份(外部副本以笔记 id 命名,否则可能删掉另一次
成功搬出的副本)。插件取回时比对三方:收到的字节、笔记记录的校验值、服务记录的校验值,字节与服务一致
而与笔记不一致时视为笔记记录被改过、内容完好,照常取回。

## 构建

```bash
go vet ./...
gofmt -l .
go build -trimpath -o blobstore.exe .                                          # 本机
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o deploy/blobstore .  # 服务器
./blobstore.exe -version
```

版本号只在 `main.go` 的 `version` 变量里,插件 zip 的文件名取自它。
