import { type ReactNode } from "react";
import { Navigate, Route, Routes, useLocation } from "react-router-dom";
import { useSession } from "./lib/session";
import { OpsProvider } from "./components/ops";
import { Shell } from "./components/shell";
import { Skeleton } from "./components/ui";
import { LoginScreen, SetupScreen } from "./screens/auth";
import { CutoverTab, ReportTab, VerificationTab } from "./screens/cutover";
import { AlertsScreen, EventsScreen, HostScreen, LogsScreen, NotFound, SettingsScreen, SystemScreen } from "./screens/global";
import { HomeScreen } from "./screens/home";
import { MigrationLayout, OverviewTab } from "./screens/migration";
import { DatabaseScreen, MigrationEventsTab, MigrationLogsTab, ReplicationTab, SourceHealthTab } from "./screens/mtabs";
import { PreflightScreen } from "./screens/preflight";
import { NewMigrationScreen } from "./screens/wizard";
import { useMig } from "./screens/migration";

function Gate({ children }: { children: ReactNode }) {
  const { status } = useSession();
  const loc = useLocation();
  if (status === "loading") return <div className="login"><div style={{ width: 320 }}><Skeleton lines={3} /></div></div>;
  if (status === "setup") return <Navigate to="/setup" replace />;
  if (status === "anonymous") return <Navigate to={`/login${loc.pathname !== "/" ? `?next=${encodeURIComponent(loc.pathname + loc.search)}` : ""}`} replace />;
  return <OpsProvider><Shell>{children}</Shell></OpsProvider>;
}

function PreflightTab() {
  const { id } = useMig();
  return <PreflightScreen mig={id} />;
}

export function App() {
  const { status } = useSession();
  return (
    <Routes>
      <Route path="/setup" element={status === "ready" ? <Navigate to="/" replace /> : <SetupScreen />} />
      <Route path="/login" element={status === "ready" ? <Navigate to="/" replace /> : status === "setup" ? <Navigate to="/setup" replace /> : <LoginScreen />} />
      <Route path="/*" element={
        <Gate>
          <Routes>
            <Route index element={<HomeScreen />} />
            <Route path="migrations/new" element={<NewMigrationScreen />} />
            <Route path="migrations/:id/setup" element={<NewMigrationScreen />} />
            <Route path="migrations/:id" element={<MigrationLayout />}>
              <Route index element={<OverviewTab />} />
              <Route path="databases/:db" element={<DatabaseScreen />} />
              <Route path="replication" element={<ReplicationTab />} />
              <Route path="source-health" element={<SourceHealthTab />} />
              <Route path="preflight" element={<PreflightTab />} />
              <Route path="cutover" element={<CutoverTab />} />
              <Route path="verification" element={<VerificationTab />} />
              <Route path="report" element={<ReportTab />} />
              <Route path="logs" element={<MigrationLogsTab />} />
              <Route path="events" element={<MigrationEventsTab />} />
            </Route>
            <Route path="logs" element={<LogsScreen />} />
            <Route path="events" element={<EventsScreen />} />
            <Route path="alerts" element={<AlertsScreen />} />
            <Route path="host" element={<HostScreen />} />
            <Route path="settings" element={<SettingsScreen />} />
            <Route path="system" element={<SystemScreen />} />
            <Route path="*" element={<NotFound />} />
          </Routes>
        </Gate>
      } />
    </Routes>
  );
}
