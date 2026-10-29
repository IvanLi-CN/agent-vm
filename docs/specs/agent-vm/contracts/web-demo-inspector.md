# Web Demo Inspector UX/UI 合同

## 目的与边界

Demo Inspector 是 Agent VM Web Demo 的审查控制面板，不是正式管理台的产品导航、权限管理或真实资源控制面板。它只控制 Demo 的场景、模拟身份、网络故障、数据密度和本地布局；所有模拟状态仍属于内存模型，刷新后由 URL 场景重新初始化。

Inspector 必须让第一次打开 Demo 的人立即知道三件事：这是安全演示、当前页面可以被模拟控制、面板不会连接真实管理 API 或资源。面板的视觉和交互不能遮蔽待审查的正式页面，也不能把模拟状态误导成生产状态。

## 结构与信息层级

Inspector 按以下顺序组织内容，不得用一组无标签的图标替代结构化字段：

1. 身份区：`SAFE DEMO`/`安全演示` eyebrow、`Demo inspector`/`演示控制面板` 标题，以及布局控制和浮动模式的显式拖动手柄。
2. 安全提示：明确显示 `In-memory simulation`/`仅内存模拟`，说明不会连接真实管理 API 或资源。
3. 场景字段：场景、模拟身份、数据密度；字段使用带可访问名称的 shadcn/ui 风格 `Select`。
4. 网络字段：在线/故障互斥切换，并把当前网络状态显式呈现。
5. 结果摘要：操作权限和模拟连接状态使用带文字的状态徽章，未知、故障和拒绝不得只用颜色表示。
6. 分享与恢复：复制安全分享链接、重置场景，以及链接只包含场景、权限、网络和数据设置的说明。

控件必须沿用 `web/components.json` 声明的 open-code shadcn/ui 基线。源码位于 `web/src/components/ui`，别名固定为 `@/components/ui`、`@/lib/utils`；组件层使用 Radix primitives、Tailwind utilities、CVA、`clsx`/`tailwind-merge` 的 `cn` helper 和 Lucide icons。Inspector 直接使用 shadcn `Button`、Radix-backed `Select`、`ContextMenu`/`DropdownMenu` 和 Tooltip primitives，业务样式只通过现有 paper-line CSS tokens 做组合，不再新增原生裸按钮来替代组件层。

布局控制按钮的交互必须明确分层：普通单击只执行宽屏默认布局切换（宽屏优先右栏，窄屏优先悬浮），不得打开菜单；鼠标右键、键盘 `Shift+F10`/ContextMenu 键以及触摸长按才打开 shadcn 菜单，菜单提供左栏、右栏、悬浮、最小化四个互斥选项。悬浮模式的 `GripVertical` shadcn icon button 是独立拖动手柄，热区至少 44px（粗指针），支持 pointer capture、边界约束和方向键移动；布局按钮与拖动手柄不能共用职责。

所有控制项必须具备可读名称、可见 focus-visible 状态和不依赖颜色的选中/错误语义；Inspector 内部不手写菜单定位、outside-click 或 menuitem ARIA 状态，交由 Radix primitives 管理焦点、portal、Esc 关闭和菜单键盘导航。组件层变更需同步 Storybook 状态与页面级浏览器验证。

## 布局模式

布局状态是 `left`、`right`、`floating`、`minimized` 四选一，布局偏好只保存在当前浏览器本地，不进入分享链接：

| 模式 | 视口与用途 | 约束 |
| --- | --- | --- |
| `left` | 宽屏固定左栏 | Demo 页面整体使用页面级 grid；Inspector 占独立 320px 栏，不覆盖正式页面；可通过位置菜单切换或最小化。 |
| `right` | 宽屏固定右栏；宽屏默认模式 | 与 `left` 对称，正式页面保留可读主内容宽度；不能通过一个额外浮层再次覆盖主内容。 |
| `floating` | 普通桌面和移动端默认模式 | `position: fixed`，具有 viewport 安全边距、最大高度和独立滚动；只允许在明确的拖动手柄上拖动。 |
| `minimized` | 暂时让出全部页面空间 | 只显示固定的恢复 bubble；bubble 必须有可访问名称，恢复后回到上一次展开布局。 |

宽屏以 `min-width: 1280px` 作为页面级 dock 判断；窄于该断点时不显示伪造的固定栏，`left`/`right` 选择仍可作为布局偏好，但渲染必须退化为不遮蔽页面的浮层/抽屉行为。移动端 Inspector 使用视口内抽屉式浮层，不能让正文横向滚动，也不能把主要页面操作推到不可见区域。

布局切换、最小化和恢复必须保持当前路由与场景，不得整页刷新；浏览器前进/后退只恢复 URL 驱动的场景设置，不恢复模拟写入结果。

## 位置控制语义

位置图标是“布局位置控制”，不是关闭按钮。必须遵循以下输入合同：

- 单击位置图标只执行当前视口的默认布局：宽屏选择 `right`，窄屏选择 `floating`；单击不得打开位置菜单。
- 右键、触控长按（约 550ms）或键盘 `ContextMenu`/`Shift+F10` 才打开位置菜单。
- 菜单提供固定左栏、固定右栏、悬浮、最小化；当前选项使用 `menuitemradio` 语义和选中状态，不依赖颜色单独传达。
- 菜单打开后，`Escape` 关闭，点击外部关闭，焦点保持在位置控制或移动到所选菜单项；菜单项最小触控尺寸为 44px。
- 位置图标必须有稳定的 accessible name（`控制面板位置`/`Inspector position`）和 `aria-expanded`；它不使用“关闭”“隐藏”等误导名称。

## 浮动模式拖动合同

### 可发现的热区

