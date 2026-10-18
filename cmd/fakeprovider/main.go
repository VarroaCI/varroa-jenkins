// Command fakeprovider serves a scripted model provider for tests. It answers the
// Messages and Chat Completions streams with the turns of a script file. It
// ships in no image.
package main

import (
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"

	"github.com/varroaci/varroa-jenkins/internal/agent/provider/providertest"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8089", "listen address")
	script := flag.String("script", "", "script file of turns")
	record := flag.String("record", "", "file that receives one JSON line per request")
	flag.Parse()

	var s providertest.Script
	if *script != "" {
		loaded, err := providertest.Load(*script)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		s = loaded
	}
	srv := providertest.New(s)
	if *record != "" {
		srv.SetRecordFile(*record)
	}
	l, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := serve(l, srv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// serve answers HTTP on l until the listener closes.
func serve(l net.Listener, h http.Handler) error {
	return http.Serve(l, h)
}
