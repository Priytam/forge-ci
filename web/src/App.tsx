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
            <Link to="/new" className="btn header-action">
              New Pipeline
            </Link>
          </nav>
        </div>
      </header>
      <main className="content">
        <Routes>
          <Route path="/" element={<Repos />} />
          <Route path="/repos/new" element={<AddRepo />} />
          <Route path="/repos/:repo" element={<PipelineList />} />
          <Route path="/repos/:repo/settings" element={<RepoSettings />} />
          <Route path="/runners" element={<Runners />} />
          <Route path="/docs" element={<Docs />} />
          <Route path="/docs/:guide" element={<Docs />} />
          <Route path="/pipelines/:id" element={<PipelineDetail />} />
          <Route path="/jobs/:id" element={<JobLog />} />
          <Route path="/new" element={<NewPipeline />} />
        </Routes>
      </main>
    </BrowserRouter>
  );
}
