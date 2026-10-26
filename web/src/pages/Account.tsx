import { useCallback, useEffect, useState } from "react";
import { useSearchParams } from "react-router-dom";
import { linkGitHubURL, listHostLinks, relativeTime, unlinkHost, type HostLink } from "../api";

/**
 * Your linked host accounts. With GitHub linked, the comments, reviews and
 * merges you make in Forge are made as you on GitHub (with your own rate
 * budget); without, Forge's bot posts them "via Forge" with your name, and
 * your approvals count in Forge's gate but are not posted as GitHub reviews.
 */
export default function Account() {
  const [params] = useSearchParams();
  const [links, setLinks] = useState<HostLink[] | null>(null);
  const [canLink, setCanLink] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const reload = useCallback(() => {
    listHostLinks()
      .then((r) => {
        setLinks(r.links);
        setCanLink(r.github_linking);
      })
      .catch((e) => setError(e instanceof Error ? e.message : String(e)));
  }, []);
  useEffect(reload, [reload]);
  const github = links?.find((l) => l.provider === "github");
  return (
    <div>
      <div className="page-head">
        <h1>Linked accounts</h1>
      </div>
      <p className="page-lede">
        Link your GitHub account so what you do in Forge's code review — comments, approvals, merges — happens on GitHub
        as you. Forge stores the access token encrypted and uses it only for your own actions.
      </p>
      {params.get("linked") === "github" && github && <div className="muted">Linked GitHub as {github.login}.</div>}
      {error && <div className="error-banner">{error}</div>}
      <div className="card settings-form">
        <h2>GitHub</h2>
        {links === null ? (
          <p className="muted">Loading…</p>
        ) : github ? (
          <p>
            Linked as <b className="mono">{github.login}</b> <span className="muted">since {relativeTime(github.created_at)}</span>{" "}
            <button
              className="btn"
              onClick={() =>
                void unlinkHost("github")
                  .then(reload)
                  .catch((e) => setError(String(e)))
              }
            >
              Unlink
            </button>
          </p>
        ) : canLink ? (
          <a className="btn btn-primary" href={linkGitHubURL}>
            Link GitHub account
          </a>
        ) : (
          <p className="muted">
            Linking is not set up on this server. An admin enables it by turning on GitHub sign-in, or by setting
            GITHUB_LINK_CLIENT_ID and GITHUB_LINK_CLIENT_SECRET (see Docs → Code review).
          </p>
        )}
      </div>
    </div>
  );
}
