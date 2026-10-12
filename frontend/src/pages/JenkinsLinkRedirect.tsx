import { useEffect, useState } from "react";
import { Link, useLocation, useNavigate, useParams } from "react-router-dom";
import { getControllerJenkinsLink } from "../api/client";
import { RecoveryState } from "../components/RecoveryState";

/**
 * Serves the headless door route /headless/:cluster/:namespace/:controller/*.
 * A Jenkins link under a headless controller opens here. The page asks the
 * BFF which Varroa page the Jenkins item names, then replaces the URL with it.
 */
export function JenkinsLinkRedirect() {
  const { cluster = "", namespace = "", controller = "" } = useParams();
  const { pathname } = useLocation();
  const navigate = useNavigate();
  const [failed, setFailed] = useState(false);

  // The raw pathname keeps %252F intact. The decoded "*" parameter would
  // turn it into %2F before the BFF sees it.
  const rest = pathname.split("/").slice(5).join("/");

  useEffect(() => {
    let cancelled = false;
    setFailed(false);
    getControllerJenkinsLink(cluster, namespace, controller, rest)
      .then((res) => {
        if (!cancelled) navigate(res.link, { replace: true });
      })
      .catch(() => {
        if (!cancelled) setFailed(true);
      });
    return () => {
      cancelled = true;
    };
  }, [cluster, namespace, controller, rest, navigate]);

  if (failed) {
    return (
      <>
        <RecoveryState
          kind="404"
          title="Build page not found"
          message="No Varroa page matches this Jenkins link."
        />
        <p>
          <Link to={`/controllers/${cluster}/${namespace}/${controller}`}>Open the controller</Link>
        </p>
      </>
    );
  }
  return <p role="status">Opening the Varroa page.</p>;
}
