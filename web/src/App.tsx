import type { ReactNode } from "react";
import { BrowserRouter, Route, Routes, useLocation } from "react-router-dom";
import Repos from "./pages/Repos";
import PipelineList from "./pages/PipelineList";
import PipelineDetail from "./pages/PipelineDetail";
import JobLog from "./pages/JobLog";
import NewPipeline from "./pages/NewPipeline";
import RepoSettings from "./pages/RepoSettings";
import Runners from "./pages/Runners";
import Docs from "./pages/Docs";
import Recipes from "./pages/Recipes";
import AddRepo from "./pages/AddRepo";
import Login from "./pages/Login";
import AdminSso from "./pages/AdminSso";
import AdminAudit from "./pages/AdminAudit";
import AdminRunnerTokens from "./pages/AdminRunnerTokens";
import AllPipelines from "./pages/AllPipelines";
import ApiTokens from "./pages/ApiTokens";
import Jobs from "./pages/Jobs";
import EnvironmentBoard from "./pages/EnvironmentBoard";
import AdminEnvironments from "./pages/AdminEnvironments";
import AdminNotifications from "./pages/AdminNotifications";
import AdminApproverGroups from "./pages/AdminApproverGroups";
import AdminServiceAccounts from "./pages/AdminServiceAccounts";
import TemplateCatalog from "./pages/TemplateCatalog";
import AdminAbout from "./pages/AdminAbout";
import Environments from "./pages/Environments";
import TestHistory from "./pages/TestHistory";
import Changes from "./pages/Changes";
import ChangeDetail from "./pages/ChangeDetail";
import Shell from "./components/Shell";

/** Sign-in renders bare; every other page sits inside the app shell. */
function Frame({ children }: { children: ReactNode }) {
  const { pathname } = useLocation();
  if (pathname.startsWith("/login")) return <main className="content content-bare">{children}</main>;
  return <Shell>{children}</Shell>;
}

export default function App() {
  return (
    <BrowserRouter>
      <Frame>
        <Routes>
          <Route path="/" element={<Repos />} />
          <Route path="/repos/new" element={<AddRepo />} />
          <Route path="/repos/:repo" element={<PipelineList />} />
          <Route path="/repos/:repo/settings" element={<RepoSettings />} />
          <Route path="/repos/:repo/environments" element={<Environments />} />
          <Route path="/repos/:repo/tests" element={<TestHistory />} />
          <Route path="/runners" element={<Runners />} />
          <Route path="/docs" element={<Docs />} />
          <Route path="/docs/:guide" element={<Docs />} />
          <Route path="/templates" element={<TemplateCatalog />} />
          <Route path="/recipes" element={<Recipes />} />
          <Route path="/recipes/:slug" element={<Recipes />} />
          <Route path="/login" element={<Login />} />
          <Route path="/admin/sso" element={<AdminSso />} />
          <Route path="/admin/environments" element={<AdminEnvironments />} />
          <Route path="/admin/notifications" element={<AdminNotifications />} />
          <Route path="/admin/approver-groups" element={<AdminApproverGroups />} />
          <Route path="/admin/service-accounts" element={<AdminServiceAccounts />} />
          <Route path="/admin/audit" element={<AdminAudit />} />
          <Route path="/admin/about" element={<AdminAbout />} />
          <Route path="/admin/runner-tokens" element={<AdminRunnerTokens />} />
          <Route path="/changes" element={<Changes />} />
          <Route path="/changes/:id" element={<ChangeDetail />} />
          <Route path="/pipelines" element={<AllPipelines />} />
          <Route path="/jobs" element={<Jobs />} />
          <Route path="/environments" element={<EnvironmentBoard />} />
          <Route path="/tokens" element={<ApiTokens />} />
          <Route path="/pipelines/:id" element={<PipelineDetail />} />
          <Route path="/jobs/:id" element={<JobLog />} />
          <Route path="/new" element={<NewPipeline />} />
        </Routes>
      </Frame>
    </BrowserRouter>
  );
}
