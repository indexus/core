package node

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/indexus/go-indexus-core/indexus"
)

func TestLocalChildrenOneLevel(t *testing.T) {
	n := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	const col = "Demo000000000000"
	for _, loc := range []string{"a", "a0z", "a_", "zz"} {
		if err := n.node.New(&indexus.Item{Collection: col, Location: loc, Id: "doc-" + loc}, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		loc    string
		status int
		want   []string
	}{
		{"@", 200, []string{"a", "z"}}, {"a", 200, []string{"a0", "a_"}}, {"a_", 200, []string{}},
		{"0123456789abcdef", 200, []string{}}, {"invalid!", 400, nil}, {"0123456789abcdefg", 400, nil},
	} {
		url := fmt.Sprintf("http://127.0.0.1:%d/zones/children/local?collection=%s&location=%s", n.port, col, tc.loc)
		resp, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Children []string
			Complete *bool
		}
		err = json.NewDecoder(resp.Body).Decode(&result)
		resp.Body.Close()
		if err != nil || resp.StatusCode != tc.status {
			t.Fatalf("%s: %d %v", tc.loc, resp.StatusCode, err)
		}
		if tc.status == 200 && (result.Children == nil || !slices.Equal(result.Children, tc.want) || result.Complete == nil || *result.Complete) {
			t.Fatalf("%s: %+v", tc.loc, result)
		}
	}
}

func TestLocalChildrenIncludesUncertainTombstoneCandidatesOnRequest(t *testing.T) {
	n := startDataNode(t, "AAAAAAAAAAAAAAAB", nil)
	const col = "Demo000000000000"
	for _, item := range []indexus.Item{
		{Collection: col, Location: "aa", Id: "seen"},
		{Collection: col, Location: "zz", Id: "a-first"},
		{Collection: col, Location: "zz", Id: "b-live"},
	} {
		if err := n.node.New(&item, "@", nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.node.Delete(&indexus.Item{Collection: col, Location: "zz", Id: "a-first"}, "@", nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		loc, flag string
		want      []string
	}{
		{"@", "", []string{"a"}}, {"@", "&include_uncertain=false", []string{"a"}},
		{"@", "&include_uncertain=true", []string{"a", "z"}},
		{"z", "&include_uncertain=true", []string{"zz"}}, {"zz", "&include_uncertain=true", []string{}},
	} {
		url := fmt.Sprintf("http://127.0.0.1:%d/zones/children/local?collection=%s&location=%s%s", n.port, col, tc.loc, tc.flag)
		r, err := http.Get(url)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			Children  []string
			Complete  *bool
			Uncertain *bool `json:"include_uncertain"`
		}
		err = json.NewDecoder(r.Body).Decode(&got)
		r.Body.Close()
		wantEcho := tc.flag == "&include_uncertain=true"
		if err != nil || r.StatusCode != 200 || got.Children == nil || !slices.Equal(got.Children, tc.want) || got.Complete == nil || *got.Complete || (got.Uncertain != nil) != wantEcho || (wantEcho && !*got.Uncertain) {
			t.Fatalf("%s: status=%d response=%+v error=%v", url, r.StatusCode, got, err)
		}
	}
}
