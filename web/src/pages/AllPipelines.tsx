import { Link } from "react-router-dom";
import PipelineBrowser from "../components/PipelineBrowser";

/** Every pipeline across repos, with filters and saved multi-repo views. */
export default function AllPipelines() {
  return (
    <div>
      <div className="breadcrumbs">
        <Link to="/">Repos</Link> <span className="crumb-sep">/</span>{" "}
        <span>pipelines</span>
      </div>
      <div className="page-head">
        <h1>Pipelines</h1>
        <div className="page-head-actions">
        </div>
      </div>
      <PipelineBrowser />
    </div>
  );
}
