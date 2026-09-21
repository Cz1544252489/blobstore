# CLAUDE.md

本文件为 Claude Code 在本项目中工作时提供指导。用法、协议与配置见 `README.md`,这里只写容易出错的地方。

## 这是什么

Trilium 笔记内容的外部存放,2026-09-20 起分三处建成,2026-09-21 合并到这一个项目:存放服务(原
`GoLanguage/blobstore`)、命令行(原 `GoLanguage/relocate`)、Trilium 插件(原 `trilium-content-relocation`
仓库)。一个 Go module、一个程序,按子命令分角色。

## 凭据隔离(核心规矩)

- **服务模式绝不读取 ETAPI 令牌、Trilium 数据库或任何能改笔记的凭据。** 服务跑在不受信任的服务器上,
  它被攻破时不能连带整个笔记库。合并前这条写成「两者编在一个二进制里等于暴露凭据」,针对的是当初
  服务进程自己握着 ETAPI 令牌的做法;要防的是进程持有凭据,不是代码在同一个程序里。
- 结构上的保证:`main.go` 不带参数(或 `serve`)时只调用 `server.Run`;`internal/server` 不 import
  `internal/relocate` 与 `plugin`,`internal/relocate` 也不 import `internal/server`。改动后用
  `go list -deps ./internal/server | grep -E "relocate|plugin"` 确认为空。
- 服务端的环境变量都是 `BLOBSTORE_*`,命令行的都是 `RELOCATE_*`,两套不混用。
- `internal/install` 只在服务器上以 root 运行,只碰服务自己的文件(程序、单元、配置),不碰任何 Trilium 凭据。

## 安装(`internal/install`)

- **已有的 `/etc/blobstore/config` 一律不动**:升级与首次安装是同一条命令,令牌、地址、分组都在配置里,
  覆盖掉就等于换令牌,所有笔记的 `webViewSrc` 与插件配置都会失效。`-addr`、`-public-base` 只用于新生成的配置。
- **程序先写 `.new` 再改名**,不直接覆盖:服务在运行时直接写会报 text file busy。
- **用 `restart` 不用 `enable --now`**:升级时服务已在运行,`--now` 不会换上新程序。
- **单元文件是 `deploy/blobstore.service`,经 `go:embed` 编进程序**;改单元要重新编译,已装的服务器下次
  `install` 时才更新。

## 服务端(`internal/server`)

- **`StoreBlob`/`ReadBlob`/`ListBlobs`/`DeleteBlob`(`store.go`)是存放逻辑的唯一入口**,`handlers.go`
  只管鉴权、路由与错误码,不直接碰文件或 `index.json`。
- **分组由令牌决定,不由路径决定**:`checkAuth` 把令牌与每个分组逐一比较(全部比完,不提前返回),返回
  带着自己 `Store` 的 `tenant`。每个分组一个 `Store`、一份 `index.json`、一把锁。`/files/` 没有令牌,
  靠路径段数区分:一段是默认分组,两段是 `<分组>/<id>`;分组名不存在与 id 不存在、`k` 不对一样回 404。
  **默认分组的目录与地址必须保持不变**,正式库里已迁出笔记的 `webViewSrc` 指向它们。
- **写入顺序是「先写临时文件并重新计算 sha256,再归档旧版本,最后改名到位」**:一次失败的写入不会动到
  现有的当前版本,失败时只可能残留 `blobs/<id>.tmp-<随机后缀>`,且 `StoreBlob` 出错前会自己删掉。
- **`DeleteBlob` 先提交索引、再删文件**,顺序不能反:最坏情况是留一个不再被索引到的孤儿文件,而不是索引
  指着不存在的文件、下次 `GET` 报 500。
- **索引分 `blobs`(当前)与 `history`(历史)**,`history` 只增不删。
- **`GET /blobs/<id>` 与 `GET /files/<id>` 共用读取代码,差别在鉴权**:别把 `/files` 裹进 `checkAuth`
  (网页视图会打不开),也别去掉 `k` 的比对(知道 id 就能取走内容)。
- **访问串每次存入新内容时重新生成**,覆盖内容会让指向旧版本的地址失效;字节完全相同的重复提交(按 sha256
  判等)返回旧记录,串、`storedAt` 都不变,不产生历史。
- **id 只做三条校验**(非空、不含路径分隔符、`filepath.Base(id) == id` 且不是 `.`/`..`),挡得住路径穿越,
  挡不住 Windows 文件名不允许的字符;调用方是笔记 id,不会撞上。
