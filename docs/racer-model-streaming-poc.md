# KAITO + Racer 模型流式加载 POC

> **实验性 POC，不是生产支持声明。** 实现位于 `poc/racer-model-streaming`。
> 设计依据：[racer-kaito-design.md](https://github.com/andyzhangx/demo/blob/master/llm/distributed-cache/racer-kaito-design.md)。
> 本地 controller/provider 单测、race、真实 `.so` ABI 测试不等于 AKS/GPU 验收。
> 本次没有部署真实集群、构建 Docker 镜像或测量冷/热缓存性能。

## 快速开始：五步部署

建议先在**独立测试 AKS 集群**跑单节点，成功后再验证多节点。以下是部署摘要；身份配置、实际参数来源、验收和回滚见后面的详细章节。

### 第一步：准备基础环境

先确认普通 **KAITO ModelMirror + ModelStreaming** 能加载目标模型。需要 GPU 节点、Blob CSI、Workload Identity，以及已经完成 mirror 的模型容器。具体前置条件见第 2 节。

### 第二步：安装 Racer

使用固定版本 **Azure/unbounded v0.18.0**，按第 3 节完成安装及 origin 身份、FIC/RBAC 配置。

**Racer origin 必须指向 ModelMirror 实际使用的 Azure account/container**，不能根据模型名或 PVC 名猜测。第 4 节说明如何确认实际容器；模型对象必须保持不可变。

### 第三步：构建并推送两个镜像

```bash
git clone --branch poc/racer-model-streaming \
  https://github.com/andyzhangx/kaito.git
cd kaito

export REGISTRY=andyzhangx
export OUTPUT_TYPE="type=docker"
export ARCH="amd64"
export PLATFORMS=linux/amd64
export VERSION=0.13.0-racer
export KAITO_BASE_IMG_NAME=kaito-base
export KAITO_BASE_IMG_TAG="$VERSION"
export RACER_STREAMING_IMAGE="$REGISTRY/kaito-vllm-racer:$VERSION"

make docker-build-workspace ARCH=amd64
# 仅当指定的 kaito-base 不存在或不兼容时才需要构建和推送。
make docker-build-kaito-base ARCH=amd64

# type=docker 将镜像加载到本机 Docker，不会推送到仓库。
# docker-container buildx builder 通常无法直接使用本机 daemon 中的 base：先推送。
docker push "$REGISTRY/$KAITO_BASE_IMG_NAME:$KAITO_BASE_IMG_TAG"
make docker-build-racer-streaming ARCH=amd64

docker push "$REGISTRY/workspace:$VERSION"
docker push "$RACER_STREAMING_IMAGE"
```

最终 Racer runtime 镜像是在 KAITO vLLM preset base 中加入 provider；构建时会检查实际 Streamer ABI。若已有兼容的仓库 kaito-base，设置已有的 `REGISTRY`、`KAITO_BASE_IMG_NAME`、`KAITO_BASE_IMG_TAG` 指向它，跳过 base 构建和推送。镜像构建、注册表认证和推送需要由部署者在有 Docker 的构建机执行，本 POC 的本地测试没有替代这些步骤。

### 第四步：更新控制器配置

修改并使用 [Helm values 示例](../examples/racer-model-streaming-values.yaml)，填写：

- Workspace namespace；
- 实际 storage account/container；
- Racer cache 名及 sidecar 镜像；
- 新构建的 runtime 镜像。

通过**现有 workspace chart** 升级控制器并应用 `racerCacheBindings`，保留原有 WI 等配置。第 6 节提供 Helm 命令及不同安装方式的注意事项；不要在已有控制器旁盲目安装第二个控制器。

### 第五步：创建新的测试 Workspace

修改 [Workspace 示例](../examples/racer-model-streaming-poc.yaml) 中的身份和 GPU 配置，完成对应 FIC 后再应用。启用开关是：

```yaml
metadata:
  annotations:
    kaito.sh/model-cache: racer
    kaito.sh/streaming-service-account: model-reader
```

每个推理 rank 都会获得 provider 配置及 `racer-object` sidecar。**先用新 Workspace 测试**；旧 StatefulSet 的部分字段不会仅靠修改 annotation 自动更新。具体操作见第 7 节，并按第 8 节检查所有 rank，而不只是 head。

### 验证状态与回滚

已通过 Go 单测、provider race 测试、**9 项真实 `.so` ABI 测试**、控制器编译及 Helm 渲染检查。

**尚未进行 Docker 镜像构建和真实 AKS/GPU 部署验证，也没有冷／热缓存性能数据。** 第 8 节包含逐 rank 检查、冷／热缓存验收及故障测试，第 9 节包含回滚步骤，第 11 节记录完整验证范围和限制。

## 1. 实现范围与数据路径

```text
ModelMirror（已有）→ Azure Blob，不变
metadata/config/listing → Azure SDK + Workload Identity，不变
vLLM → Model Streamer Azure backend
     → libkaito_racer_cache.so（C ABI）
     → http://127.0.0.1:18080/<blob-key>（Range GET，无 Azure token）
     → Racer native sidecar → Racer cache server → Azure Blob origin
```

- 仅支持现有 preset + ModelMirror + ModelStreaming 路径；不是下载整个模型再加载。
- Provider 导出 `cache_abi_version() == 1`、`blob_read`、`shutdown`。账户/容器逐次严格校验。
- 精确校验 HTTP 206、Content-Range、返回长度；拒绝部分成功、重定向和非 loopback endpoint。
- 默认每次读取总超时 30s、最大并发 16、最多 3 次尝试，瞬态错误使用指数退避加 jitter；鉴权、范围、短读错误不重试。
- 管理员通过 `racerCacheBindings` 授权命名空间及 Azure account/container，Workspace 不能自由指定 sidecar/hostPath。
- 当前 preset 路径的单节点/多节点 StatefulSet 复用同一个 PodTemplate mutator；所有 rank（不只 head）得到 provider env、runtime image、sidecar。
- `kaito.sh/model-cache: disabled` 可单独关闭 Racer，而继续使用 Mirror + Streaming。
- 不支持运行中透明 direct-Azure fail-open。关闭 Racer 后重建工作负载才是可靠的直接 Azure 对照/回滚路径。

## 2. 前置条件

准备独立测试 AKS 集群：

1. Linux amd64 GPU 节点，Kubernetes >= 1.30，支持 native sidecar（initContainer `restartPolicy: Always`）。
2. 已安装 KAITO、Azure Blob CSI Driver，启用 OIDC issuer 与 Azure Workload Identity。
3. 已验证普通 [ModelMirror](proposals/20260520-model-mirror.md) 和 [ModelStreaming](../website/docs/model-mirror-streaming.md) 可成功加载同一个 safetensors 模型。
4. 有私有镜像仓库和有权限推送镜像的 Docker 构建机。
5. 推理 namespace 中已有 streaming ServiceAccount/FIC；具体绑定必须遵循原 ModelStreaming 文档。
6. 安装 Racer 前评估其成本、节点缓存磁盘与 hostPath 安全范围。此 POC 不会替你自动创建 Azure 资源。

**模型对象必须不可变。** 用新的容器或 revision 前缀发布新版本；不能覆盖已缓存的同名 blob。
Provider ABI 没有 ETag/version 参数，本实现不做覆盖写失效保证；缺少安全失效/一致性承诺时不要用可变路径。

## 3. 安装并验证 Racer

采用固定发行版 **Azure/unbounded v0.18.0**；不同版本不要混用 server/sidecar。
官方文档：[Azure Blob guide](https://unbounded-cloud.io/guides/azure-blob-cache/)、[CLI](https://unbounded-cloud.io/reference/cli/)、[racer-object](https://unbounded-cloud.io/reference/racer-object/)。
先审阅对应版本帮助与渲染结果，再对测试集群应用。下列为 Linux amd64 构建机操作：

```bash
curl -fL -o /tmp/kubectl-unbounded \
  https://github.com/Azure/unbounded/releases/download/v0.18.0/kubectl-unbounded-linux-amd64
printf '%s  %s\n' \
  13bc415f9ecdb5091c5de7b633665a45f7b57c7b21d60037f212f218ec26b3668c \
  /tmp/kubectl-unbounded | sha256sum -c -
chmod +x /tmp/kubectl-unbounded
mkdir -p "$HOME/.local/bin"
cp /tmp/kubectl-unbounded "$HOME/.local/bin/kubectl-unbounded"
export PATH="$HOME/.local/bin:$PATH"
kubectl unbounded --help
kubectl unbounded manage blobs install --help
```

管理员创建/配置**专用于 Racer origin 的** Azure Managed Identity。它需要：

- 读取目标 Azure Blob 容器的 `Storage Blob Data Reader`（尽量缩小到容器范围）；
- 当前集群的 Workload Identity FIC，subject 必须匹配 Racer server 实际 ServiceAccount；
- 对应 ServiceAccount 的 `azure.workload.identity/client-id` annotation 和 server Pod 的 WI label。

**KAITO inference 的 WI 仍保留**，用于 metadata/listing/config；不要把 Azure token/SAS 交给 provider。
Racer origin 身份和 KAITO streaming 身份可以不同，应分别审计。

```bash
export CACHE_NAME=kaito-model-cache
export AZURE_CONTAINER_URI=az://YOUR_ACCOUNT/YOUR_ACTUAL_MIRROR_CONTAINER
kubectl unbounded manage blobs install "$CACHE_NAME" \
  --from-container "$AZURE_CONTAINER_URI" \
  --sku Standard_L8s_v3 --count 2
```

为刚创建的 cache 配置 origin Workload Identity（该字段已核对 v0.18.0 CRD）：

```bash
export ORIGIN_CLIENT_ID=YOUR_ORIGIN_MANAGED_IDENTITY_CLIENT_ID
kubectl patch clustercache "$CACHE_NAME" --type=merge \
  -p "{\"spec\":{\"origin\":{\"azure\":{\"credential\":{\"default\":null,\"workloadIdentity\":{\"clientID\":\"$ORIGIN_CLIENT_ID\"}}}}}}"
kubectl get clustercache "$CACHE_NAME" -o yaml
```

operator 会根据 credential 配置 origin Pod/SA；从实际 origin Pod 的 `spec.serviceAccountName` 和 namespace
取得 FIC subject `system:serviceaccount:<namespace>:<serviceaccount>`，再建立对应 FIC。
不能更改既有 cache 的 containerURL；切换容器需创建新 cache。

以下为 FIC/RBAC 命令模板，所有变量都需使用上面实际查到的资源；不要复用 inference 的 subject：

```bash
OIDC_ISSUER=$(az aks show -g "$AKS_RESOURCE_GROUP" -n "$AKS_NAME" \
  --query oidcIssuerProfile.issuerUrl -o tsv)
az identity federated-credential create \
  --resource-group "$IDENTITY_RESOURCE_GROUP" --identity-name "$ORIGIN_IDENTITY_NAME" \
  --name kaito-racer-poc --issuer "$OIDC_ISSUER" \
  --subject "system:serviceaccount:${ORIGIN_NAMESPACE}:${ORIGIN_SERVICEACCOUNT}" \
  --audiences api://AzureADTokenExchange
# STORAGE_CONTAINER_RESOURCE_ID 必须是目标 storageAccount/blobServices/default/containers/<container> 的完整 ARM ID。
az role assignment create --assignee-object-id "$ORIGIN_PRINCIPAL_ID" \
  --assignee-principal-type ServicePrincipal --role 'Storage Blob Data Reader' \
  --scope "$STORAGE_CONTAINER_RESOURCE_ID"
```


CLI 会申请缓存资源，SKU/count 仅为示例，需核对区域容量与费用。按官方说明完成 origin 身份配置，
检查实际部署 YAML、Pod events、server 日志以及一次冷读，不能只凭 Pod Ready 认为鉴权已成功。
从该发行版部署中取得**实际 server 使用的 racer-object 镜像**，解析并固定为 `@sha256:...`，
作为后面的 `SIDECAR_IMAGE`，不要猜测镜像仓库地址。

已通过基础 Racer 独立测试后，记下 cache 名。此 POC 与 v0.18.0 sidecar 约定一致，挂载
`/run/racer/<cache-name>/client`，排除存在 `racer.unbounded-cloud.io/exclude` 标签的节点。
检查该目录由 Racer node client 创建，socket 权限允许 UID/GID 65532 访问；hostPath 在 Pod Security 中需要管理员许可。

## 4. 找到实际 Mirror account/container

先完成普通 ModelMirror + Streaming，再检查资源：

```bash
kubectl get modelmirrors -A
kubectl -n YOUR_WORKSPACE_NAMESPACE get pvc
# 从 ModelMirror 状态/对应 PVC 选择本模型已经完成 mirror 的 PVC，不要选择任意一个。
export MIRROR_PVC=YOUR_ACTUAL_MIRROR_PVC
export WORKSPACE_NAMESPACE=kaito-poc
PV=$(kubectl -n "$WORKSPACE_NAMESPACE" get pvc "$MIRROR_PVC" -o jsonpath='{.spec.volumeName}')
kubectl get pv "$PV" -o jsonpath='{.spec.csi.volumeAttributes}'
echo
```

从 PV 属性/实际 Azure 资源确认 `storageAccount`、`containerName`。动态 CSI 容器经常是 `pvc-...`；
**不要**使用 StorageClass 名、PVC 名、模型名猜测容器。缺失时由管理员从 Blob CSI 配置解析并确认。
Racer origin 必须指向这个完整容器；sidecar 中 `/blob-key` 对应容器根目录的 blob key，不能额外重写模型前缀。

## 5. 使用 Makefile 构建控制器和 Racer runtime

### 直接复用现有 `kaito-base`

**不需要另一种 streaming base 镜像或额外的 base 构建目标。** Racer runtime 直接使用现有 `make docker-build-kaito-base` 构建的镜像，其输入地址由已有变量组成：

```text
${REGISTRY}/${KAITO_BASE_IMG_NAME}:${KAITO_BASE_IMG_TAG}
```

现有 `docker/presets/models/tfs/Dockerfile` 已包含 vLLM、Ray 和 Model Streamer。Racer 构建只在这套环境上加入 `libkaito_racer_cache.so` 并校验兼容性：

```text
已有 kaito-base（vLLM + Ray + Model Streamer）
        │
        │ 加入 Racer provider 并检查 ABI
        ▼
RACER_STREAMING_IMAGE（最终推理容器镜像）
```

`docker/racer-streaming/Dockerfile` 内部用 `ARG KAITO_BASE_IMAGE` / `FROM ${KAITO_BASE_IMAGE}` 接收 Make 自动拼出的地址；**不需要额外设置一个 base 镜像地址变量**。

- `CONTROLLER_IMAGE`：包含本 POC 编排逻辑的 KAITO Workspace 控制器镜像。
- `REGISTRY`、`KAITO_BASE_IMG_NAME`、`KAITO_BASE_IMG_TAG`：复用现有 kaito-base 构建配置。base 不是额外运行的容器。
- `RACER_STREAMING_IMAGE`（部署时赋给 `RACER_RUNTIME_IMAGE`）：最终推理镜像，包含 Racer provider。
- 管理员 binding 中的 `objectImage`：单独的 `racer-object` sidecar 镜像，不由这里的目标构建。

### 构建与推送

以下命令在本 POC 分支的仓库根目录、具备 Docker/buildx 和 registry 权限的构建机执行：

```bash
git clone --branch poc/racer-model-streaming \
  https://github.com/andyzhangx/kaito.git
cd kaito

export REGISTRY=andyzhangx
export OUTPUT_TYPE=type=docker
export ARCH=amd64
export PLATFORMS=linux/amd64
export VERSION=0.13.0-racer
export KAITO_BASE_IMG_NAME=kaito-base
export KAITO_BASE_IMG_TAG="$VERSION"
export RACER_STREAMING_IMAGE="$REGISTRY/kaito-vllm-racer:$VERSION"

make docker-build-workspace ARCH=amd64

# 仅当这个 kaito-base 镜像不存在或不兼容时执行；已有兼容镜像可跳过这两行。
make docker-build-kaito-base ARCH=amd64
docker push "$REGISTRY/$KAITO_BASE_IMG_NAME:$KAITO_BASE_IMG_TAG"

make docker-build-racer-streaming ARCH=amd64
export CONTROLLER_TAG="$VERSION"
export CONTROLLER_IMAGE="$REGISTRY/workspace:$CONTROLLER_TAG"
export RACER_RUNTIME_IMAGE="$RACER_STREAMING_IMAGE"
docker push "$CONTROLLER_IMAGE"
docker push "$RACER_STREAMING_IMAGE"
```

**已有兼容的 kaito-base 时，设置这三个已有变量指向它，跳过 base 构建/推送即可。** 例如已有 `registry.example/team/kaito-base:tested`，设置 `REGISTRY=registry.example/team`、`KAITO_BASE_IMG_NAME=kaito-base`、`KAITO_BASE_IMG_TAG=tested`，然后执行 `make docker-build-racer-streaming ARCH=amd64`。最终 runtime 地址可以通过 `RACER_STREAMING_IMAGE` 独立指定。

| 目标 | 上述示例的结果 |
|---|---|
| `docker-build-workspace ARCH=amd64` | `andyzhangx/workspace:0.13.0-racer` |
| `docker-build-kaito-base ARCH=amd64`（可选） | `andyzhangx/kaito-base:0.13.0-racer` |
| `docker-build-racer-streaming ARCH=amd64` | `andyzhangx/kaito-vllm-racer:0.13.0-racer` |

- 现有 base 默认名称为 `kaito-base`、默认 tag 为 `v0.0.1`。**`KAITO_BASE_IMG_TAG` 不会随 `VERSION` 自动变化**，所以上例显式设置；Racer 输出的默认 tag 则沿用 `IMG_TAG` / `VERSION` 规则。
- Racer 目标仅支持 `ARCH=amd64`、`PLATFORMS=linux/amd64`，无效平台/空配置会在调用 Docker 前报错。原有 workspace/base 目标不变。
- 当前 Make 接口按现有 base 的 name/tag 组成输入地址；最终 runtime 必须使用不同的输出 tag，不能覆盖 base。
- `OUTPUT_TYPE=type=docker` 只加载本机镜像，不会推送。Racer 构建使用 `--pull`，base 必须能从 registry 拉取；该目标不会自动构建或推送 base。
- 可显式使用 `OUTPUT_TYPE=type=registry` 让 buildx 直接推送，此时不要再依赖本机有镜像或重复 `docker push`。

### 保留兼容性校验

复用的是**兼容的** kaito-base，不是任意旧版本。当前 POC 要求对应的 KAITO preset 代码及 `runai-model-streamer==0.16.0`、`runai-model-streamer-azure==0.16.0`；当前分支的 requirements 已固定这两个版本。

Provider 使用 Ubuntu 22.04 / glibc 2.35 构建，当前 preset 使用 Debian bookworm / glibc 2.36。最终镜像构建会检查 wheel 版本、native cache-loader markers，并实际 `dlopen` provider，校验 `cache_abi_version`、`blob_read`、`shutdown`。检查失败应修正 base，不要跳过检查。无需从上游 main 额外构建 Streamer。

构建命令测试使用 mock Docker，**不代表已经构建/推送真实镜像或完成 AKS/GPU、Racer 连接与冷/热缓存验收**。

## 6. 配置控制器的管理员绑定

复制 [values 示例](../examples/racer-model-streaming-values.yaml)，将以下项替换为真实值：

```yaml
# racer-values.yaml（合并到现有 workspace chart values，不覆盖 WI 等配置）
featureGates:
  ModelMirror: true
  ModelStreaming: true
racerCacheBindings:
  - namespace: kaito-poc
    account: YOUR_ACTUAL_STORAGE_ACCOUNT
    container: YOUR_ACTUAL_MIRROR_CONTAINER
    runtimeImage: YOUR_REGISTRY/kaito/vllm-racer@sha256:YOUR_DIGEST
    objectImage: YOUR_ACTUAL_RACER_OBJECT_IMAGE@sha256:YOUR_DIGEST
    cacheName: kaito-model-cache
    immutable: true
```

`namespace` 是允许使用此绑定的 **Workspace namespace**。数组可以授权多个精确的 namespace/account/container 组合；
同一个 cacheName 不允许绑定到不同的存储源。控制器从实际 StreamingConfig 再次校验账户/容器，
不匹配时 fail closed，不会把别的容器静默映射到同一个 Racer cache。

```bash
# 先 helm list -A 找到已有 workspace chart 的 release/namespace；不要安装第二个 controller。
helm list -A
export KAITO_RELEASE=YOUR_EXISTING_WORKSPACE_RELEASE
export KAITO_NAMESPACE=YOUR_EXISTING_KAITO_NAMESPACE
helm upgrade "$KAITO_RELEASE" ./charts/kaito/workspace \
  --namespace "$KAITO_NAMESPACE" --reuse-values \
  --set image.repository="$REGISTRY/workspace" \
  --set image.tag="$CONTROLLER_TAG" \
  -f racer-values.yaml
```

`--reuse-values` 保留原配置；仍需检查最终渲染结果，特别是 WI、ServiceAccount、feature gates。
如果原安装是另一套 chart/AKS 托管 add-on，不要盲目执行该命令覆盖它；在独立测试集群安装本 chart，
或者按原安装方式更新 workspace controller 镜像及 `--racer-cache-bindings` JSON 参数。

## 7. 启动新的测试 Workspace

使用你已经成功验证直接 Azure Streaming 的同一个 preset/spec，新增：

```yaml
metadata:
  annotations:
    kaito.sh/model-cache: racer
    kaito.sh/streaming-service-account: model-reader
```

Workspace 样例在 [examples/racer-model-streaming-poc.yaml](../examples/racer-model-streaming-poc.yaml)，
controller values 在 [examples/racer-model-streaming-values.yaml](../examples/racer-model-streaming-values.yaml)。
Mirror/Streaming 由 feature gates 开启；不要设置 `kaito.sh/model-streaming: disabled`。
`immutable: true` 是管理员 binding 的承诺，不是 Workspace 可以自行声明的授权。
样例默认 **单节点**，先完成单节点正确性验收，再按既有多节点 preset 方法配置 tensor/pipeline parallelism，
不能仅把 nodeCount 从 1 改为 2 就认为模型并行配置正确。

**用新 Workspace 测试。** 当前已有 StatefulSet 更新逻辑不更新所有 initContainer/affinity 字段。
修改 Racer annotation、binding 或 sidecar 镜像后，单纯 patch/rollout restart 旧 StatefulSet 不一定收敛。
应保存 Workspace YAML、停止测试流量，并删除它管理的 StatefulSet，让 controller 从新模板重建。
先核对对象 ownerReferences 和数据保留策略，避免误删镜像 PVC、ModelMirror 或非 POC 对象。

## 8. 验证：所有 rank，而不只是 head

```bash
kubectl -n "$WORKSPACE_NAMESPACE" get sts,pods -o wide
# 选定 POC 的 StatefulSet 后检查 spec.template（包括 initContainers 与 affinity）。
kubectl -n "$WORKSPACE_NAMESPACE" get sts YOUR_POC_STATEFULSET -o yaml
kubectl -n "$WORKSPACE_NAMESPACE" logs YOUR_POC_POD -c racer-object
kubectl -n "$WORKSPACE_NAMESPACE" logs YOUR_POC_POD -c YOUR_INFERENCE_CONTAINER
```

每个 Pod/rank 应具备：

- 同一个 digest-pinned provider runtime；非 sidecar 的 initContainers 保留原镜像。
- `RUNAI_STREAMER_EXPERIMENTAL_AZURE_CACHE_ENABLED=1`、正确的 `_LIB` 路径。
- `KAITO_RACER_ENDPOINT=http://127.0.0.1:18080` 和正确 account/container。
- 名为 `racer-object` 的 native sidecar、只读 socket mount、原 streaming SA/WI 设置。
- 排除 cache-server node 的 affinity；controller 保留原有 affinity 约束。
- Model Streamer 日志确认 cache provider loaded；缓存指标/日志能证明读取确实命中 Racer。

PodTemplate 注入并不等于 Ray env 已传递成功；必须检查每个实际 worker 的加载日志及 cache 访问。
sidecar 首次启动/重启竞态可能导致有界读取失败，之后由工作负载重启恢复；此 POC 没有跨进程无损续读保证。

### 正确性及性能验收矩阵

| 场景 | 必须验证 |
|---|---|
| 关闭 Racer 的基线 | 同一模型 revision、GPU、节点数、batch/并行设置，ModelReady 与 smoke inference 成功 |
| 冷缓存 | 新缓存/已确认清空的测试 cache；分开记录 mirror 时间与 model-load 时间、origin bytes、错误率 |
| 热缓存 | 重启同样的 inference Pod，不重启/清除 Racer；核对 cache hit、origin bytes、输出正确性 |
| 第二 rank/第二节点 | 每个 rank 的加载日志、正确性和缓存命中均验证，不以 head 日志替代 |
| 非零/16 MiB 跨界读取 | 将 cache range 与同一不可变 Azure blob 的相同字节范围逐字节比较 |
| sidecar 停止/server 重启 | 有界失败、无错误成功或挂死；恢复后的完整重试结果正确 |
| origin 403、404、429/503 | 权限/不存在立即失败；仅瞬态错误按界限重试；不泄漏凭据 |
| immutable 前提 | 容器/revision 不覆盖写；若变更则用新的 immutable 路径/cache 绑定 |

分别记录 time-to-ModelReady、加载吞吐、每个 rank 时间、Racer hit/origin bytes、失败率及首次推理结果。
**本次没有实测性能数据，不宣称提升倍数。** 不要在共享/生产 cache 上做破坏性故障注入。

## 9. 回滚

1. 将 Workspace 的 `kaito.sh/model-cache` 改为 `disabled`。
2. 保持 Mirror/Streaming feature gates 开启；按第 7 节重建该 POC 工作负载，验证 sidecar/provider env 已消失、原 preset image 恢复。
3. 确认直接 Azure Streaming 与 smoke inference 成功。
4. 若不再测试，移除 controller Racer binding；再按原 Helm revision 回滚控制器。
5. 最后按 Racer 官方步骤清理仅属于此次 POC 的 cache 资源和 RBAC；不要删除共享 Blob 数据。

## 10. 本地测试

```bash
go test ./pkg/workspace/inference/... ./cmd/workspace
go vet ./pkg/workspace/inference/... ./cmd/workspace
go build ./cmd/workspace
helm lint charts/kaito/workspace
cd tools/racer-cache-provider
CGO_ENABLED=1 go test -race ./...
CGO_ENABLED=1 go build -buildmode=c-shared -o /tmp/libkaito_racer_cache.so .
python3 test_abi.py /tmp/libkaito_racer_cache.so
```

Makefile 构建参数的回归测试不需要 Docker daemon；它调用真实 GNU Make，但用 mock 记录 Docker 命令而不会构建或推送镜像：

```bash
python3 hack/test-racer-build-targets.py
# 非系统安装的 GNU Make 可通过 MAKE=/path/to/make 指定。
```

覆盖 workspace 原有命令、streaming base/runtime 默认 tag、输入 digest/输出覆盖、`type=docker`/`type=registry`、额外 build flags，以及无效配置在触发 Docker 前失败。

CGO 需要 C 编译器与 glibc 开发工具。本地 ABI 测试调用真正编译的 `.so`，使用 loopback mock server；
覆盖 ABI 版本、并发 C 调用、精确/跨界读取、授权绑定、短读/403、空指针、整数溢出、有限错误缓冲、shutdown 后重用和无凭据转发。
它不能替代真实 Racer server、Azure WI 或 GPU 推理验收。

## 11. 此分支的验证记录（2026-10-24）

- 通过：`CGO_ENABLED=0 go test ./pkg/... ./api/... ./presets/...`；最终改动后又运行推理相关包及 workspace cmd 单测。
- 通过：推理相关包/workspace cmd 的 `go vet`、workspace controller 编译。
- 通过：独立 provider 模块 `go test -race`、`go vet`、实际 `c-shared` 编译和 9 项 Python ctypes ABI 测试。
- 通过：从 PyPI 下载并核验 SHA-256 的 core/Azure 0.16.0 wheel，与真实 `.so` 一起执行 runtime capability check。
- 通过：Helm lint、开/关两种配置的 template 渲染及 flag JSON 一致性、示例 YAML 与相对链接校验、ruff、gofmt、diff whitespace 检查。
- 未执行：Docker 镜像构建/推送、真实 AKS/Racer/WI/GPU 测试、冷/热性能测试、全仓库 race 和完整 golangci-lint。
- 环境没有 make 和系统 C 编译器；provider 本地测试使用临时 Zig 0.14.1 GNU-target 编译器。它不是最终 Docker 构建结果。
- 独立审查服务不可用；进行了源代码/官方 ABI 与配置一致性自查，但不标记为独立审查通过。

### Makefile 构建入口补充验证

Racer 构建已统一复用现有 `docker-build-kaito-base` 配置；GNU Make 4.4.1 的 8 项构建命令回归测试通过，覆盖共享 name/tag、默认值、覆盖配置、平台校验和并行执行时的失败前置校验。Docker 被 mock；没有实际构建或推送镜像。Python ruff、文档 shell 语法/链接及 diff 格式检查通过。
