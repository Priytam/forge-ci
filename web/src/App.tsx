import type { ReactNode } from "react";
import SettingsLayout from "./components/SettingsLayout";
import AdminApiTokens from "./pages/AdminApiTokens";
import { SETTINGS_HOME } from "./components/nav";
import { BrowserRouter, Navigate, Route, Routes, useLocation } from "react-router-dom";
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
import RepoChanges from "./pages/RepoChanges";
import RepoQueue from "./pages/RepoQueue";
import RepoMergeRules from "./pages/RepoMergeRules";
import ChangeDetail from "./pages/ChangeDetail";
import AdminMergeRules from "./pages/AdminMergeRules";
import Account from "./pages/Account";
import Shell from "./components/Shell";
import { ToastProvider } from "./components/ui/Toast";

/** Sign-in renders bare; every other page sits inside the app shell. */
function Frame({ children }: { children: ReactNode }) {
  const { pathname } = useLocation();
  if (pathname.startsWith("/login")) return <main className="content content-bare">{children}</main>;
  return (
    <ToastProvider>
      <Shell>{children}</Shell>
    </ToastProvider>
  );
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
          <Route path="/repos/:repo/changes" element={<RepoChanges />} />
          <Route path="/repos/:repo/queue" element={<RepoQueue />} />
          <Route path="/repos/:repo/merge-rules" element={<RepoMergeRules />} />
          <Route path="/runners" element={<Runners />} />
          <Route path="/docs" element={<Docs />} />
          <Route path="/docs/:guide" element={<Docs />} />
          <Route path="/templates" element={<TemplateCatalog />} />
          <Route path="/recipes" element={<Recipes />} />
          <Route path="/recipes/:slug" element={<Recipes />} />
          <Route path="/login" element={<Login />} />
          <Route path="/admin" element={<SettingsLayout />}>
            <Route index element={<Navigate to={SETTINGS_HOME} replace />} />
            <Route path="sso" element={<AdminSso />} />
            <Route path="environments" element={<AdminEnvironments />} />
            <Route path="notifications" element={<AdminNotifications />} />
            <Route path="approver-groups" element={<AdminApproverGroups />} />
            <Route path="merge-rules" element={<AdminMergeRules />} />
            <Route path="service-accounts" element={<AdminServiceAccounts />} />
            <Route path="audit" element={<AdminAudit />} />
            <Route path="about" element={<AdminAbout />} />
            <Route path="runner-tokens" element={<AdminRunnerTokens />} />
            <Route path="api-tokens" element={<AdminApiTokens />} />
          </Route>
          <Route path="/changes" element={<Changes />} />
          <Route path="/changes/:id" element={<ChangeDetail />} />
          <Route path="/pipelines" element={<AllPipelines />} />
          <Route path="/jobs" element={<Jobs />} />
          <Route path="/environments" element={<EnvironmentBoard />} />
          <Route path="/tokens" element={<ApiTokens />} />
          <Route path="/account" element={<Account />} />
          <Route path="/pipelines/:id" element={<PipelineDetail />} />
          <Route path="/jobs/:id" element={<JobLog />} />
          <Route path="/new" element={<NewPipeline />} />
        </Routes>
      </Frame>
    </BrowserRouter>
  );
}
