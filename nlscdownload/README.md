# NLSC MODELSET 下载与 3D Tiles 构建

本工具链用于归档台湾国土测绘中心（NLSC）3D 地图页面使用的
MODELSET `NODE` 响应，并将其中的几何、法线、纹理和层级关系转换为可由
CesiumJS 加载的 3D Tiles 1.1 数据集。

工具链分为三个独立阶段：

1. `nlsc-modelset-download`：递归下载原始 MODELSET 四叉树节点。
2. `nlsc-modelset-build`：解码节点并生成带内嵌纹理的 GLB 和 `tileset.json`。
3. `nlsc-modelset-preview`：通过 HTTP 启动 CesiumJS 本地预览。

下载和构建均支持中断后续跑。下载阶段在当前网络环境中可以通过 Chrome
DevTools Protocol（CDP）复用已打开的 NLSC 页面；构建和预览阶段不依赖
CDP，也不需要浏览器保持打开。

## 能力与边界

| 能力 | 状态 | 说明 |
|---|---|---|
| MODELSET 四叉树递归下载 | 支持 | 从指定 `level/row/col` 根节点遍历到 `max-level` |
| HTTP 204 空节点剪枝 | 支持 | 写入空节点标记，不再访问其后代 |
| 下载断点续传 | 支持 | 已完成的响应和空节点会跳过 |
| 并发与失败重试 | 支持 | 并发数、重试次数、超时均可配置 |
| CDP 下载传输 | 支持 | 使用已打开的 NLSC 页面请求资源 |
| MODELSET 解码 | 支持 | 生成 glTF 2.0 Binary（GLB）内容 |
| 原始纹理 | 支持 | 纹理直接嵌入 GLB，不生成外部 PNG 依赖 |
| 3D Tiles 层级 | 支持 | 保留可用的父子节点并生成 HLOD 树 |
| 构建断点续跑 | 支持 | `nodes/` 中的记录是完成标记 |
| 按经纬度 BBOX 限制下载 | 支持 | 仅向下遍历与范围相交的节点；边界节点完整保留，不裁切几何 |
| 下载结束自动触发构建 | 不支持 | 两个命令是独立阶段，需要依次执行或由外部脚本编排 |
| 官方 3D Tiles Validator | 未集成 | 当前构建器执行自身的结构和 GLB 完整性检查 |

## 环境要求

- Go 1.16 或更高版本。
- macOS 或 Linux；归档锁使用 `flock`。
- 足够的磁盘空间。构建后的 GLB 总量通常会大于压缩的原始响应。
- 下载阶段使用 CDP 时，需要 Chrome 或 Microsoft Edge 开启远程调试端口。
- Cesium 预览需要能够访问 CesiumJS CDN；卫星底图还需要访问 ArcGIS World Imagery。

所有命令均应从 `tiler` 源代码仓库根目录执行。

## 1. 准备 CDP 浏览器

以端口 `29222` 启动一个独立的 Edge 实例：

```bash
open -na "Microsoft Edge" --args \
  --remote-debugging-port=29222 \
  --user-data-dir="$HOME/.nlsc-cdp-profile"
```

Chrome 可使用相同参数。随后在该浏览器中打开任意
`https://3dmaps.nlsc.gov.tw/` 页面，并保持这个标签页存在。无需手工选择图层、
缩放地图或逐个点击模型。

检查 CDP 是否可用：

```bash
curl http://localhost:29222/json/version
```

`nlsc-modelset-download` 会自动查找 URL 以
`https://3dmaps.nlsc.gov.tw/` 开头的页面。若浏览器或该标签页被关闭，本次下载会
报错退出；重新打开页面后执行完全相同的命令即可续跑。

## 2. 下载原始 MODELSET 节点

### 单个子树

