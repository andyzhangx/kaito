# KAITO + Racer 模型流式加载 POC

> 实验分支 `poc/racer-model-streaming`，先在独立测试 AKS 集群验证。下面是部署步骤，不代表已经完成镜像构建、AKS/GPU 或冷/热缓存验收。

数据路径：ModelMirror 写 Azure Blob；metadata/config/listing 仍走 Azure SDK + Workload Identity；权重 Range GET 走 **vLLM → Model Streamer → `libkaito_racer_cache.so` → 本 Pod 的 Racer sidecar → 节点缓存 → Azure origin**。不先下载整个模型，也不在故障时透明回退到 Azure。

## 第一步：准备环境和模型

- Kubernetes >= 1.30、Linux amd64 GPU 节点，已安装 KAITO、Blob CSI，开启 OIDC issuer / Azure Workload Identity。
- 先按 [ModelMirror + Streaming 指南](../website/docs/model-mirror-streaming.md) 在 `kaito-workspace` 验证同一个 safetensors 模型；保留已有 StorageClass、streaming identity 和控制器安装配置。
- 构建机需要 Docker/buildx、镜像仓库推送权限；集群需要相应拉取权限。安装 Racer 的管理员需要创建 CRD、ClusterRole、webhook、DaemonSet、ClusterCache 等权限。
- Racer dataplane 需要可用的 `io_uring`，以 privileged/root 运行，使用 `/run/racer`、`/var/lib/racer/{identity,slabs}`、`/dev` hostPath；应用 sidecar 也需要 hostPath。只对这些受信工作负载授权，**不要关闭全局安全策略**。

```bash
kubectl create namespace kaito-workspace --dry-run=client -o yaml | kubectl apply -f -
kubectl -n kaito-workspace get modelmirrors \
  -o custom-columns='NAME:.metadata.name,MODE:.spec.mode,MODEL:.spec.source.modelID,PHASE:.status.phase'
export MIRROR_NAME=YOUR_READY_MANAGED_MODEL_MIRROR
# 同名 PVC → 已绑定 PV；仅解析本分支使用的 Managed Blob CSI 路径。
# Python 只打印 account/container，不输出可能包含 secret 引用的完整 volumeHandle。
if ! STORAGE_NAMES=$(python3 - <<'PY_STORAGE'
import json, os, re, subprocess

def get(*args):
    return json.loads(subprocess.check_output(['kubectl', *args, '-o', 'json']))

name = os.environ['MIRROR_NAME']
mirror = get('-n', 'kaito-workspace', 'get', 'modelmirror', name)
assert mirror['spec'].get('mode', 'Managed') == 'Managed', '需要 Managed mirror'
assert mirror['status'].get('phase') == 'Ready', '先等待 mirror Ready'
pvc = get('-n', 'kaito-workspace', 'get', 'pvc', name)
assert pvc['status'].get('phase') == 'Bound', 'PVC 尚未 Bound'
pv_name = pvc['spec'].get('volumeName')
assert pv_name, 'PVC 未关联 PV'
csi = get('get', 'pv', pv_name)['spec'].get('csi', {})
assert csi.get('driver') == 'blob.csi.azure.com', '需要 Azure Blob CSI'
parts = csi.get('volumeHandle', '').split('#')
assert len(parts) >= 3, '与本分支支持的 volumeHandle 格式不符'
account, container = parts[1:3]
assert re.fullmatch(r'[a-z0-9]{3,24}', account), 'account 缺失或无效'
assert re.fullmatch(r'[a-z0-9][a-z0-9-]{1,61}[a-z0-9]', container) and '--' not in container, 'container 缺失或无效'
print(account, container)
PY_STORAGE
); then
  echo '无法解析存储位置；停止，不要继续部署 Racer。' >&2
  exit 1
fi
read -r ACCOUNT CONTAINER <<< "$STORAGE_NAMES"
export ACCOUNT CONTAINER
export CACHE_NAME=kaito-model-cache
printf 'Racer origin: https://%s.blob.core.windows.net/%s\n' "$ACCOUNT" "$CONTAINER"
```

