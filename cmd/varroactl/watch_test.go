package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// watch tests
// ---------------------------------------------------------------------------

func TestWatch_InitialTable(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/stream/brood") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			flusher.Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"items":[
			{"namespace":"team-a","name":"ctrl-1","phase":"Running","version":"2.0","miteConnected":true,"jenkinsHealth":"ok"}
		]}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"watch", "-n", "team-a", "--server", srv.URL})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWatch_DebouncedRelist(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	var listCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/stream/brood") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "event: heartbeat\ndata: {\"name\":\"ctrl-1\",\"namespace\":\"team-a\"}\n\n")
			flusher.Flush()
			<-r.Context().Done()
			return
		}
		listCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"items":[
			{"namespace":"team-a","name":"ctrl-1","phase":"Running","version":"2.0","miteConnected":true}
		]}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(2500 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"watch", "--server", srv.URL})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if listCount.Load() < 2 {
		t.Errorf("expected at least 2 list calls, got %d", listCount.Load())
	}
}

func TestWatch_ChangedRowOnly(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	var listCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/stream/brood") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "event: snapshot\ndata: {\"name\":\"ctrl-1\",\"namespace\":\"team-a\"}\n\n")
			flusher.Flush()
			<-r.Context().Done()
			return
		}
		n := listCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			fmt.Fprintf(w, `{"items":[
				{"namespace":"team-a","name":"ctrl-1","phase":"Running","version":"2.0","miteConnected":true}
			]}`)
		} else {
			fmt.Fprintf(w, `{"items":[
				{"namespace":"team-a","name":"ctrl-1","phase":"Stopped","version":"2.0","miteConnected":true}
			]}`)
		}
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"watch", "--server", srv.URL})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWatch_DeletedRow(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	var listCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/stream/brood") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			fmt.Fprintf(w, "event: disconnected\ndata: {\"name\":\"ctrl-1\",\"namespace\":\"team-a\"}\n\n")
			flusher.Flush()
			<-r.Context().Done()
			return
		}
		n := listCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if n == 1 {
			fmt.Fprintf(w, `{"items":[
				{"namespace":"team-a","name":"ctrl-1","phase":"Running","version":"2.0","miteConnected":true}
			]}`)
		} else {
			fmt.Fprintf(w, `{"items":[]}`)
		}
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(500 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"watch", "--server", srv.URL})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWatch_WwithJSONIsUsageError(t *testing.T) {
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"items":[]}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()

	root.SetArgs([]string{"get", "controller", "-w", "-o", "json", "--server", srv.URL})
	err := root.Execute()
	if err == nil {
		t.Fatal("expected usage error for -w -o json")
	}
	if !strings.Contains(err.Error(), "usage") && !strings.Contains(err.Error(), "only supported") {
		t.Errorf("expected usage error message, got %v", err)
	}
}

func TestWatch_CtxCancelExits0(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/stream/brood") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			flusher.Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"items":[]}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"watch", "--server", srv.URL})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("expected nil on cancel, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Two items with same ns/name in different clusters render two rows
// ---------------------------------------------------------------------------

func TestWatch_TwoClustersSameName(t *testing.T) {
	defer overrideSignalCtx()()
	testSetup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/stream/brood") {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			flusher.Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		// Two items, same ns/name, different clusters
		fmt.Fprintf(w, `{"items":[
			{"namespace":"team-a","name":"ctrl-1","phase":"Running","version":"2.0","cluster":"core","miteConnected":true},
			{"namespace":"team-a","name":"ctrl-1","phase":"Running","version":"2.0","cluster":"remote","miteConnected":true}
		]}`)
	}))
	defer srv.Close()

	version = "test"
	root := newRootCmd()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		time.Sleep(1500 * time.Millisecond)
		cancel()
	}()

	root.SetArgs([]string{"watch", "--server", srv.URL})
	root.SetContext(ctx)
	if err := root.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
