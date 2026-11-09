# 仓库、目录与服务布局

状态：目标布局初稿，2026-11-09。以下代码目录是规划；当前 a8s 仓库仅初始化设计文档。

## 1. 仓库边界

| 仓库 | 目标职责 | 主要公开边界 |
| --- | --- | --- |
| `a8s` | 组件契约、能力包工具链、RuntimeClass、Deployment 生命周期、执行入口、Agentc 接入 | 公共 API、CLI、SDK、包格式和 Agentc 契约 |
| `cell-box` | 通用高性能 Sandbox、Volume、checkpoint、快速 resume | Cellbox API / SDK |
| `codex-fork` | 必须在 Codex 内部实现的扩展和宿主注册口 | 固定版本的源码依赖及必要扩展接口 |
| `hcodex` | 多人聊天、参与者、作者和发送规则 | 聊天 SDK / API，调用 a8s 执行入口 |
| `colony` | 数字员工产品、组织授权、界面、渠道、业务工具和记忆接入 | 面向产品用户的接口，消费 hcodex 和 a8s |
| `agenthub` 等 Agent 内容仓库 | Agent 定义、Skills、CLI 和构建输入 | 构建出独立能力包，推送到现有 OCI 仓库 |

Agent 内容仓库和 OCI registry 是不同组件。当前本地 `agenthub` 是 Agent 内容与构建输入仓库，不据此宣称已有独立制品服务。

Agentc 初期在 a8s monorepo 内独立组织和发布，对外仅依赖 Agentc 契约及对应 harness。其实现不能反向依赖 a8s 控制器、Colony 业务代码或 hcodex 聊天内部实现。有独立维护者或发布需要时，可以在不改变契约的前提下拆仓库。

## 2. 目标目录

```text
a8s/
├── api/                         # 公共资源、操作、事件和能力包契约
├── artifacts/                   # 包检查、构建、登记、OCI 分发
├── control/
│   ├── resources/               # Agentc 登记、RuntimeClass、Deployment、Workspace
│   ├── controllers/             # 生命周期协调
│   └── releases/                # 完整规格快照、更新进度和回滚
├── execution/                   # 输入、交互、事件、准入和路由
├── agentc/
│   ├── contract/                # 执行器接口、能力描述与运行规格
│   └── codex/                   # 首个实现，独立构建与发布
│       ├── adapter/             # 包加载、配置、协议转换
│       ├── host/                # app-server 宿主与扩展注册
│       ├── store/               # Codex 原生 Thread / queue 存储
│       └── handoff/             # 排空、交接、恢复与环境绑定转换
├── providers/
│   └── cellbox/                 # 使用 Cellbox 公共 API
├── storage/                     # 平台存储适配，不包含 Codex 原生状态实现
├── infra/                       # 连接、身份接入、日志和追踪
├── sdk/                         # 上层调用客户端
├── cmd/
│   ├── a8s/                     # CLI 组装入口
│   └── a8s-server/              # 控制面与执行网关组装入口
├── images/
│   └── environments/            # 可复用基础环境镜像的构建输入
├── deploy/                      # 服务部署配置；CRD 方案确定后再加入相应资源
└── tmp/docs/                    # 本轮设计文档
    ├── README.md
    ├── architecture.md
    ├── kubernetes-mapping.md
    └── repository-layout.md
```

目录表示能力边界，不预先确定全部模块使用相同语言。Codex 相关实现可以保留现有 Rust 依赖；控制面语言在 API 和迁移设计确定后选择。后续 Agentc 实现按需要增加，不提前创建空实现。

`images/environments` 只承载基础环境构建输入，不放业务 Agent Package。Agentc 产物单独发布，RuntimeClass 引用固定版本。为了启动性能产生的缓存或 Cellbox prepared 产物应记录来源组合，不改变组件的逻辑独立性。

## 3. 依赖方向

```text
CLI / SDK / 上层应用 → 公共 a8s API

control / execution → Agentc contract ← Agentc 实现
control             → provider contract ← Cellbox adapter
control / execution → storage interface ← 存储适配

cmd / 组装入口 → 核心模块 + 具体适配器（依赖注入）

agentc/codex → 固定版本 codex-fork
Agent Package → 独立制品格式和能力要求
```

依赖注入发生在组装入口。Control 依赖接口，具体实现由入口装配；Agentc 只依赖契约与自身依赖。不同模块通过公共入口交互，不直接读写其他模块的私有表或绕过发布控制修改运行归属。

Artifacts 负责生成和验证制品；Control 负责将固定制品版本发布到 Deployment。CLI 不自己实现排空、归属切换和回滚状态机。

## 4. 服务与进程

