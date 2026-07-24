import { BrowserRouter, Link, Route, Routes } from "react-router-dom";
import Repos from "./pages/Repos";
import PipelineList from "./pages/PipelineList";
import PipelineDetail from "./pages/PipelineDetail";
import JobLog from "./pages/JobLog";
import NewPipeline from "./pages/NewPipeline";
import RepoSettings from "./pages/RepoSettings";
import Runners from "./pages/Runners";
import Docs from "./pages/Docs";
import AddRepo from "./pages/AddRepo";
import Login from "./pages/Login";
import AdminSso from "./pages/AdminSso";
import AdminAudit from "./pages/AdminAudit";
import Environments from "./pages/Environments";
import AuthMenu from "./components/AuthMenu";
import AdminNav from "./components/AdminNav";

export default function App() {
  return (
    <BrowserRouter>
      <header className="app-header">
        <div className="app-header-inner">
          <Link to="/" className="brand">
            <span className="brand-mark">⚙</span> Forge CI
          </Link>
          <nav className="header-nav">
            <Link to="/runners" className="header-link">
              Runners
            </Link>
            <Link to="/docs" className="header-link">
              Docs
            </Link>
            <AdminNav />
            <Link to="/new" className="btn header-action">
              Run pipeline
            </Link>
            <AuthMenu />
          </nav>
        </div>
      </header>
      <main className="content">
        <Routes>
          <Route path="/" element={<Repos />} />
          <Route path="/repos/new" element={<AddRepo />} />
          <Route path="/repos/:repo" element={<PipelineList />} />
          <Route path="/repos/:repo/settings" element={<RepoSettings />} />
          <Route path="/repos/:repo/environments" element={<Environments />} />
          <Route path="/runners" element={<Runners />} />
          <Route path="/docs" element={<Docs />} />
          <Route path="/docs/:guide" element={<Docs />} />
          <Route path="/login" element={<Login />} />
          <Route path="/admin/sso" element={<AdminSso />} />
          <Route path="/admin/audit" element={<AdminAudit />} />
          <Route path="/pipelines/:id" element={<PipelineDetail />} />
          <Route path="/jobs/:id" element={<JobLog />} />
          <Route path="/new" element={<NewPipeline />} />
        </Routes>
      </main>
    </BrowserRouter>
  );
}
