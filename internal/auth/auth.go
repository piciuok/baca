// Package auth keeps the UI token out of URLs. The token (StateDir/token, mode 600) never
// travels in a link: the page is opened with a one-time code derived from it, exchanged for an
// HttpOnly cookie, and a client checks that the server on the port knows the token before
// opening anything (so another program squatting the port learns nothing).
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// CodeTTL is how long an opening code is valid.
const CodeTTL = 2 * time.Minute

// Cookie is the name of the session cookie.
const Cookie = "baca_session"

func mac(token, msg string) string {
	h := hmac.New(sha256.New, []byte(token))
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}

// Equal compares secrets in constant time; empty never matches.
func Equal(a, b string) bool {
	return a != "" && b != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Code is a one-time opening code: "<unix time>.<nonce>.<mac>".
func Code(token string, now time.Time) string {
	n := make([]byte, 8)
	_, _ = rand.Read(n)
	head := strconv.FormatInt(now.Unix(), 10) + "." + hex.EncodeToString(n)
	return head + "." + mac(token, "open:"+head)
}

// CheckCode tells whether code was made with token within CodeTTL of now.
func CheckCode(token, code string, now time.Time) bool {
	i := strings.LastIndex(code, ".")
	if i < 0 || token == "" {
		return false
	}
	head := code[:i]
	if !Equal(code[i+1:], mac(token, "open:"+head)) {
		return false
	}
	ts, err := strconv.ParseInt(strings.SplitN(head, ".", 2)[0], 10, 64)
	if err != nil {
		return false
	}
	age := now.Sub(time.Unix(ts, 0))
	return age > -10*time.Second && age <= CodeTTL
}

// Session is the cookie value: derived from the token, so it survives restarts and updates,
// and never appears in a URL.
func Session(token string) string { return mac(token, "session") }

// Answer proves knowledge of the token for a client's nonce.
func Answer(token, nonce string) string { return mac(token, "hello:"+nonce) }

// PageURL is the address that opens the UI once.
func PageURL(base, token string) string {
	return strings.TrimRight(base, "/") + "/?c=" + url.QueryEscape(Code(token, time.Now()))
}

// ErrNotBaca: something else answers on the port.
var ErrNotBaca = errors.New("the port is used by another program, not baca")

// Verify checks that the server at base knows token (GET /api/hello with a fresh nonce).
func Verify(ctx context.Context, base, token string) error {
	n := make([]byte, 16)
	_, _ = rand.Read(n)
	nonce := hex.EncodeToString(n)
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/hello?n="+nonce, nil)
	if err != nil {
		return err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var body struct {
		Answer string `json:"answer"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&body) != nil {
		return ErrNotBaca
	}
	if !Equal(body.Answer, Answer(token, nonce)) {
		return fmt.Errorf("%w (it does not know this user's token)", ErrNotBaca)
	}
	return nil
}