`ACCOUNT`/`CONTAINER` **不是要求新建存储资源**：默认 StorageClass 可动态创建它们；这里把已下载模型的存储位置传给 Racer origin 和 KAITO binding。先按上表的 `MODEL` 选择与基线 Workspace 相同、Ready 的 Managed ModelMirror；尚无 mirror 时先完成普通 streaming（不加 Racer annotation）。解析方式与本分支 [WI Blob provider](../pkg/workspace/inference/modelstreaming/azure/wiblob.go) 一致，不适用于任意 CSI driver/Static mirror，也不能仅靠 `volumeAttributes` 或从 PVC/模型名猜测。后续 values 中的 account/container 填这里打印的实际值。**不要覆盖已缓存的 blob key**：用新容器或 revision 前缀发布新版本；本 provider ABI 不携带 ETag，不能保证覆盖写失效。

## 第二步：安装 Racer，并使用 GPU 节点专用 NVMe

固定 **Azure/unbounded v0.27.0**（commit `75d423e09cef984d02cbd7f7d80b0c9eb5150695`），不是追踪 latest。以下依据该 tag 的 [Azure Blob 安装指南](https://github.com/Azure/unbounded/blob/v0.27.0/docs/content/guides/azure-blob-cache.md)、[Racer 节点/磁盘说明](https://github.com/Azure/unbounded/blob/v0.27.0/docs/content/reference/racer.md) 和 [workload overrides](https://github.com/Azure/unbounded/blob/v0.27.0/docs/content/reference/workload-overrides.md)。无需猜测 Helm chart，也不要使用这个版本不存在的 `manage blobs install` 命令。

**已有旧版安装不要直接重跑安装命令。** v0.27.0 将 control/origin 合并到 `daemonset/racer`（容器仍叫 `dataplane`），不再部署独立 `racer-controller` 或每 cache 的 origin DaemonSet。升级前按 [官方迁移说明](https://github.com/Azure/unbounded/blob/v0.27.0/docs/content/reference/racer.md#upgrading-from-per-cache-origin-daemonsets) 为每个 origin identity **额外添加**信任 `system:serviceaccount:unbounded-system:racer` 的 FIC，保留旧 FIC 到 rollout/读取验收成功后再清理。operator 会清理其管理的旧对象；`racer-dataplane-config` 迁移到 `racer-settings`。在批准的维护窗口内协调 operator、dataplane 和应用 sidecar 升级；不要假设跨版本兼容，也不要手动删除 cache、identity 或 checkpoint。本节其余步骤按新测试集群安装编写。

### 2.1 安装 CLI 和 operator

Linux amd64 构建机/管理机执行；下载的是 **tar.gz**，不是裸二进制：

```bash
mkdir -p /tmp/racer-v027 "$HOME/.local/bin"
curl -fL -o /tmp/racer-v027/plugin.tar.gz \
  https://github.com/Azure/unbounded/releases/download/v0.27.0/kubectl-unbounded-linux-amd64.tar.gz
printf '%s  %s\n' \
  6e2e985b2eb634e68a0a9fd9fc0b7f514c562196921912874cb2538d36ca3931 \
  /tmp/racer-v027/plugin.tar.gz | sha256sum -c - || exit 1
tar -xzf /tmp/racer-v027/plugin.tar.gz -C /tmp/racer-v027
install -m 0755 /tmp/racer-v027/kubectl-unbounded "$HOME/.local/bin/kubectl-unbounded"
export PATH="$HOME/.local/bin:$PATH"
kubectl unbounded version
kubectl unbounded install --namespace unbounded-system --timeout=5m
kubectl -n unbounded-system rollout status deployment/unbounded-operator --timeout=5m
```

operator 留在 `unbounded-system`；工作负载仍在 `kaito-workspace`。官方发行版使用 `ghcr.io/azure/{unbounded-operator,racer,racer-object}:v0.27.0`；其 [release BOM](https://github.com/Azure/unbounded/releases/download/v0.27.0/unbounded-release-bom-v0.27.0.json) digest 已与 GHCR 匿名 manifest 核对，CLI 包已与官方 checksums 核对（不等于执行过 cosign 签名验证）。无需私有 Racer 源码；受限网络须先允许访问 GitHub/GHCR，或由管理员提供同版本镜像镜像仓库并使用 CLI 的 `--image-registry`。本仓库不代替提供 AKS、Azure identity、模型数据或 KAITO 构建镜像。

### 2.2 只选择 GPU 节点上的专用空闲 NVMe

**这是可能覆盖磁盘内容的配置，不是格式化教程。不要执行 wipe/mkfs。** 不得选择 OS、AKS 临时/ephemeral OS、kubelet、containerd、模型、PV 或其他已分配磁盘。GPU VM 有 NVMe 不代表有空闲 NVMe；若本地 NVMe 已被 AKS 用作临时盘，必须另备专用可丢弃磁盘，否则此 NVMe 方案的前提不成立。

先列出 GPU、节点池和 taint，再通过已批准的节点登录方式，在每个候选节点的 **host shell** 只读检查：

```bash
kubectl get nodes -L agentpool,node.kubernetes.io/instance-type
kubectl get nodes -o custom-columns='NAME:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu,TAINTS:.spec.taints'
# 以下在候选节点上运行，不是在管理机上运行：
lsblk -e7 -o NAME,PATH,TYPE,SIZE,MODEL,SERIAL,FSTYPE,MOUNTPOINTS
find /dev/disk/by-id -maxdepth 1 -type l -name 'nvme-*' -printf '%f -> %l\n'
findmnt
swapon --show
# 逐台填写实际选中的整盘 by-id，不可复制示例占位符：
DEVICE=/dev/disk/by-id/YOUR_DEDICATED_NVME_ID
readlink -f "$DEVICE"
lsblk -o NAME,TYPE,FSTYPE,MOUNTPOINTS "$DEVICE"
ls -l "/sys/class/block/$(basename "$(readlink -f "$DEVICE")")/holders"
sudo fuser -v "$DEVICE"
```

由磁盘所有者确认：无分区、无 holders、无挂载/交换区/进程占用、无其他用途，且允许丢弃全部内容；上述检查不能代替所有权确认。v0.27.0 还要求设备首尾各 1 MiB 已全零，不满足会拒绝设备；清理旧内容是另一个需要单独批准的运维操作。保留 `/var/lib/racer/identity`；slabs 目录仍用于 checkpoint，不可删除。

确认后才在管理机上为**每一个**所选 GPU 节点设置精确匹配的 annotation 和 opt-in label：

```bash
export GPU_NODE=YOUR_VERIFIED_GPU_NODE
export NVME_BY_ID=YOUR_DEDICATED_NVME_BY_ID_BASENAME
# 仅转义正则元字符，生成 Rust regex 接受的单个 by-id 精确匹配式。
NVME_PATTERN=$(python3 - <<'PY'
import os, re
s = os.environ['NVME_BY_ID']
assert s.startswith('nvme-') and '/' not in s and not re.search(r'-part[0-9]+$', s)
print('^' + ''.join('\\' + c if c in r'\.^$|?*+()[]{}' else c for c in s) + '$')
PY
)
printf '%s\n' "$NVME_PATTERN"
kubectl annotate node "$GPU_NODE" "racer.unbounded-cloud.io/block-devices=$NVME_PATTERN" --overwrite
kubectl label node "$GPU_NODE" kaito.sh/racer-nvme=true --overwrite
```

例如精确形式为 `^nvme-eui\.0123456789abcdef$`，实际 ID 必须来自该节点，**不要用 `nvme-.*`**。annotation 在容器内匹配 `/host/dev/disk/by-id` 的 basename，使用 Rust regex，限 1024 bytes；此流程要求 host 有 `/dev/disk/by-id`。未匹配/设备被拒绝/容量不足会回退到文件 slab，不能把 Pod Ready 当作 NVMe 已启用。

### 2.3 在创建 cache 前限制 dataplane 调度

用 operator 支持的 overrides，而不是直接编辑它管理的 DaemonSet。下面只允许带上一节 opt-in label 的 GPU 节点，并容忍常见 GPU taint；按实际 taint 调整，不要泛化成容忍全部 taint。v0.27.0 的 Azure origin 直接在同一 dataplane Pod 内运行，没有单独的 origin 调度配置。

```bash
cat > /tmp/racer-placement.yaml <<'EOF'
apiVersion: overrides.unbounded-cloud.io/v1alpha1
overrides:
  - component: racer
    kind: DaemonSet
    patch:
      spec:
        template:
          spec:
            nodeSelector:
              kaito.sh/racer-nvme: "true"
            tolerations:
              - key: nvidia.com/gpu
                operator: Exists
                effect: NoSchedule
EOF
kubectl unbounded overrides validate -f /tmp/racer-placement.yaml
# 合并独立 data key，保留已有其他组件 overrides；先审阅是否已有 Racer override。
kubectl -n unbounded-system get configmap unbounded-component-overrides -o yaml
# 仅上条返回 NotFound 时创建：
# kubectl -n unbounded-system create configmap unbounded-component-overrides
python3 - <<'PY' > /tmp/racer-placement-patch.json
import json
print(json.dumps({'data': {'kaito-racer.yaml': open('/tmp/racer-placement.yaml').read()}}))
PY
kubectl -n unbounded-system patch configmap unbounded-component-overrides \
  --type=merge --patch-file=/tmp/racer-placement-patch.json
```

已存在 Racer 部署时，调度 override 的变更可能触发 rollout，须先规划维护窗口。磁盘 annotation **仅在 dataplane 启动时读取**，变更本身不触发重启：新安装应如上先配置再创建 cache；已有安装只能在停止该节点缓存流量后、审批窗口内逐节点替换该节点的 dataplane Pod，验证后再处理下一台。不要盲目 `rollout restart daemonset` 全量重启，不要同时修改所有节点的设备选择。

### 2.4 配置 Azure origin 并创建 ClusterCache

使用专用于 Racer origin 的已有 user-assigned identity；streaming 的 `model-reader` identity 是另一个独立绑定，仍负责 metadata/listing。以下变量由部署者填写，角色 scope 是第一步容器的完整 ARM ID：

```bash
export AKS_RESOURCE_GROUP=YOUR_AKS_RESOURCE_GROUP AKS_NAME=YOUR_AKS_NAME
export IDENTITY_RESOURCE_GROUP=YOUR_IDENTITY_RESOURCE_GROUP
export ORIGIN_IDENTITY_NAME=YOUR_ORIGIN_IDENTITY_NAME
export STORAGE_CONTAINER_RESOURCE_ID=YOUR_CONTAINER_ARM_RESOURCE_ID
ORIGIN_CLIENT_ID=$(az identity show -g "$IDENTITY_RESOURCE_GROUP" -n "$ORIGIN_IDENTITY_NAME" --query clientId -o tsv)
ORIGIN_PRINCIPAL_ID=$(az identity show -g "$IDENTITY_RESOURCE_GROUP" -n "$ORIGIN_IDENTITY_NAME" --query principalId -o tsv)
OIDC_ISSUER=$(az aks show -g "$AKS_RESOURCE_GROUP" -n "$AKS_NAME" --query oidcIssuerProfile.issuerUrl -o tsv)
az identity federated-credential create \
  --resource-group "$IDENTITY_RESOURCE_GROUP" --identity-name "$ORIGIN_IDENTITY_NAME" \
  --name kaito-racer-v027 --issuer "$OIDC_ISSUER" \
  --subject "system:serviceaccount:unbounded-system:racer" \
  --audiences api://AzureADTokenExchange
az role assignment create --assignee-object-id "$ORIGIN_PRINCIPAL_ID" \
  --assignee-principal-type ServicePrincipal --role 'Storage Blob Data Reader' \
  --scope "$STORAGE_CONTAINER_RESOURCE_ID"
kubectl apply -f - <<EOF
apiVersion: racer.unbounded-cloud.io/v1alpha1
kind: ClusterCache
metadata:
  name: ${CACHE_NAME}
spec:
  origin:
    azure:
      containerURL: https://${ACCOUNT}.blob.core.windows.net/${CONTAINER}
      credential:
        workloadIdentity:
          clientID: ${ORIGIN_CLIENT_ID}
EOF
kubectl -n unbounded-system rollout status daemonset/racer --timeout=5m
kubectl -n unbounded-system get leases -l racer.unbounded-cloud.io/control=true
kubectl unbounded overrides status
kubectl -n unbounded-system get daemonset racer -o yaml
kubectl -n unbounded-system get pods -o wide
kubectl -n unbounded-system logs daemonset/racer -c dataplane --tail=100
```

ClusterCache 是 cluster-scoped，没有 workload namespace。创建它后 operator 才自动部署 `racer` DaemonSet（含选举的 control seats 和内置 Azure origin）；短暂 NotFound 时等资源出现再运行 rollout 检查。逐 GPU 节点检查 dataplane 日志中的设备选择/拒绝/文件回退，不能仅看 DaemonSet 的期望数或单个 Pod 日志；所有推理 rank 必须落在实际有 Racer socket 的节点。origin 不能切换容器，换源要新建 cache。SAS/shared key 不是此安装路径的鉴权方式，不能给 provider/sidecar 放 Azure token。origin 使用 operator 为 `racer` ServiceAccount 投射的 Entra token；这不替代应用自身 metadata/listing 所用的 streaming WI。不要添加未经 v0.27.0 定义的 `rawNVMe` CRD 字段；裸设备由上面的 Node annotation 配置。

## 第三步：构建并推送 KAITO 控制器和 runtime

**不需要新建专用 base 镜像。** 复用已有、可拉取的兼容 KAITO vLLM runtime（保留 Ray/vLLM 环境，包含 `runai-model-streamer` 和 `runai-model-streamer-azure` **均为 0.16.0**）。下例使用与本分支 `base_images.yaml` 一致、已在 MCR 发布的 `0.6.3`。这里只构建控制器和加入 provider 的派生 runtime。Dockerfile 会检查 ABI；已确认该 tag 存在，但尚未实际构建验证其兼容性。

```bash
git clone --branch poc/racer-model-streaming https://github.com/andyzhangx/kaito.git
cd kaito
export REGISTRY=YOUR_REGISTRY
export OUTPUT_TYPE=type=docker ARCH=amd64 PLATFORMS=linux/amd64
export VERSION=0.13.0-racer
export EXISTING_RUNTIME_BASE=mcr.microsoft.com/aks/kaito/kaito-base:0.6.3
export RACER_STREAMING_IMAGE="$REGISTRY/kaito-vllm-racer:$VERSION"
make docker-build-workspace ARCH=amd64
docker buildx build --platform linux/amd64 --load \
  --build-arg KAITO_BASE_IMAGE="$EXISTING_RUNTIME_BASE" \
  -f docker/racer-streaming/Dockerfile -t "$RACER_STREAMING_IMAGE" .
docker push "$REGISTRY/workspace:$VERSION"
docker push "$RACER_STREAMING_IMAGE"
```

`type=docker` / `--load` 只加载本机镜像，不推送；隔离 builder 需要能从仓库读取已有 runtime。`KAITO_BASE_IMAGE` 是此 Dockerfile 实际使用的构建参数，不是要求另建镜像。Racer 自身镜像不由这些 KAITO 构建命令生成。

## 第四步：配置现有 KAITO 控制器

编辑 [values 示例](../examples/racer-model-streaming-values.yaml)：`namespace: kaito-workspace`，`account`/`container`/`cacheName` 使用前面的实际值；`runtimeImage` 填第三步推送的镜像；`objectImage` 使用与 server 同版本的公开 sidecar：

```text
ghcr.io/azure/racer-object@sha256:5c43d6c4dd2a18998568be84252040f88f6be7b93ec2e55fdf0ddeb8a92663ba
```

保留原有 Workload Identity、StorageClass 等 values。只升级**现有** workspace Helm release，不在托管控制器旁安装第二份；非 Helm/AKS 托管安装先按其受支持的更新方式切换自定义控制器。

```bash
helm list -A
export KAITO_RELEASE=YOUR_EXISTING_WORKSPACE_RELEASE
export KAITO_CONTROLLER_NAMESPACE=YOUR_EXISTING_CONTROLLER_NAMESPACE
helm get values "$KAITO_RELEASE" -n "$KAITO_CONTROLLER_NAMESPACE" -o yaml > /tmp/kaito-current-values.yaml
helm upgrade "$KAITO_RELEASE" ./charts/kaito/workspace \
  -n "$KAITO_CONTROLLER_NAMESPACE" \
  -f /tmp/kaito-current-values.yaml -f examples/racer-model-streaming-values.yaml \
  --set-string image.repository="$REGISTRY/workspace" --set-string image.tag="$VERSION" \
  --wait --timeout=5m
```

这里 `KAITO_CONTROLLER_NAMESPACE` 保持原安装值；不是要求移动控制器。values 文件是 Helm 输入，不能 `kubectl apply`。

## 第五步：创建 Workspace 并验收

编辑 [Workspace 示例](../examples/racer-model-streaming-poc.yaml) 中 `model-reader` 的 client ID 和 GPU/preset；为该 identity 配置 FIC subject **`system:serviceaccount:kaito-workspace:model-reader`** 及原 Streaming 所需 Blob 权限（遵循第一步指南）。实际所选 GPU 节点还须满足示例的 `apps: racer-model` selector 和资源需求；KAITO 新建的 GPU 节点必须先完成第二步磁盘审核/标注，不能假设自带可用缓存。

```bash
kubectl apply -f examples/racer-model-streaming-poc.yaml
kubectl -n kaito-workspace get workspace racer-model -o yaml
kubectl -n kaito-workspace get pods -o wide
kubectl -n kaito-workspace get statefulsets -o yaml
```

启用开关是 `kaito.sh/model-cache: racer`，runtime 用 `kaito.sh/runtime: vllm`。用新 Workspace 测试；旧 StatefulSet 并非所有字段都能随 annotation 更新。

逐一检查**全部 rank**：runtime 镜像正确、provider env 存在、`racer-object` 为 `restartPolicy: Always` 的 native sidecar；它挂载 `/run/racer/${CACHE_NAME}/client`，运行 `sidecar --api=azure --cache=... --listen=127.0.0.1:18080`。KAITO 已负责注入，不要再开启 Racer webhook 的 `object-sidecar` 或 `client-socket` 注入标签。endpoint 仅 Pod 内可见，无 Service/Ingress；HTTP 路径是 **`/<container>/<blob-key>`**，不是只有 blob key。

在每个推理 Pod 的应用容器内，用 Python 做一次真实范围读取（替换实际 blob key，不下载整个权重）：

```bash
export POD=YOUR_INFERENCE_POD APP_CONTAINER=YOUR_INFERENCE_CONTAINER
export BLOB_KEY=YOUR_ACTUAL_SAFETENSORS_BLOB_KEY
kubectl -n kaito-workspace exec -i "$POD" -c "$APP_CONTAINER" -- \
  python3 - "$CONTAINER" "$BLOB_KEY" <<'PY'
import sys, urllib.parse, urllib.request
path = '/' + urllib.parse.quote(sys.argv[1], safe='') + '/' + urllib.parse.quote(sys.argv[2], safe='/')
r = urllib.request.urlopen(urllib.request.Request('http://127.0.0.1:18080' + path, headers={'Range': 'bytes=0-15'}), timeout=30)
body = r.read()
assert r.status == 206 and len(body) == 16
assert r.headers['Content-Range'].startswith('bytes 0-15/')
print(r.status, r.headers['Content-Range'], body.hex())
PY
```

再验证模型 Ready/推理请求、冷读和热读的一致性、origin/sidecar 错误及缓存命中。403 排查 account/container 和 origin FIC/RBAC；503/hostPath 失败排查该 GPU 节点的 dataplane/socket；明确区分真实 NVMe 使用与 slab 回退。对比直接 Azure 时，使用同一模型/资源的新 Workspace 并设置 `kaito.sh/model-cache: disabled`；不清空/格式化磁盘，不删除共享 cache 或 identity。

## 验证边界

此前 PoC 记录过 Go controller/provider 单测、provider race、9 项真实 `.so` ABI 测试、控制器编译及 Helm 渲染；不代表这些检查在每次文档升级中均已重跑。本次 v0.27.0 升级核对了 tag 源码、发行包校验和、CLI 帮助、overrides 离线校验及 release BOM/公开镜像 manifest。源码中的 sidecar flags、`/run/racer/<cache>/client`、`/<container>/<blob>`、单 Range/206/Content-Range 契约仍与 provider 一致，无需改变 KAITO provider ABI；这不是运行真实 v0.27.0 dataplane 的集成测试。本次重跑的 controller/provider 普通单测、Helm lint/render、10 个存储解析 mock 场景及 Bash 语法检查通过；环境缺少 GNU Make 和 C compiler，无法重跑完整 Make 检查、race 或 `.so` ABI 测试。**仍未构建/推送这些 Docker 镜像，未安装集群，未验证 AKS NVMe、Azure WI、GPU 或缓存性能**。本地/mock 测试不能替代以上部署验收。