| 部署单元 | 包含什么 | 初期策略 |
| --- | --- | --- |
| `a8s-server` | API、资源管理、Controller、执行网关、适配器组装 | 一个服务内保持模块边界，按需要部署副本 |
| Agentc 执行组件 | harness、适配器、宿主及原生存储/恢复接入 | 按执行容量和运行形态部署，不强制一个全局常驻服务 |
| `cellbox-server` | Cellbox 已有沙箱服务 | 沿用其部署与资源管理方式 |
| 构建任务 | Agent Package 或 Agentc 的构建与发布 | CLI / CI 按需运行，不增设常驻构建服务 |
| 共享基础设施 | 数据库、OCI 仓库、文件存储、模型代理 | 复用现有设施，各模块有独立数据所有权 |

hcodex 可以继续作为 SDK 嵌入上层应用，是否服务化由使用方式决定。Codex Thread Store 初期可以继续作为 Agentc 的进程内存储适配，不为目录结构额外部署一个 threadstore 服务。

不新增 Scheduler、Node agent、专用 boxd、独立 agenthub 服务或自建镜像仓库。Agentc 的进程守护、公共镜像缓存和控制器的本地辅助逻辑，先在对应模块内实现。

## 5. 现有实现与迁移来源

以下为 2026-11-09 的本地源码观察，用来识别职责来源，不代表新架构已经迁移或上线。源仓库中的既有脏改保持原样。

| 现有位置 | 当前职责 | 目标归属与注意事项 |
| --- | --- | --- |
| Colony `apps/server/src/infra/runtime/` | 运行环境创建、查询、停止、删除及协调 | 逐步进入 a8s Control 与 provider 适配；当前代码直接操作 K8s，不能宣称已接入 Cellbox |
| Colony `apps/server/src/platform.ts` | 业务、运行环境和 Workspace 路由的组装 | Colony 保留产品组装，运行管理通过 a8s SDK 接入 |
| hcodex `ha/pg`、`ha/ext`、`server` | 原生 PG 存储、handoff、执行环境绑定和自定义宿主 | 作为 Codex Agentc 的完整实现边界迁移，固定其 codex-fork 依赖 |
| hcodex `chat` 中 Workspace 路由与切换逻辑 | 准入、排空、目标切换 | 通用归属与发布决策进入 a8s；harness 检查进入 Agentc；聊天层保留多人规则 |
| Colony `packages/agentfile` | 包构建以及 Codex 准备、启动 | 构建进入 artifacts；Codex 专属逻辑进入 Agentc 适配。当前为未跟踪工作区内容 |
| Colony `packages/agentc` | 通用子进程 start/stop/wait | 是复用候选工具；范围小于本文 Agentc，不直接当成完整执行器平台。当前为未跟踪工作区内容 |
| agenthub `agents/`、`schema/` | Agent 内容、构建输入和既有控制器声明格式 | 保留内容来源；将耦合的 Agent、harness、sandbox 声明按新模型拆分时保留兼容路径 |

当前 hcodex 的 PG Thread Store 通过 codex-fork 的注册口注入宿主。不能把它写成“原版 Codex 仅改配置就支持官方远程 gRPC Thread Store”。原生存储与恢复能力应以实际采用版本验证。

现有配置字段与本文示意字段不等同，本次不复制旧 schema 充当新 API，也不直接搬动未提交的实现。

## 6. 迁移顺序

1. 固定核心对象、Agentc 接口、Cellbox provider 接口与状态所有权。
2. 建立 a8s 公共入口，兼容接管已有运行管理调用，保持原有数据和实例可用。
3. 抽取 Codex 专属运行支持，同时保留构建锁定、配置、Thread ID 和存储兼容。
4. 收敛 hcodex 与 Colony 中的运行归属和发布切换逻辑，每项状态只保留一个写入权威。
5. 接入独立 Agent Package、Agentc 发布和 RuntimeClass revision，统一 Deployment 创建、更新、回滚与下线。
6. 在同一控制契约下验证 Cellbox suspend/resume 与故障重建路径。

代码位置调整和行为切换分步完成，不以搬目录代替功能验证，也不把当前已跑通的系统当成需要从零重新建设。

## 7. 架构验收场景

以下是后续重构需要证明的行为，本次未创建测试文件或执行运行验证：

- 同一 Agent Package 在两个兼容 RuntimeClass 上使用，无需重新构建能力包。
- 升级 Agentc 不修改基础环境镜像；环境不兼容时在部署前明确报告。
- 更新 Sandbox Image 不修改 Agent Package，旧部署保持原来的固定组合直到显式发布。
- 创建、更新、下线只经过一套生命周期控制，hcodex 和 Colony 不直接改运行目标。
- 下线保留历史与 Workspace；删除按明确策略处理数据。
- 发布失败、控制面重启或操作超时后，状态可查询且没有无条件重复外部操作。
- Cellbox 快速 resume、Agentc 上下文恢复和事件补读分别验证，报告其实际支持范围。
- 添加新的 Agentc 实现时，上层渠道与 Cellbox 无需引入该 harness 的内部知识。
