# a8s 设计文档

状态：初始架构设计，2026-11-09。当前仓库只包含设计文档，未包含平台实现。

a8s 是 Agent 的组件与部署生命周期管理层。它管理独立的 Agent Package、Agentc，以及组合执行器和沙箱环境的 RuntimeClass，为上层提供创建、发布、更新、回滚、下线和执行访问能力。

```text
Deployment
├── Agent Package：指令、Skills、CLI
├── RuntimeClass（固定版本）
│   ├── Agentc：独立发布的 Agent 执行器
│   └── sandbox：环境镜像与执行环境配置
└── Workspace：持久工作数据
```

## 阅读顺序

1. [架构设计](architecture.md)：目标、核心对象、模块边界、状态归属和核心流程。
2. [Kubernetes 对应关系](kubernetes-mapping.md)：用熟悉的概念理解 a8s，以及类比的适用范围。
3. [仓库、目录与服务布局](repository-layout.md)：代码组织、已有项目的边界、部署单元和迁移顺序。

## 已确认的设计约束

- Agent Package 与 Sandbox Image 的构建、发布和版本解耦；部署阶段通过 RuntimeClass 组合。
- Agentc 独立构建、分发和版本化。可以基于 Codex app-server、Claude Code 等 harness 构建实现，通过适配满足 Agentc 契约。
- Sandbox 配置暂时放在 a8s RuntimeClass 内，不引入独立的 a8s Sandbox 顶层资源。
- a8s RuntimeClass 引用 Agentc 和 Sandbox Image；它不是 Kubernetes 原生的 `node.k8s.io/RuntimeClass`。
- a8s 管理 Agent 部署与执行归属；Cellbox 管理 Box、Volume、checkpoint 和快速 resume；Kubernetes 管理节点与 Pod 调度。
- hcodex 提供多人聊天，Colony 提供数字员工产品。渠道、组织和具体业务接入留在上层。
- 模块、仓库和服务分别划分。初期不按每个模块创建独立服务。

## 本轮范围

现有系统已经跑通。本轮目标是整理更清楚、可维护的目标架构，作为后续重构依据。

本次仅初始化新仓库和设计文档；没有迁移或修改 Cellbox、codex-fork、hcodex、Colony、agenthub 的代码与数据。目录规划表示目标结构，尚未创建实现占位文件。

## 决策状态

上述概念和职责边界作为后续设计基线。公共 API 的具体 schema、能力包目录格式、Agentc 分发格式、控制面资源采用 CRD 还是数据库、具体实现语言与进程拓扑，留待接口设计时确定。示例均为概念示意，不是已实现的接口。