浮动模式面板顶部必须显示独立的面板级拖动手柄，而不是把标题文字或业务按钮作为隐藏热区。手柄应使用已登记 shadcn/ui `Button` 的 `ghost`/`secondary` variant，内部只放 `GripHorizontal` 图标，横跨面板主要宽度但不显示“拖动”操作文案，通过 Tooltip 和 accessible name 解释用途；手柄具有清晰的 hover/pressed/focus-visible 状态和至少 44×44px 的移动端触控目标。固定左栏、固定右栏和最小化模式不显示该手柄，避免暗示不可用的拖动。

手柄只负责移动面板，不负责打开位置菜单；位置图标仍是单独的控制。手柄与标题、Select、网络切换和操作按钮之间必须有清晰的 hit-area 边界，避免一次触控同时触发拖动和业务操作。

### 指针与触控

- 鼠标左键或触控在手柄按下后开始拖动，并使用 pointer capture 保持连续事件；右键不启动拖动，保留位置菜单语义。
- 面板在拖动中显示 `grabbing` 光标和稳定的 active 状态，不改变周围布局尺寸。
- 面板位置以 viewport 为边界，至少保留 8px 安全边距；窗口 resize 后自动夹回可见区域。
- 松开、取消或 pointer capture 丢失时结束拖动，并以 `left/top` 保存到本地；不得保存到 URL、服务端或模拟模型。
- 拖动不得改变场景、权限、网络、数据或操作记录；也不得触发真实 API、文件读取或页面导航。

### 键盘替代

拖动手柄是可聚焦按钮，声明 `aria-keyshortcuts="ArrowLeft ArrowRight ArrowUp ArrowDown"`。获得焦点后：

- 方向键按 16px 移动；`Shift`+方向键按 48px 移动。
- 位置同样遵守 viewport 安全边距并立即保存。
- 手柄必须提供 accessible name；方向键移动是拖动的等价替代，不要求用户完成鼠标手势。

## 状态、错误与安全

- Inspector bootstrap 失败时，停止渲染正式页面和 Inspector，显示安全错误面；不得回退到真实 `/api/v1`。
- 面板操作只影响 in-memory mock model；未知结果、网络故障和权限拒绝必须在正式页面和操作记录中可见。
- 分享链接只允许 `demo`、`d_persona`、`d_network`、`d_data` 等已登记设置；布局、拖动坐标、原因、文件元数据、凭据和任何敏感字段不得进入 URL。
- 复制链接失败时提供不泄露敏感信息的状态提示；刷新清理模拟写入，但保留 URL 驱动的场景设置。
- 所有 icon-only control 必须有 accessible name；装饰图标 `aria-hidden="true"`；颜色、阴影和图标不能成为唯一状态来源。

## 参考案例与设计结论

- [shadcn/ui Resizable](https://ui.shadcn.com/docs/components/base/resizable) 的 `ResizableHandle` 是面板之间的窄分隔手柄；`withHandle` 只增加可视 grip，不把它做成带业务文案的按钮。
- [Vaul Drawer API](https://vaul.emilkowal.ski/api) 将 `Drawer.Handle` 作为独立部件，明确区别于标题和关闭控件，并支持 `handleOnly` 让拖动只从 handle 开始。
- [Material 3 Bottom sheets](https://m3.material.io/components/bottom-sheets/overview) 将 drag handle 作为可选视觉提示，同时要求至少 48dp 的可访问触控热区。
- [Apple Human Interface Guidelines — Windows](https://developer.apple.com/design/human-interface-guidelines/windows) 将可移动窗口的 frame/title bar 与业务按钮分开，窗口通过 frame 移动。

因此 Inspector 采用“顶部居中 shadcn Button + GripHorizontal + 44px 以上整条热区 + Tooltip/accessible name”的组合；位置按钮只负责布局菜单，不能兼任拖动手柄。`ResizableHandle` 只用于调整大小，不被冒充为浮动面板移动控件。

## 响应式与可访问性门禁

验收至少覆盖 1440×900、1024×768、390×844 三类视口，并检查：

- 宽屏 dock 不覆盖主内容，固定栏可最小化；普通桌面浮层不越出视口；移动端抽屉不造成横向滚动。
- 所有按钮、选择器和位置菜单可通过键盘到达；`Tab` 顺序先经过面板身份/布局控制，再经过字段和操作。
- 位置菜单的打开、关闭、选中、最小化和恢复均可通过键盘完成；拖动有方向键替代。
- 焦点环在浅色背景上可见，普通文本对比度满足 4.5:1，非文本控件边界满足 3:1；减少动画时不丢失状态反馈。
- 触控目标至少 44×44px；固定元素和滚动内容不遮挡键盘焦点；窄屏正文不要求横向滚动。

## 测试与证据

实现必须保留以下证据：

- 单元/模型测试：布局默认值、布局本地持久化、位置坐标校正和场景重置不污染分享状态。
- Playwright 页面测试：单击不打开菜单、右键/长按/`Shift+F10` 打开菜单、四种布局、指针拖动、方向键移动、最小化/恢复、三类视口和 bootstrap fail-closed。
- Storybook：至少覆盖 Inspector 默认浮动、固定栏、最小化、位置菜单打开和拖动手柄可见状态；Storybook accessibility 是早期筛查，不能替代页面级验收。
- 视觉证据：使用合成 Demo 截图，分别展示浮动手柄、拖动后位置、宽屏 dock 和移动端最小化恢复；不使用真实管理台、真实资源或含敏感数据的截图。

## 非目标

Inspector 不提供任意像素级布局编辑器、服务端布局同步、正式产品角色管理、真实文件上传、真实管理 API 操作或独立的第二套管理台视觉系统。
