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
kubectl get modelmirrors -A
kubectl -n kaito-workspace get pvc
export MIRROR_PVC=YOUR_COMPLETED_MODEL_MIRROR_PVC
PV=$(kubectl -n kaito-workspace get pvc "$MIRROR_PVC" -o jsonpath='{.spec.volumeName}')
kubectl get pv "$PV" -o jsonpath='{.spec.csi.volumeAttributes}'; echo
export ACCOUNT=YOUR_ACTUAL_STORAGE_ACCOUNT
export CONTAINER=YOUR_ACTUAL_MIRROR_CONTAINER
export CACHE_NAME=kaito-model-cache
```

从该模型的 PV 属性/实际 Azure 资源确认 account/container；不能用模型名、PVC 名或 StorageClass 名猜测。后续 origin 和 KAITO binding 必须使用同一组值。**不要覆盖已缓存的 blob key**：用新容器或 revision 前缀发布新版本；本 provider ABI 不携带 ETag，不能保证覆盖写失效。

## 第二步：安装 Racer，并使用 GPU 节点专用 NVMe

固定 **Azure/unbounded v0.18.0**。以下依据该 tag 的 [Azure Blob 安装指南](https://github.com/Azure/unbounded/blob/v0.18.0/docs/content/guides/azure-blob-cache.md)、[Racer 节点/磁盘说明](https://github.com/Azure/unbounded/blob/v0.18.0/docs/content/reference/racer.md) 和 [workload overrides](https://github.com/Azure/unbounded/blob/v0.18.0/docs/content/reference/workload-overrides.md)。无需猜测 Helm chart，也不要使用这个版本不存在的 `manage blobs install` 命令。

### 2.1 安装 CLI 和 operator

Linux amd64 构建机/管理机执行；下载的是 **tar.gz**，不是裸二进制：

```bash
mkdir -p /tmp/racer-v018 "$HOME/.local/bin"
curl -fL -o /tmp/racer-v018/plugin.tar.gz \
  https://github.com/Azure/unbounded/releases/download/v0.18.0/kubectl-unbounded-linux-amd64.tar.gz
printf '%s  %s\n' \
  dd9e41f857c08c783071c8d6772bcf477f3a26a7a47d556dd9ebcb0197a0daa0 \
  /tmp/racer-v018/plugin.tar.gz | sha256sum -c -
tar -xzf /tmp/racer-v018/plugin.tar.gz -C /tmp/racer-v018
install -m 0755 /tmp/racer-v018/kubectl-unbounded "$HOME/.local/bin/kubectl-unbounded"
export PATH="$HOME/.local/bin:$PATH"
kubectl unbounded version
kubectl unbounded install --namespace unbounded-system --timeout=5m
kubectl -n unbounded-system rollout status deployment/unbounded-operator --timeout=5m
```

operator 留在 `unbounded-system`；工作负载仍在 `kaito-workspace`。官方发行版使用 `ghcr.io/azure/{unbounded-operator,racer-controller,racer-dataplane,racer-object}:v0.18.0`，这些镜像的 manifest 已通过匿名拉取接口核对。无需私有 Racer 源码；受限网络须先允许访问 GitHub/GHCR，或由管理员提供同版本镜像镜像仓库并使用 CLI 的 `--image-registry`。本仓库不代替提供 AKS、Azure identity、模型数据或 KAITO 构建镜像。

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

由磁盘所有者确认：无分区、无 holders、无挂载/交换区/进程占用、无其他用途，且允许丢弃全部内容；上述检查不能代替所有权确认。v0.18.0 还要求设备首尾各 1 MiB 已全零，不满足会拒绝设备；清理旧内容是另一个需要单独批准的运维操作。保留 `/var/lib/racer/identity`；slabs 目录仍用于 checkpoint，不可删除。

确认后才在管理机上为**每一个**所选 GPU 节点设置精确匹配的 annotation 和 opt-in label：

```bash
export GPU_NODE=YOUR_VERIFIED_GPU_NODE
export NVME_BY_ID=YOUR_DEDICATED_NVME_BY_ID_BASENAME
# 仅转义正则元字符，生成 Go/Rust 都接受的单个 by-id 精确匹配式。
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

例如精确形式为 `^nvme-eui\.0123456789abcdef$`，实际 ID 必须来自该节点，**不要用 `nvme-.*`**。annotation 匹配 `/dev/disk/by-id` 的 basename，支持 Go/Rust 正则；未匹配/设备被拒绝/容量不足会回退到文件 slab，不能把 Pod Ready 当作 NVMe 已启用。

### 2.3 在创建 cache 前限制 dataplane 调度

