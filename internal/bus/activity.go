package bus

// ActivityPayload is the single publish/consume schema for Jenkins activity
// events on the bus. It is published via ActivitySubject(cluster, ns, controller) by
// the gateway bus bridge and consumed by the BFF's bus-routed ingest.
//
// Cluster is stamped by the publishing gateway from its resolved identity,
// always serialized.
//
// Both name (routing alias read by bus_fanout.deliverBusEvent) and controller
// (display alias read by the frontend/store) carry the same value — the stream
// identity controller name. The duplication keeps existing fanout routing
// untouched while giving the frontend the field it expects.
type ActivityPayload struct {
	Event       string `json:"event"` // routing + type alias
	Name        string `json:"name"`  // routing alias for controller
	Namespace   string `json:"namespace"`
	Cluster     string `json:"cluster"`
	Source      string `json:"source"` // "jenkins"
	Type        string `json:"type"`
	Actor       string `json:"actor,omitempty"`
	Controller  string `json:"controller,omitempty"` // display alias = Name
	Message     string `json:"message,omitempty"`
	ItemPath    string `json:"itemPath,omitempty"`
	BuildNumber int64  `json:"buildNumber,omitempty"`
	Result      string `json:"result,omitempty"`
	URL         string `json:"url,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
}