```bash
go run ./cmd/nlsc-modelset-download \
  -output "<data-root>/source-main" \
  -base-url "https://mapserver02.nlsc.gov.tw/oview//oview" \
  -layer 113_B \
  -root-level 3 \
  -root-row 1 \
  -root-col 0 \
  -max-level 11 \
  -west 120.530000 -south 24.311000 \
  -east 120.565000 -north 24.329000 \
  -concurrency 4 \
  -retries 3 \
  -cdp-url "http://localhost:29222"
```

`output` 必须是空目录，或是由同一图层、服务地址和兼容根节点创建的已有下载
目录。首次运行后，`download-config.json` 会绑定数据集身份，防止把不同区域或
图层混在一起。

### 下载参数

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `-output` | 无 | 原始归档目录，必填 |
| `-base-url` | `mapserver01` | MODELSET 服务端点 |
| `-layer` | `113_H` | NLSC MODELSET 图层名 |
| `-root-level` | `1` | 子树根节点层级 |
| `-root-row` | `0` | 子树根节点行号 |
| `-root-col` | `0` | 子树根节点列号 |
| `-max-level` | `9` | 下载的最深层级 |
| `-west` | 无 | 可选下载范围最小经度；四个范围参数必须同时提供 |
| `-south` | 无 | 可选下载范围最小纬度 |
| `-east` | 无 | 可选下载范围最大经度 |
| `-north` | 无 | 可选下载范围最大纬度 |
| `-concurrency` | `4` | 并发请求数 |
| `-retries` | `3` | 首次请求失败后的重试次数 |
| `-retry-delay` | `2s` | 重试等待时间 |
| `-timeout` | `90s` | 单次请求超时；CDP 调用也使用该值 |
| `-cdp-url` | 空 | Chrome/Edge 远程调试地址；空值表示直接 HTTP |
| `-progress-every` | `100` | 每处理多少节点输出一次进度 |

### 下载结果含义

结束时会输出类似统计：

```text
visited=4165 requested=4165 downloaded=3753 empty=412 skipped=0 failed=0 ...
```

- `downloaded`：本次成功写入的非空响应。
- `empty`：服务返回 HTTP 204 的空节点。
- `skipped`：磁盘中已有完成响应或空节点标记。
- `failed`：重试后仍失败的节点；应为 `0` 才视为本次运行成功。
- `bytes`：本次实际下载量，不是目录总大小。

按 `Ctrl+C` 可以安全终止。未完成的 `.part` 文件不会被当作完成结果；再次执行
原命令时只会重新请求未完成节点。服务端不支持真正的字节范围续传，因此单个
中断文件会从头下载，但已完成的其他节点不会重下。

经纬度范围只限制四叉树递归：下载器读取 NODE 的 ECEF 边界并仅进入相交节点。
边界瓦片仍完整保存，因此不会切断建筑；实际覆盖会略大于输入 BBOX。对已有 L9
归档使用相同目录、把 `-max-level` 提高并加入 BBOX，可只补目标范围的更深层节点。

## 3. 构建 3D Tiles

下载完成后，单独运行构建器：

```bash
go run ./cmd/nlsc-modelset-build \
  -input "<data-root>/source-main" \
  -output "<data-root>/tiles-main" \
  -min-level 3 \
  -max-level 11 \
  -workers 4 \
  -allow-missing-children
```

这一步完全离线，不使用 CDP。每个 worker 读取一个 `.response`，解码几何、法线
和纹理，写入一个或多个 GLB，最后提交对应的 `nodes/LEVEL/ROW/COL.json` 完成
标记。纹理嵌入 GLB，所以移动数据集时不需要另外复制纹理目录。

### 构建参数

