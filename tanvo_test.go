package tanvo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var png = append([]byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}, make([]byte, 24)...)

type fake struct {
	mu        sync.Mutex
	srv       *httptest.Server
	posts     []map[string]any
	headers   []http.Header
	runs      map[string]*Generation
	polls     map[string]int
	byKey     map[string]string
	failNext  []string // "status:code"
	doneAfter int
	uploads   []string
}

func newFake(t *testing.T) *fake {
	f := &fake{runs: map[string]*Generation{}, polls: map[string]int{}, byKey: map[string]string{}, doneAfter: 1}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) client(opts ...Option) *Client {
	c := New(append([]Option{WithBaseURL(f.srv.URL)}, opts...)...)
	c.sleep = func(context.Context, time.Duration) error { return nil }
	return c
}

func send(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fake) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := io.ReadAll(r.Body)
	if strings.HasPrefix(r.URL.Path, "/put/") {
		f.uploads = append(f.uploads, r.Header.Get("Content-Type"))
		return
	}
	if strings.HasPrefix(r.URL.Path, "/files/") {
		_, _ = w.Write(png)
		return
	}
	if len(f.failNext) > 0 {
		parts := strings.SplitN(f.failNext[0], ":", 2)
		f.failNext = f.failNext[1:]
		status, _ := strconv.Atoi(parts[0])
		if parts[1] == "rate_limited" {
			w.Header().Set("Retry-After", "2")
		}
		send(w, status, map[string]any{"error": parts[1], "message": "injected " + parts[1]})
		return
	}
	auth := r.Header.Get("Authorization")
	if auth != "" && auth != "Bearer sk_good" {
		send(w, 401, map[string]any{"error": "invalid_api_key", "message": "That API key is invalid."})
		return
	}
	switch {
	case r.URL.Path == "/api/v1/models":
		send(w, 200, map[string]any{"models": []map[string]any{
			{"id": "studio-image-v1", "kind": "image", "defaults": map[string]any{"aspect": "1:1", "resolution": "1K", "format": "PNG"}},
			{"id": "seedream-4", "kind": "image", "defaults": map[string]any{"aspect": "1:1", "resolution": "2K"}},
			{"id": "studio-video-v1", "kind": "video", "defaults": map[string]any{"aspect": "16:9", "resolution": "480p", "duration": 5}},
		}})
	case r.URL.Path == "/api/v1/apps":
		send(w, 200, map[string]any{"apps": []map[string]any{{
			"slug": "ai-pet-portrait-generator", "kind": "image", "name": "Pet Portrait Generator", "model": "seedream-4",
			"inputs": []map[string]any{{"id": "in1", "label": "Pet photo", "min": 1, "max": 1}},
			"looks":  []map[string]any{{"id": "renaissance", "prompt": "oil portrait", "model": "seedream-4", "options": map[string]any{}}, {"id": "royal", "prompt": "Paint the pet as a monarch.", "model": "seedream-4", "options": map[string]any{"aspect": "3:4"}}},
		}}})
	case r.URL.Path == "/api/v1/me":
		tier := "anonymous"
		if auth != "" {
			tier = "paid"
		}
		send(w, 200, map[string]any{"tier": tier, "credits": 5, "watermarked": auth == "", "concurrency": 1})
	case r.Method == "POST" && r.URL.Path == "/api/v1/uploads":
		send(w, 200, map[string]any{"upload_url": f.srv.URL + "/put/" + strconv.Itoa(len(f.uploads)), "url": "https://media.tanvo.ai/uploads/u/" + strconv.Itoa(len(f.uploads)) + ".png"})
	case r.Method == "POST" && r.URL.Path == "/api/v1/generations":
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		f.posts = append(f.posts, b)
		f.headers = append(f.headers, r.Header.Clone())
		if auth == "" && b["model"] != "studio-image-v1" {
			send(w, 403, map[string]any{"error": "needs_api_key", "message": "The free tier covers the house image engine only."})
			return
		}
		if opts, _ := b["options"].(map[string]any); opts["resolution"] == "8K" {
			send(w, 400, map[string]any{"error": "invalid", "message": "That model or setting is not available.", "issues": []string{"options.resolution: 8K"}})
			return
		}
		key, _ := b["requestKey"].(string)
		if id, ok := f.byKey[key]; ok {
			send(w, 202, map[string]any{"generation": f.runs[id]})
			return
		}
		id := fmt.Sprintf("gen%d", len(f.runs)+1)
		g := &Generation{ID: id, Kind: b["kind"].(string), Model: b["model"].(string), Cost: 5, Status: "PROCESSING", CreatedAt: int64(1790000000000 + len(f.runs))}
		f.runs[id], f.byKey[key] = g, id
		send(w, 202, map[string]any{"generation": g})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/generations/"):
		id := strings.TrimPrefix(r.URL.Path, "/api/v1/generations/")
		g, ok := f.runs[id]
		if !ok {
			send(w, 404, map[string]any{"error": "not_found", "message": "No such generation."})
			return
		}
		f.polls[id]++
		if f.polls[id] >= f.doneAfter {
			g.Status, g.Outputs = "SUCCEEDED", []Output{{URL: f.srv.URL + "/files/" + id + "/0.png", Mime: "image/png"}}
		}
		send(w, 200, map[string]any{"generation": g})
	case r.URL.Path == "/api/v1/generations":
		list := []*Generation{}
		for i := len(f.runs); i >= 1 && len(list) < 2; i-- {
			list = append(list, f.runs[fmt.Sprintf("gen%d", i)])
		}
		send(w, 200, map[string]any{"generations": list, "next_before": list[len(list)-1].CreatedAt})
	default:
		send(w, 404, map[string]any{"error": "not_found", "message": r.URL.Path})
	}
}

