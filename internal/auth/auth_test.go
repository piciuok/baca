package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCode(t *testing.T) {
	now := time.Now()
	c := Code("tok", now)
	if !CheckCode("tok", c, now.Add(time.Minute)) {
		t.Fatal("fresh code refused")
	}
	if CheckCode("tok", c, now.Add(CodeTTL+time.Second)) || CheckCode("other", c, now) || CheckCode("", c, now) {
		t.Fatal("stale, foreign or empty-token code accepted")
	}
	parts := strings.Split(c, ".")
	forged := parts[0] + "1." + parts[1] + "." + parts[2] // time changed, mac not
	if CheckCode("tok", forged, now) || CheckCode("tok", "nonsense", now) {
		t.Fatal("forged code accepted")
	}
	if Code("tok", now) == c {
		t.Fatal("codes repeat")
	}
}

func TestVerify(t *testing.T) {
	real := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"answer": Answer("tok", r.URL.Query().Get("n"))})
	}))
	defer real.Close()
	squatter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"answer": "guess"})
	}))
	defer squatter.Close()
	if err := Verify(context.Background(), real.URL, "tok"); err != nil {
		t.Fatal(err)
	}
	if err := Verify(context.Background(), squatter.URL, "tok"); err == nil {
		t.Fatal("squatter accepted")
	}
}
