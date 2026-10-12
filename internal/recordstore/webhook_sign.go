package recordstore

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
)

// SignWebhook returns the X-Varroa-Signature value for one delivery. The
// signed input is the decimal Unix timestamp, a period, and the exact
// request body. The value has the form "v1=<hex>". A receiver that
// verifies the signature must use the same input.
func SignWebhook(key []byte, ts int64, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strconv.FormatInt(ts, 10)))
	mac.Write([]byte("."))
	mac.Write(body)
	return "v1=" + hex.EncodeToString(mac.Sum(nil))
}
