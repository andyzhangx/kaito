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
assert mirror['spec']['source']['modelID'] == 'Qwen/Qwen3-Coder-30B-A3B-Instruct', '请选择本次 Qwen 模型的 mirror'
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

用 operator 支持的 overrides 限制调度，不直接编辑其 DaemonSet。先检查目标节点的实际 taint：

```bash
kubectl get node "$GPU_NODE" -o jsonpath='{.spec.taints}{"\n"}'
```

下面只允许 opt-in 节点，并容忍 `nvidia.com/gpu:NoSchedule` 和本 PoC A100 节点使用的 `sku=gpu:NoSchedule`。两个 key 不同，不能互相匹配；若遗漏 `sku=gpu`，DaemonSet 可能显示 `DESIRED=0`，而不是创建 Pending Pod。按实际 taint 调整，不泛化为容忍全部 taint：

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
              - key: sku
                operator: Equal
                value: gpu
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

### 2.5 管理机：沿用 Model Streaming 的 identity，为 Azure origin 新增绑定

本 PoC 可以复用 [KAITO Model Streaming 官方指南](https://kaito-project.github.io/kaito/docs/model-mirror-streaming/) 中的 kubelet user-assigned managed identity，并沿用同名的 `KUBELET_*`、`OIDC_ISSUER` 变量，无需再创建独立的 origin identity。

**复用 identity 不等于复用 ServiceAccount/FIC。** 保留第一步 streaming ServiceAccount 的原有 FIC，它仍负责应用 metadata/listing；在同一个 identity 上，为 Racer 的 `system:serviceaccount:unbounded-system:racer` **新增另一条 FIC**，不能替换原绑定。

如果仍在执行官方指南的 shell 中，可以沿用其 identity 变量。为方便重新打开 shell 后执行，下面从 2.1 的 AKS 参数恢复官方指南同名变量；必须使用第一步 Model Streaming 所在的同一个集群：

```bash
: "${AKS_SUBSCRIPTION_ID:?}" "${AKS_RESOURCE_GROUP:?}" "${AKS_NAME:?}"
SUBSCRIPTION="$AKS_SUBSCRIPTION_ID"
RESOURCE_GROUP="$AKS_RESOURCE_GROUP"
CLUSTER_NAME="$AKS_NAME"

KUBELET_RESOURCE_ID=$(az aks show --subscription "$SUBSCRIPTION" \
  --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" \
  --query identityProfile.kubeletidentity.resourceId -o tsv)
KUBELET_CLIENT_ID=$(az aks show --subscription "$SUBSCRIPTION" \
  --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" \
  --query identityProfile.kubeletidentity.clientId -o tsv)
KUBELET_OBJECT_ID=$(az aks show --subscription "$SUBSCRIPTION" \
  --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" \
  --query identityProfile.kubeletidentity.objectId -o tsv)
: "${KUBELET_RESOURCE_ID:?}" "${KUBELET_CLIENT_ID:?}" "${KUBELET_OBJECT_ID:?}"
KUBELET_IDENTITY_NAME=$(basename "$KUBELET_RESOURCE_ID")
KUBELET_IDENTITY_RG=$(echo "$KUBELET_RESOURCE_ID" | cut -d/ -f5)
OIDC_ISSUER=$(az aks show --subscription "$SUBSCRIPTION" \
  --resource-group "$RESOURCE_GROUP" --name "$CLUSTER_NAME" \
  --query oidcIssuerProfile.issuerUrl -o tsv)
: "${KUBELET_IDENTITY_NAME:?}" "${KUBELET_IDENTITY_RG:?}" "${OIDC_ISSUER:?}"

# 从 identity resource ID 获取实际订阅，不假定 identity 一定位于 AKS 订阅。
# 使用区别于 streaming 原 FIC 的名称；若同名 FIC 已存在，先核对其内容。
az identity federated-credential create \
  --subscription "$(echo "$KUBELET_RESOURCE_ID" | cut -d/ -f3)" \
  --resource-group "$KUBELET_IDENTITY_RG" \
  --identity-name "$KUBELET_IDENTITY_NAME" \
  --name "racer-wi-${CLUSTER_NAME}" \
  --issuer "$OIDC_ISSUER" \
  --subject "system:serviceaccount:unbounded-system:racer" \
  --audiences "api://AzureADTokenExchange"
```

**检查已有 Blob 权限是否覆盖实际 origin。** 官方指南在 AKS `MC_` 资源组作用域授予 `Storage Blob Data Reader`；如果第一步使用的 storage account 位于该资源组中，可以继承已有权限，不必重复授权。如果 storage account 位于其他资源组/订阅，则不能假定已覆盖。

先填写**第一步已有 storage account** 的实际订阅/资源组并检查权限，不要新建空容器：

```bash
export STORAGE_SUBSCRIPTION_ID=YOUR_EXISTING_STORAGE_SUBSCRIPTION_ID
export STORAGE_RESOURCE_GROUP=YOUR_EXISTING_STORAGE_RESOURCE_GROUP
STORAGE_ACCOUNT_RESOURCE_ID=$(az storage account show --subscription "$STORAGE_SUBSCRIPTION_ID" \
  -g "$STORAGE_RESOURCE_GROUP" -n "$ACCOUNT" --query id -o tsv)
: "${STORAGE_ACCOUNT_RESOURCE_ID:?}" "${CONTAINER:?}"
STORAGE_CONTAINER_RESOURCE_ID="$STORAGE_ACCOUNT_RESOURCE_ID/blobServices/default/containers/$CONTAINER"
printf 'Origin role scope: %s\n' "$STORAGE_CONTAINER_RESOURCE_ID"
az role assignment list \
  --subscription "$STORAGE_SUBSCRIPTION_ID" \
  --assignee-object-id "$KUBELET_OBJECT_ID" \
  --scope "$STORAGE_CONTAINER_RESOURCE_ID" \
  --include-inherited \
  --fill-principal-name false \
  --query '[].{Role:roleDefinitionName,Scope:scope,PrincipalId:principalId}' \
  -o table
```

仅在确认没有覆盖目标容器的 Blob 读取权限时，由有角色分配权限的管理员补充：

```bash
az role assignment create --subscription "$STORAGE_SUBSCRIPTION_ID" \
  --assignee-object-id "$KUBELET_OBJECT_ID" --assignee-principal-type ServicePrincipal \
  --role 'Storage Blob Data Reader' --scope "$STORAGE_CONTAINER_RESOURCE_ID"
```

身份与 AKS OIDC 的 Entra tenant 必须匹配本环境；不要把不同 tenant 的 identity 当作默认可用。origin 使用 operator 为 `unbounded-system/racer` ServiceAccount 投射的 token；不向 provider/sidecar 放 Azure token，此路径不支持 SAS/shared key。FIC/RBAC 传播需要时间，最终需验证实际 origin 读取；角色存在也不代表网络访问一定可达。

> **权限边界：**FIC 只决定哪个 ServiceAccount 可以使用 identity，不会隔离该 identity 已有的 Azure 权限。复用 kubelet identity 会使 Racer 能够使用该 identity 的其他已授予权限；新增容器级 Reader 不会缩小这些权限。受控 PoC 可以复用，生产环境若要求最小权限和独立审计，仍建议使用仅有目标容器读取权限的专用 identity。

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
          clientID: ${KUBELET_CLIENT_ID}
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

以上是首次安装流程。已观察到一次测试集群的 Racer DaemonSet/Pod Ready；这不代表完成了 NVMe、模型读取或性能验收，仍须执行第五步。

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

本 PoC 直接使用 `kubectl edit deployment` 修改**现有** workspace 控制器的镜像和启动参数，不需要 Helm values 文件，也不新装第二份控制器。前提是普通 ModelMirror/streaming 所需的 CRD、RBAC、StorageClass 和 Workload Identity 已按 streaming guide 配置完成；编辑 Deployment 不会创建这些依赖，也不会安装 Racer 或创建 ClusterCache。

**先确认控制器的管理方式。** Helm 安装的 Deployment 可以临时手改，但后续 Helm upgrade/rollback 可能覆盖修改；由 AKS 托管 add-on、Operator 或 GitOps 管理时，修改还可能被自动恢复，应先通过其受支持的管理入口更新，不要与自动协调器反复争用。以下操作仅适用于允许手动维护的现有 Deployment。

### 4.1 定位并备份现有 Deployment

```bash
kubectl get deployment -A
export KAITO_CONTROLLER_NAMESPACE=YOUR_EXISTING_CONTROLLER_NAMESPACE
export KAITO_DEPLOYMENT=YOUR_EXISTING_WORKSPACE_CONTROLLER_DEPLOYMENT

# Inspect container names, images and all existing arguments before editing.
kubectl -n "$KAITO_CONTROLLER_NAMESPACE" get deployment "$KAITO_DEPLOYMENT" \
  -o jsonpath='{range .spec.template.spec.containers[*]}{"Container: "}{.name}{"\nImage: "}{.image}{"\nArgs:\n"}{range .args[*]}{.}{"\n"}{end}{"\n"}{end}'
export KAITO_CONTROLLER_CONTAINER=YOUR_EXISTING_CONTROLLER_CONTAINER_NAME

KAITO_DEPLOYMENT_BACKUP=$(mktemp /tmp/kaito-controller-before.XXXXXX.yaml)
kubectl -n "$KAITO_CONTROLLER_NAMESPACE" get deployment "$KAITO_DEPLOYMENT" -o yaml \
  > "$KAITO_DEPLOYMENT_BACKUP"
printf 'Deployment backup: %s\n' "$KAITO_DEPLOYMENT_BACKUP"

# Print actual image references from step 3; kubectl edit does not expand shell variables.
printf 'Controller image: %s\nRuntime image: %s\n' \
  "$REGISTRY/workspace:$VERSION" "$RACER_STREAMING_IMAGE"
kubectl -n "$KAITO_CONTROLLER_NAMESPACE" edit deployment "$KAITO_DEPLOYMENT"
```

`KAITO_CONTROLLER_NAMESPACE` 保持原安装值，不是要求移动控制器；`KAITO_CONTROLLER_CONTAINER` 必须是上面确认的 workspace 控制器容器，不是同 Pod 中的其它 sidecar。

### 4.2 修改控制器容器的 image 和 args

在编辑器中找到 `spec.template.spec.containers` 下的**控制器容器**，完成以下三项修改。下面都是字段/列表项示例，**不是完整 Deployment，不能直接 `kubectl apply`，也不要用它们覆盖完整 args 数组**。

1. **修改 `image`**：填第三步打印的实际控制器镜像，例如 `YOUR_REGISTRY/workspace:0.13.0-racer`。必须使用包含本分支实现的镜像；仅给旧控制器添加 `--racer-cache-bindings` 可能导致 unknown flag、无法启动。不要在编辑器中填写字面量 `$REGISTRY` 或 `$VERSION`。
2. **修改已有的 `--feature-gates=...` 参数**：保留所有其它 gate 的值，把 `ModelMirror`、`ModelStreaming` 改为 `true`；缺少时追加到同一条参数中，不要新增重复的 `--feature-gates`。例如，**仅当原参数只有以下三个 gate 时**：

   ```yaml
   # Before
   - --feature-gates=disableNodeAutoProvisioning=true,ModelMirror=false,ModelStreaming=false
   # After (replace the old item; do not keep both)
   - --feature-gates=disableNodeAutoProvisioning=true,ModelMirror=true,ModelStreaming=true
   ```

3. **添加或修改 `--racer-cache-bindings`**：下面整个单引号字符串是 `args` 中的**一项**。若已有该参数，保留其它 binding，将本 binding 合并到同一个 JSON 数组，不要增加第二条同名参数。

   ```yaml
   - '--racer-cache-bindings=[{"namespace":"kaito-workspace","account":"YOUR_ACTUAL_STORAGE_ACCOUNT","container":"YOUR_ACTUAL_MIRROR_CONTAINER","cacheName":"kaito-model-cache","objectImage":"ghcr.io/azure/racer-object@sha256:5c43d6c4dd2a18998568be84252040f88f6be7b93ec2e55fdf0ddeb8a92663ba","runtimeImage":"YOUR_REGISTRY/kaito-vllm-racer:0.13.0-racer","immutable":true}]'
   ```

   保存前替换所有占位符：
   - `namespace` 是**模型 Workspace 的 namespace**，此例为 `kaito-workspace`，不是控制器或 Racer 所在的 namespace。
   - `account` / `container` 使用第一步从 Ready ModelMirror 解析出的真实存储位置，`cacheName` 使用第二步已创建的 ClusterCache 名称。控制器按 Workspace namespace 和实际 account/container 匹配；启用 Racer 却无匹配项会报错。
   - `objectImage` 是公开的 Racer v0.27.0 sidecar 镜像，保持与 Racer server 相同版本，不是 KAITO 控制器镜像。
   - `runtimeImage` 填第三步打印的 `$RACER_STREAMING_IMAGE` **实际值**，即含 Racer provider 的推理镜像；也可使用该镜像对应的不可变 digest。不要填控制器镜像或普通未加入 provider 的 base 镜像，不使用 `latest`。
   - `immutable: true` 是管理员承诺不覆盖已存在的模型 blob key，不会自动启用 Azure Blob 不可变策略。本 provider ABI 不携带 ETag；更新模型应使用新容器或 revision 前缀，避免命中旧缓存。

**保留全部其它配置**，尤其是已有的 `--default-model-mirror-storage-class`、`--default-streaming-service-account`、其它启动参数、ServiceAccount、Workload Identity 标签/环境变量、镜像拉取配置和调度约束。开启 feature gates 并不让所有 Workspace 自动使用 Racer，具体 Workspace 仍需第五步的 `kaito.sh/model-cache: racer` annotation。

### 4.3 检查控制器更新

保存退出后，`image` / `args` 的 Pod template 变更会自动触发滚动更新，不需要额外 `rollout restart`。

```bash
kubectl -n "$KAITO_CONTROLLER_NAMESPACE" \
  rollout status deployment/"$KAITO_DEPLOYMENT" --timeout=5m
kubectl -n "$KAITO_CONTROLLER_NAMESPACE" get deployment "$KAITO_DEPLOYMENT" \
  -o jsonpath='{range .spec.template.spec.containers[*]}{"Container: "}{.name}{"\nImage: "}{.image}{"\nArgs:\n"}{range .args[*]}{.}{"\n"}{end}{"\n"}{end}'
kubectl -n "$KAITO_CONTROLLER_NAMESPACE" logs deployment/"$KAITO_DEPLOYMENT" \
  -c "$KAITO_CONTROLLER_CONTAINER" --tail=100
```

确认新镜像/参数已生效，且没有 unknown flag、binding 校验失败或镜像拉取失败。失败时停止第五步，检查新 Pod 的 events/logs，并根据备份审阅恢复本次修改的字段。控制器 rollout 成功不等于模型已经通过 Racer 加载，仍需完成第五步的真实推理和性能验证。

## 第五步：用真实模型验证推理和性能

**验收目标是模型通过 Racer 加载并完成真实推理，不是只看 Workspace Ready 或一次 Range 请求。** 本流程统一使用 `Qwen/Qwen3-Coder-30B-A3B-Instruct`，复用第一步该模型的 Ready ModelMirror；三组对比必须使用相同模型、不可变权重及推理配置。

`A3B` 表示每个 token 约激活 3B 参数，不代表只需加载 3B 权重；未量化的约 30B 参数仅 BF16/FP16 权重就约需 60 GB，另需 KV cache 和 runtime 工作空间。示例保留单张 A100 80 GB 的 `Standard_NC24ads_A100_v4`；部署前核对实际显存、dtype、`max_model_len`、并发和 KV cache 配置，不直接使用模型最大上下文长度假定能装下。确认所构建 runtime 的 vLLM 支持该模型架构，并在三组中保持版本和参数一致。

### 5.1 准备模型、身份和空闲 GPU

完成第三、四步后，先编辑 [Workspace 示例](../examples/racer-model-streaming-poc.yaml)：填写 `model-reader` 的 client ID，确认 preset/GPU；该 identity 必须有 FIC subject **`system:serviceaccount:kaito-workspace:model-reader`** 和第一步要求的 Blob 读取权限。示例中的 Namespace/ServiceAccount 只在首次准备时单独创建或经审阅后更新，不在每轮测试中覆盖已有身份配置。

```bash
export NS=kaito-workspace
export GPU_NODE=YOUR_VERIFIED_IDLE_A100_NODE
export MODEL=Qwen/Qwen3-Coder-30B-A3B-Instruct
export BENCH_ID=$(date -u +%Y%m%d%H%M%S)
export RESULTS="$PWD/racer-results-$BENCH_ID"
mkdir -p "$RESULTS"
kubectl get workspace -A
kubectl get pods -A --field-selector "spec.nodeName=$GPU_NODE" -o wide
kubectl describe node "$GPU_NODE"
kubectl -n "$NS" get serviceaccount model-reader
kubectl get node "$GPU_NODE" --show-labels
```

确认目标节点有足够的**未分配** GPU/CPU/内存，且已完成第二步 NVMe 授权、设备验证及 Racer Ready 检查。若唯一 GPU 已被现有模型占用，**停止并取得服务中断许可**，先保存原 Workspace/配置和恢复方案；不要直接删业务 Workspace、改副本数与 controller 对抗，或为测试自动新增付费 GPU 节点。

下面将测试 Workspace 固定到已核验节点的 hostname 标签，不依赖泛化的 `apps` 标签。节点必须同时有 `kaito.sh/racer-nvme=true`。如果该节点不满足资源需求就停止，不能假设 KAITO 新建节点自带已审核磁盘和缓存。

### 5.2 串行执行直接 Azure、Racer 首次加载和热加载

| CASE | `kaito.sh/model-cache` | 目的 |
| --- | --- | --- |
| `azure` | `disabled` | 保留 ModelStreaming，直接 Azure 读取作为基线 |
| `racer-first` | `racer` | 首次通过 Racer 加载；是否真正冷缓存须由指标证明 |
| `racer-warm` | `racer` | 上一组退出后，用新 Pod 加载相同权重，验证缓存复用 |

**每次只创建一组**，完成本节及 5.3–5.5、保存结果后才释放该测试 Workspace 的 GPU，继续下一组。不要修改运行中的 Workspace annotation 来切换路径，旧 StatefulSet 不一定更新所有字段。

以下生成命令需要管理机已有 Python 3 和 PyYAML。先将示例中的身份准备好；这里只提取 Workspace，不重复 apply Namespace/ServiceAccount：

```bash
export CASE=azure # 下一轮依次改为 racer-first、racer-warm
export WS="racer-bench-$BENCH_ID-$CASE"
export GPU_HOSTNAME=$(kubectl get node "$GPU_NODE" -o jsonpath='{.metadata.labels.kubernetes\.io/hostname}')
: "${GPU_HOSTNAME:?}"
python3 - <<'PY' > "$RESULTS/$CASE.workspace.json"
import json, os, yaml
with open('examples/racer-model-streaming-poc.yaml') as f:
    ws = next(d for d in yaml.safe_load_all(f) if d and d.get('kind') == 'Workspace')
case = os.environ['CASE']
assert case in ('azure', 'racer-first', 'racer-warm')
ws['metadata']['name'] = os.environ['WS']
ws['metadata']['namespace'] = os.environ['NS']
ws['metadata']['labels'] = {'racer-benchmark-id': os.environ['BENCH_ID']}
ws['metadata']['annotations']['kaito.sh/model-cache'] = 'disabled' if case == 'azure' else 'racer'
ws['resource']['labelSelector']['matchLabels'] = {
    'kubernetes.io/hostname': os.environ['GPU_HOSTNAME'], 'kaito.sh/racer-nvme': 'true'}
ws['inference']['preset']['name'] = os.environ['MODEL']
print(json.dumps(ws, indent=2))
PY
# 创建前保存 Racer/origin 指标快照，尤其是 racer-first；见下文。
date -u +%FT%TZ > "$RESULTS/$CASE.apply-start.txt"
kubectl create -f "$RESULTS/$CASE.workspace.json"
kubectl -n "$NS" get workspace "$WS" -w
```

看到 Ready 后退出 watch，继续以下检查；若出现 Failed、调度失败或长时间未就绪，检查事件并停止该轮，不把重试后的加载标记成“冷启动”。模型首次 mirror/download 时间要单独记录；进行性能比较前，应确认相同 ModelMirror 已 Ready，并记录对象版本/ETag/大小，不能把下载到 Blob 的时间归入 Racer 加载性能。

```bash
kubectl -n "$NS" get pods -l "kaito.sh/workspace=$WS" -o wide
kubectl -n "$NS" get workspace "$WS" -o yaml > "$RESULTS/$CASE.workspace-state.yaml"
kubectl -n "$NS" get pods -l "kaito.sh/workspace=$WS" -o json > "$RESULTS/$CASE.pods.json"
kubectl -n "$NS" get events --sort-by=.lastTimestamp > "$RESULTS/$CASE.events.txt"
export POD=YOUR_INFERENCE_POD APP_CONTAINER=YOUR_INFERENCE_CONTAINER
kubectl -n "$NS" logs "$POD" -c "$APP_CONTAINER" --timestamps > "$RESULTS/$CASE.runtime.log"
```

逐一检查**全部 rank**：模型、dtype/量化、并行度、vLLM 参数、GPU 型号一致。Racer runtime 应基于与 Azure 基线相同的 base **digest** 构建，记录各应用镜像的实际 `imageID` 和 vLLM/runai-model-streamer 版本；不能拿 `0.7.1` 普通 runtime 与基于 `0.6.3` 的 Racer runtime 得出缓存加速比。未对齐时只能报告各自功能验证结果。

Racer 两组必须有正确 provider env 和 `racer-object` native sidecar（`restartPolicy: Always`），挂载 `/run/racer/${CACHE_NAME}/client`，运行 `sidecar --api=azure --cache=... --listen=127.0.0.1:18080`。Azure 组应走直接 Azure，而非 Racer/本地完整权重缓存；不要关闭 ModelStreaming。不要再开启 Racer webhook 的重复注入标签。

**记录启动时间和缓存证据：**
- 从保存的 Pod JSON 取 `metadata.creationTimestamp`、`PodScheduled`/`Ready` condition 的 `lastTransitionTime`；Pod 创建→Ready 是端到端时间，不是纯模型加载时间。
- 用 timestamped runtime 日志中的加载开始/完成信息计算模型加载耗时；镜像拉取、调度、镜像准备、CUDA/JIT、HTTP server 启动等分别说明，不将总启动差值全部归因于存储。
- 每轮开始前和结束后保存 Racer/origin 的实际监控快照及对应节点 dataplane/sidecar 日志；使用所部署版本真实提供的指标名，比较 origin 读取字节、请求/错误和缓存命中变化。计数器重启清零或其他客户端流量会污染差值。
- `racer-first` 只有在权重未被预读、缓存未命中且有 origin 读取证据时才算冷缓存；无法证明则标记 `cache-state=unknown`。Range 预读放在加载测量之后。**不清空/格式化共享 NVMe，不删除共享 cache、ModelMirror 或 identity 来制造冷缓存。**
- `racer-warm` 保留同一节点/缓存/不可变对象，但必须重新创建推理 Pod。若命中证据不足或容量不足发生淘汰，不能宣称热缓存加速。

### 5.3 发送真实推理请求

在单独终端执行，只绑定本机，退出或切换 CASE 时用 Ctrl-C 停止：

```bash
kubectl -n "$NS" port-forward --address 127.0.0.1 "pod/$POD" 18000:5000
```

在原终端查询模型 ID 并请求推理（5000 是 KAITO runtime 默认 HTTP 端口，先核对实际 Pod）：

```bash
export API=http://127.0.0.1:18000
curl --fail --silent --show-error "$API/v1/models" > "$RESULTS/$CASE.models.json"
export SERVED_MODEL=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["data"][0]["id"])' "$RESULTS/$CASE.models.json")
python3 - <<'PY' > "$RESULTS/$CASE.request.json"
import json, os
print(json.dumps({'model': os.environ['SERVED_MODEL'], 'messages': [
    {'role': 'user', 'content': 'What is 17 + 25? Answer the number, then explain briefly.'}],
    'temperature': 0, 'max_tokens': 128}))
PY
curl --fail --silent --show-error --max-time 120 \
  -H 'Content-Type: application/json' \
  --data-binary "@$RESULTS/$CASE.request.json" "$API/v1/chat/completions" \
  > "$RESULTS/$CASE.response.json"
python3 - <<'PY'
import json, os
with open(os.path.join(os.environ['RESULTS'], os.environ['CASE'] + '.response.json')) as f:
    r = json.load(f)
text = r['choices'][0]['message']['content']
assert text and '42' in text, r
assert r['usage']['completion_tokens'] > 0, r
print(text)
print(r['usage'])
PY
```

这是基础正确性检查，不是模型质量评测；不要要求三组输出逐字节相同。错误、空输出或不合理回答应先诊断，不能仅凭 HTTP 200 验收。

### 5.4 测首响应延迟和生成吞吐

以下小规模流式测试只依赖 Python 标准库，先运行 1 次预热，再分别以并发 1/4 发起 20 个请求，每个最多生成 128 tokens。它测**模型 Ready 后**的服务性能，不包含权重加载。正式负载测试应在获批时段增加样本量；20 次的 P95 仅作 smoke benchmark，不能作为生产 SLA。

```bash
cat > "$RESULTS/bench.py" <<'PY'
import concurrent.futures, json, math, os, time, urllib.request

api, model = os.environ['API'], os.environ['SERVED_MODEL']
def request(i):
    payload = {'model': model, 'messages': [{'role': 'user', 'content':
        f'Request {i}: Explain how a hash table handles collisions and give an example.'}],
        'temperature': 0, 'max_tokens': 128, 'stream': True,
        'stream_options': {'include_usage': True}}
    req = urllib.request.Request(api + '/v1/chat/completions',
        data=json.dumps(payload).encode(), headers={'Content-Type': 'application/json'})
    start = time.perf_counter()
    first = None
    usage = None
    done = False
    text = ''
    with urllib.request.urlopen(req, timeout=120) as response:
        for raw in response:
            if not raw.startswith(b'data:'):
                continue
            data = raw[5:].strip()
            if data == b'[DONE]':
                done = True
                break
            chunk = json.loads(data)
            if chunk.get('error'):
                raise RuntimeError(chunk['error'])
            usage = chunk.get('usage') or usage
            for choice in chunk.get('choices', []):
                content = choice.get('delta', {}).get('content') or ''
                if content:
                    if first is None:
                        first = time.perf_counter()
                    text += content
    end = time.perf_counter()
    assert done and first is not None and text and usage, 'incomplete SSE or missing usage'
    tokens = usage['completion_tokens']
    assert tokens > 0
    return {'request': i, 'ttft_s': first-start, 'latency_s': end-start,
            'completion_tokens': tokens, 'output_tokens_per_s': tokens/(end-start)}

def percentile(rows, key, p):
    values = sorted(r[key] for r in rows)
    return values[max(0, math.ceil(p*len(values))-1)]

request(-1)  # 预热，不计入结果；每轮使用相同请求集合。
concurrency = int(os.environ.get('CONCURRENCY', '1'))
start = time.perf_counter()
with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
    rows = list(pool.map(request, range(20)))
wall = time.perf_counter() - start
print(json.dumps({'case': os.environ['CASE'], 'concurrency': concurrency,
    'requests': len(rows), 'wall_s': wall,
    'ttft_p50_s': percentile(rows, 'ttft_s', .5),
    'ttft_p95_s': percentile(rows, 'ttft_s', .95),
    'latency_p95_s': percentile(rows, 'latency_s', .95),
    'aggregate_output_tokens_per_s': sum(r['completion_tokens'] for r in rows)/wall,
    'samples': rows}, indent=2))
PY
CONCURRENCY=1 python3 "$RESULTS/bench.py" > "$RESULTS/$CASE.c1.json"
CONCURRENCY=4 python3 "$RESULTS/bench.py" > "$RESULTS/$CASE.c4.json"
```

这里 `ttft_s` 是客户端观察到的首个**非空 content chunk** 延迟；`output_tokens_per_s` 是包含 prefill/排队的请求端到端输出速率，不等于纯 decode tokens/s。并发场景使用 aggregate 吞吐，不能把各请求速率简单相加。若服务不支持 SSE usage，停止并适配其实际 API，不能用字符数冒充 token 数。

三组使用相同客户端、请求集合、顺序和 prefix/KV/LMCache 配置，记录实际 token 数。port-forward 会引入额外延迟，以上用于同条件 smoke 对比；高负载结论应使用相同节点位置的集群内客户端。Racer 优化的是权重读取/冷启动，**不预期常驻 GPU 后稳态 tokens/s 必然提升**。

### 5.5 补充读取验收、释放测试资源和汇总

Racer 两组完成加载测量后，在每个 rank 的应用容器内做真实 Range 读取，验证 sidecar 路径和返回契约（HTTP 路径是 `/<container>/<blob-key>`）：

```bash
export BLOB_KEY=YOUR_ACTUAL_SAFETENSORS_BLOB_KEY
kubectl -n "$NS" exec -i "$POD" -c "$APP_CONTAINER" -- \
  python3 - "$CONTAINER" "$BLOB_KEY" <<'PY'
import sys, urllib.parse, urllib.request
path = '/' + urllib.parse.quote(sys.argv[1], safe='') + '/' + urllib.parse.quote(sys.argv[2], safe='/')
r = urllib.request.urlopen(urllib.request.Request('http://127.0.0.1:18080' + path,
    headers={'Range': 'bytes=0-15'}), timeout=30)
body = r.read()
assert r.status == 206 and len(body) == 16
assert r.headers['Content-Range'].startswith('bytes 0-15/')
print(r.status, r.headers['Content-Range'], body.hex())
PY
```

403 排查 account/container 和 origin FIC/RBAC；503/hostPath 失败排查目标节点 dataplane/socket。检查真正使用了审核过的 NVMe，没有文件 slab 回退；Ready 或单次 Range 成功不能替代完整模型读取证据。

保存该轮日志/指标/推理结果后，停止 port-forward。**只有以下名称和标签都属于本次测试且允许删除时**才释放测试 Workspace；不删除 Namespace、ServiceAccount、ModelMirror、缓存或现有业务 Workspace：

```bash
case "$WS" in
  "racer-bench-$BENCH_ID-azure"|"racer-bench-$BENCH_ID-racer-first"|"racer-bench-$BENCH_ID-racer-warm") ;;
  *) echo 'Refusing to delete a non-benchmark Workspace' >&2; exit 1 ;;
esac
ACTUAL_ID=$(kubectl -n "$NS" get workspace "$WS" -o jsonpath='{.metadata.labels.racer-benchmark-id}')
if [ "$ACTUAL_ID" != "$BENCH_ID" ]; then
  echo 'Benchmark label mismatch; stop' >&2; exit 1
fi
kubectl -n "$NS" delete workspace "$WS" --wait=true --timeout=5m
kubectl -n "$NS" get pods -l "kaito.sh/workspace=$WS" -o wide
```

确认关联推理 Pod 已退出、GPU 已释放，再回到 5.2 选择下一 CASE；不删除共享模型资源。若有获准暂停的原业务，三组结束后按事先保存的恢复方案恢复，并重新验证服务。

最终按三组汇总：模型/对象版本、镜像和库版本、GPU、缓存状态证据、mirror/拉镜像耗时、Pod→Ready、模型加载耗时、TTFT P50/P95、端到端延迟 P95、aggregate tokens/s、实际输出 token 数、错误和 origin 字节增量。只有配置一致且冷/热状态有证据时才计算加载加速比；未实测的栏位填“未测”，不填写推算收益。

## 验证边界

此前 PoC 记录过 Go controller/provider 单测、provider race、9 项真实 `.so` ABI 测试、控制器编译及 Helm 渲染；不代表这些检查在每次文档升级中均已重跑。本次 v0.27.0 升级核对了 tag 源码、发行包校验和、CLI 帮助、overrides 离线校验及 release BOM/公开镜像 manifest。源码中的 sidecar flags、`/run/racer/<cache>/client`、`/<container>/<blob>`、单 Range/206/Content-Range 契约仍与 provider 一致，无需改变 KAITO provider ABI；这不是运行真实 v0.27.0 dataplane 的集成测试。本次重跑的 controller/provider 普通单测、Helm lint/render、10 个存储解析 mock 场景及 Bash 语法检查通过；环境缺少 GNU Make 和 C compiler，无法重跑完整 Make 检查、race 或 `.so` ABI 测试。后续集群检查已确认自定义 controller 镜像在运行，Racer DaemonSet 在补充 `sku=gpu:NoSchedule` toleration 后达到 `DESIRED=CURRENT=READY=1`；这些观察不代表镜像构建可复现性、NVMe 使用或 Azure origin 读取已验收。**本文新增的三组真实模型/性能流程尚未执行，没有性能结果。** 本地语法/mock 检查不能替代以上部署验收。
