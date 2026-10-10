# Tanvo for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/tanvoai/tanvo-go.svg)](https://pkg.go.dev/github.com/tanvoai/tanvo-go) [![license](https://img.shields.io/badge/license-MIT-blue)](LICENSE)

The Go client for [Tanvo](https://tanvo.ai/?utm_source=github&utm_medium=referral), an AI image, video and music studio. Generate images on Nano Banana, Seedream, GPT Image and Qwen, video on Kling, Veo, Seedance and Wan, and songs on Suno, or run one of 80 ready-made photo apps (pet portraits, figurines, old photo restoration, trend videos) on your own photos.

- Standard library only. Go 1.21+. Safe for concurrent use; every call takes a `context.Context`.
- **Works without an API key** on the free tier (house image engine, watermarked).
- Local files and bytes are uploaded for you; results download with one call.
- Retries that never double-charge (every submit carries an idempotency key).

```bash
go get github.com/tanvoai/tanvo-go
```

## Quick start

```go
client := tanvo.New() // reads TANVO_API_KEY; without one it runs on the free tier

img, err := client.GenerateImage(ctx, tanvo.ImageParams{Prompt: "a paper lantern over a misty lake at dawn", Aspect: "16:9"})
if err != nil {
	log.Fatal(err)
}
fmt.Println(img.URL())
client.Download(ctx, img, "out", 0)
```

Create a key under [Settings → API keys](https://tanvo.ai/settings/apikeys?utm_source=github&utm_medium=referral) to use every model, get clean output and have runs land in your history:

```go
client := tanvo.New(tanvo.WithAPIKey(os.Getenv("TANVO_API_KEY")))
```

## Ready-made apps

```go
apps, _ := client.Apps(ctx, "image") // each with Inputs and preset Looks

portrait, err := client.GenerateFromApp(ctx, tanvo.AppParams{
	App:     "ai-pet-portrait-generator",
	Look:    "royal",
	Photos:  []tanvo.File{tanvo.PathFile("rex.jpg")}, // or URLFile / BytesFile, in the order of the app's inputs
	Details: "Rex, a very good boy",
})
```

## Images

```go
// Any model: client.Models(ctx, "image") lists ids, options and prices
client.GenerateImage(ctx, tanvo.ImageParams{Prompt: "isometric cutaway of a tiny ramen shop", Model: "nano-banana-2", Resolution: "2K"})

// Edit or combine photos
client.GenerateImage(ctx, tanvo.ImageParams{
	Prompt: "change the jacket to dark green; keep the face, pose and background exactly as they are",
	Model:  "nano-banana-2",
	Images: []tanvo.File{tanvo.PathFile("portrait.jpg")},
})
```

## Video

Video takes one to several minutes, so `GenerateVideo` returns at once by default.

```go
job, _ := client.GenerateVideo(ctx, tanvo.VideoParams{Prompt: "slow dolly-in on a lighthouse in a storm", Model: "kling-3-0", Resolution: "720p", Duration: 5})
clip, _ := client.Wait(ctx, job.ID, 10*time.Minute, 0)

// Image to video with an end frame and sound, waiting in one call
client.GenerateVideo(ctx, tanvo.VideoParams{
	Prompt: "the cat turns its head and blinks", Model: "seedance-2-5",
	Image: &tanvo.File{Path: "cat.jpg"}, EndImage: &tanvo.File{URL: "https://example.com/end.jpg"},
	Audio: tanvo.Bool(true), Common: tanvo.Common{Wait: tanvo.Bool(true)},
})
```

## Songs

```go
song, _ := client.GenerateMusic(ctx, tanvo.MusicParams{
	Prompt: "an upbeat birthday song for my sister Ana: surfing, terrible puns, a big singalong chorus",
	Style:  "acoustic pop, hand claps, warm female vocal",
})
for _, take := range song.Outputs {
	fmt.Println(take.Title, take.Seconds, take.URL, take.Cover)
}
```

`Mode: "Lyrics"` sings your own lyrics; `Mode: "Instrumental"` has no vocals.

## Runs and webhooks

```go
g, _ := client.Get(ctx, "cm…")
runs, next, _ := client.List(ctx, 20, 0)

// The finished run is POSTed to you instead of polling
client.GenerateVideo(ctx, tanvo.VideoParams{Prompt: "…", Model: "veo-3-1", Common: tanvo.Common{WebhookURL: "https://example.com/hooks/tanvo"}})

// In the handler, with the secret from Settings → Webhooks and the raw body
http.HandleFunc("/hooks/tanvo", func(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g, err := tanvo.VerifyWebhook(os.Getenv("TANVO_WEBHOOK_SECRET"), r.Header, body, 0)
	if err != nil {
		http.Error(w, "bad signature", http.StatusUnauthorized)
		return
	}
	// de-duplicate on r.Header.Get("webhook-id"), then use g.Outputs
	w.WriteHeader(http.StatusNoContent)
})
```

## Errors

Every API failure is a `*tanvo.Error` with a stable `Code`, the HTTP `Status` and a `Kind`. Match the kind with `errors.Is`:

| Sentinel | When | What to do |
|---|---|---|
| `tanvo.ErrInvalidRequest` | Bad option, refused prompt, file too large | Fix the input (`Issues` says what) |
| `tanvo.ErrAuth` | Missing or invalid key | Check the key |
| `tanvo.ErrQuota` | Out of credits, free tier limit, `busy` | Top up, add a key, or set `Common.WaitForSlot` |
| `tanvo.ErrService` | Upstream model or network failure | Already retried twice with the same request key |

Failed runs are refunded automatically.

## Settings

| | |
|---|---|
| `TANVO_API_KEY` | API key (or `tanvo.WithAPIKey`) |
| `TANVO_BASE_URL` | Another deployment (or `tanvo.WithBaseURL`; default `https://tanvo.ai`) |

Full API reference: [tanvo.ai/developers/api](https://tanvo.ai/developers/api?utm_source=github&utm_medium=referral).

## Also from Tanvo

- [tanvo-mcp](https://github.com/tanvoai/tanvo-mcp): Tanvo inside Claude, Cursor and other MCP clients
- [tanvo-python](https://github.com/tanvoai/tanvo-python) and [tanvo-js](https://github.com/tanvoai/tanvo-js): Python and JavaScript clients

## License

MIT
