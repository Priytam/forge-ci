import { BrowserRouter, Link, Route, Routes } from "react-router-dom";
import Repos from "./pages/Repos";
import PipelineList from "./pages/PipelineList";
import PipelineDetail from "./pages/PipelineDetail";
import JobLog from "./pages/JobLog";
import NewPipeline from "./pages/NewPipeline";
import RepoSettings from "./pages/RepoSettings";

export default function App() {
  return (
    <BrowserRouter>
      <header className="app-header">
        <div className="app-header-inner">
          <Link to="/" className="brand">
            <span className="brand-mark">⚙</span> Forge CI
          </Link>
          <Link to="/new" className="btn btn-primary header-action">
            New Pipeline
          </Link>
        </div>
      </header>
      <main className="content">
        <Routes>
          <Route path="/" element={<Repos />} />
          <Route path="/repos/:repo" element={<PipelineList />} />
          <Route path="/repos/:repo/settings" element={<RepoSettings />} />
          <Route path="/pipelines/:id" element={<PipelineDetail />} />
          <Route path="/jobs/:id" element={<JobLog />} />
          <Route path="/new" element={<NewPipeline />} />
        </Routes>
      </main>
    </BrowserRouter>
  );
}
