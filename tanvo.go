// Package tanvo is the Go client for Tanvo (https://tanvo.ai), an AI image, video and music studio: images on
// Nano Banana, Seedream, GPT Image and Qwen, video on Kling, Veo, Seedance and Wan, songs on Suno, and 80
// ready-made photo apps. It uses only the standard library.
//
// Without an API key (option or TANVO_API_KEY) the client runs on the anonymous free tier: the house image engine
// (studio-image-v1) only, watermarked. Create a key at https://tanvo.ai/settings/apikeys for everything else.
//
//	client := tanvo.New()
//	img, err := client.GenerateImage(ctx, tanvo.ImageParams{Prompt: "a paper lantern over a misty lake", Aspect: "16:9"})
package tanvo

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version of this client.
const Version = "0.1.0"

// DefaultBaseURL is the production service.
const DefaultBaseURL = "https://tanvo.ai"

// Client talks to the Tanvo API. It is safe for concurrent use.
type Client struct {
	apiKey  string
	anonID  string
	baseURL string
	http    *http.Client
	sleep   func(ctx context.Context, d time.Duration) error

	mu     sync.Mutex
	models []Model
	apps   []App
}

// Option configures a Client.
type Option func(*Client)

// WithAPIKey sets the API key (default: TANVO_API_KEY).
func WithAPIKey(key string) Option { return func(c *Client) { c.apiKey = key } }

// WithBaseURL points the client at another deployment (default: TANVO_BASE_URL, then https://tanvo.ai).
func WithBaseURL(u string) Option { return func(c *Client) { c.baseURL = strings.TrimRight(u, "/") } }

// WithHTTPClient sets the HTTP client (default: 60 s timeout).
func WithHTTPClient(h *http.Client) Option { return func(c *Client) { c.http = h } }

// WithAnonID fixes the free-tier wallet id (default: a new id per client).
func WithAnonID(id string) Option { return func(c *Client) { c.anonID = id } }

// New creates a client.
func New(opts ...Option) *Client {
	c := &Client{apiKey: os.Getenv("TANVO_API_KEY"), baseURL: DefaultBaseURL, http: &http.Client{Timeout: 60 * time.Second}, sleep: sleepCtx}
	if u := os.Getenv("TANVO_BASE_URL"); u != "" {
		c.baseURL = strings.TrimRight(u, "/")
	}
	for _, o := range opts {
		o(c)
	}
	if c.apiKey == "" && c.anonID == "" {
		c.anonID = "go-" + uuid()
	}
	return c
}

// ─── Types ───────────────────────────────────────────────────────────────────

// Output is one generated file. Songs also carry Cover, Title, Lyrics and Seconds.
type Output struct {
	URL     string  `json:"url"`
	Mime    string  `json:"mime"`
	Cover   string  `json:"cover,omitempty"`
	Title   string  `json:"title,omitempty"`
	Lyrics  string  `json:"lyrics,omitempty"`
	Seconds float64 `json:"seconds,omitempty"`
}

// Generation is one run. Status is PENDING, PROCESSING, SUCCEEDED or FAILED; Cost is in credits (refunded on failure).
type Generation struct {
	ID        string         `json:"id"`
	Kind      string         `json:"kind"`
	Model     string         `json:"model"`
	Prompt    string         `json:"prompt"`
	Options   map[string]any `json:"options"`
	Cost      int            `json:"cost"`
	Status    string         `json:"status"`
	Error     *string        `json:"error"`
	CreatedAt int64          `json:"createdAt"`
	Outputs   []Output       `json:"outputs"`
}

// Done reports whether the run has finished, successfully or not.
func (g *Generation) Done() bool {
	return g.Status == "SUCCEEDED" || g.Status == "FAILED" || g.Status == "CANCELED"
}

// URL is the first output's URL, or "" while running or after a failure.
func (g *Generation) URL() string {
	if len(g.Outputs) == 0 {
		return ""
	}
	return g.Outputs[0].URL
}

// Model is one entry of the model catalogue.
type Model struct {
	ID                string         `json:"id"`
	Kind              string         `json:"kind"`
	Name              string         `json:"name"`
	FreeTier          bool           `json:"free_tier"`
	Inputs            []string       `json:"inputs"`
	Options           map[string]any `json:"options"`
	Defaults          map[string]any `json:"defaults"`
	CreditsAtDefaults int            `json:"credits_at_defaults"`
	PromptMaxChars    int            `json:"prompt_max_chars"`
}

