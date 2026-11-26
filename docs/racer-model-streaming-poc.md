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

`ACCOUNT`/`CONTAINER` **不是要求新建存储资源**：默认 StorageClass 可动态创建它们；这里把已下载模型的存储位置传给 Racer origin 和 KAITO binding。先按上表的 `MODEL` 选择与基线 Workspace 相同、Ready 的 Managed ModelMirror；尚无 mirror 时先完成普通 streaming（不加 Racer annotation）。解析方式与本分支 [WI Blob provider](../internal/inference/modelstreaming/azure/wiblob.go) 一致，不适用于任意 CSI driver/Static mirror，也不能仅靠 `volumeAttributes` 或从 PVC/模型名猜测。后续 values 中的 account/container 填这里打印的实际值。**不要覆盖已缓存的 blob key**：用新容器或 revision 前缀发布新版本；本 provider ABI 不携带 ETag，不能保证覆盖写失效。

## 第二步：首次安装 Racer，并使用 GPU 节点专用 NVMe

本节只用于**尚未安装 Racer 的测试集群**，固定 **Azure/unbounded v0.27.0**（commit `75d423e09cef984d02cbd7f7d80b0c9eb5150695`）。依据该 tag 的 [Azure Blob 安装指南](https://github.com/Azure/unbounded/blob/v0.27.0/docs/content/guides/azure-blob-cache.md)、[节点/磁盘说明](https://github.com/Azure/unbounded/blob/v0.27.0/docs/content/reference/racer.md) 和 [workload overrides](https://github.com/Azure/unbounded/blob/v0.27.0/docs/content/reference/workload-overrides.md)。顺序是：检查环境 → 安装 operator → 确认磁盘 → 限定节点 → 配置 origin identity → 创建 cache → 验证。**不要提前创建 ClusterCache。**

### 2.1 管理机：确认目标集群和首次安装前提

以下管理命令在同一个 Linux amd64 Bash 会话执行，需要 `kubectl`、`az`、`curl`、`python3` 和第一步导出的 `ACCOUNT`、`CONTAINER`、`CACHE_NAME`。先填写 AKS 的实际订阅、资源组和名称；核对 kube-context 确实指向此 AKS，而不只是名称相似：

```bash
set -euo pipefail
: "${ACCOUNT:?先完成第一步}" "${CONTAINER:?先完成第一步}" "${CACHE_NAME:?先完成第一步}"
export AKS_SUBSCRIPTION_ID=YOUR_AKS_SUBSCRIPTION_ID
export AKS_RESOURCE_GROUP=YOUR_AKS_RESOURCE_GROUP AKS_NAME=YOUR_AKS_NAME
kubectl config current-context
kubectl cluster-info
az account show --query '{subscription:id,tenant:tenantId}' -o yaml
az aks show --subscription "$AKS_SUBSCRIPTION_ID" -g "$AKS_RESOURCE_GROUP" -n "$AKS_NAME" \
  --query '{id:id,fqdn:fqdn,privateFqdn:privateFqdn,oidc:oidcIssuerProfile.issuerUrl,workloadIdentity:securityProfile.workloadIdentity.enabled}' -o yaml
kubectl get deployments,daemonsets -A
kubectl get crds -o name
kubectl get namespace unbounded-system --ignore-not-found
```

**人工检查通过后再继续**：OIDC/Workload Identity 已启用；没有 Unbounded operator、Racer 工作负载或 `racer.unbounded-cloud.io` CRD。发现已有安装、残留资源或不明用途的 `unbounded-system` namespace，停止并审阅现状，不覆盖安装。查询失败（权限、网络等）也必须停止；`--ignore-not-found` 仅允许明确的 NotFound，不代表忽略所有错误。

### 2.2 管理机：安装 CLI 和 operator

下载的是 **tar.gz**，不是裸二进制：

```bash
RACER_INSTALL_DIR=$(mktemp -d /tmp/racer-v027.XXXXXX)
mkdir -p "$HOME/.local/bin"
curl -fL -o "$RACER_INSTALL_DIR/plugin.tar.gz" \
  https://github.com/Azure/unbounded/releases/download/v0.27.0/kubectl-unbounded-linux-amd64.tar.gz
printf '%s  %s\n' \
  6e2e985b2eb634e68a0a9fd9fc0b7f514c562196921912874cb2538d36ca3931 \
  "$RACER_INSTALL_DIR/plugin.tar.gz" | sha256sum -c -
tar -xzf "$RACER_INSTALL_DIR/plugin.tar.gz" -C "$RACER_INSTALL_DIR"
install -m 0755 "$RACER_INSTALL_DIR/kubectl-unbounded" "$HOME/.local/bin/kubectl-unbounded"
export PATH="$HOME/.local/bin:$PATH"
kubectl unbounded version # 应为 0.27.0，commit 75d423e09
kubectl unbounded install --namespace unbounded-system --timeout=5m
kubectl -n unbounded-system rollout status deployment/unbounded-operator --timeout=5m
kubectl wait --for=condition=Established crd/clustercaches.racer.unbounded-cloud.io --timeout=5m
```

operator 留在 `unbounded-system`，应用仍在 `kaito-workspace`。**此时没有 `daemonset/racer` 是正常的**：它在首次创建 ClusterCache 后才部署。官方镜像为 `ghcr.io/azure/{unbounded-operator,racer,racer-object}:v0.27.0`；[release BOM](https://github.com/Azure/unbounded/releases/download/v0.27.0/unbounded-release-bom-v0.27.0.json) digest 已与 GHCR 匿名 manifest 核对，CLI 包已与官方 checksums 核对（不等于执行过 cosign 签名验证）。无需私有 Racer 源码；管理机/集群需能访问 GitHub/GHCR，受限网络可由管理员将同版本镜像同步到可访问的仓库，并使用 CLI 的 `--image-registry`。

### 2.3 GPU 节点 host：确认目标 NVMe

下面使用已检查的 A100 节点及设备作为示例；**Racer raw-device 会覆盖磁盘内容，须先由所有者确认该盘专供可丢弃缓存并批准使用**。不得使用 OS/ephemeral OS、kubelet、containerd、模型或 PV 已占用的盘。

在该 GPU 节点的 **host shell** 只读检查：

```bash
DEVICE=/dev/disk/by-id/nvme-Microsoft_NVMe_Direct_Disk_2fe19d8b05b600000001
readlink -f "$DEVICE" # 当前节点应为 /dev/nvme0n1
lsblk -o NAME,TYPE,SIZE,FSTYPE,MOUNTPOINTS "$DEVICE"
ls -l "/sys/class/block/$(basename "$(readlink -f "$DEVICE")")/holders"
swapon --show
```

无挂载不等于可安全覆盖，仍需确认无分区、holders、进程占用或其他用途。v0.27.0 还要求设备首尾各 1 MiB 全零；不满足时停止并另行申请清理批准，本节不执行擦盘。`...000001` 与 `...000001_1` 是同一 `/dev/nvme0n1` 的两个别名，选前者即可。

### 2.4 管理机：设置固定正则并限制调度

**仅在完成上述确认后**，在管理机直接设置精确表达式，无需动态生成：

```bash
GPU_NODE=aks-ws4fd45a3ae-15362417-vmss000000
kubectl annotate node "$GPU_NODE" \
  'racer.unbounded-cloud.io/block-devices=^nvme-Microsoft_NVMe_Direct_Disk_2fe19d8b05b600000001$'
kubectl label node "$GPU_NODE" kaito.sh/racer-nvme=true
```

正则匹配 `/host/dev/disk/by-id` 的 basename，不带路径。**节点名和序列号仅适用于当前示例**，其他节点或重建后需重新核对，勿改成 `nvme-.*`。不加 `--overwrite`，已有配置冲突时先审阅。必须在首次创建 cache 前完成；设备选择仅在 dataplane 启动时读取。

**调度限制仍须保留**：磁盘正则不限制 Pod 调度，其他节点没有匹配设备时可能回退到文件缓存。安装后按后文 slab 指标确认实际使用了 NVMe。

用 operator 支持的 overrides 限制调度，不直接编辑其 DaemonSet。下面只允许 opt-in 节点并容忍常见 GPU taint；按实际 taint 调整，不泛化为容忍全部 taint：

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
kubectl -n unbounded-system get configmap unbounded-component-overrides \
  --ignore-not-found -o yaml
```

若 ConfigMap 已存在，**先审阅所有 data key**：有 Racer override 或 `kaito-racer.yaml` 就停止，不覆盖；只有其他组件配置才继续。以下仅在明确 NotFound 时创建；已有 ConfigMap 则合并新 key，保留其他组件配置。任何查询、创建或 patch 失败均停止：

```bash
OVERRIDES_CM=$(kubectl -n unbounded-system get configmap unbounded-component-overrides \
  --ignore-not-found -o name)
if [ -z "$OVERRIDES_CM" ]; then
  kubectl -n unbounded-system create configmap unbounded-component-overrides \
    --from-file=kaito-racer.yaml=/tmp/racer-placement.yaml
else
  python3 - <<'PY' > /tmp/racer-placement-patch.json
import json
print(json.dumps({'data': {'kaito-racer.yaml': open('/tmp/racer-placement.yaml').read()}}))
PY
  kubectl -n unbounded-system patch configmap unbounded-component-overrides \
    --type=merge --patch-file=/tmp/racer-placement-patch.json
fi
```

### 2.5 管理机：给 Azure origin 配置独立 identity

本例要求**已准备好专用于 Racer origin 的 user-assigned identity**，并有创建 FIC/角色分配的权限；尚未准备时先请 Azure 管理员创建再继续。不要复用 streaming 的 `model-reader` 绑定，它仍独立负责应用 metadata/listing。填写 identity 和**第一步已有 storage account** 各自的实际订阅/资源组，不能假定它们与 AKS 相同，也不要新建空容器：

```bash
export IDENTITY_SUBSCRIPTION_ID=YOUR_IDENTITY_SUBSCRIPTION_ID
export IDENTITY_RESOURCE_GROUP=YOUR_IDENTITY_RESOURCE_GROUP
export ORIGIN_IDENTITY_NAME=YOUR_DEDICATED_ORIGIN_IDENTITY_NAME
export STORAGE_SUBSCRIPTION_ID=YOUR_EXISTING_STORAGE_SUBSCRIPTION_ID
export STORAGE_RESOURCE_GROUP=YOUR_EXISTING_STORAGE_RESOURCE_GROUP
ORIGIN_CLIENT_ID=$(az identity show --subscription "$IDENTITY_SUBSCRIPTION_ID" \
  -g "$IDENTITY_RESOURCE_GROUP" -n "$ORIGIN_IDENTITY_NAME" --query clientId -o tsv)
ORIGIN_PRINCIPAL_ID=$(az identity show --subscription "$IDENTITY_SUBSCRIPTION_ID" \
  -g "$IDENTITY_RESOURCE_GROUP" -n "$ORIGIN_IDENTITY_NAME" --query principalId -o tsv)
OIDC_ISSUER=$(az aks show --subscription "$AKS_SUBSCRIPTION_ID" \
  -g "$AKS_RESOURCE_GROUP" -n "$AKS_NAME" --query oidcIssuerProfile.issuerUrl -o tsv)
STORAGE_ACCOUNT_RESOURCE_ID=$(az storage account show --subscription "$STORAGE_SUBSCRIPTION_ID" \
  -g "$STORAGE_RESOURCE_GROUP" -n "$ACCOUNT" --query id -o tsv)
: "${ORIGIN_CLIENT_ID:?}" "${ORIGIN_PRINCIPAL_ID:?}" "${OIDC_ISSUER:?}" "${STORAGE_ACCOUNT_RESOURCE_ID:?}"
STORAGE_CONTAINER_RESOURCE_ID="$STORAGE_ACCOUNT_RESOURCE_ID/blobServices/default/containers/$CONTAINER"
printf 'Origin role scope: %s\n' "$STORAGE_CONTAINER_RESOURCE_ID"
az identity federated-credential create --subscription "$IDENTITY_SUBSCRIPTION_ID" \
  --resource-group "$IDENTITY_RESOURCE_GROUP" --identity-name "$ORIGIN_IDENTITY_NAME" \
  --name kaito-racer --issuer "$OIDC_ISSUER" \
  --subject system:serviceaccount:unbounded-system:racer \
  --audiences api://AzureADTokenExchange
az role assignment create --subscription "$STORAGE_SUBSCRIPTION_ID" \
  --assignee-object-id "$ORIGIN_PRINCIPAL_ID" --assignee-principal-type ServicePrincipal \
  --role 'Storage Blob Data Reader' --scope "$STORAGE_CONTAINER_RESOURCE_ID"
```

身份与 AKS OIDC 的 Entra tenant 必须匹配本环境；不要把不同 tenant 的 identity 当作默认可用。origin 使用 operator 为 `unbounded-system/racer` ServiceAccount 投射的 token；不向 provider/sidecar 放 Azure token，此路径不支持 SAS/shared key。

### 2.6 管理机：最后创建 ClusterCache

确认节点、磁盘、placement 和 identity 均已就绪，再创建（不是覆盖）第一步模型容器对应的 cache：

```bash
kubectl create -f - <<EOF
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
kubectl -n unbounded-system get daemonset racer
```

ClusterCache 是 cluster-scoped，无需 workload namespace。operator 此时才部署 `daemonset/racer`，其中 `dataplane` 容器包含 Azure origin 和选举的 control seats。若最后一条返回 NotFound，稍等后重查；其他错误立即排查，出现 DaemonSet 后再做下一节检查。容器 URL 不可改，换源要新建 cache；不要添加 v0.27.0 没有的 `rawNVMe` 字段。

### 2.7 管理机：确认每台目标节点真正使用 NVMe

```bash
kubectl -n unbounded-system rollout status daemonset/racer --timeout=5m
kubectl unbounded overrides status
kubectl get nodes -l kaito.sh/racer-nvme=true
kubectl -n unbounded-system get daemonset racer -o yaml
kubectl -n unbounded-system get pods -o wide
kubectl -n unbounded-system get leases -l racer.unbounded-cloud.io/control=true
# 对每台目标 GPU 节点，填写该节点实际的 Racer Pod 名并逐个检查：
RACER_POD=YOUR_RACER_POD_ON_THIS_GPU_NODE
kubectl -n unbounded-system logs "$RACER_POD" -c dataplane
```

前两项通过后再继续部署 KAITO Racer Workspace；实际读取在第三至五步完成后验收：

- overrides 无错误，实际 DaemonSet selector/taint 容忍符合计划；**每台 opt-in GPU 节点恰有一个 Ready Racer Pod**，非目标节点没有 Racer Pod。不能只看 DaemonSet 汇总就绪数。
- 每个 Pod 的启动日志确认选中该节点的预期设备；排查设备拒绝、容量不足和文件 slab 回退。无匹配/设备不可用时会回退，**Ready 不等于 NVMe 成功**；没有设备使用证据就停止，不能把文件缓存测试当作 NVMe 测试。`/var/lib/racer/slabs` 仍保存 checkpoint，其目录存在不代表回退；保留 identity/checkpoint 目录。
- 日志无 origin 鉴权错误，但这不等于读权限已验收。完成第三至五步后，必须用第一步已有模型执行实际冷/热读取，检查 origin 读取成功及缓存命中；FIC/RBAC 传播期间的 403/503 要诊断，不用透明 Azure 回退掩盖失败。所有推理 rank 必须落在实际有 Racer socket 的目标节点。

以上仅是首次安装流程，本文尚未执行实际 AKS/GPU 部署或 NVMe/读取验收。

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
