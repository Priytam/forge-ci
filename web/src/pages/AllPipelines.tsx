import PipelineBrowser from "../components/PipelineBrowser";

/** Every pipeline across repos, with filters and saved multi-repo views. */
export default function AllPipelines() {
  return (
    <div>
      <div className="page-head">
        <h1>Pipelines</h1>
        <div className="page-head-actions">
        </div>
      </div>
      <PipelineBrowser />
    </div>
  );
}
