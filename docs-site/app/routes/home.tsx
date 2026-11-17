import { Link } from 'react-router';
export default function Home() {
  return <div className="docs-home">
    <h1>Agent VM 文档</h1>
    <p className="lead">为 Agent 会话提供受租约约束的工作环境。</p>
    <p>了解如何使用客户端与会话容器，以及控制服务的架构、接口和设计约束。</p>
    <section><h2>开始使用</h2><ul className="document-list">
      <li><Link to="/docs/overview">项目概览</Link><p>了解 Agent VM 的组成、运行边界与项目入口。</p></li>
      <li><Link to="/docs/cli">CLI 使用与命令</Link><p>配置客户端，了解会话、租约、连接与源码同步命令。</p></li>
    </ul></section>
    <section><h2>开发文档</h2><ul className="document-list">
      <li><Link to="/docs/architecture">系统架构</Link><p>理解蓝绿服务、身份边界、共享状态与故障恢复。</p></li>
      <li><Link to="/docs/glossary">领域术语</Link><p>区分会话、容器、租约、镜像与产品发布等核心概念。</p></li>
      <li><Link to="/docs/docs-site-design">文档工程</Link><p>文档站的技术方案、内容适配与构建维护。</p></li>
    </ul></section>
  </div>;
}