| 参数 | 默认值 | 说明 |
|---|---:|---|
| `-input` | 无 | 包含 `raw/` 和 `download-config.json` 的下载目录，必填 |
| `-output` | 无 | 3D Tiles 输出目录，必填 |
| `-workers` | `2` | 并行解码/写入 worker 数 |
| `-min-level` | `1` | 参与构建的最低层级 |
| `-max-level` | `9` | 参与构建的最高层级 |
| `-limit` | `0` | 仅处理排序后的前 N 个节点；`0` 表示全部 |
| `-finalize-only` | `false` | 只从已有 `nodes/` 标记重建 `tileset.json` |
| `-allow-missing-children` | `false` | 允许整条分支提前停止；同一父节点下只下载部分子节点仍会报错 |
| `-format-version` | `14` | MODELSET 原始格式版本 |
| `-vertex-size` | `64` | 原始顶点记录字节数 |

`-limit` 用于固定样本的烟雾测试，不表示“继续下一批”。正式构建不要设置它。
`-finalize-only` 不会重新解码源文件，也不会重新验证全部 GLB，只适合转换已经
完成、仅需重新发布层级清单的情况。

### worker 数建议

- 本地 SSD：可以从 `4` 或 `8` 开始。
- SMB/NAS：建议先使用 `2` 到 `4`；更多 worker 可能受网络存储随机读写限制，
  不一定线性加速。
- 两个输出目录可以并行构建，但总 worker 数应结合 CPU 温度、内存和 SMB 吞吐量
  调整。

构建结束统计示例：

```text
discovered=4165 converted=... skipped=... noModel=... failed=0 ...
```

`noModel` 表示该 NODE 存在但没有可渲染网格，不等同于失败；`failed=0` 且命令
退出码为 `0` 才表示构建完成。

## 推荐的数据目录

以下结构适合把一个场景拆成两个 MODELSET 子树。标有“程序生成”的内容不要手工
修改；日志由示例命令中的 `tee` 生成，并非程序归档的一部分。

```text
3d/
├── README.md                         # 项目说明（人工维护）
├── source-main.log                   # 主子树下载日志（可选）
├── source-east.log                   # 东侧子树下载日志（可选）
├── build-main.log                    # 主子树构建日志（可选）
├── build-east.log                    # 东侧子树构建日志（可选）
├── source-main/                      # 主子树原始归档
│   ├── download-config.json          # 下载身份清单（程序生成）
│   ├── .download.lock                # 单写者锁文件（程序生成）
│   ├── raw/
│   │   └── LEVEL/ROW/COL.response    # gzip MODELSET NODE 响应
│   └── empty/
│       └── LEVEL/ROW/COL.empty       # HTTP 204 空节点标记
├── source-east/                      # 东侧子树原始归档，结构同上
├── tiles-main/                       # 主子树 3D Tiles 输出
│   ├── tileset.json                  # Cesium 入口，3D Tiles 1.1
│   ├── build-config.json             # 构建身份清单（程序生成）
│   ├── .build.lock                   # 单写者锁文件（程序生成）
│   ├── content/
│   │   └── LEVEL/ROW/COL[-N].glb     # glTF 2.0 模型，纹理已内嵌
│   └── nodes/
│       └── LEVEL/ROW/COL.json        # 构建记录和断点标记
├── tiles-east/                       # 东侧子树 3D Tiles 输出，结构同上
└── www/                              # 可选的静态部署页面/配置
```

`.part` 是原子写入期间的临时文件，成功提交后会被重命名。`nodes/` 不只是调试
信息，也是构建续跑的关键状态，不能在构建过程中删除。`www/` 不由下载器或构建器
自动生成；仓库自带的预览器将页面嵌入 Go 程序，因此本地预览不要求该目录有内容。

## 双子树场景

当目标范围跨越 MODELSET 的四叉树边界时，需要分别下载和构建多个根节点。例如：

```bash
go run ./cmd/nlsc-modelset-download \
  -output "<data-root>/source-main" \
  -base-url "https://mapserver02.nlsc.gov.tw/oview//oview" \
  -layer 113_B -root-level 3 -root-row 1 -root-col 0 \
  -max-level 9 -concurrency 4 -retries 3 \
  -cdp-url "http://localhost:29222" \
  2>&1 | tee "<data-root>/source-main.log"

go run ./cmd/nlsc-modelset-download \
  -output "<data-root>/source-east" \
  -base-url "https://mapserver02.nlsc.gov.tw/oview//oview" \
  -layer 113_B -root-level 3 -root-row 0 -root-col 0 \
  -max-level 9 -concurrency 4 -retries 3 \
  -cdp-url "http://localhost:29222" \
  2>&1 | tee "<data-root>/source-east.log"
```