var ctx = context.Background()

func TestFreeImageFillsModelDefaults(t *testing.T) {
	f := newFake(t)
	c := f.client(WithAPIKey(""))
	me, err := c.Me(ctx)
	if err != nil || me.Tier != "anonymous" {
		t.Fatalf("me: %v %+v", err, me)
	}
	g, err := c.GenerateImage(ctx, ImageParams{Prompt: "a cat"})
	if err != nil || g.Status != "SUCCEEDED" || g.Cost != 5 || !strings.HasSuffix(g.URL(), ".png") {
		t.Fatalf("image: %v %+v", err, g)
	}
	opts := f.posts[0]["options"].(map[string]any)
	if opts["aspect"] != "1:1" || opts["resolution"] != "1K" || opts["format"] != "PNG" || f.posts[0]["requestKey"] == "" {
		t.Fatalf("body: %+v", f.posts[0])
	}
	if h := f.headers[0]; !strings.HasPrefix(h.Get("x-anon-id"), "go-") || !strings.HasPrefix(h.Get("User-Agent"), "tanvo-go/") {
		t.Fatalf("headers: %v", h)
	}
}

func TestFreeTierHintAndErrorKinds(t *testing.T) {
	f := newFake(t)
	_, err := f.client(WithAPIKey("")).GenerateVideo(ctx, VideoParams{Prompt: "a fox"})
	var e *Error
	if !errors.As(err, &e) || e.Code != "needs_api_key" || !errors.Is(err, ErrQuota) || !strings.Contains(e.Message, "settings/apikeys") {
		t.Fatalf("got %v", err)
	}
	_, err = f.client(WithAPIKey("sk_bad")).Me(ctx)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("want auth error, got %v", err)
	}
	_, err = f.client(WithAPIKey("sk_good")).GenerateImage(ctx, ImageParams{Prompt: "x", Resolution: "8K"})
	if !errors.As(err, &e) || !errors.Is(err, ErrInvalidRequest) || e.Issues[0] != "options.resolution: 8K" {
		t.Fatalf("want invalid, got %v", err)
	}
}

func TestOptionsOverrideDefaultsAndIdempotency(t *testing.T) {
	f := newFake(t)
	c := f.client(WithAPIKey("sk_good"))
	a, _ := c.GenerateImage(ctx, ImageParams{Prompt: "x", Model: "seedream-4", Aspect: "16:9", Common: Common{Wait: Bool(false), RequestKey: "job-1"}})
	b, _ := c.GenerateImage(ctx, ImageParams{Prompt: "x", Model: "seedream-4", Aspect: "16:9", Common: Common{Wait: Bool(false), RequestKey: "job-1"}})
	if a.ID != b.ID || len(f.runs) != 1 {
		t.Fatalf("idempotency: %s %s %d", a.ID, b.ID, len(f.runs))
	}
	opts := f.posts[0]["options"].(map[string]any)
	if opts["aspect"] != "16:9" || opts["resolution"] != "2K" || len(opts) != 2 || f.headers[0].Get("Authorization") != "Bearer sk_good" {
		t.Fatalf("options: %+v", opts)
	}
}

