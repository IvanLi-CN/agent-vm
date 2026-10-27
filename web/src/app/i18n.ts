import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import type { Locale } from "./types";

const resources = {
  "zh-CN": {
    translation: {
      product: "Agent VM",
      productSub: "检修工作台",
      nav: { overview: "概览", vms: "VM", images: "镜像", operations: "操作与审计", system: "系统" },
      shell: { openMenu: "打开导航", closeMenu: "关闭导航", language: "语言", operator: "维护者", synthetic: "合成合同数据", refresh: "刷新", lastUpdated: "观测于 {{time}}" },
      common: { all: "全部", search: "搜索", clear: "清除", copy: "复制", copied: "已复制", cancel: "取消", confirm: "确认", retry: "重试", loading: "加载中", noData: "暂无记录", viewAll: "查看全部", page: "第 {{page}} 页", previous: "上一页", next: "下一页", close: "关闭", reason: "原因", reasonPlaceholder: "说明这次受控操作的原因", required: "必填", selected: "已选择", selectedTarget: "当前目标" },
      status: { available: "可用", stale: "已过期", unverified: "未验证", missing: "缺失", failed: "失败", active: "运行中", cached: "已缓存", preparing: "准备中", closing: "收尾中", deleting: "删除中", absent: "不存在", effective: "有效租约", expired: "已过期租约", ready: "就绪", degraded: "降级", unavailable: "不可用", unknown: "未知", passed: "通过", retired: "已退役", verifying: "验证中", running: "运行中", queued: "排队中", reconciling: "恢复核对", succeeded: "已完成", outcome_unknown: "结果未知" },
      overview: { title: "概览", subtitle: "容量、准入、压力与来源新鲜度分开呈现。", admission: "准入", accepting: "可承接新会话", draining: "正在排空", admissionUnknown: "准入状态未知", capacity: "容量余量", cpuRemaining: "逻辑 CPU 余量", cpuTotal: "容器 CPU 容量", memoryRemaining: "逻辑内存余量", memoryTotal: "容器可用内存", disk: "物理磁盘", pressure: "运行压力", cpuSome: "CPU some.avg10", cpuFull: "CPU full.avg10", threshold: "启动压力阈值", memoryPressure: "内存压力", workload: "工作环境计数", sources: "来源新鲜度", noReasons: "没有阻断原因", staleNote: "数据源已超过允许新鲜度，数值不以零替代。", unknownNote: "观测尚未验证，不能据此判定健康。" },
      vms: { title: "VM", subtitle: "定位会话、核对固定镜像与租约，再执行受控动作。", filterState: "状态", filterLease: "租约", filterSearch: "按 task ID 或 session ID 搜索", allStates: "全部状态", allLeases: "全部租约", listTitle: "已有会话 VM", count: "{{count}} 个目标", detailEmpty: "选择一个目标查看身份、影响与动作。", identity: "身份", session: "会话", generation: "generation", image: "固定镜像", resources: "资源", lease: "租约", expires: "到期时间", ports: "端口映射", stateEvidence: "状态依据", updated: "更新时间", actions: "受控动作", actionHint: "动作会重新校验当前版本、generation 与租约。", noResults: "没有符合筛选的 VM。", impact: "影响范围", actionConfirm: "确认 {{action}}", actionConfirmText: "这会影响当前目标及其会话。请核对身份、影响与原因后提交。", previewUnavailable: "无法生成当前操作预览。", submitted: "操作已提交", operationId: "操作 ID", conflict: "目标已变化，请重新核对后再试。", targetChanged: "目标版本已变化", lastError: "安全错误", port: "{{host}} → {{guest}} / {{protocol}}", cpuVcpu: "CPU（vCPU）", memoryBytes: "内存（bytes）", diskBytes: "磁盘（bytes）", portName: "端口名称", protocol: "协议", guestPort: "来宾端口", hostPort: "主机端口" },
      images: { title: "镜像", subtitle: "管理不可变版本、默认选择和引用安全。", listTitle: "镜像目录", count: "{{count}} 个版本", selected: "已选择", created: "创建时间", detailEmpty: "选择镜像查看验证、引用与可用动作。", version: "版本标签", fingerprint: "fingerprint", architecture: "架构", source: "来源", references: "引用", default: "默认镜像", defaultForNew: "仅影响新会话", integrity: "完整性", suitability: "适用性", findings: "验证发现", import: "导入镜像", trusted: "可信源", local: "本地包", sourceId: "来源 ID", expectedFingerprint: "预期 fingerprint", packageFormat: "包格式", unified: "unified", split: "metadata + rootfs", expectedSize: "预期大小（bytes）", expectedSha: "预期 SHA-256", importReason: "导入原因", startImport: "开始导入", importStarted: "导入操作已创建", changeDefault: "切换默认", retire: "退役", delete: "删除", cannotDelete: "当前不可删除", deleteBlocked: "仍有引用或是默认镜像，后端会拒绝删除。", safeToDelete: "没有已知引用，可请求删除预览。", referenceDefault: "默认", referenceActive: "活跃 VM", referenceCached: "缓存", referencePending: "待创建", referenceVerify: "验证中", legacy: "legacy 配置", uploadHint: "完整接收后才进行完整性与适用性验证；本地上传不可续传。" },
      operations: { title: "操作与审计", subtitle: "跟踪持久操作直到明确终态；请求错误不等于 native 失败。", operations: "持久操作", audit: "审计记录", filter: "状态筛选", allStatuses: "全部状态", phase: "阶段", target: "目标", actor: "操作身份", outcome: "结果", occurred: "发生时间", reason: "原因", operation: "操作", safeCode: "安全错误码", runningHint: "运行中的操作每 2 秒重新查询。", unknownHint: "结果未知，先恢复核对，不自动重提。", failedHint: "该操作被 authority 拒绝或失败，可查看安全错误码。", noAudit: "暂无审计事件" },
      system: { title: "系统", subtitle: "组件就绪、状态能力与维护围栏。", productVersion: "产品版本", components: "组件状态", capabilities: "状态能力", maintenance: "受控维护", draining: "排空", reconciliation: "待恢复核对", garbage: "待回收缓存", maintainHint: "维护动作先预览影响，再确认并记录审计。", enterDrain: "进入排空", exitDrain: "退出排空", reconcile: "恢复核对", gc: "回收缓存", source: "来源" },
      dialog: { reasonRequired: "请填写操作原因。", confirmDanger: "该动作需要二次确认。", impactSessions: "受影响会话", impactRefs: "受影响镜像引用", expires: "预览有效期至 {{time}}", changes: "变更", preview: "预览依据", submit: "确认提交", noImpact: "没有额外影响。" },
      errors: { stale_target: "目标版本已变化，已拒绝本次操作。请重新加载并预览。", authority_unavailable: "控制权威暂时不可用；结果可能未知，请查询操作记录。", operation_conflict: "相同操作 ID 对应了不同请求。请使用新的操作 ID。", image_referenced: "镜像仍被引用，不能删除。", default_image_required: "请先切换默认镜像，再删除此镜像。", image_not_applicable: "镜像未通过 Agent VM 适用性验证。", fingerprint_mismatch: "上传内容摘要不匹配。", staging_capacity_low: "上传暂不可用：预留磁盘空间不足。", lease_expired: "租约已过期，请重新加载目标。", sqlite_unavailable: "状态数据库暂不可用；结果可能未知，请查询操作记录。", observation_stale: "来源观测已过期。", observation_unverified: "来源观测尚未验证。", generic: "请求未完成，未公开内部诊断。" },
    },
  },
  en: {
    translation: {
      product: "Agent VM",
      productSub: "Maintenance desk",
      nav: { overview: "Overview", vms: "VMs", images: "Images", operations: "Operations & Audit", system: "System" },
      shell: { openMenu: "Open navigation", closeMenu: "Close navigation", language: "Language", operator: "Operator", synthetic: "Synthetic contract data", refresh: "Refresh", lastUpdated: "Observed {{time}}" },
      common: { all: "All", search: "Search", clear: "Clear", copy: "Copy", copied: "Copied", cancel: "Cancel", confirm: "Confirm", retry: "Retry", loading: "Loading", noData: "No records", viewAll: "View all", page: "Page {{page}}", previous: "Previous", next: "Next", close: "Close", reason: "Reason", reasonPlaceholder: "Explain why this controlled action is needed", required: "Required", selected: "Selected", selectedTarget: "Selected target" },
      status: { available: "Available", stale: "Stale", unverified: "Unverified", missing: "Missing", failed: "Failed", active: "Active", cached: "Cached", preparing: "Preparing", closing: "Closing", deleting: "Deleting", absent: "Absent", effective: "Effective lease", expired: "Expired lease", ready: "Ready", degraded: "Degraded", unavailable: "Unavailable", unknown: "Unknown", passed: "Passed", retired: "Retired", verifying: "Verifying", running: "Running", queued: "Queued", reconciling: "Reconciling", succeeded: "Succeeded", outcome_unknown: "Outcome unknown" },
      overview: { title: "Overview", subtitle: "Capacity, admission, pressure and source freshness stay separate.", admission: "Admission", accepting: "Ready for new sessions", draining: "Draining", admissionUnknown: "Admission is unknown", capacity: "Capacity remaining", cpuRemaining: "Logical CPU remaining", cpuTotal: "Container CPU capacity", memoryRemaining: "Logical memory remaining", memoryTotal: "Container memory available", disk: "Physical disk", pressure: "Runtime pressure", cpuSome: "CPU some.avg10", cpuFull: "CPU full.avg10", threshold: "Startup pressure threshold", memoryPressure: "Memory pressure", workload: "Workload counts", sources: "Source freshness", noReasons: "No blocking reasons", staleNote: "This source is older than its freshness window; the value is not shown as zero.", unknownNote: "This observation is not verified and cannot be treated as healthy." },
      vms: { title: "VMs", subtitle: "Locate a session, verify its pinned image and lease, then take a controlled action.", filterState: "State", filterLease: "Lease", filterSearch: "Search task ID or session ID", allStates: "All states", allLeases: "All leases", listTitle: "Existing session VMs", count: "{{count}} targets", detailEmpty: "Select a target to see its identity, impact and actions.", identity: "Identity", session: "Session", generation: "Generation", image: "Pinned image", resources: "Resources", lease: "Lease", expires: "Expires", ports: "Port mappings", stateEvidence: "State evidence", updated: "Updated", actions: "Controlled actions", actionHint: "Actions re-check the current version, generation and lease.", noResults: "No VMs match these filters.", impact: "Impact", actionConfirm: "Confirm {{action}}", actionConfirmText: "This will affect the selected target and its session. Verify identity, impact and reason before submitting.", previewUnavailable: "The current action preview could not be created.", submitted: "Operation submitted", operationId: "Operation ID", conflict: "The target changed. Review it again before retrying.", targetChanged: "Target version changed", lastError: "Safe error", port: "{{host}} → {{guest}} / {{protocol}}", cpuVcpu: "CPU (vCPU)", memoryBytes: "Memory (bytes)", diskBytes: "Disk (bytes)", portName: "Port name", protocol: "Protocol", guestPort: "Guest port", hostPort: "Host port" },
      images: { title: "Images", subtitle: "Manage immutable versions, default selection and reference safety.", listTitle: "Image catalog", count: "{{count}} versions", selected: "Selected", created: "Created", detailEmpty: "Select an image to review verification, references and actions.", version: "Version label", fingerprint: "Fingerprint", architecture: "Architecture", source: "Source", references: "References", default: "Default image", defaultForNew: "Applies to new sessions only", integrity: "Integrity", suitability: "Suitability", findings: "Verification findings", import: "Import image", trusted: "Trusted source", local: "Local package", sourceId: "Source ID", expectedFingerprint: "Expected fingerprint", packageFormat: "Package format", unified: "Unified", split: "Metadata + rootfs", expectedSize: "Expected size (bytes)", expectedSha: "Expected SHA-256", importReason: "Import reason", startImport: "Start import", importStarted: "Import operation created", changeDefault: "Select default", retire: "Retire", delete: "Delete", cannotDelete: "Cannot delete now", deleteBlocked: "There are references or this is the default; the authority will reject deletion.", safeToDelete: "No known references; request a deletion preview.", referenceDefault: "Default", referenceActive: "Active VMs", referenceCached: "Cached", referencePending: "Pending creation", referenceVerify: "Verifying", legacy: "Legacy config", uploadHint: "Integrity and suitability checks start after the complete stream is received; local uploads cannot resume." },
      operations: { title: "Operations & Audit", subtitle: "Track durable operations to a clear terminal state; a request error is not native failure.", operations: "Durable operations", audit: "Audit records", filter: "Status filter", allStatuses: "All statuses", phase: "Phase", target: "Target", actor: "Actor", outcome: "Outcome", occurred: "Occurred", reason: "Reason", operation: "Operation", safeCode: "Safe error code", runningHint: "Running operations refresh every 2 seconds.", unknownHint: "The outcome is unknown. Reconcile before starting another operation.", failedHint: "The authority rejected or failed this operation; inspect its safe error code.", noAudit: "No audit events" },
      system: { title: "System", subtitle: "Component readiness, state capabilities and maintenance fences.", productVersion: "Product version", components: "Components", capabilities: "State capabilities", maintenance: "Controlled maintenance", draining: "Draining", reconciliation: "Reconciliation pending", garbage: "Garbage collection pending", maintainHint: "Preview impact, confirm, and record an audit event for maintenance actions.", enterDrain: "Enter drain", exitDrain: "Exit drain", reconcile: "Reconcile", gc: "Garbage collect", source: "Source" },
      dialog: { reasonRequired: "Enter a reason for this action.", confirmDanger: "This action requires a second confirmation.", impactSessions: "Affected sessions", impactRefs: "Affected image references", expires: "Preview valid until {{time}}", changes: "Changes", preview: "Preview basis", submit: "Confirm and submit", noImpact: "No additional impact." },
      errors: { stale_target: "The target version changed, so this action was rejected. Reload and preview again.", authority_unavailable: "The control authority is temporarily unavailable; the outcome may be unknown. Query operations.", operation_conflict: "This operation ID was used for a different request. Use a new operation ID.", image_referenced: "The image is still referenced and cannot be deleted.", default_image_required: "Select another default image before deleting this one.", image_not_applicable: "The image has not passed Agent VM suitability checks.", fingerprint_mismatch: "The uploaded content digest does not match.", staging_capacity_low: "Upload is temporarily unavailable because the staging reserve is low.", lease_expired: "The lease has expired. Reload the target.", sqlite_unavailable: "The state database is temporarily unavailable; query the operation because the outcome may be unknown.", observation_stale: "The source observation is stale.", observation_unverified: "The source observation is unverified.", generic: "The request did not complete. Internal diagnostics were not exposed." },
    },
  },
};