两个名字只是项目分组名称；它们代表两个完整四叉树子树，并不是按经度线精确裁切
的几何范围。构建时每个下载根节点必须输出到独立目录：

```bash
go run ./cmd/nlsc-modelset-build \
  -input "<data-root>/source-main" \
  -output "<data-root>/tiles-main" \
  -min-level 3 -max-level 9 -workers 4 \
  2>&1 | tee "<data-root>/build-main.log"

go run ./cmd/nlsc-modelset-build \
  -input "<data-root>/source-east" \
  -output "<data-root>/tiles-east" \
  -min-level 3 -max-level 9 -workers 4 \
  2>&1 | tee "<data-root>/build-east.log"
```

## 断点恢复和数据安全

### 下载续跑

直接重复原下载命令。下载器会检查：

- `raw/.../*.response` 存在且非空：跳过。
- `empty/.../*.empty` 存在：跳过并剪枝。
- 只有 `.part`：重新下载该节点。
- 图层、服务地址或根节点与 `download-config.json` 不兼容：拒绝运行。
- 同一归档已有另一个写入者：由 `.download.lock` 拒绝第二个进程。

### 构建续跑

直接重复原构建命令。构建器会同时校验源文件大小、修改时间、节点记录以及 GLB
头部和文件长度；全部一致才计为 `skipped`，否则重新生成该节点。

`build-config.json` 绑定源归档清单、原始格式和层级范围。若更换源区域、格式或
`min/max-level`，请使用新的空输出目录，不要删除清单后混写旧数据。

### 从 L9 扩展到 L10/L11

可以对同一个源目录重跑下载命令并把 `-max-level` 改为 `10` 或 `11`；已完成的 L9
及更浅节点会跳过。若只对 BBOX 补深层数据，构建时应使用
`-allow-missing-children`，让范围外完整分支继续以 L9 作为叶节点。同一父节点声明的
子节点必须全部存在或全部缺失，不能只保留其中一部分。构建输出的配置包含
最大层级，因此已有 L9 输出目录不能直接改为 L11 继续构建；应使用新输出目录，
验证完成后再切换正式目录。

## CesiumJS 本地预览

### 单个 tileset

```bash
go run ./cmd/nlsc-modelset-preview \
  -tiles-dir "<data-root>/tiles-main" \
  -listen "127.0.0.1:18080"
```

打开 `http://127.0.0.1:18080/`。预览器提供 CesiumJS、卫星影像开关、无光照颜色、
包围盒、线框和屏幕空间误差控制。3D Tiles 必须通过 HTTP 访问，不能直接用
`file://` 打开 `tileset.json`。

可用查询参数把指定经纬度作为画面中心，例如：

```text
http://127.0.0.1:18080/?lon=120.56185&lat=24.31400&height=900
```

| 参数 | 含义 |
|---|---|
| `lon` | 画面中心目标经度，不是相机所在经度 |
| `lat` | 画面中心目标纬度，不是相机所在纬度 |
| `height` | 相机到目标点的观察距离（米），不是目标海拔 |

旧示例中的 `lat=24.305` 是修复 `camera.lookAt` 之前使用的相机位置，不应继续作为
目标点；它位于本项目 L11 范围南侧，会看到错误区域或没有高精度建筑。

### 同时预览 main 与 east

当前预览器会在主目录同级自动查找 `<主目录>-east` 或
`<主目录>-east-full`。在不修改正式数据目录名称的情况下，可建立一个仅用于运行的
相对路径入口：