func TestVideoReturnsAtOnceThenWaits(t *testing.T) {
	f := newFake(t)
	f.doneAfter = 3
	c := f.client(WithAPIKey("sk_good"))
	g, err := c.GenerateVideo(ctx, VideoParams{Prompt: "a fox", Duration: 10, Audio: Bool(true), EndImage: &File{URL: "https://x/end.jpg"}})
	if err != nil || g.Status != "PROCESSING" {
		t.Fatalf("video: %v %+v", err, g)
	}
	opts := f.posts[0]["options"].(map[string]any)
	if opts["duration"] != float64(10) || opts["audio"] != true || f.posts[0]["endImageUrl"] != "https://x/end.jpg" {
		t.Fatalf("body: %+v", f.posts[0])
	}
	done, err := c.Wait(ctx, g.ID, time.Minute, time.Millisecond)
	if err != nil || done.Status != "SUCCEEDED" {
		t.Fatalf("wait: %v %+v", err, done)
	}
}

func TestWaitTimeoutReturnsLastState(t *testing.T) {
	f := newFake(t)
	f.doneAfter = 1 << 30
	c := f.client(WithAPIKey("sk_good"))
	g, _ := c.GenerateVideo(ctx, VideoParams{Prompt: "x"})
	last, err := c.Wait(ctx, g.ID, time.Nanosecond, time.Millisecond)
	if err != nil || last.Status != "PROCESSING" {
		t.Fatalf("got %v %+v", err, last)
	}
}

func TestRetriesServiceErrorsWithSameKey(t *testing.T) {
	f := newFake(t)
	c := f.client(WithAPIKey("sk_good"))
	_, _ = c.Models(ctx, "")
	f.failNext = []string{"502:provider", "500:server_error"}
	g, err := c.GenerateImage(ctx, ImageParams{Prompt: "x", Common: Common{Wait: Bool(false)}})
	if err != nil || g.Status != "PROCESSING" || len(f.posts) != 1 {
		t.Fatalf("got %v %+v posts=%d", err, g, len(f.posts))
	}
	f.failNext = []string{"502:provider", "502:provider", "502:provider"}
	if _, err := c.GenerateImage(ctx, ImageParams{Prompt: "y", Common: Common{Wait: Bool(false)}}); !errors.Is(err, ErrService) {
		t.Fatalf("want service error, got %v", err)
	}
}

func TestRateLimitAndBusy(t *testing.T) {
	f := newFake(t)
	c := f.client(WithAPIKey("sk_good"))
	_, _ = c.Models(ctx, "")
	f.failNext = []string{"429:rate_limited"}
	if me, err := c.Me(ctx); err != nil || me.Tier != "paid" {
		t.Fatalf("rate limit: %v", err)
	}
	f.failNext = []string{"409:busy"}
	if _, err := c.GenerateImage(ctx, ImageParams{Prompt: "x", Common: Common{Wait: Bool(false)}}); !errors.Is(err, ErrQuota) {
		t.Fatalf("want busy, got %v", err)
	}
	f.failNext = []string{"409:busy"}
	if g, err := c.GenerateImage(ctx, ImageParams{Prompt: "x", Common: Common{Wait: Bool(false), WaitForSlot: true}}); err != nil || g.Status != "PROCESSING" {
		t.Fatalf("wait for slot: %v", err)
	}
}

func TestUploadsLocalFilesAndBytes(t *testing.T) {
	f := newFake(t)
	path := filepath.Join(t.TempDir(), "a.png")
	_ = os.WriteFile(path, png, 0o644)
	_, err := f.client(WithAPIKey("sk_good")).GenerateImage(ctx, ImageParams{Prompt: "winter", Model: "seedream-4", Images: []File{PathFile(path), URLFile("https://example.com/a.jpg"), BytesFile(png, "")}, Common: Common{Wait: Bool(false)}})
	if err != nil {
		t.Fatal(err)
	}
	urls := f.posts[0]["imageUrls"].([]any)
	if urls[1] != "https://example.com/a.jpg" || !strings.HasPrefix(urls[0].(string), "https://media.tanvo.ai/uploads/") || len(f.uploads) != 2 || f.uploads[0] != "image/png" || f.uploads[1] != "image/png" {
		t.Fatalf("urls %v uploads %v", urls, f.uploads)
	}
}

