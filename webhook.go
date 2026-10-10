package tanvo

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ErrWebhook is returned (wrapped) when a webhook's headers, timestamp or signature do not check out.
var ErrWebhook = errors.New("tanvo: webhook verification failed")

// VerifyWebhook checks a completion webhook (Standard Webhooks) and returns the Generation it carries.
// Pass the raw request body exactly as received. tolerance <= 0 means 5 minutes. The same webhook-id can arrive
// more than once (retries, manual resends): de-duplicate on it.
//
//	body, _ := io.ReadAll(r.Body)
//	g, err := tanvo.VerifyWebhook(secret, r.Header, body, 0)
func VerifyWebhook(secret string, header http.Header, body []byte, tolerance time.Duration) (*Generation, error) {
	if tolerance <= 0 {
		tolerance = 5 * time.Minute
	}
	id, ts, sig := header.Get("webhook-id"), header.Get("webhook-timestamp"), header.Get("webhook-signature")
	if id == "" || ts == "" || sig == "" {
		return nil, fmt.Errorf("%w: webhook-id, webhook-timestamp and webhook-signature are required", ErrWebhook)
	}
	sent, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: webhook-timestamp is not a number", ErrWebhook)
	}
	if math.Abs(float64(time.Now().Unix()-sent)) > tolerance.Seconds() {
		return nil, fmt.Errorf("%w: timestamp is more than %s from now", ErrWebhook, tolerance)
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil {
		return nil, fmt.Errorf("%w: the secret is not base64 after whsec_", ErrWebhook)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id + "." + ts + "."))
	mac.Write(body)
	expected := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	ok := false
	for _, part := range strings.Fields(sig) {
		if v, found := strings.CutPrefix(part, "v1,"); found && hmac.Equal([]byte(v), []byte(expected)) {
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("%w: signature does not match; check the secret under Settings → Webhooks", ErrWebhook)
	}
	var g Generation
	if err := json.Unmarshal(body, &g); err != nil {
		return nil, fmt.Errorf("%w: body is not a generation: %v", ErrWebhook, err)
	}
	return &g, nil
}