用 operator 支持的 overrides，而不是直接编辑它管理的 DaemonSet。下面只允许带上一节 opt-in label 的 GPU 节点，并容忍常见 GPU taint；按实际 taint 调整，不要泛化成容忍全部 taint。origin 会继承 dataplane 的 selector/affinity/tolerations。

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
  --name kaito-racer-origin --issuer "$OIDC_ISSUER" \
  --subject "system:serviceaccount:unbounded-system:racer-origin-${CACHE_NAME}" \
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
kubectl -n unbounded-system rollout status deployment/racer-controller --timeout=5m
kubectl -n unbounded-system rollout status daemonset/racer-dataplane --timeout=5m
kubectl -n unbounded-system rollout status "daemonset/racer-origin-${CACHE_NAME}" --timeout=5m
kubectl unbounded overrides status
kubectl -n unbounded-system get daemonset racer-dataplane "racer-origin-${CACHE_NAME}" -o yaml
kubectl -n unbounded-system get pods -o wide
kubectl -n unbounded-system logs daemonset/racer-dataplane -c dataplane --tail=100
kubectl -n unbounded-system logs "daemonset/racer-origin-${CACHE_NAME}" --tail=100
```

ClusterCache 是 cluster-scoped，没有 workload namespace。创建它后 operator 才自动部署 Racer controller/dataplane/origin；短暂 NotFound 时等资源出现再运行 rollout 检查。逐 GPU 节点检查 dataplane 日志中的设备选择/拒绝/文件回退，不能仅看 DaemonSet 的期望数或单个 Pod 日志；所有推理 rank 必须落在实际有 Racer socket 的节点。origin 不能切换容器，换源要新建 cache。SAS/shared key 不是此安装路径的鉴权方式，不能给 provider/sidecar 放 Azure token。

## 第三步：构建并推送 KAITO 控制器和 runtime

本分支的 `docker-build-racer-streaming` **确实依赖已有 KAITO vLLM base**：它只加入 provider 并检查 `runai-model-streamer{,-azure}==0.16.0` ABI，不能删掉实际构建依赖。base 不是第三个运行组件；已有兼容、可从仓库拉取的 base 时跳过它的构建/推送。

```bash
git clone --branch poc/racer-model-streaming https://github.com/andyzhangx/kaito.git
cd kaito
export REGISTRY=YOUR_REGISTRY
export OUTPUT_TYPE=type=docker ARCH=amd64 PLATFORMS=linux/amd64
export VERSION=0.13.0-racer
export KAITO_BASE_IMG_NAME=kaito-base KAITO_BASE_IMG_TAG="$VERSION"
export RACER_STREAMING_IMAGE="$REGISTRY/kaito-vllm-racer:$VERSION"
make docker-build-workspace ARCH=amd64
# 若已有兼容 base，将上面 NAME/TAG 改为已有值并跳过下面两行：
make docker-build-kaito-base ARCH=amd64
docker push "$REGISTRY/$KAITO_BASE_IMG_NAME:$KAITO_BASE_IMG_TAG"
make docker-build-racer-streaming ARCH=amd64
docker push "$REGISTRY/workspace:$VERSION"
docker push "$RACER_STREAMING_IMAGE"
```

`type=docker` 只加载本机镜像，不推送；buildx 的隔离 builder 需要能从仓库读取 base。Racer 自身镜像不由这些 KAITO 构建目标生成。

## 第四步：配置现有 KAITO 控制器

编辑 [values 示例](../examples/racer-model-streaming-values.yaml)：`namespace: kaito-workspace`，`account`/`container`/`cacheName` 使用前面的实际值；`runtimeImage` 填第三步推送的镜像；`objectImage` 使用与 server 同版本的公开 sidecar：

```text
ghcr.io/azure/racer-object@sha256:83156c106d243d5c4552698fe3eddc614128f3f74210d025b0a556800be77b1e
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

逐一检查**全部 rank**：runtime 镜像正确、provider env 存在、`racer-object` 为 `restartPolicy: Always` 的 native sidecar；它挂载 `/run/racer/${CACHE_NAME}/client`，运行 `sidecar --api=azure --cache=... --listen=127.0.0.1:18080`。KAITO 已负责注入，不要再开启 Racer webhook 的 sidecar 注入标签。endpoint 仅 Pod 内可见，无 Service/Ingress；HTTP 路径是 **`/<container>/<blob-key>`**，不是只有 blob key。

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

已有本地检查覆盖 Go controller/provider 单测、provider race、9 项真实 `.so` ABI 测试、控制器编译及 Helm 渲染。本次安装文档额外核对了 v0.18.0 源码、发行包校验和、CLI 帮助和公开镜像 manifest；示例用离线回归检查。**仍未构建/推送这些 Docker 镜像，未安装集群，未验证 AKS NVMe、Azure WI、GPU 或缓存性能**。本地/mock 测试不能替代以上部署验收。
