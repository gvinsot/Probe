package notion

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gvinsot/Probe/app/internal/acceptance"
)

const page = "0123456789abcdef0123456789abcdef"
const pageDashed = "01234567-89ab-cdef-0123-456789abcdef"

func TestParsePage(t *testing.T) {
	for ref, want := range map[string]string{
		page:                               pageDashed,
		pageDashed:                         pageDashed,
		"0123456789ABCDEF0123456789ABCDEF": pageDashed,
		"https://www.notion.so/acme/Checkout-rules-" + page + "?pvs=4":         pageDashed,
		"https://acme.notion.site/" + page:                                     pageDashed,
		"https://www.notion.so/acme/" + strings.Repeat("f", 32) + "?p=" + page: strings.Repeat("f", 8) + "-ffff-ffff-ffff-" + strings.Repeat("f", 12),
	} {
		if got, err := ParsePage(ref); err != nil || got != want {
			t.Errorf("ParsePage(%q) = %q, %v; want %q", ref, got, err, want)
		}
	}
	for _, ref := range []string{"", "abc", "https://evil.example/" + page, "http://www.notion.so/" + page, page + "00", "../" + page} {
		if _, err := ParsePage(ref); err == nil {
			t.Errorf("ParsePage(%q) accepted", ref)
		}
	}
	pages, err := ParsePages(page + ", https://www.notion.so/x-" + page)
	if err != nil || len(pages) != 1 {
		t.Fatalf("duplicates: %v %v", pages, err)
	}
	if _, err := ParsePages(strings.Repeat(page+",", 1) + "1" + page[1:] + ",2" + page[1:] + ",3" + page[1:] + ",4" + page[1:] + ",5" + page[1:]); err == nil {
		t.Fatal("more than 5 pages accepted")
	}
}

