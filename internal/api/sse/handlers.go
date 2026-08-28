package sse

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// jsonError writes the uniform {"error": msg} envelope (N1) for the rare
// pre-stream failure paths in this package.
func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// HandleMiteStream serves a per-controller SSE stream of mite heartbeat and
// snapshot events. URL path must be parsed by the caller; cluster, namespace and name
// are passed as parameters. Key format: cluster/ns/name.
func HandleMiteStream(b EventSource, cluster, namespace, name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tracer := otel.Tracer("varroa-bff")
		_, span := tracer.Start(r.Context(), "sse.miteStream",
			trace.WithAttributes(
				attribute.String("jenkins.controller", name),
				attribute.String("k8s.namespace", namespace),
				attribute.String("cluster", cluster),
			),
		)
		defer span.End()

		flusher, ok := w.(http.Flusher)
		if !ok {
			jsonError(w, http.StatusInternalServerError, "streaming not supported")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		key := cluster + "/" + namespace + "/" + name
		ch := b.Subscribe(key)
		defer b.Unsubscribe(key, ch)

		// Keepalive ticker.
		keepalive := time.NewTicker(30 * time.Second)
		defer keepalive.Stop()

		// Send initial connected status.
		sendSSE(w, flusher, "init", map[string]interface{}{
			"name":      name,
			"namespace": namespace,
		})

		for {
			select {
			case <-r.Context().Done():
				return

			case <-keepalive.C:
				fmt.Fprintf(w, ": ping\n\n")
				flusher.Flush()

			case record, ok := <-ch:
				if !ok {
					return
				}
				sendSSE(w, flusher, record.Event, record.Data)
			}
		}
	}
}

// HandleBroodStream serves a brood-wide SSE stream of mite connect/disconnect
// events.
func HandleBroodStream(b EventSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tracer := otel.Tracer("varroa-bff")
		_, span := tracer.Start(r.Context(), "sse.broodStream")
		defer span.End()

		flusher, ok := w.(http.Flusher)
		if !ok {
			jsonError(w, http.StatusInternalServerError, "streaming not supported")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch := b.SubscribeAll()
		defer b.Unsubscribe("*", ch)

		keepalive := time.NewTicker(30 * time.Second)
		defer keepalive.Stop()

		for {
			select {
			case <-r.Context().Done():
				return

			case <-keepalive.C:
				fmt.Fprintf(w, ": ping\n\n")
				flusher.Flush()

			case record, ok := <-ch:
				if !ok {
					return
				}
				sendSSE(w, flusher, record.Event, record.Data)
			}
		}
	}
}

// HandleControllerStream serves a per-controller SSE stream of brood events
// filtered to the given key (format: "namespace/name").
func HandleControllerStream(b EventSource, key string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tracer := otel.Tracer("varroa-bff")
		_, span := tracer.Start(r.Context(), "sse.controllerStream",
			trace.WithAttributes(attribute.String("controller.key", key)),
		)
		defer span.End()

		flusher, ok := w.(http.Flusher)
		if !ok {
			jsonError(w, http.StatusInternalServerError, "streaming not supported")
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		ch := b.Subscribe(key)
		defer b.Unsubscribe(key, ch)

		keepalive := time.NewTicker(30 * time.Second)
		defer keepalive.Stop()

		for {
			select {
			case <-r.Context().Done():
				return

			case <-keepalive.C:
				fmt.Fprintf(w, ": ping\n\n")
				flusher.Flush()

			case record, ok := <-ch:
				if !ok {
					return
				}
				sendSSEMessage(w, flusher, record.Data)
			}
		}
	}
}

func sendSSE(w http.ResponseWriter, flusher http.Flusher, event string, data interface{}) {
	js, err := json.Marshal(data)
	if err != nil {
		slog.Default().Error("marshal error", "event", event, "error", err)
		return
	}
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, js)
	flusher.Flush()
}

// sendSSEMessage sends an unnamed SSE message (triggers onmessage in browsers).
func sendSSEMessage(w http.ResponseWriter, flusher http.Flusher, data interface{}) {
	js, err := json.Marshal(data)
	if err != nil {
		slog.Default().Error("marshal error", "error", err)
		return
	}
	fmt.Fprintf(w, "data: %s\n\n", js)
	flusher.Flush()
}
