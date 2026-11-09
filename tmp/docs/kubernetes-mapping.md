# a8s 与 Kubernetes 的对应关系

状态：概念映射初稿，2026-11-09。本文中的对应关系帮助理解职责，不表示 a8s 复刻 Kubernetes，也不表示所有概念存在一一映射。

## 1. 两个抽象层

Kubernetes 管理容器工作负载和底层节点资源。a8s 在其上管理 Agent 工作负载，复用 Cellbox 提供的沙箱能力。

```text
a8s 层：        Agent Package → Agentc → Agent 执行
运行环境组合：  RuntimeClass = agentcRef + sandbox 配置
基础设施层：    Cellbox → Kubernetes / 容器运行时 / gVisor
```

在 a8s 层，Agentc 可以类比为执行 Agent bundle 的 runc；在实际 K8s 部署中，Agentc 自身仍然是一个程序，由底层容器运行环境承载。两种观察角度不能混用。

## 2. 对象对应

| a8s 概念 | 最接近的 K8s / 容器概念 | 对应范围与差异 |
| --- | --- | --- |
| Agent Package | OCI 镜像作为可分发工作负载制品的角色 | 内容为指令、Skills、CLI；不是 Sandbox Image 的文件层，不提供完整 rootfs |
| Agentc | runc 的“执行准备好的制品”角色 | Agentc 驱动模型与工具执行；不是 OCI runtime，也不承担容器隔离职责 |
| a8s RuntimeClass | RuntimeClass 的“选择运行方式”角色 | a8s 自己的组合资源，包含 Agentc 引用和 sandbox 配置；与 K8s 原生资源字段不同 |
| RuntimeClass 内的 Sandbox Image | 机器 / 执行环境镜像的角色 | 提供 Linux、Python、Node 等环境；在 a8s 的工作负载类比中没有严格的一一对应对象 |
| Deployment | Deployment / StatefulSet 的声明式工作负载角色 | 绑定 Agent Package、RuntimeClass revision、Workspace；会话和工作数据决定具体更新策略 |
| 部署规格快照 / revision | Pod Template 的版本 / ControllerRevision | 固定完整运行组合；不是另一个 Agent Package |
| Instance | Pod 作为可替换运行实体的角色 | Agentc、工作区、Box 的逻辑组合，不要求与一个实际 Pod 永久一一对应 |
| Workspace | PVC / Volume 的持久工作空间角色 | 是逻辑工作环境和存储引用；物理存储由 Cellbox 或其他选定后端持有 |
| 稳定执行入口 | Service 加应用协议网关 | 按 Workspace/Thread 归属访问实例，不能将同一会话任意负载均衡 |
| Thread / Turn / 审批交互 | 无直接对应 | 属于 Agent 领域，不能强行等同于 Pod、Job 或容器进程 |

## 3. RuntimeClass 的准确含义

Kubernetes 原生 `node.k8s.io/RuntimeClass` 选择底层容器运行时配置，核心字段引用 runtime handler；容器镜像由 Pod 规格声明。a8s 的 RuntimeClass 是另一层的运行组合，不把下面的内容写入 K8s 原生 RuntimeClass。

```text
a8s RuntimeClass: codex-python@rc-1
├── agentcRef: codex@固定版本
└── sandbox
    ├── provider: cellbox
    ├── image: python-node@固定digest
    └── 环境配置与约束
```

Sandbox 暂时内嵌于 a8s RuntimeClass，不单独增加 a8s Sandbox 顶层资源。Cellbox 内已有的模板、revision、Box 和 Volume 保留各自含义。

如选择用 CRD 实现 a8s 资源，应使用自己的 API group，并在名称或文档中明确 a8s RuntimeClass 的身份；不会复用 `node.k8s.io`。具体 API group 和 Kind 命名属于后续接口设计。

## 4. 组件对应

| a8s 模块 | 类比对象 | 保留的职责 |
| --- | --- | --- |
| API | kube-apiserver 的资源入口 | 创建、读取和修改资源，校验必要不变量，提供状态 |
| Controller | workload controller | 观察期望与实际状态，推进创建、更新、恢复和下线 |
| Agentc 接口与适配 | CRI 的接口隔离思想 | 将具体执行器行为封装在版本化契约后面，不采用 CRI 的容器协议 |
| Execution Gateway | Service 加应用层网关 | 稳定访问、会话路由、事件、执行准入 |
| 平台资源存储 | etcd 的控制面状态存储角色 | 保存唯一权威的期望状态和发布进度；并非决定另建 etcd |
| Artifacts | BuildKit / OCI registry 的配套角色 | 构建、校验、登记、分发制品；使用现有仓库 |
| CLI / SDK | kubectl / client SDK | 使用公共契约，不维护另一套部署状态机 |
| Cellbox Provider | 基础设施适配 | 使用 Cellbox API，实现 a8s 所需的沙箱操作 |

不设置 a8s Node、Scheduler 或类似 kubelet 的节点代理。Agentc 内部的进程启动、信号处理和健康报告是本地执行器职责，不因此独立建设节点管理体系。

## 5. 借鉴的设计原则

- 声明式管理：写入期望规格，控制器持续观察并推进。
- 不可变版本：Agent Package、Agentc、RuntimeClass revision 明确固定。
- 规格与状态分离：请求受理、环境运行、执行器就绪、发布完成分别表达。
- 可替换实例与持久数据分离：替换计算资源不隐式删除工作区。
- 清楚的所有权：上层决定目标，适配器执行操作并反馈，不重复管理同一状态机。

与普通容器流量切换相比，Agent 更新还要检查正在执行的 Turn、原生队列、审批等待和 Workspace 写入归属。恢复也需要区分 Cellbox checkpoint resume 与 Agentc 根据持久历史恢复上下文。

## 6. 不采用的类比

- 不把 Agent Package 与 Sandbox Image 解释成同一容器镜像的应用层和环境层；它们是独立制品。
- 不把 Agentc 等同于真实的 runc/runsc、Kubernetes CRI 实现或节点代理。
- 不把 Cellbox 等同于 a8s 控制面；Cellbox 保持通用沙箱边界。
- 不把 K8s 多副本或 Pod 重启等同于 Agent 工具的无损续跑。
- 不为追求命名对称引入没有实际职责的服务。

## 7. 官方参考

- [Kubernetes Components](https://kubernetes.io/docs/concepts/overview/components/)：API、控制器、调度器、kubelet 和容器运行时的职责。
- [Controllers](https://kubernetes.io/docs/concepts/architecture/controller/)：期望状态、实际状态与控制循环。
- [RuntimeClass](https://kubernetes.io/docs/concepts/containers/runtime-class/)：原生运行时选择配置。
- [Container Runtime Interface](https://kubernetes.io/docs/concepts/containers/cri/)：kubelet 与容器运行时之间的接口边界。
- [StatefulSets](https://kubernetes.io/docs/concepts/workloads/controllers/statefulset/)：稳定身份和持久存储的工作负载管理。
- [runc](https://github.com/opencontainers/runc)：按 OCI 规范创建和运行容器的工具。
