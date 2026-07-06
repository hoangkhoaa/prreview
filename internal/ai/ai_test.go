package ai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"prreview/internal/config"
)

const sampleDiff = `diff --git a/svc/user.go b/svc/user.go
index 111..222 100644
--- a/svc/user.go
+++ b/svc/user.go
@@ -10,6 +10,8 @@ func GetUser(id string) (*User, error) {
 	u, err := db.Find(id)
 	if err != nil {
 		return nil, err
 	}
+	fmt.Println("debug", u)
+	u.LastSeen = time.Now()
 	return u, nil
 }
diff --git a/svc/old.go b/svc/old.go
deleted file mode 100644
--- a/svc/old.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package svc
-
-func old() {}
`

func TestParseDiffAndSnap(t *testing.T) {
	idx := parseDiff(sampleDiff)

	if _, ok := idx.files["svc/user.go"]; !ok {
		t.Fatalf("expected svc/user.go in index, got %v", idx.paths())
	}
	// hunk starts at new line 10: lines 10-13 context, 14-15 added, 16-17 context
	if !idx.added["svc/user.go"][14] || !idx.added["svc/user.go"][15] {
		t.Errorf("expected lines 14,15 to be added, got %v", idx.added["svc/user.go"])
	}
	if got := idx.snap("svc/user.go", 14); got != 14 {
		t.Errorf("snap exact: got %d, want 14", got)
	}
	if got := idx.snap("svc/user.go", 19); got != 17 {
		t.Errorf("snap nearby: got %d, want 17", got)
	}
	if got := idx.snap("svc/user.go", 500); got != 0 {
		t.Errorf("snap far away: got %d, want 0", got)
	}
	if got := idx.snap("svc/missing.go", 14); got != 0 {
		t.Errorf("snap unknown file: got %d, want 0", got)
	}
	// deleted files (+++ /dev/null) must not be indexed
	if _, ok := idx.files["svc/old.go"]; ok {
		t.Errorf("deleted file should not be commentable")
	}
}

func TestDedupe(t *testing.T) {
	idx := parseDiff(sampleDiff)
	in := []Finding{
		{Path: "svc/user.go", Line: 14, Severity: "minor", Confidence: 60, Comment: "debug print"},
		{Path: "svc/user.go", Line: 15, Severity: "major", Confidence: 80, Comment: "debug print left"},
		{Path: "svc/user.go", Line: 999, Severity: "major", Confidence: 90, Comment: "unanchored"},
		{Path: "other.go", Line: 3, Severity: "major", Confidence: 90, Comment: "file not in diff"},
	}
	out := dedupe(idx, in)
	if len(out) != 2 {
		t.Fatalf("want 2 findings (merged dup + unanchored kept with line 0), got %d: %+v", len(out), out)
	}
	if out[0].Severity != "major" {
		t.Errorf("dedupe should keep the more severe duplicate, got %+v", out[0])
	}
	if out[1].Line != 0 {
		t.Errorf("unanchored finding should have line 0, got %d", out[1].Line)
	}
}

func TestOpenAIClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("unexpected auth header %q", got)
		}
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "deepseek-chat" {
			t.Errorf("unexpected model %v", req["model"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": `{"findings": []}`}},
			},
		})
	}))
	defer srv.Close()

	cfg := &config.Config{APIKey: "test-key", Provider: "openai", BaseURL: srv.URL, Model: "deepseek-chat"}
	client, err := newLLM(cfg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := client.complete(context.Background(), llmRequest{
		system: "sys", sharedContext: "ctx", instruction: "go", maxTokens: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out != `{"findings": []}` {
		t.Errorf("unexpected output %q", out)
	}
}