```bash
mkdir -p "<data-root>/.preview"
ln -sfn "../tiles-main" "<data-root>/.preview/tiles"
ln -sfn "../tiles-east" "<data-root>/.preview/tiles-east"

go run ./cmd/nlsc-modelset-preview \
  -tiles-dir "<data-root>/.preview/tiles" \
  -listen "127.0.0.1:18080"
```

访问以下地址检查服务：

```text
http://127.0.0.1:18080/healthz
http://127.0.0.1:18080/tile-sources.json
http://127.0.0.1:18080/tiles/tileset.json
http://127.0.0.1:18080/tiles-east/tileset.json
```

### 嵌入已有 CesiumJS 应用

```js
const tileset = await Cesium.Cesium3DTileset.fromUrl(
  "/tiles-main/tileset.json",
  {
    maximumScreenSpaceError: 8,
    dynamicScreenSpaceError: false,
    skipLevelOfDetail: false,
    shadows: Cesium.ShadowMode.DISABLED
  }
);

viewer.scene.primitives.add(tileset);
await viewer.zoomTo(tileset);
```

如果纹理因场景光照显得过暗，可以使用不受光照影响的 shader：

```js
tileset.customShader = new Cesium.CustomShader({
  lightingModel: Cesium.LightingModel.UNLIT
});
```

## 验证与排错

### 基本检查

```bash
test -f "<data-root>/source-main/download-config.json"
test -f "<data-root>/tiles-main/build-config.json"
test -f "<data-root>/tiles-main/tileset.json"

find "<data-root>/source-main/raw" -name '*.response' | wc -l
find "<data-root>/tiles-main/content" -name '*.glb' | wc -l
find "<data-root>/tiles-main/nodes" -name '*.json' | wc -l
du -sh "<data-root>"/*
```

### 常见错误

| 错误或现象 | 原因 | 处理方法 |
|---|---|---|
| `no open ... page found in CDP` | 没有可用 NLSC 标签页 | 在远程调试浏览器中打开 NLSC 3D 地图页面 |
| `output directory ... different dataset` | 输出目录属于其他图层或根节点 | 使用正确原命令，或换一个空目录 |
| `output directory is not empty and has no download-config.json` | 非空目录缺少身份清单 | 不要把日志放进归档目录；使用空目录或恢复匹配清单 |
| `another builder is using output` | 同一输出目录已有构建进程 | 等待现有进程结束，不要并发写同一个输出 |
| `advertises missing child` / `partially downloaded sibling set` | 同一父节点的子节点只下载了一部分 | 重跑下载补齐该组子节点；该缺口不能用 `-allow-missing-children` 跳过 |
| `not reachable from root` | 输入包含不属于当前根树的可渲染节点 | 检查输入根节点、层级范围和是否混入其他区域 |
| 模型很暗 | 场景光照改变了原始纹理外观 | 在 Cesium 中启用 `UNLIT` shader |
| 放大后没有细节 | `maximumScreenSpaceError` 太高或未下载更深层 | 降低 SSE，并核对下载/构建的最大层级 |

运行相关单元测试：

```bash
go test ./cmd/nlsc-modelset-download ./nlscdownload \
  ./cmd/nlsc-modelset-build ./nlscmodelset \
  ./cmd/nlsc-modelset-preview
```

## 更换城市或区域

图层名不能根据城市名称猜测，根节点也不能直接由本命令的 BBOX 参数计算。更换
区域时，应先在 NLSC 页面确认实际 MODELSET 图层，并通过网络请求或节点空间信息
确定覆盖目标范围的一个或多个根 `level/row/col`。随后：

1. 每个根节点使用独立的 `source-*` 目录。
2. 每个源目录使用独立的 `tiles-*` 输出目录。
3. 先用较浅的 `max-level` 做小规模验证。
4. 确认定位、纹理和层级正确后，再扩展到 L9 或 L10。

下载和转换仅用于你有权归档、处理和展示的数据；部署或再分发前请自行确认数据源
的使用条款、署名要求和访问限制。