func TestGenerateFromApp(t *testing.T) {
	f := newFake(t)
	c := f.client(WithAPIKey("sk_good"))
	if _, err := c.GenerateFromApp(ctx, AppParams{App: "ai-pet-portrait-generator", Look: "royal"}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want missing photo error, got %v", err)
	}
	if _, err := c.GenerateFromApp(ctx, AppParams{App: "ai-pet-portrait-generator", Look: "nope", Photos: []File{URLFile("https://x/r.jpg")}}); err == nil || !strings.Contains(err.Error(), "looks: renaissance, royal") {
		t.Fatalf("want unknown look, got %v", err)
	}
	g, err := c.GenerateFromApp(ctx, AppParams{App: "ai-pet-portrait-generator", Look: "royal", Photos: []File{URLFile("https://x/rex.jpg")}, Details: "Rex"})
	if err != nil || g.Status != "SUCCEEDED" {
		t.Fatalf("run: %v %+v", err, g)
	}
	b := f.posts[0]
	if b["model"] != "seedream-4" || b["prompt"] != "Paint the pet as a monarch.\n\nThis one is for: Rex." || b["options"].(map[string]any)["aspect"] != "3:4" {
		t.Fatalf("body: %+v", b)
	}
}

func TestListAndDownload(t *testing.T) {
	f := newFake(t)
	c := f.client(WithAPIKey("sk_good"))
	for i := 0; i < 3; i++ {
		_, _ = c.GenerateImage(ctx, ImageParams{Prompt: "x", Common: Common{Wait: Bool(false)}})
	}
	list, next, err := c.List(ctx, 2, 0)
	if err != nil || len(list) != 2 || next != list[1].CreatedAt {
		t.Fatalf("list: %v %d %d", err, len(list), next)
	}
	g, _ := c.Wait(ctx, list[0].ID, time.Minute, time.Millisecond)
	path, err := c.Download(ctx, g, t.TempDir(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != string(png) {
		t.Fatal("downloaded bytes differ")
	}
}

func TestNetworkErrorIsServiceError(t *testing.T) {
	c := New(WithAPIKey("sk_good"), WithBaseURL("http://127.0.0.1:9"))
	c.sleep = func(context.Context, time.Duration) error { return nil }
	var e *Error
	if _, err := c.Me(ctx); !errors.As(err, &e) || e.Code != "network" || !errors.Is(err, ErrService) {
		t.Fatalf("got %v", err)
	}
}

func TestVerifyWebhook(t *testing.T) {
	key := []byte(strings.Repeat("k", 24))
	secret := "whsec_" + base64.StdEncoding.EncodeToString(key)
	body := []byte(`{"id":"gen1","kind":"image","model":"m","status":"SUCCEEDED","cost":5,"outputs":[{"url":"https://x/0.png","mime":"image/png"}]}`)
	sign := func(ts int64, b []byte) http.Header {
		m := hmac.New(sha256.New, key)
		m.Write([]byte("msg_1." + strconv.FormatInt(ts, 10) + "."))
		m.Write(b)
		h := http.Header{}
		h.Set("webhook-id", "msg_1")
		h.Set("webhook-timestamp", strconv.FormatInt(ts, 10))
		h.Set("webhook-signature", "v1,"+base64.StdEncoding.EncodeToString(m.Sum(nil)))
		return h
	}
	now := time.Now().Unix()
	g, err := VerifyWebhook(secret, sign(now, body), body, 0)
	if err != nil || g.ID != "gen1" || g.URL() != "https://x/0.png" {
		t.Fatalf("valid: %v %+v", err, g)
	}
	if _, err := VerifyWebhook(secret, sign(now, body), []byte(strings.Replace(string(body), "gen1", "gen2", 1)), 0); !errors.Is(err, ErrWebhook) {
		t.Fatal("tampered body accepted")
	}
	if _, err := VerifyWebhook(secret, sign(now-600, body), body, 0); !errors.Is(err, ErrWebhook) {
		t.Fatal("stale timestamp accepted")
	}
	if _, err := VerifyWebhook(secret, http.Header{}, body, 0); !errors.Is(err, ErrWebhook) {
		t.Fatal("missing headers accepted")
	}
}