func TestFromEnv(t *testing.T) {
	get := func(v map[string]string) func(string) string { return func(n string) string { return v[n] } }
	none := func(string) ([]byte, error) { return nil, fs.ErrNotExist }
	c, err := FromEnv(get(nil), none)
	if err != nil || c.Configured() || c.URL != DefaultURL {
		t.Fatalf("unset: %+v %v", c, err)
	}
	c, err = FromEnv(get(map[string]string{TokenEnv: "ntn_x"}), none)
	if err != nil || c.Token != "ntn_x" {
		t.Fatalf("token: %+v %v", c, err)
	}
	for name, v := range map[string]map[string]string{
		"http":  {URLEnv: "http://notion.test"},
		"query": {URLEnv: "https://notion.test?x=1"},
		"file":  {TokenEnv + "_FILE": "/absent"},
		"bool":  {AllowInsecureHTTPEnv: "maybe"},
	} {
		if _, err := FromEnv(get(v), none); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func rich(s string) string { return fmt.Sprintf(`{"rich_text":[{"plain_text":%q}]}`, s) }

func TestFetchRendersPage(t *testing.T) {
	var versions []string
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/pages/"+pageDashed, func(w http.ResponseWriter, r *http.Request) {
		versions = append(versions, r.Header.Get("Notion-Version"))
		if r.Header.Get("Authorization") != "Bearer ntn_secret" {
			http.Error(w, `{"message":"API token is invalid."}`, 401)
			return
		}
		fmt.Fprint(w, `{"object":"page","url":"https://www.notion.so/Checkout-`+page+`","last_edited_time":"2026-09-01T10:00:00.000Z",
"properties":{"Name":{"type":"title","title":[{"plain_text":"Checkout "},{"plain_text":"architecture"}]},"Tags":{"type":"multi_select"}}}`)
	})
	mux.HandleFunc("/v1/blocks/"+pageDashed+"/children", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start_cursor") == "" {
			fmt.Fprint(w, `{"results":[
{"id":"b1","type":"paragraph","paragraph":`+rich("Prices are integer cents.")+`},
{"id":"b2","type":"bulleted_list_item","bulleted_list_item":`+rich("Services talk over gRPC")+`,"has_children":true},
{"id":"b3","type":"heading_1","heading_1":`+rich("Acceptance criteria")+`}
],"has_more":true,"next_cursor":"c2"}`)
			return
		}
		fmt.Fprint(w, `{"results":[
{"id":"b4","type":"to_do","to_do":{"rich_text":[{"plain_text":"Totals never use floats"}],"checked":false}},
{"id":"b5","type":"numbered_list_item","numbered_list_item":`+rich("Refunds are validated")+`},
{"id":"b6","type":"heading_1","heading_1":`+rich("Risks")+`},
{"id":"b7","type":"bulleted_list_item","bulleted_list_item":`+rich("Legacy client")+`},
{"id":"b8","type":"code","code":{"rich_text":[{"plain_text":"- not a list"}],"language":"go"}},
{"id":"b9","type":"child_page","child_page":{"title":"Runbook"}}
],"has_more":false}`)
	})
	mux.HandleFunc("/v1/blocks/b2/children", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"results":[{"id":"b2a","type":"bulleted_list_item","bulleted_list_item":`+rich("mTLS inside the cluster")+`}],"has_more":false}`)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := &Client{Config: Config{URL: server.URL, Token: "ntn_secret", AllowInsecureHTTP: true}}
	p, err := client.Fetch(context.Background(), pageDashed)
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Checkout architecture" || p.Truncated || versions[0] != APIVersion || !strings.HasPrefix(p.URL, "https://www.notion.so/") {
		t.Fatalf("page %+v %v", p, versions)
	}
	text := p.Intent()
	for _, want := range []string{
		"Notion page: Checkout architecture\n\nLast edited: 2026-09-01T10:00:00.000Z · Link: https://www.notion.so/",
		"## Page content\n\nPrices are integer cents.\n\n• Services talk over gRPC\n  • mTLS inside the cluster\n",
		"### Acceptance criteria\n\n- [ ] Totals never use floats\n1. Refunds are validated\n",
		"### Risks\n\n• Legacy client\n```go\n- not a list\n```",
		"Sub-page: Runbook",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("intent lacks %q:\n%s", want, text)
		}
	}
	doc, err := acceptance.Parse(text)
	if err != nil || len(doc.Criteria) != 2 || doc.Criteria[0].Text != "Totals never use floats" {
		t.Fatalf("criteria %+v %v", doc.Criteria, err)
	}

	if _, err := client.Fetch(context.Background(), strings.Repeat("0", 8)+"-0000-0000-0000-"+strings.Repeat("0", 12)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
	bad := &Client{Config: Config{URL: server.URL, Token: "wrong", AllowInsecureHTTP: true}}
	if _, err := bad.Fetch(context.Background(), pageDashed); err == nil || !strings.Contains(err.Error(), "HTTP 401") {
		t.Fatalf("unauthorized: %v", err)
	}
	if _, err := client.Fetch(context.Background(), page); err == nil {
		t.Fatal("non-canonical ID fetched")
	}
}

func TestBlockBudgetMarksTruncation(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if strings.HasPrefix(r.URL.Path, "/v1/pages/") {
			fmt.Fprint(w, `{"object":"page","properties":{}}`)
			return
		}
		// Every block has children: the depth limit stops the recursion.
		fmt.Fprintf(w, `{"results":[{"id":"n%d","type":"toggle","toggle":%s,"has_children":true}],"has_more":false}`, calls, rich("level"))
	}))
	defer server.Close()
	client := &Client{Config: Config{URL: server.URL, Token: "t", AllowInsecureHTTP: true}}
	p, err := client.Fetch(context.Background(), pageDashed)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || calls > maxDepth+3 || !strings.Contains(p.Intent(), "read only part") {
		t.Fatalf("truncated %v after %d calls", p.Truncated, calls)
	}
}