// Look is one preset of an app: a tuned prompt, model and settings with a real example output.
type Look struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Prompt  string         `json:"prompt"`
	Model   string         `json:"model"`
	Options map[string]any `json:"options"`
	URL     string         `json:"url"`
}

// AppInput is one photo slot of an app.
type AppInput struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Hint  string `json:"hint"`
	Min   int    `json:"min"`
	Max   int    `json:"max"`
}

// App is a ready-made app (pet portraits, figurines, trend videos, songs…).
type App struct {
	Slug        string     `json:"slug"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Description string     `json:"description"`
	Category    string     `json:"category"`
	Model       string     `json:"model"`
	URL         string     `json:"url"`
	Inputs      []AppInput `json:"inputs"`
	Looks       []Look     `json:"looks"`
}

// Account is who the client runs as.
type Account struct {
	Tier        string `json:"tier"`
	Email       string `json:"email,omitempty"`
	Credits     int    `json:"credits"`
	Watermarked bool   `json:"watermarked"`
	Concurrency int    `json:"concurrency"`
}

// File is a photo input: a public https URL, a local path, or bytes (set Data, and Mime if you know it).
type File struct {
	URL  string
	Path string
	Data []byte
	Mime string
}

// URLFile, PathFile and BytesFile are shorthands for File.
func URLFile(u string) File                { return File{URL: u} }
func PathFile(p string) File               { return File{Path: p} }
func BytesFile(b []byte, mime string) File { return File{Data: b, Mime: mime} }

// Common holds the settings every generate call accepts.
type Common struct {
	// Wait for the result. Nil means the default for the kind: wait for images and songs, return at once for video.
	Wait *bool
	// Timeout for waiting (default 3 min images, 10 min video, 5 min songs). On timeout the run is returned as it stands.
	Timeout time.Duration
	// RequestKey makes the submit idempotent: a repeat with the same key returns the first run, uncharged. Made for you if empty.
	RequestKey string
	// WebhookURL receives the finished run (https only) instead of polling.
	WebhookURL string
	// WaitForSlot waits for a free concurrency slot instead of returning a busy error.
	WaitForSlot bool
}

// ImageParams for GenerateImage. Empty fields use the model's defaults.
type ImageParams struct {
	Prompt         string
	Model          string // default studio-image-v1 (free tier)
	Aspect         string
	Resolution     string
	Format         string
	Images         []File
	NegativePrompt string
	Common
}

// VideoParams for GenerateVideo. Empty fields use the model's defaults.
type VideoParams struct {
	Prompt         string
	Model          string // default studio-video-v1
	Aspect         string
	Resolution     string
	Duration       int
	Audio          *bool
	Image          *File // start frame
	EndImage       *File // end frame, on models that support it
	NegativePrompt string
	Common
}

// MusicParams for GenerateMusic.
type MusicParams struct {
	Prompt string
	Mode   string // Describe (default), Lyrics or Instrumental
	Style  string
	Title  string
	Vocal  string // "m" or "f"
	Model  string // default suno-v6
	Common
}

// AppParams for GenerateFromApp.
type AppParams struct {
	App     string
	Look    string // default: the app's first look
	Photos  []File // in the order of the app's inputs
	Details string // a personal touch woven into the prompt
	Common
}

// Bool returns a pointer to b, for Common.Wait and VideoParams.Audio.
func Bool(b bool) *bool { return &b }

// ─── Errors ──────────────────────────────────────────────────────────────────

// Kind classifies an API error.
type Kind int

const (
	KindService        Kind = iota // upstream or network failure: safe to retry with the same RequestKey
	KindInvalidRequest             // bad parameters, refused prompt, file too large, not found
	KindAuth                       // missing or invalid API key
	KindQuota                      // credits, free tier limits, rate limits, busy
)

// Error is returned for every API failure. Code is the API's stable error string.
type Error struct {
	Code       string
	Message    string
	Status     int
	Issues     []string
	RetryAfter time.Duration
	Kind       Kind
}

func (e *Error) Error() string {
	if len(e.Issues) > 0 {
		return fmt.Sprintf("tanvo: %s: %s (%s)", e.Code, e.Message, strings.Join(e.Issues, "; "))
	}
	return fmt.Sprintf("tanvo: %s: %s", e.Code, e.Message)
}

// Sentinel errors for errors.Is: errors.Is(err, tanvo.ErrQuota).
var (
	ErrInvalidRequest = errors.New("tanvo: invalid request")
	ErrAuth           = errors.New("tanvo: authentication")
	ErrQuota          = errors.New("tanvo: quota")
	ErrService        = errors.New("tanvo: service")
)

// Is matches the sentinel for the error's kind.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrInvalidRequest:
		return e.Kind == KindInvalidRequest
	case ErrAuth:
		return e.Kind == KindAuth
	case ErrQuota:
		return e.Kind == KindQuota
	case ErrService:
		return e.Kind == KindService
	}
	return false
}

var kinds = map[string]Kind{
	"invalid": KindInvalidRequest, "rejected": KindInvalidRequest, "too_large": KindInvalidRequest, "not_found": KindInvalidRequest,
	"unauthorized": KindAuth, "invalid_api_key": KindAuth, "unknown_anon_id": KindAuth,
	"credits": KindQuota, "allowance": KindQuota, "needs_api_key": KindQuota, "anon_ip_daily": KindQuota, "trial_closed": KindQuota, "rate_limited": KindQuota, "limit": KindQuota, "busy": KindQuota,
}
var needsKey = map[string]bool{"needs_api_key": true, "allowance": true, "anon_ip_daily": true, "trial_closed": true}

// ─── HTTP ────────────────────────────────────────────────────────────────────

func (c *Client) do(ctx context.Context, method, path string, body any, out any, retries int) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	rateLimited := false
	for attempt := 0; ; attempt++ {
		err := c.once(ctx, method, path, payload, out)
		var e *Error
		if err == nil || !errors.As(err, &e) {
			return err
		}
		if e.Code == "rate_limited" && !rateLimited {
			rateLimited = true
			wait := e.RetryAfter
			if wait <= 0 || wait > time.Minute {
				wait = 5 * time.Second
			}
			if err := c.sleep(ctx, wait); err != nil {
				return err
			}
			attempt--
			continue
		}
		if e.Kind == KindService && attempt < retries {
			if err := c.sleep(ctx, []time.Duration{time.Second, 3 * time.Second}[min(attempt, 1)]); err != nil {
				return err
			}
			continue
		}
		return e
	}
}

func (c *Client) once(ctx context.Context, method, path string, payload []byte, out any) error {
	var rd io.Reader
	if payload != nil {
		rd = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/api/v1"+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "tanvo-go/"+Version)
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	} else {
		req.Header.Set("x-anon-id", c.anonID)
	}
	res, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &Error{Code: "network", Message: fmt.Sprintf("could not reach %s: %v", c.baseURL, err), Kind: KindService}
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if res.StatusCode >= 200 && res.StatusCode < 300 {
		if out == nil || len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, out); err != nil {
			return &Error{Code: "bad_response", Message: "unreadable response: " + err.Error(), Status: res.StatusCode, Kind: KindService}
		}
		return nil
	}
	var body struct {
		Error   string   `json:"error"`
		Message string   `json:"message"`
		Issues  []string `json:"issues"`
	}
	_ = json.Unmarshal(raw, &body)
	e := &Error{Code: body.Error, Message: body.Message, Status: res.StatusCode, Issues: body.Issues, Kind: KindService}
	if e.Code == "" {
		e.Code = "http_" + strconv.Itoa(res.StatusCode)
	}
	if e.Message == "" {
		e.Message = http.StatusText(res.StatusCode)
	}
	if k, ok := kinds[e.Code]; ok {
		e.Kind = k
	}
	if s, err := strconv.Atoi(res.Header.Get("Retry-After")); err == nil {
		e.RetryAfter = time.Duration(s) * time.Second
	}
	if needsKey[e.Code] && c.apiKey == "" {
		e.Message += " Get a key at https://tanvo.ai/settings/apikeys (then use WithAPIKey or set TANVO_API_KEY)."
	}
	return e
}

// ─── Catalogue and account ───────────────────────────────────────────────────

// Models lists every model with its options, defaults and price at defaults (cached). kind "" means all.
func (c *Client) Models(ctx context.Context, kind string) ([]Model, error) {
	c.mu.Lock()
	cached := c.models
	c.mu.Unlock()
	if cached == nil {
		var r struct {
			Models []Model `json:"models"`
		}
		if err := c.do(ctx, http.MethodGet, "/models", nil, &r, 2); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.models, cached = r.Models, r.Models
		c.mu.Unlock()
	}
	out := []Model{}
	for _, m := range cached {
		if kind == "" || m.Kind == kind {
			out = append(out, m)
		}
	}
	return out, nil
}

// Apps lists the ready-made apps with their looks (cached). kind "" means all.
func (c *Client) Apps(ctx context.Context, kind string) ([]App, error) {
	c.mu.Lock()
	cached := c.apps
	c.mu.Unlock()
	if cached == nil {
		var r struct {
			Apps []App `json:"apps"`
		}
		if err := c.do(ctx, http.MethodGet, "/apps", nil, &r, 2); err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.apps, cached = r.Apps, r.Apps
		c.mu.Unlock()
	}
	out := []App{}
	for _, a := range cached {
		if kind == "" || a.Kind == kind {
			out = append(out, a)
		}
	}
	return out, nil
}

// Me returns who the client runs as and the credits left.
func (c *Client) Me(ctx context.Context) (*Account, error) {
	var a Account
	if err := c.do(ctx, http.MethodGet, "/me", nil, &a, 2); err != nil {
		return nil, err
	}
	return &a, nil
}

// ─── Generation ──────────────────────────────────────────────────────────────

// options fills the model's defaults, then the caller's non-empty settings.
func (c *Client) options(ctx context.Context, model string, given map[string]any) map[string]any {
	out := map[string]any{}
	if ms, err := c.Models(ctx, ""); err == nil {
		for _, m := range ms {
			if m.ID == model {
				for k, v := range m.Defaults {
					out[k] = v
				}
			}
		}
	}
	for k, v := range given {
		switch x := v.(type) {
		case string:
			if x != "" {
				out[k] = x
			}
		case int:
			if x != 0 {
				out[k] = x
			}
		case *bool:
			if x != nil {
				out[k] = *x
			}
		default:
			if v != nil {
				out[k] = v
			}
		}
	}
	return out
}

func (c *Client) submit(ctx context.Context, body map[string]any, common Common, waitByDefault bool, timeout time.Duration, every time.Duration) (*Generation, error) {
	if common.Timeout > 0 {
		timeout = common.Timeout
	}
	deadline := time.Now().Add(timeout)
	body["requestKey"] = common.RequestKey
	if common.RequestKey == "" {
		body["requestKey"] = uuid()
	}
	if common.WebhookURL != "" {
		body["webhookUrl"] = common.WebhookURL
	}
	for k, v := range body {
		if s, ok := v.(string); ok && s == "" {
			delete(body, k)
		}
		if l, ok := v.([]string); ok && len(l) == 0 {
			delete(body, k)
		}
	}
	var r struct {
		Generation Generation `json:"generation"`
	}
	for {
		err := c.do(ctx, http.MethodPost, "/generations", body, &r, 2)
		var e *Error
		if err != nil && errors.As(err, &e) && e.Code == "busy" && common.WaitForSlot && time.Now().Add(5*time.Second).Before(deadline) {
			if err := c.sleep(ctx, 5*time.Second); err != nil {
				return nil, err
			}
			continue
		}
		if err != nil {
			return nil, err
		}
		break
	}
	wait := waitByDefault
	if common.Wait != nil {
		wait = *common.Wait
	}
	if !wait {
		return &r.Generation, nil
	}
	return c.Wait(ctx, r.Generation.ID, time.Until(deadline), every)
}

func (c *Client) refs(ctx context.Context, files []File) ([]string, error) {
	urls := make([]string, 0, len(files))
	for _, f := range files {
		u, err := c.ref(ctx, f)
		if err != nil {
			return nil, err
		}
		urls = append(urls, u)
	}
	return urls, nil
}

// GenerateImage makes an image from a prompt, or edits / combines Images. It waits for the result by default.
func (c *Client) GenerateImage(ctx context.Context, p ImageParams) (*Generation, error) {
	model := orDefault(p.Model, "studio-image-v1")
	urls, err := c.refs(ctx, p.Images)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"kind": "image", "model": model, "prompt": p.Prompt, "negativePrompt": p.NegativePrompt, "imageUrls": urls,
		"options": c.options(ctx, model, map[string]any{"aspect": p.Aspect, "resolution": p.Resolution, "format": p.Format})}
	return c.submit(ctx, body, p.Common, true, 3*time.Minute, 4*time.Second)
}

// GenerateVideo makes a video from a prompt, or animates Image. It returns at once by default; use Wait.
func (c *Client) GenerateVideo(ctx context.Context, p VideoParams) (*Generation, error) {
	model := orDefault(p.Model, "studio-video-v1")
	body := map[string]any{"kind": "video", "model": model, "prompt": p.Prompt, "negativePrompt": p.NegativePrompt,
		"options": c.options(ctx, model, map[string]any{"aspect": p.Aspect, "resolution": p.Resolution, "duration": p.Duration, "audio": p.Audio})}
	if p.Image != nil {
		u, err := c.ref(ctx, *p.Image)
		if err != nil {
			return nil, err
		}
		body["imageUrls"] = []string{u}
	}
	if p.EndImage != nil {
		u, err := c.ref(ctx, *p.EndImage)
		if err != nil {
			return nil, err
		}
		body["endImageUrl"] = u
	}
	return c.submit(ctx, body, p.Common, false, 10*time.Minute, 6*time.Second)
}

// GenerateMusic writes two takes of a song (MP3 with cover art, title and lyrics). It waits by default.
func (c *Client) GenerateMusic(ctx context.Context, p MusicParams) (*Generation, error) {
	opts := map[string]any{"tier": orDefault(p.Mode, "Describe"), "resolution": "song"}
	for k, v := range map[string]string{"style": p.Style, "title": p.Title, "vocal": p.Vocal} {
		if v != "" {
			opts[k] = v
		}
	}
	body := map[string]any{"kind": "music", "model": orDefault(p.Model, "suno-v6"), "prompt": p.Prompt, "options": opts}
	return c.submit(ctx, body, p.Common, true, 5*time.Minute, 4*time.Second)
}

// GenerateFromApp runs an app's look on the user's photos, e.g. AppParams{App: "ai-pet-portrait-generator", Look: "royal", Photos: …}.
func (c *Client) GenerateFromApp(ctx context.Context, p AppParams) (*Generation, error) {
	apps, err := c.Apps(ctx, "")
	if err != nil {
		return nil, err
	}
	var app *App
	for i := range apps {
		if apps[i].Slug == p.App {
			app = &apps[i]
		}
	}
	if app == nil || len(app.Looks) == 0 {
		return nil, &Error{Code: "not_found", Message: fmt.Sprintf("unknown app %q; see Apps()", p.App), Kind: KindInvalidRequest}
	}
	look := &app.Looks[0]
	if p.Look != "" {
		look = nil
		ids := []string{}
		for i := range app.Looks {
			ids = append(ids, app.Looks[i].ID)
			if app.Looks[i].ID == p.Look {
				look = &app.Looks[i]
			}
		}
		if look == nil {
			return nil, &Error{Code: "not_found", Message: fmt.Sprintf("%s has no look %q; looks: %s", app.Name, p.Look, strings.Join(ids, ", ")), Kind: KindInvalidRequest}
		}
	}
	needed := 0
	for _, in := range app.Inputs {
		if in.Min > 0 {
			needed++
		}
	}
	if len(p.Photos) < needed {
		return nil, &Error{Code: "invalid", Message: fmt.Sprintf("%s needs %d photo(s)", app.Name, needed), Kind: KindInvalidRequest}
	}
	prompt := look.Prompt
	if d := strings.TrimSpace(p.Details); d != "" {
		prompt += "\n\nThis one is for: " + d + "."
	}
	body := map[string]any{"kind": app.Kind, "model": look.Model, "prompt": prompt, "options": c.options(ctx, look.Model, look.Options)}
	if app.Kind != "music" {
		urls, err := c.refs(ctx, p.Photos)
		if err != nil {
			return nil, err
		}
		body["imageUrls"] = urls
	}
	switch app.Kind {
	case "video":
		return c.submit(ctx, body, p.Common, false, 10*time.Minute, 6*time.Second)
	case "music":
		return c.submit(ctx, body, p.Common, true, 5*time.Minute, 4*time.Second)
	}
	return c.submit(ctx, body, p.Common, true, 3*time.Minute, 4*time.Second)
}

// ─── Runs ────────────────────────────────────────────────────────────────────

// Get reads one run.
func (c *Client) Get(ctx context.Context, id string) (*Generation, error) {
	var r struct {
		Generation Generation `json:"generation"`
	}
	if err := c.do(ctx, http.MethodGet, "/generations/"+url.PathEscape(id), nil, &r, 2); err != nil {
		return nil, err
	}
	return &r.Generation, nil
}

// Wait polls until the run finishes. On timeout it returns the last state seen (still running) without an error.
// every <= 0 means 4 seconds.
func (c *Client) Wait(ctx context.Context, id string, timeout, every time.Duration) (*Generation, error) {
	if every <= 0 {
		every = 4 * time.Second
	}
	deadline := time.Now().Add(timeout)
	if err := c.sleep(ctx, min(2*time.Second, every)); err != nil {
		return nil, err
	}
	var last *Generation
	for {
		g, err := c.Get(ctx, id)
		if err == nil {
			last = g
			if g.Done() {
				return g, nil
			}
		} else if !errors.Is(err, ErrService) {
			return nil, err // a failed read is not a failed run, but a bad key or unknown id will not clear
		}
		if time.Now().Add(every).After(deadline) {
			if last == nil {
				return c.Get(ctx, id)
			}
			return last, nil
		}
		if err := c.sleep(ctx, every); err != nil {
			return nil, err
		}
	}
}

// List returns runs newest first; pass the returned cursor as before for the next page (0 when there is none).
func (c *Client) List(ctx context.Context, limit int, before int64) ([]Generation, int64, error) {
	q := url.Values{}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if before > 0 {
		q.Set("before", strconv.FormatInt(before, 10))
	}
	var r struct {
		Generations []Generation `json:"generations"`
		NextBefore  *int64       `json:"next_before"`
	}
	if err := c.do(ctx, http.MethodGet, "/generations?"+q.Encode(), nil, &r, 2); err != nil {
		return nil, 0, err
	}
	var next int64
	if r.NextBefore != nil {
		next = *r.NextBefore
	}
	return r.Generations, next, nil
}

// ─── Files ───────────────────────────────────────────────────────────────────

var mimes = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp", ".mp4": "video/mp4", ".mov": "video/quicktime", ".mp3": "audio/mpeg", ".wav": "audio/wav", ".m4a": "audio/mp4"}

// Upload stores bytes and returns a URL usable as a photo input.
func (c *Client) Upload(ctx context.Context, data []byte, mime string) (string, error) {
	if mime == "" {
		mime = http.DetectContentType(data)
	}
	var slot struct {
		UploadURL string `json:"upload_url"`
		URL       string `json:"url"`
	}
	if err := c.do(ctx, http.MethodPost, "/uploads", map[string]any{"mime": mime, "bytes": len(data)}, &slot, 0); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, slot.UploadURL, bytes.NewReader(data))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mime)
	res, err := c.http.Do(req)
	if err != nil {
		return "", &Error{Code: "upload_failed", Message: err.Error(), Kind: KindService}
	}
	res.Body.Close()
	if res.StatusCode >= 300 {
		return "", &Error{Code: "upload_failed", Message: "upload failed with HTTP " + strconv.Itoa(res.StatusCode), Status: res.StatusCode, Kind: KindService}
	}
	return slot.URL, nil
}

func (c *Client) ref(ctx context.Context, f File) (string, error) {
	switch {
	case f.URL != "":
		if strings.HasPrefix(f.URL, "http://") && !strings.HasPrefix(c.baseURL, "http://") {
			return "", &Error{Code: "invalid", Message: "only https URLs can be fetched by the service: " + f.URL, Kind: KindInvalidRequest}
		}
		return f.URL, nil
	case f.Path != "":
		data, err := os.ReadFile(f.Path)
		if err != nil {
			return "", err
		}
		mime := f.Mime
		if mime == "" {
			mime = mimes[strings.ToLower(filepath.Ext(f.Path))]
		}
		return c.Upload(ctx, data, mime)
	case len(f.Data) > 0:
		return c.Upload(ctx, f.Data, f.Mime)
	}
	return "", &Error{Code: "invalid", Message: "empty File: set URL, Path or Data", Kind: KindInvalidRequest}
}

// Download saves output index of g into dir (created if needed) and returns the file path.
func (c *Client) Download(ctx context.Context, g *Generation, dir string, index int) (string, error) {
	if index < 0 || index >= len(g.Outputs) {
		return "", fmt.Errorf("tanvo: generation %s has no output %d (status %s)", g.ID, index, g.Status)
	}
	u := g.Outputs[index].URL
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	res, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return "", fmt.Errorf("tanvo: download failed with HTTP %d", res.StatusCode)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	ext := filepath.Ext(strings.SplitN(u, "?", 2)[0])
	if ext == "" {
		ext = ".bin"
	}
	path := filepath.Join(dir, fmt.Sprintf("tanvo-%s-%d%s", g.ID, index, ext))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, res.Body); err != nil {
		return "", err
	}
	return path, nil
}

// ─── helpers ─────────────────────────────────────────────────────────────────

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
