import { Link } from 'react-router';
import { DocsBody } from 'fumadocs-ui/layouts/docs/page';
import { Page } from '../components/page';
export default function Home() {
  return <Page breadcrumb={{ enabled: false }}><DocsBody className="docs-home">
    <h1>Agent VM 文档</h1>
    <p className="lead">为 Agent 会话提供受租约约束的工作环境。</p>
    <p>按使用、维护、部署和开发任务阅读客户端、控制服务、镜像与运行边界。</p>
    <section><h2>开始使用</h2><ul className="document-list">
      <li><Link to="/docs/overview">项目概览</Link><p>了解 Agent VM 的组成、运行边界与项目入口。</p></li>
      <li><Link to="/docs/quickstart">快速开始</Link><p>配置客户端、获取会话、连接来宾并安全结束工作。</p></li>
      <li><Link to="/docs/connections">连接与认证</Link><p>区分 SSH、HTTPS、Token、来宾 host key 和文件传输。</p></li>
      <li><Link to="/docs/cli">CLI 使用与命令</Link><p>查阅命令、参数、输出、错误和恢复身份。</p></li>
    </ul></section>
    <section><h2>使用指南</h2><ul className="document-list">
      <li><Link to="/docs/workflows">日常工作流</Link><p>完成同步、端口、续租、调整、释放和未知结果恢复。</p></li>
      <li><Link to="/docs/console">管理台使用</Link><p>维护者按概览、VM、镜像、操作审计和系统页面工作。</p></li>
      <li><Link to="/docs/troubleshooting">故障排查</Link><p>按症状定位配置、生命周期、部署和镜像问题。</p></li>
    </ul></section>
    <section><h2>部署运维</h2><ul className="document-list">
      <li><Link to="/docs/deployment">部署入口</Link><p>从安装、蓝绿切换、维护到验收进入受保护流程。</p></li>
      <li><Link to="/docs/operations">拓扑与恢复运行手册</Link><p>核对控制器、槽位、状态后端和恢复证据。</p></li>
      <li><Link to="/docs/image-build">镜像构建与发布</Link><p>理解锁定输入、sidecar、验证和导入边界。</p></li>
    </ul></section>
    <section><h2>开发文档</h2><ul className="document-list">
      <li><Link to="/docs/product">项目产品定义</Link><p>了解角色、核心场景、能力地图、边界和成功标准。</p></li>
      <li><Link to="/docs/architecture">系统架构</Link><p>理解蓝绿服务、身份边界、共享状态与故障恢复。</p></li>
      <li><Link to="/docs/documentation-guide">文档组织与编写规范</Link><p>按唯一来源、主题职责和网页发布规则维护文档。</p></li>
    </ul></section>
  </DocsBody></Page>;
}
