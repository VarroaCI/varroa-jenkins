package forge

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"testing"
)

// TestCreateIssue_GiteaLabelOnLaterPage checks that a label that sits past the
// first page of the repository's labels still resolves.
func TestCreateIssue_GiteaLabelOnLaterPage(t *testing.T) {
	c := giteaClientFor(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/labels": func(w http.ResponseWriter, r *http.Request) {
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			if page == 1 {
				body := "["
				for i := 0; i < maxLabelPage; i++ {
					if i > 0 {
						body += ","
					}
					body += fmt.Sprintf(`{"id":%d,"name":"l%d"}`, 100+i, i)
				}
				body += "]"
				_, _ = io.WriteString(w, body)
				return
			}
			_, _ = io.WriteString(w, `[{"id":7,"name":"ops"}]`)
		},
		"POST /repos/o/r/issues": jsonReply(`{"html_url":"https://example.com/o/r/issues/8","number":8}`),
	})
	issue, err := c.CreateIssue(context.Background(), testRepo, "t", "b", []string{"ops"})
	if err != nil || issue.Number != 8 {
		t.Fatalf("issue %+v err %v", issue, err)
	}
}
