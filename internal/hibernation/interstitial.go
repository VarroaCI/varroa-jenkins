package hibernation

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// InterstitialParams controls the shared controller wake page.
type InterstitialParams struct {
	StatusPath                string
	TargetURL                 string
	HTTPStatus                int
	RedirectOnNonWakeResponse bool
}

// WriteInterstitial renders the wake page used by both the BFF and operator.
func WriteInterstitial(w http.ResponseWriter, p InterstitialParams) {
	statusPath, _ := json.Marshal(p.StatusPath)
	targetURL, _ := json.Marshal(p.TargetURL)
	status := p.HTTPStatus
	if status == 0 {
		status = http.StatusOK
	}
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "5")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>Waking Controller</title>
<style>
/*
 * This page is served from the controller's own domain, so it cannot reach the
 * dashboard stylesheet or its design tokens. The values below are copied
 * verbatim from frontend/src/styles/tokens.css and must be kept in step with
 * it — check them against that file rather than trusting this copy. Note the
 * mark uses --accent-fill/--on-accent, not --accent: tokens.css records that
 * --accent cannot carry label text (4.18:1, under the 4.5:1 floor). Both
 * themes are honoured via prefers-color-scheme, since there is no app shell
 * here to carry the user's explicit theme choice.
 */
:root {
  --bg: #FAF6EC; --surface: #FFFFFF; --surface-3: #F2EAD9;
  --border: #EADFC9; --text: #2A1B0E; --text-2: #6E5A44;
  --accent: #C2611C; --accent-fill: #9C4413; --on-accent: #FFFFFF;
}
@media (prefers-color-scheme: dark) {
  :root {
    --bg: #14100A; --surface: #1E1710; --surface-3: #2E2316;
    --border: #332817; --text: #F3E8D6; --text-2: #BCA98C;
    --accent: #C2611C; --accent-fill: #D9762A; --on-accent: #2A1B0E;
  }
}
* { box-sizing: border-box; }
body {
  font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
  display: flex; justify-content: center; align-items: center;
  min-height: 100vh; min-height: 100dvh; margin: 0;
  background: var(--bg); color: var(--text);
}
.card {
  background: var(--surface); border: 1px solid var(--border);
  border-radius: 14px; padding: 40px 32px;
  box-shadow: 0 1px 2px rgba(0,0,0,.06), 0 8px 24px rgba(0,0,0,.10);
  text-align: center; max-width: 420px; margin: 24px;
}
.mark {
  width: 40px; height: 40px; margin: 0 auto 20px;
  border-radius: 11px; background: var(--accent-fill);
  display: flex; align-items: center; justify-content: center;
  color: var(--on-accent); font-weight: 700; font-size: 19px;
}
h1 { font-size: 21px; font-weight: 700; letter-spacing: -.3px; margin: 0 0 10px; }
p { color: var(--text-2); font-size: 14px; line-height: 1.55; margin: 0; }
.bar {
  width: 100%%; height: 4px; margin: 24px 0 0;
  border-radius: 999px; background: var(--surface-3); overflow: hidden;
}
.bar i {
  display: block; width: 40%%; height: 100%%; border-radius: 999px;
  background: var(--accent); animation: slide 1.4s ease-in-out infinite;
}
@keyframes slide {
  0%% { transform: translateX(-100%%); }
  100%% { transform: translateX(250%%); }
}
.hint { font-size: 12.5px; margin-top: 18px; }
.hint:empty { display: none; }
/* Honour users who have asked for less motion. */
@media (prefers-reduced-motion: reduce) {
  .bar i { animation: none; width: 100%%; opacity: .55; }
}
</style>
</head>
<body>
<div class="card">
<div class="mark" aria-hidden="true">V</div>
<h1>Waking Controller</h1>
<p>This controller is not ready to serve requests yet. It is starting up, and you will be redirected as soon as it is ready.</p>
<div class="bar" role="progressbar" aria-label="Controller starting"><i></i></div>
<p class="hint" id="hint"></p>
</div>
<script>
var start = Date.now();
var statusPath = %s;
var targetURL = %s;
var redirectOnNonWake = %t;
function redirect() { window.location.href = targetURL; }
function poll() {
  fetch(statusPath).then(function(r) {
    if (r.status >= 500) return;
    return r.json().then(function(d) {
      if (d && d.varroaWake === true) {
        if (d.phase === 'Connected') redirect();
      } else if (redirectOnNonWake) {
        redirect();
      }
    }).catch(function() {
      if (redirectOnNonWake) redirect();
    });
  }).catch(function() {});
  if (Date.now() - start > 600000) {
    document.getElementById('hint').textContent = 'It\'s taking longer than expected. You can try refreshing the page or contact support.';
  }
}
setInterval(poll, 3000);
poll();
</script>
</body>
</html>`, statusPath, targetURL, p.RedirectOnNonWakeResponse)
}