- **`index.json` 每次全量读改写**;`ReadBlob`、`ListBlobs` 不加锁,只读一次索引加一次文件。
- **`X-Relocation-Filename` 用 `url.PathUnescape` 解码**,不用 `QueryUnescape`(后者把 `+` 当空格)。
- **自检一律用一个新的空目录当 `BLOBSTORE_ROOT`**。

## 命令行(`internal/relocate`)

- **配置只读插件的配置笔记**(经 ETAPI 搜 `#relocationConfig`),不再读 fork 的 `contentRelocationServices`
  选项。服务选择:`-service` 按名字或 id;不给时用 `default`,再不行用第一个。
- **`list` 不要令牌,其余命令都要**:`Run` 里除 `list` 外一律先 `loadToken`,新加的命令自动带上。
- **文件名编码必须与服务端的解码配对**:`blob.go` 的 `encodeURIComponent` 照 JavaScript 的保留字符集手写,
  别换成标准库的 `url.QueryEscape`(空格编成 `+`)或 `url.PathEscape`(不转义 `!~*'()`)。
- **数据库连接始终走 `mode=ro` 的 sqlite3 URI**,在 Python 里用 `pathlib.Path(db).resolve().as_uri()` 构造,
  不要手拼 Windows 路径。
- **令牌不打印**:`Service.Token` 只在 `blob.go` 里拼进请求头,输出只用名字、地址与 id。
- **`move` 从第 5 步、`back` 从第 4 步起不可撤回**,失败时只报停在第几步,不自动改回。
- **搜索结果里含受保护的笔记时整条搜索返回 401**,查询要带 `note.isProtected = false`。
- **`Note.Attributes` 依赖 ETAPI 在响应里内嵌属性列表**;升级 Trilium 后先用一篇测试笔记跑一次 `status`。
- **`-min`/`-max` 以 1048576 字节为一 MB**,与 `relocationSize` 的 KiB/MiB 是两套换算。
- **位置标签是改值不是删除**:表格列可能绑着它。`#ToLocal` 只在笔记本来就有时跟着改。
- **`relocationSize` 的写法与 Trilium 一致**,整数倍的 MiB 不带 `.0`(3155440 字节 → `3 MiB`)。

## 插件(`plugin/`)

- **后端笔记的标题必须是 `relocationBackend`**:Trilium 把子笔记按标题作为变量传给父笔记的脚本。
  改名要同时改 `plugin.go` 与小部件。
- **传给 `runOnBackend` 的函数会被转成字符串送到后端执行**,只能用参数、`api` 与 `relocationBackend`;
  函数体里不写可选链与空值合并,sucrase 会把它们改写成只在前端存在的辅助函数。
- **不改动实体的调用不走 `runOnBackend`**:它返回前要等本窗口追上服务器最新的实体变更编号,那条变更若
  永远不推送过来,调用就一直等到刷新页面。所以前端直接读配置笔记,只有建配置笔记与搬运经后端。
- 后端里的全局对象经 `globalThis` 取(`fetch`、`crypto`、`TextEncoder`):脚本在 Trilium 的模块里求值,
  模块自己的导入可能同名。
- 写笔记的操作放在 `api.transactional` 里,并在其中复查笔记状态;外部副本以笔记 id 命名,撤销上传前必须
  确认笔记仍是 `file`。
- 标签名、协议与操作次序,插件与命令行保持一致,改一处核对另一处。有意的差别:插件取回时比对三方校验值,
  命令行只比对笔记记录的那一个。

## 检查

```
go vet ./...
gofmt -l .
go build -trimpath -o blobstore.exe .
go list -deps ./internal/server | grep -E "relocate|plugin"    # 应当为空
```

- 服务端没有单元测试:起一个实例,新的临时目录当 `BLOBSTORE_ROOT`,设两个分组,用 curl 走一遍:带与不带
  令牌的 POST、GET、DELETE、列表,重复提交相同与不同内容,分组之间互相读删,`/files/` 的路径穿越
  (`..%2f..%2findex.json`)。
- 命令行:`list`、`status`、带 `-dry-run` 的 `move` 是只读的;改过 `move`/`back` 之后,先拿一篇小笔记走一遍往返。
- 插件:改完 JS 用 Trilium 源码里的 sucrase 转换一遍小部件、用 `new Function` 解析后端脚本,再
  `blobstore plugin` 生成 zip,在测试实例里导入走一遍往返。
- 注释一律英文;README 与界面文字用中文,按 `~/.claude/TERMINOLOGY.md`。

## 版本与提交

- 版本号只在 `main.go` 的 `version` 里,插件 zip 带同一个版本号。功能有变化时推进,推进到哪一版由用户定;
  只改文档或注释不推进。
- 未经用户明确要求不提交、不推送;Gitea 直连,不设代理。
