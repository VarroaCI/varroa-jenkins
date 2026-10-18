package provider

import (
	"bufio"
	"io"
	"strings"
)

// sseMaxLine caps one stream line. A line above the cap ends the scan with an
// error from the scanner.
const sseMaxLine = 1 << 20

// ScanSSE reads a server-sent event stream. It calls onLine for every line it
// reads, which lets a caller reset an idle timer. It calls fn for each event
// once the blank line that ends the event is read. Comment lines that start
// with a colon are ignored. A pending event at end of input is dispatched too.
func ScanSSE(r io.Reader, onLine func(), fn func(event, data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), sseMaxLine)
	var event string
	var data []string
	dispatch := func() error {
		if event == "" && len(data) == 0 {
			return nil
		}
		err := fn(event, strings.Join(data, "\n"))
		event = ""
		data = data[:0]
		return err
	}
	for sc.Scan() {
		if onLine != nil {
			onLine()
		}
		line := sc.Text()
		switch {
		case line == "":
			if err := dispatch(); err != nil {
				return err
			}
		case strings.HasPrefix(line, ":"):
			continue
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimPrefix(strings.TrimPrefix(line, "event:"), " ")
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return dispatch()
}
