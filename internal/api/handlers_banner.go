package api

import (
	"fmt"
	"net/http"
)

// HandleVarroaBanner serves the JavaScript snippet that renders the Varroa
// header bar inside Jenkins controllers. The plugin injects a <script> tag
// pointing at this endpoint with query params for controller context.
// miteConnected is resolved live from the registry so the banner stays
// accurate even when Jenkins caches the URL across operator restarts.
func (s *Server) HandleVarroaBanner(w http.ResponseWriter, r *http.Request) {
	controller := r.URL.Query().Get("controller")
	namespace := r.URL.Query().Get("namespace")
	phase := r.URL.Query().Get("phase")
	miteConnected := s.deps.MiteRegistry != nil && s.deps.MiteRegistry.Connected(namespace, controller)
	backURL := r.URL.Query().Get("back")

	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	fmt.Fprintf(w, `(function(){
var c=%q,n=%q,p=%q,m=%t,b=%q;

// Phase pill colors matching the Varroa design system.
var phaseColors={Connected:{bg:"rgba(47,143,78,.15)",fg:"#2F8F4E",dot:"#2F8F4E"},Running:{bg:"rgba(44,125,140,.15)",fg:"#2C7D8C",dot:"#2C7D8C"},Provisioning:{bg:"rgba(199,127,14,.15)",fg:"#C77F0E",dot:"#C77F0E"},Failed:{bg:"rgba(190,58,40,.15)",fg:"#BE3A28",dot:"#BE3A28"}};
var pc=phaseColors[p]||{bg:"rgba(138,121,96,.15)",fg:"#8A7960",dot:"#8A7960"};

// Build the vbar exactly like the mockup.
var el=document.createElement("div");
el.innerHTML=
 '<div style="display:flex;align-items:center;gap:13px;padding:0 18px;height:54px;background:#241608;color:#F3E4C8;border-bottom:2px solid #C2611C;font-family:-apple-system,BlinkMacSystemFont,Inter,Segoe UI,sans-serif;font-size:14px;line-height:1.5">'+
  // Logo
  '<svg width="26" height="26"><polygon points="13,2 23,7.5 23,18.5 13,24 3,18.5 3,7.5" fill="#E0A21A"/><polygon points="13,7 19,10.5 19,15.5 13,19 7,15.5 7,10.5" fill="#241608"/><circle cx="13" cy="13" r="2" fill="#E0A21A"/></svg>'+
  // Brand name
  '<span style="font-weight:700;font-size:15px;letter-spacing:-.2px;color:#FBEFD6">Varroa</span>'+
  // Divider
  '<span style="width:1px;height:22px;background:rgba(255,255,255,.16);flex-shrink:0"></span>'+
  // Controller name
  '<span style="font-weight:600;font-size:13.5px;color:#F3E4C8">'+c+'</span>'+
  // Phase pill with proper coloring
  '<span style="display:inline-flex;align-items:center;gap:6px;font-size:11px;font-weight:700;padding:3px 10px 3px 8px;border-radius:20px;letter-spacing:.1px;background:'+pc.bg+';color:'+pc.fg+'"><span style="width:7px;height:7px;border-radius:50%%;background:'+pc.dot+'"></span>'+p+'</span>'+
  // Mite chip with pulse animation
  (m
   ? '<span style="display:inline-flex;align-items:center;gap:6px;font-size:11px;font-weight:600;padding:3px 9px;border-radius:7px;background:rgba(255,255,255,.10);color:#EAD7B5">'+
     '<span class="v-pulse" style="position:relative;width:8px;height:8px;flex-shrink:0"><i style="position:absolute;inset:0;border-radius:50%%;background:#2F8F4E"></i></span> mite linked</span>'
   : '<span style="display:inline-flex;align-items:center;gap:6px;font-size:11px;font-weight:600;padding:3px 9px;border-radius:7px;background:rgba(255,255,255,.10);color:#EAD7B5"><span style="width:8px;height:8px;border-radius:50%%;background:#8A7960;flex-shrink:0"></span> mite offline</span>')+
  // Spacer
  '<div style="margin-left:auto"></div>'+
  // Namespace chip
  '<span style="font-size:11px;font-weight:600;padding:3px 9px;border-radius:7px;background:rgba(255,255,255,.10);color:#EAD7B5">ns/'+n+'</span>'+
  // Control plane link
  '<a href="'+b+'" style="font-size:12.5px;font-weight:600;color:#EBC07A;text-decoration:none;display:inline-flex;align-items:center;gap:6px">↩ Control plane</a>'+
  // Avatar
  '<div style="width:30px;height:30px;border-radius:50%%;background:#C2611C;color:#fff;display:grid;place-items:center;font-weight:700;font-size:11.5px;border:2px solid rgba(255,255,255,.22)">NB</div>'+
 '</div>';

// Inject CSS for the mite pulse animation.
var style=document.createElement("style");
style.textContent='.v-pulse.on i::after,.v-pulse.on i::before{content:"";position:absolute;inset:0;border-radius:50%%;border:2px solid #2F8F4E;animation:vping 1.9s cubic-bezier(0,0,.2,1) infinite}.v-pulse.on i::before{animation-delay:0.95s}@keyframes vping{0%%{transform:scale(1);opacity:0.7}80%%,100%%{transform:scale(3);opacity:0}}';
document.head.appendChild(style);

// Add 'on' class to pulse if mite is connected.
if(m) el.querySelector('.v-pulse').classList.add('on');

var target=document.querySelector("#page-header, #header");
if(target) target.insertBefore(el.firstChild, target.firstChild); else document.body.prepend(el.firstChild);
})();`, controller, namespace, phase, miteConnected, backURL)
}
