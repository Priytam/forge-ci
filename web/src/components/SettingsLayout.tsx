import { Outlet } from "react-router-dom";

/**
 * Frame for every admin page. The settings menu itself lives in the main
 * sidebar while you are under /admin (see Shell), so there is only ever one
 * left-hand menu; this keeps every settings page to one column width.
 */
export default function SettingsLayout() {
  return (
    <div className="settings-main">
      <Outlet />
    </div>
  );
}