function browserLocale(): Locale {
  if (typeof navigator !== "undefined" && navigator.language.toLowerCase().startsWith("zh")) return "zh-CN";
  return "en";
}

export function resolveLocale(): Locale {
  if (typeof window === "undefined") return "en";
  const saved = window.localStorage.getItem("agent-vm-locale");
  return saved === "zh-CN" || saved === "en" ? saved : browserLocale();
}

void i18n.use(initReactI18next).init({ resources, lng: resolveLocale(), fallbackLng: "en", interpolation: { escapeValue: false } });

i18n.addResource("zh-CN", "translation", "vms.selectedTarget", "已选择");
i18n.addResource("en", "translation", "vms.selectedTarget", "Selected");
i18n.addResource("zh-CN", "translation", "images.expectedMetadataSha", "预期 metadata SHA-256");
i18n.addResource("zh-CN", "translation", "images.expectedRootfsSha", "预期 rootfs SHA-256");
i18n.addResource("zh-CN", "translation", "images.packageFile", "unified 镜像包");
i18n.addResource("zh-CN", "translation", "images.metadataFile", "metadata 文件");
i18n.addResource("zh-CN", "translation", "images.rootfsFile", "rootfs 文件");
i18n.addResource("zh-CN", "translation", "images.chooseFile", "选择文件");
i18n.addResource("zh-CN", "translation", "images.fileRequired", "请先选择完整的镜像文件。");
i18n.addResource("zh-CN", "translation", "images.expectedShaRequired", "请填写预期 SHA-256。");
i18n.addResource("zh-CN", "translation", "images.targetUnavailable", "镜像目录版本不可用，请刷新后重试。");
i18n.addResource("en", "translation", "images.expectedMetadataSha", "Expected metadata SHA-256");
i18n.addResource("en", "translation", "images.expectedRootfsSha", "Expected rootfs SHA-256");
i18n.addResource("en", "translation", "images.packageFile", "Unified image package");
i18n.addResource("en", "translation", "images.metadataFile", "Metadata file");
i18n.addResource("en", "translation", "images.rootfsFile", "Rootfs file");
i18n.addResource("en", "translation", "images.chooseFile", "Choose a file");
i18n.addResource("en", "translation", "images.fileRequired", "Select the complete image content first.");
i18n.addResource("en", "translation", "images.expectedShaRequired", "Enter the expected SHA-256.");
i18n.addResource("en", "translation", "images.targetUnavailable", "The image catalog version is unavailable. Refresh and try again.");

export async function changeLocale(locale: Locale): Promise<void> {
  if (typeof window !== "undefined") window.localStorage.setItem("agent-vm-locale", locale);
  await i18n.changeLanguage(locale);
}

export default i18n;
