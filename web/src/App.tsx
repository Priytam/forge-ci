import { BrowserRouter, Link, Route, Routes } from "react-router-dom";
import PipelineList from "./pages/PipelineList";
import PipelineDetail from "./pages/PipelineDetail";
import JobLog from "./pages/JobLog";
import NewPipeline from "./pages/NewPipeline";

export default function App() {
  return (
    <BrowserRouter>
      <header className="app-header">
        <div className="app-header-inner">
          <Link to="/" className="brand">
            <span className="brand-mark">⚙</span> Forge CI
          </Link>
        </div>
      </header>
      <main className="content">
        <Routes>
          <Route path="/" element={<PipelineList />} />
          <Route path="/pipelines/:id" element={<PipelineDetail />} />
          <Route path="/jobs/:id" element={<JobLog />} />
          <Route path="/new" element={<NewPipeline />} />
        </Routes>
      </main>
    </BrowserRouter>
  );
}
